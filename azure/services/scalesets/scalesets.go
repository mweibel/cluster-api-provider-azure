/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scalesets

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-11-01/compute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/pkg/errors"
	infrav1 "sigs.k8s.io/cluster-api-provider-azure/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-azure/azure"
	"sigs.k8s.io/cluster-api-provider-azure/azure/converters"
	"sigs.k8s.io/cluster-api-provider-azure/azure/services/resourceskus"
	azureutil "sigs.k8s.io/cluster-api-provider-azure/util/azure"
	"sigs.k8s.io/cluster-api-provider-azure/util/generators"
	"sigs.k8s.io/cluster-api-provider-azure/util/slice"
	"sigs.k8s.io/cluster-api-provider-azure/util/tele"
)

const serviceName = "scalesets"

type (
	// ScaleSetScope defines the scope interface for a scale sets service.
	ScaleSetScope interface {
		azure.ClusterDescriber
		azure.AsyncStatusUpdater
		GetBootstrapData(context.Context) (string, error)
		GetVMImage(context.Context) (*infrav1.Image, error)
		SaveVMImageToStatus(*infrav1.Image)
		MaxSurge() (int, error)
		ScaleSetSpec() azure.ScaleSetSpec
		VMSSExtensionSpecs() []azure.ResourceSpecGetter
		SetAnnotation(string, string)
		SetProviderID(string)
		SetVMSSState(*azure.VMSS)
	}

	// Service provides operations on Azure resources.
	Service struct {
		Scope ScaleSetScope
		Client
		resourceSKUCache *resourceskus.Cache
	}
)

// New creates a new service.
func New(scope ScaleSetScope, skuCache *resourceskus.Cache) *Service {
	return &Service{
		Client:           NewClient(scope),
		Scope:            scope,
		resourceSKUCache: skuCache,
	}
}

// Name returns the service name.
func (s *Service) Name() string {
	return serviceName
}

// Reconcile idempotently gets, creates, and updates a scale set.
func (s *Service) Reconcile(ctx context.Context) (retErr error) {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "scalesets.Service.Reconcile")
	defer done()

	if err := s.validateSpec(ctx); err != nil {
		// do as much early validation as possible to limit calls to Azure
		return err
	}

	var err error

	scaleSetSpec := s.Scope.ScaleSetSpec()

	// check if there is an ongoing long running operation
	var (
		future      = s.Scope.GetLongRunningOperationState(s.Scope.ScaleSetSpec().Name, serviceName)
		fetchedVMSS *azure.VMSS
	)

	defer func() {
		// save the updated state of the VMSS for the MachinePoolScope to use for updating K8s state
		if fetchedVMSS == nil {
			fetchedVMSS, err = s.getVirtualMachineScaleSet(ctx, scaleSetSpec.Name)
			if err != nil && !azure.ResourceNotFound(err) {
				log.Error(err, "failed to get vmss in deferred update")
			}
		}

		if fetchedVMSS != nil {
			// Transform the VMSS resource representation to conform to the cloud-provider-azure representation
			providerID, err := azureutil.ConvertResourceGroupNameToLower(azure.ProviderIDPrefix + fetchedVMSS.ID)
			if err != nil {
				log.Error(err, "failed to parse VMSS ID", "ID", fetchedVMSS.ID)
			}
			s.Scope.SetProviderID(providerID)
			s.Scope.SetVMSSState(fetchedVMSS)
		}
	}()

	if future == nil {
		fetchedVMSS, err = s.getVirtualMachineScaleSet(ctx, scaleSetSpec.Name)
	} else {
		fetchedVMSS, err = s.getVirtualMachineScaleSetIfDone(ctx, future)
	}

	switch {
	case err != nil && !azure.ResourceNotFound(err):
		// There was an error and it was not an HTTP 404 not found. This is either a transient error, like long running operation not done, or an Azure service error.
		return errors.Wrapf(err, "failed to get VMSS %s", scaleSetSpec.Name)
	case err != nil && azure.ResourceNotFound(err):
		// HTTP(404) resource was not found, so we need to create it with a PUT
		future, err = s.createVMSS(ctx)
		if err != nil {
			return errors.Wrap(err, "failed to start creating VMSS")
		}
	case err == nil:
		// HTTP(200)
		// VMSS already exists and may have changes; update it with a PATCH
		// we do this to avoid overwriting fields in networkProfile modified by cloud-provider
		future, err = s.patchVMSSIfNeeded(ctx, fetchedVMSS)
		if err != nil {
			return errors.Wrap(err, "failed to start updating VMSS")
		}
	}

	// Try to get the VMSS to update status if we have created a long running operation. If the VMSS is still in a long
	// running operation, getVirtualMachineScaleSetIfDone will return an azure.WithTransientError and requeue.
	if future != nil {
		fetchedVMSS, err = s.getVirtualMachineScaleSetIfDone(ctx, future)
		if err != nil {
			return errors.Wrapf(err, "failed to get VMSS %s after create or update", scaleSetSpec.Name)
		}
	}

	// If we get to here, we have completed any long running VMSS operations (creates / updates)
	s.Scope.DeleteLongRunningOperationState(s.Scope.ScaleSetSpec().Name, serviceName)
	// This also means that the VMSS extensions were successfully installed
	// Note: we want to handle UpdatePutStatus when VMSSExtensions have an error when scalesets become an async service
	s.Scope.UpdatePutStatus(infrav1.BootstrapSucceededCondition, serviceName, nil)

	return nil
}

// Delete deletes a scale set asynchronously. Delete sends a DELETE request to Azure and if accepted without error,
// the VMSS will be considered deleted. The actual delete in Azure may take longer, but should eventually complete.
func (s *Service) Delete(ctx context.Context) error {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "scalesets.Service.Delete")
	defer done()

	var err error

	vmssSpec := s.Scope.ScaleSetSpec()

	defer func() {
		// save the updated state of the VMSS for the MachinePoolScope to use for updating K8s state
		fetchedVMSS, err := s.getVirtualMachineScaleSet(ctx, vmssSpec.Name)
		if err != nil && !azure.ResourceNotFound(err) {
			log.Error(err, "failed to get vmss in deferred update")
		}

		if fetchedVMSS != nil {
			s.Scope.SetVMSSState(fetchedVMSS)
		}
	}()

	// check if there is an ongoing long running operation
	future := s.Scope.GetLongRunningOperationState(vmssSpec.Name, serviceName)
	if future != nil {
		// if the operation is not complete this will return an error
		_, err := s.GetResultIfDone(ctx, future)
		if err != nil {
			return errors.Wrap(err, "failed to get result from future")
		}

		// ScaleSet has been deleted
		s.Scope.DeleteLongRunningOperationState(vmssSpec.Name, serviceName)
		// Note: we want to handle UpdateDeleteStatus when VMSSExtensions have an error when scalesets become an async service
		s.Scope.UpdateDeleteStatus(infrav1.BootstrapSucceededCondition, serviceName, nil)

		return nil
	}

	// no long running delete operation is active, so delete the ScaleSet
	log.V(2).Info("deleting VMSS", "scale set", vmssSpec.Name)
	future, err = s.Client.DeleteAsync(ctx, s.Scope.ResourceGroup(), vmssSpec.Name)
	if err != nil {
		if azure.ResourceNotFound(err) {
			// already deleted
			return nil
		}
		return errors.Wrapf(err, "failed to delete VMSS %s in resource group %s", vmssSpec.Name, s.Scope.ResourceGroup())
	}

	s.Scope.SetLongRunningOperationState(future)
	if future != nil {
		// if future exists, check state of the future
		if _, err = s.GetResultIfDone(ctx, future); err != nil {
			return errors.Wrap(err, "not done with long running operation, or failed to get result")
		}
	}

	// future is either nil, or the result of the future is complete
	s.Scope.DeleteLongRunningOperationState(vmssSpec.Name, serviceName)
	// Note: we want to handle UpdateDeleteStatus when VMSSExtensions have an error when scalesets become an async service
	s.Scope.UpdateDeleteStatus(infrav1.BootstrapSucceededCondition, serviceName, nil)

	return nil
}

func (s *Service) createVMSS(ctx context.Context) (*infrav1.Future, error) {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "scalesets.Service.createVMSS")
	defer done()

	spec := s.Scope.ScaleSetSpec()

	vmss, err := s.buildVMSSFromSpec(ctx, spec)
	if err != nil {
		return nil, errors.Wrap(err, "failed building VMSS from spec")
	}

	future, err := s.Client.CreateOrUpdateAsync(ctx, s.Scope.ResourceGroup(), spec.Name, vmss)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create VMSS")
	}

	log.V(2).Info("starting to create VMSS", "scale set", spec.Name)
	s.Scope.SetLongRunningOperationState(future)
	return future, err
}

func (s *Service) patchVMSSIfNeeded(ctx context.Context, infraVMSS *azure.VMSS) (*infrav1.Future, error) {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "scalesets.Service.patchVMSSIfNeeded")
	defer done()

	spec := s.Scope.ScaleSetSpec()

	vmss, err := s.buildVMSSFromSpec(ctx, spec)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate scale set update parameters for %s", spec.Name)
	}

	patch, err := getVMSSUpdateFromVMSS(vmss)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate vmss patch for %s", spec.Name)
	}

	maxSurge, err := s.Scope.MaxSurge()
	if err != nil {
		return nil, errors.Wrap(err, "failed to calculate maxSurge")
	}

	hasModelChanges := hasModelModifyingDifferences(infraVMSS, vmss)
	if maxSurge > 0 && (hasModelChanges || !infraVMSS.HasEnoughLatestModelOrNotMixedModel()) {
		// surge capacity with the intention of lowering during instance reconciliation
		surge := spec.Capacity + int64(maxSurge)
		log.V(4).Info("surging...", "surge", surge)
		patch.Sku.Capacity = to.Int64Ptr(surge)
	}

	// If there are no model changes and no increase in the replica count, do not update the VMSS.
	// Decreases in replica count is handled by deleting AzureMachinePoolMachine instances in the MachinePoolScope
	if *patch.Sku.Capacity <= infraVMSS.Capacity && !hasModelChanges {
		log.V(4).Info("nothing to update on vmss", "scale set", spec.Name, "newReplicas", *patch.Sku.Capacity, "oldReplicas", infraVMSS.Capacity, "hasChanges", hasModelChanges)
		return nil, nil
	}

	log.V(4).Info("patching vmss", "scale set", spec.Name, "patch", patch)
	future, err := s.UpdateAsync(ctx, s.Scope.ResourceGroup(), spec.Name, patch)
	if err != nil {
		if azure.ResourceConflict(err) {
			return nil, azure.WithTransientError(err, 30*time.Second)
		}
		return nil, errors.Wrap(err, "failed updating VMSS")
	}

	s.Scope.SetLongRunningOperationState(future)
	log.V(2).Info("successfully started to update vmss", "scale set", spec.Name)
	return future, err
}

func hasModelModifyingDifferences(infraVMSS *azure.VMSS, vmss compute.VirtualMachineScaleSet) bool {
	other := converters.SDKToVMSS(vmss, []compute.VirtualMachineScaleSetVM{})
	return infraVMSS.HasModelChanges(*other)
}

func (s *Service) validateSpec(ctx context.Context) error {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scalesets.Service.validateSpec")
	defer done()

	spec := s.Scope.ScaleSetSpec()

	sku, err := s.resourceSKUCache.Get(ctx, spec.Size, resourceskus.VirtualMachines)
	if err != nil {
		return errors.Wrapf(err, "failed to get SKU %s in compute api", spec.Size)
	}

	// Checking if the requested VM size has at least 2 vCPUS
	vCPUCapability, err := sku.HasCapabilityWithCapacity(resourceskus.VCPUs, resourceskus.MinimumVCPUS)
	if err != nil {
		return azure.WithTerminalError(errors.Wrap(err, "failed to validate the vCPU capability"))
	}

	if !vCPUCapability {
		return azure.WithTerminalError(errors.New("vm size should be bigger or equal to at least 2 vCPUs"))
	}

	// Checking if the requested VM size has at least 2 Gi of memory
	MemoryCapability, err := sku.HasCapabilityWithCapacity(resourceskus.MemoryGB, resourceskus.MinimumMemory)
	if err != nil {
		return azure.WithTerminalError(errors.Wrap(err, "failed to validate the memory capability"))
	}

	if !MemoryCapability {
		return azure.WithTerminalError(errors.New("vm memory should be bigger or equal to at least 2Gi"))
	}

	// enable ephemeral OS
	if spec.OSDisk.DiffDiskSettings != nil && !sku.HasCapability(resourceskus.EphemeralOSDisk) {
		return azure.WithTerminalError(fmt.Errorf("vm size %s does not support ephemeral os. select a different vm size or disable ephemeral os", spec.Size))
	}

	if spec.SecurityProfile != nil && !sku.HasCapability(resourceskus.EncryptionAtHost) {
		return azure.WithTerminalError(errors.Errorf("encryption at host is not supported for VM type %s", spec.Size))
	}

	// Fetch location and zone to check for their support of ultra disks.
	location := s.Scope.Location()
	zones, err := s.resourceSKUCache.GetZones(ctx, location)
	if err != nil {
		return azure.WithTerminalError(errors.Wrapf(err, "failed to get the zones for location %s", location))
	}

	for _, zone := range zones {
		hasLocationCapability := sku.HasLocationCapability(resourceskus.UltraSSDAvailable, location, zone)
		err := fmt.Errorf("vm size %s does not support ultra disks in location %s. select a different vm size or disable ultra disks", spec.Size, location)

		// Check support for ultra disks as data disks.
		for _, disks := range spec.DataDisks {
			if disks.ManagedDisk != nil &&
				disks.ManagedDisk.StorageAccountType == string(compute.StorageAccountTypesUltraSSDLRS) &&
				!hasLocationCapability {
				return azure.WithTerminalError(err)
			}
		}
		// Check support for ultra disks as persistent volumes.
		if spec.AdditionalCapabilities != nil && spec.AdditionalCapabilities.UltraSSDEnabled != nil {
			if *spec.AdditionalCapabilities.UltraSSDEnabled &&
				!hasLocationCapability {
				return azure.WithTerminalError(err)
			}
		}
	}

	// Checking if selected availability zones are available selected VM type in location
	azsInLocation, err := s.resourceSKUCache.GetZonesWithVMSize(ctx, spec.Size, s.Scope.Location())
	if err != nil {
		return errors.Wrapf(err, "failed to get zones for VM type %s in location %s", spec.Size, s.Scope.Location())
	}

	for _, az := range spec.FailureDomains {
		if !slice.Contains(azsInLocation, az) {
			return azure.WithTerminalError(errors.Errorf("availability zone %s is not available for VM type %s in location %s", az, spec.Size, s.Scope.Location()))
		}
	}

	return nil
}

func (s *Service) buildVMSSFromSpec(ctx context.Context, vmssSpec azure.ScaleSetSpec) (compute.VirtualMachineScaleSet, error) {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scalesets.Service.buildVMSSFromSpec")
	defer done()

	sku, err := s.resourceSKUCache.Get(ctx, vmssSpec.Size, resourceskus.VirtualMachines)
	if err != nil {
		return compute.VirtualMachineScaleSet{}, errors.Wrapf(err, "failed to get find SKU %s in compute api", vmssSpec.Size)
	}

	if vmssSpec.AcceleratedNetworking == nil {
		// set accelerated networking to the capability of the VMSize
		accelNet := sku.HasCapability(resourceskus.AcceleratedNetworking)
		vmssSpec.AcceleratedNetworking = &accelNet
	}

	extensions, err := s.generateExtensions()
	if err != nil {
		return compute.VirtualMachineScaleSet{}, err
	}

	storageProfile, err := s.generateStorageProfile(ctx, vmssSpec, sku)
	if err != nil {
		return compute.VirtualMachineScaleSet{}, err
	}

	securityProfile, err := getSecurityProfile(vmssSpec, sku)
	if err != nil {
		return compute.VirtualMachineScaleSet{}, err
	}

	priority, evictionPolicy, billingProfile, err := converters.GetSpotVMOptions(vmssSpec.SpotVMOptions)
	if err != nil {
		return compute.VirtualMachineScaleSet{}, errors.Wrapf(err, "failed to get Spot VM options")
	}

	// Get the node outbound LB backend pool ID
	var backendAddressPools []compute.SubResource
	if vmssSpec.PublicLBName != "" {
		if vmssSpec.PublicLBAddressPoolName != "" {
			backendAddressPools = append(backendAddressPools,
				compute.SubResource{
					ID: to.StringPtr(azure.AddressPoolID(s.Scope.SubscriptionID(), s.Scope.ResourceGroup(), vmssSpec.PublicLBName, vmssSpec.PublicLBAddressPoolName)),
				})
		}
	}

	osProfile, err := s.generateOSProfile(ctx, vmssSpec)
	if err != nil {
		return compute.VirtualMachineScaleSet{}, err
	}

	vmss := compute.VirtualMachineScaleSet{
		Location: to.StringPtr(s.Scope.Location()),
		Sku: &compute.Sku{
			Name:     to.StringPtr(vmssSpec.Size),
			Tier:     to.StringPtr("Standard"),
			Capacity: to.Int64Ptr(vmssSpec.Capacity),
		},
		Zones: to.StringSlicePtr(vmssSpec.FailureDomains),
		Plan:  s.generateImagePlan(ctx),
		VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
			SinglePlacementGroup: to.BoolPtr(false),
			UpgradePolicy: &compute.UpgradePolicy{
				Mode: compute.UpgradeModeManual,
			},
			Overprovision: to.BoolPtr(false),
			VirtualMachineProfile: &compute.VirtualMachineScaleSetVMProfile{
				OsProfile:       osProfile,
				StorageProfile:  storageProfile,
				SecurityProfile: securityProfile,
				DiagnosticsProfile: &compute.DiagnosticsProfile{
					BootDiagnostics: &compute.BootDiagnostics{
						Enabled: to.BoolPtr(true),
					},
				},
				NetworkProfile: &compute.VirtualMachineScaleSetNetworkProfile{
					NetworkInterfaceConfigurations: &[]compute.VirtualMachineScaleSetNetworkConfiguration{
						{
							Name: to.StringPtr(vmssSpec.Name),
							VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
								Primary:            to.BoolPtr(true),
								EnableIPForwarding: to.BoolPtr(true),
								IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
									{
										Name: to.StringPtr(vmssSpec.Name),
										VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
											Subnet: &compute.APIEntityReference{
												ID: to.StringPtr(azure.SubnetID(s.Scope.SubscriptionID(), vmssSpec.VNetResourceGroup, vmssSpec.VNetName, vmssSpec.SubnetName)),
											},
											Primary:                         to.BoolPtr(true),
											PrivateIPAddressVersion:         compute.IPVersionIPv4,
											LoadBalancerBackendAddressPools: &backendAddressPools,
										},
									},
								},
								EnableAcceleratedNetworking: vmssSpec.AcceleratedNetworking,
							},
						},
					},
				},
				Priority:       priority,
				EvictionPolicy: evictionPolicy,
				BillingProfile: billingProfile,
				ExtensionProfile: &compute.VirtualMachineScaleSetExtensionProfile{
					Extensions: &extensions,
				},
			},
		},
	}

	// Assign Identity to VMSS
	if vmssSpec.Identity == infrav1.VMIdentitySystemAssigned {
		vmss.Identity = &compute.VirtualMachineScaleSetIdentity{
			Type: compute.ResourceIdentityTypeSystemAssigned,
		}
	} else if vmssSpec.Identity == infrav1.VMIdentityUserAssigned {
		userIdentitiesMap, err := converters.UserAssignedIdentitiesToVMSSSDK(vmssSpec.UserAssignedIdentities)
		if err != nil {
			return vmss, errors.Wrapf(err, "failed to assign identity %q", vmssSpec.Name)
		}
		vmss.Identity = &compute.VirtualMachineScaleSetIdentity{
			Type:                   compute.ResourceIdentityTypeUserAssigned,
			UserAssignedIdentities: userIdentitiesMap,
		}
	}

	// Provisionally detect whether there is any Data Disk defined which uses UltraSSDs.
	// If that's the case, enable the UltraSSD capability.
	for _, dataDisk := range vmssSpec.DataDisks {
		if dataDisk.ManagedDisk != nil && dataDisk.ManagedDisk.StorageAccountType == string(compute.StorageAccountTypesUltraSSDLRS) {
			vmss.VirtualMachineScaleSetProperties.AdditionalCapabilities = &compute.AdditionalCapabilities{
				UltraSSDEnabled: to.BoolPtr(true),
			}
		}
	}

	// Set Additional Capabilities if any is present on the spec.
	if vmssSpec.AdditionalCapabilities != nil {
		// Set UltraSSDEnabled if a specific value is set on the spec for it.
		if vmssSpec.AdditionalCapabilities.UltraSSDEnabled != nil {
			vmss.AdditionalCapabilities.UltraSSDEnabled = vmssSpec.AdditionalCapabilities.UltraSSDEnabled
		}
	}

	if vmssSpec.TerminateNotificationTimeout != nil {
		vmss.VirtualMachineScaleSetProperties.VirtualMachineProfile.ScheduledEventsProfile = &compute.ScheduledEventsProfile{
			TerminateNotificationProfile: &compute.TerminateNotificationProfile{
				NotBeforeTimeout: to.StringPtr(fmt.Sprintf("PT%dM", *vmssSpec.TerminateNotificationTimeout)),
				Enable:           to.BoolPtr(true),
			},
		}
	}

	tags := infrav1.Build(infrav1.BuildParams{
		ClusterName: s.Scope.ClusterName(),
		Lifecycle:   infrav1.ResourceLifecycleOwned,
		Name:        to.StringPtr(vmssSpec.Name),
		Role:        to.StringPtr(infrav1.Node),
		Additional:  s.Scope.AdditionalTags(),
	})

	vmss.Tags = converters.TagsToMap(tags)
	return vmss, nil
}

// getVirtualMachineScaleSet provides information about a Virtual Machine Scale Set and its instances.
func (s *Service) getVirtualMachineScaleSet(ctx context.Context, vmssName string) (*azure.VMSS, error) {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scalesets.Service.getVirtualMachineScaleSet")
	defer done()

	vmss, err := s.Client.Get(ctx, s.Scope.ResourceGroup(), vmssName)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get existing vmss")
	}

	vmssInstances, err := s.Client.ListInstances(ctx, s.Scope.ResourceGroup(), vmssName)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list instances")
	}

	return converters.SDKToVMSS(vmss, vmssInstances), nil
}

// getVirtualMachineScaleSetIfDone gets a Virtual Machine Scale Set and its instances from Azure if the future is completed.
func (s *Service) getVirtualMachineScaleSetIfDone(ctx context.Context, future *infrav1.Future) (*azure.VMSS, error) {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scalesets.Service.getVirtualMachineScaleSetIfDone")
	defer done()

	vmss, err := s.GetResultIfDone(ctx, future)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get result from future")
	}

	vmssInstances, err := s.Client.ListInstances(ctx, future.ResourceGroup, future.Name)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list instances")
	}

	return converters.SDKToVMSS(vmss, vmssInstances), nil
}

func (s *Service) generateExtensions() ([]compute.VirtualMachineScaleSetExtension, error) {
	extensions := make([]compute.VirtualMachineScaleSetExtension, len(s.Scope.VMSSExtensionSpecs()))
	for i, extensionSpec := range s.Scope.VMSSExtensionSpecs() {
		extensionSpec := extensionSpec
		parameters, err := extensionSpec.Parameters(nil)
		if err != nil {
			return nil, err
		}
		vmssextension, ok := parameters.(compute.VirtualMachineScaleSetExtension)
		if !ok {
			return nil, errors.Errorf("%T is not a compute.VirtualMachineScaleSetExtension", parameters)
		}
		extensions[i] = vmssextension
	}

	return extensions, nil
}

// generateStorageProfile generates a pointer to a compute.VirtualMachineScaleSetStorageProfile which can utilized for VM creation.
func (s *Service) generateStorageProfile(ctx context.Context, vmssSpec azure.ScaleSetSpec, sku resourceskus.SKU) (*compute.VirtualMachineScaleSetStorageProfile, error) {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scalesets.Service.generateStorageProfile")
	defer done()

	storageProfile := &compute.VirtualMachineScaleSetStorageProfile{
		OsDisk: &compute.VirtualMachineScaleSetOSDisk{
			OsType:       compute.OperatingSystemTypes(vmssSpec.OSDisk.OSType),
			CreateOption: compute.DiskCreateOptionTypesFromImage,
			DiskSizeGB:   vmssSpec.OSDisk.DiskSizeGB,
		},
	}

	// enable ephemeral OS
	if vmssSpec.OSDisk.DiffDiskSettings != nil {
		if !sku.HasCapability(resourceskus.EphemeralOSDisk) {
			return nil, fmt.Errorf("vm size %s does not support ephemeral os. select a different vm size or disable ephemeral os", vmssSpec.Size)
		}

		storageProfile.OsDisk.DiffDiskSettings = &compute.DiffDiskSettings{
			Option: compute.DiffDiskOptions(vmssSpec.OSDisk.DiffDiskSettings.Option),
		}
	}

	if vmssSpec.OSDisk.ManagedDisk != nil {
		storageProfile.OsDisk.ManagedDisk = &compute.VirtualMachineScaleSetManagedDiskParameters{}
		if vmssSpec.OSDisk.ManagedDisk.StorageAccountType != "" {
			storageProfile.OsDisk.ManagedDisk.StorageAccountType = compute.StorageAccountTypes(vmssSpec.OSDisk.ManagedDisk.StorageAccountType)
		}
		if vmssSpec.OSDisk.ManagedDisk.DiskEncryptionSet != nil {
			storageProfile.OsDisk.ManagedDisk.DiskEncryptionSet = &compute.DiskEncryptionSetParameters{ID: to.StringPtr(vmssSpec.OSDisk.ManagedDisk.DiskEncryptionSet.ID)}
		}
	}

	dataDisks := make([]compute.VirtualMachineScaleSetDataDisk, len(vmssSpec.DataDisks))
	for i, disk := range vmssSpec.DataDisks {
		dataDisks[i] = compute.VirtualMachineScaleSetDataDisk{
			CreateOption: compute.DiskCreateOptionTypesEmpty,
			DiskSizeGB:   to.Int32Ptr(disk.DiskSizeGB),
			Lun:          disk.Lun,
			Name:         to.StringPtr(azure.GenerateDataDiskName(vmssSpec.Name, disk.NameSuffix)),
		}

		if disk.ManagedDisk != nil {
			dataDisks[i].ManagedDisk = &compute.VirtualMachineScaleSetManagedDiskParameters{
				StorageAccountType: compute.StorageAccountTypes(disk.ManagedDisk.StorageAccountType),
			}

			if disk.ManagedDisk.DiskEncryptionSet != nil {
				dataDisks[i].ManagedDisk.DiskEncryptionSet = &compute.DiskEncryptionSetParameters{ID: to.StringPtr(disk.ManagedDisk.DiskEncryptionSet.ID)}
			}
		}
	}
	storageProfile.DataDisks = &dataDisks

	image, err := s.Scope.GetVMImage(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get VM image")
	}

	s.Scope.SaveVMImageToStatus(image)

	imageRef, err := converters.ImageToSDK(image)
	if err != nil {
		return nil, err
	}

	storageProfile.ImageReference = imageRef

	return storageProfile, nil
}

func (s *Service) generateOSProfile(ctx context.Context, vmssSpec azure.ScaleSetSpec) (*compute.VirtualMachineScaleSetOSProfile, error) {
	sshKey, err := base64.StdEncoding.DecodeString(vmssSpec.SSHKeyData)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode ssh public key")
	}
	bootstrapData, err := s.Scope.GetBootstrapData(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to retrieve bootstrap data")
	}

	osProfile := &compute.VirtualMachineScaleSetOSProfile{
		ComputerNamePrefix: to.StringPtr(vmssSpec.Name),
		AdminUsername:      to.StringPtr(azure.DefaultUserName),
		CustomData:         to.StringPtr(bootstrapData),
	}

	switch vmssSpec.OSDisk.OSType {
	case string(compute.OperatingSystemTypesWindows):
		// Cloudbase-init is used to generate a password.
		// https://cloudbase-init.readthedocs.io/en/latest/plugins.html#setting-password-main
		//
		// We generate a random password here in case of failure
		// but the password on the VM will NOT be the same as created here.
		// Access is provided via SSH public key that is set during deployment
		// Azure also provides a way to reset user passwords in the case of need.
		osProfile.AdminPassword = to.StringPtr(generators.SudoRandomPassword(123))
		osProfile.WindowsConfiguration = &compute.WindowsConfiguration{
			EnableAutomaticUpdates: to.BoolPtr(false),
		}
	default:
		osProfile.LinuxConfiguration = &compute.LinuxConfiguration{
			DisablePasswordAuthentication: to.BoolPtr(true),
			SSH: &compute.SSHConfiguration{
				PublicKeys: &[]compute.SSHPublicKey{
					{
						Path:    to.StringPtr(fmt.Sprintf("/home/%s/.ssh/authorized_keys", azure.DefaultUserName)),
						KeyData: to.StringPtr(string(sshKey)),
					},
				},
			},
		}
	}

	return osProfile, nil
}

func (s *Service) generateImagePlan(ctx context.Context) *compute.Plan {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "scalesets.Service.generateImagePlan")
	defer done()

	image, err := s.Scope.GetVMImage(ctx)
	if err != nil {
		log.Error(err, "failed to get vm image, disabling Plan")
		return nil
	}

	if image.SharedGallery != nil && image.SharedGallery.Publisher != nil && image.SharedGallery.SKU != nil && image.SharedGallery.Offer != nil {
		return &compute.Plan{
			Publisher: image.SharedGallery.Publisher,
			Name:      image.SharedGallery.SKU,
			Product:   image.SharedGallery.Offer,
		}
	}

	if image.Marketplace == nil || !image.Marketplace.ThirdPartyImage {
		return nil
	}

	if image.Marketplace.Publisher == "" || image.Marketplace.SKU == "" || image.Marketplace.Offer == "" {
		return nil
	}

	return &compute.Plan{
		Publisher: to.StringPtr(image.Marketplace.Publisher),
		Name:      to.StringPtr(image.Marketplace.SKU),
		Product:   to.StringPtr(image.Marketplace.Offer),
	}
}

func getVMSSUpdateFromVMSS(vmss compute.VirtualMachineScaleSet) (compute.VirtualMachineScaleSetUpdate, error) {
	jsonData, err := vmss.MarshalJSON()
	if err != nil {
		return compute.VirtualMachineScaleSetUpdate{}, err
	}

	var update compute.VirtualMachineScaleSetUpdate
	if err := update.UnmarshalJSON(jsonData); err != nil {
		return update, err
	}

	// wipe out network profile, so updates won't conflict with Cloud Provider updates
	update.VirtualMachineProfile.NetworkProfile = nil
	return update, nil
}

func getSecurityProfile(vmssSpec azure.ScaleSetSpec, sku resourceskus.SKU) (*compute.SecurityProfile, error) {
	if vmssSpec.SecurityProfile == nil {
		return nil, nil
	}

	if !sku.HasCapability(resourceskus.EncryptionAtHost) {
		return nil, azure.WithTerminalError(errors.Errorf("encryption at host is not supported for VM type %s", vmssSpec.Size))
	}

	return &compute.SecurityProfile{
		EncryptionAtHost: to.BoolPtr(*vmssSpec.SecurityProfile.EncryptionAtHost),
	}, nil
}

// IsManaged returns always returns true as CAPZ does not support BYO scale set.
func (s *Service) IsManaged(ctx context.Context) (bool, error) {
	return true, nil
}
