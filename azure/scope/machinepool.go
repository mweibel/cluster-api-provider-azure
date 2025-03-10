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

package scope

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"github.com/Azure/go-autorest/autorest/to"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	infrav1 "sigs.k8s.io/cluster-api-provider-azure/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-azure/azure"
	machinepool "sigs.k8s.io/cluster-api-provider-azure/azure/scope/strategies/machinepool_deployments"
	"sigs.k8s.io/cluster-api-provider-azure/azure/services/roleassignments"
	"sigs.k8s.io/cluster-api-provider-azure/azure/services/scalesets"
	"sigs.k8s.io/cluster-api-provider-azure/azure/services/virtualmachineimages"
	infrav1exp "sigs.k8s.io/cluster-api-provider-azure/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-azure/util/futures"
	"sigs.k8s.io/cluster-api-provider-azure/util/tele"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/noderefutil"
	capierrors "sigs.k8s.io/cluster-api/errors"
	expv1 "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ScalesetsServiceName is the name of the scalesets service.
// TODO: move this to scalesets.go once we remove the usage in this package,
// added here to avoid a circular dependency.
const ScalesetsServiceName = "scalesets"

type (
	// MachinePoolScopeParams defines the input parameters used to create a new MachinePoolScope.
	MachinePoolScopeParams struct {
		Client           client.Client
		MachinePool      *expv1.MachinePool
		AzureMachinePool *infrav1exp.AzureMachinePool
		ClusterScope     azure.ClusterScoper
	}

	// MachinePoolScope defines a scope defined around a machine pool and its cluster.
	MachinePoolScope struct {
		azure.ClusterScoper
		AzureMachinePool *infrav1exp.AzureMachinePool
		MachinePool      *expv1.MachinePool
		client           client.Client
		patchHelper      *patch.Helper
		vmssState        *azure.VMSS
	}

	// NodeStatus represents the status of a Kubernetes node.
	NodeStatus struct {
		Ready   bool
		Version string
	}
)

// NewMachinePoolScope creates a new MachinePoolScope from the supplied parameters.
// This is meant to be called for each reconcile iteration.
func NewMachinePoolScope(params MachinePoolScopeParams) (*MachinePoolScope, error) {
	if params.Client == nil {
		return nil, errors.New("client is required when creating a MachinePoolScope")
	}

	if params.MachinePool == nil {
		return nil, errors.New("machine pool is required when creating a MachinePoolScope")
	}

	if params.AzureMachinePool == nil {
		return nil, errors.New("azure machine pool is required when creating a MachinePoolScope")
	}

	helper, err := patch.NewHelper(params.AzureMachinePool, params.Client)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init patch helper")
	}

	return &MachinePoolScope{
		client:           params.Client,
		MachinePool:      params.MachinePool,
		AzureMachinePool: params.AzureMachinePool,
		patchHelper:      helper,
		ClusterScoper:    params.ClusterScope,
	}, nil
}

// ScaleSetSpec returns the scale set spec.
func (m *MachinePoolScope) ScaleSetSpec() azure.ScaleSetSpec {
	return azure.ScaleSetSpec{
		Name:                         m.Name(),
		Size:                         m.AzureMachinePool.Spec.Template.VMSize,
		Capacity:                     int64(to.Int32(m.MachinePool.Spec.Replicas)),
		SSHKeyData:                   m.AzureMachinePool.Spec.Template.SSHPublicKey,
		OSDisk:                       m.AzureMachinePool.Spec.Template.OSDisk,
		DataDisks:                    m.AzureMachinePool.Spec.Template.DataDisks,
		SubnetName:                   m.AzureMachinePool.Spec.Template.SubnetName,
		VNetName:                     m.Vnet().Name,
		VNetResourceGroup:            m.Vnet().ResourceGroup,
		PublicLBName:                 m.OutboundLBName(infrav1.Node),
		PublicLBAddressPoolName:      azure.GenerateOutboundBackendAddressPoolName(m.OutboundLBName(infrav1.Node)),
		AcceleratedNetworking:        m.AzureMachinePool.Spec.Template.AcceleratedNetworking,
		Identity:                     m.AzureMachinePool.Spec.Identity,
		UserAssignedIdentities:       m.AzureMachinePool.Spec.UserAssignedIdentities,
		SecurityProfile:              m.AzureMachinePool.Spec.Template.SecurityProfile,
		SpotVMOptions:                m.AzureMachinePool.Spec.Template.SpotVMOptions,
		FailureDomains:               m.MachinePool.Spec.FailureDomains,
		TerminateNotificationTimeout: m.AzureMachinePool.Spec.Template.TerminateNotificationTimeout,
	}
}

// Name returns the Azure Machine Pool Name.
func (m *MachinePoolScope) Name() string {
	// Windows Machine pools names cannot be longer than 9 chars
	if m.AzureMachinePool.Spec.Template.OSDisk.OSType == azure.WindowsOS && len(m.AzureMachinePool.Name) > 9 {
		return "win-" + m.AzureMachinePool.Name[len(m.AzureMachinePool.Name)-5:]
	}
	return m.AzureMachinePool.Name
}

// ProviderID returns the AzureMachinePool ID by parsing Spec.FakeProviderID.
func (m *MachinePoolScope) ProviderID() string {
	parsed, err := noderefutil.NewProviderID(m.AzureMachinePool.Spec.ProviderID)
	if err != nil {
		return ""
	}
	return parsed.ID()
}

// SetProviderID sets the AzureMachinePool providerID in spec.
func (m *MachinePoolScope) SetProviderID(v string) {
	m.AzureMachinePool.Spec.ProviderID = v
}

// ProvisioningState returns the AzureMachinePool provisioning state.
func (m *MachinePoolScope) ProvisioningState() infrav1.ProvisioningState {
	if m.AzureMachinePool.Status.ProvisioningState != nil {
		return *m.AzureMachinePool.Status.ProvisioningState
	}
	return ""
}

// SetVMSSState updates the machine pool scope with the current state of the VMSS.
func (m *MachinePoolScope) SetVMSSState(vmssState *azure.VMSS) {
	m.vmssState = vmssState
}

// NeedsRequeue return true if any machines are not on the latest model or the VMSS is not in a terminal provisioning
// state.
func (m *MachinePoolScope) NeedsRequeue() bool {
	state := m.AzureMachinePool.Status.ProvisioningState
	if m.vmssState == nil {
		return state != nil && infrav1.IsTerminalProvisioningState(*state)
	}

	if !m.vmssState.HasLatestModelAppliedToAll() {
		return true
	}

	desiredMatchesActual := len(m.vmssState.Instances) == int(m.DesiredReplicas())
	return !(state != nil && infrav1.IsTerminalProvisioningState(*state) && desiredMatchesActual)
}

// DesiredReplicas returns the replica count on machine pool or 0 if machine pool replicas is nil.
func (m MachinePoolScope) DesiredReplicas() int32 {
	return to.Int32(m.MachinePool.Spec.Replicas)
}

// MaxSurge returns the number of machines to surge, or 0 if the deployment strategy does not support surge.
func (m MachinePoolScope) MaxSurge() (int, error) {
	if surger, ok := m.getDeploymentStrategy().(machinepool.Surger); ok {
		surgeCount, err := surger.Surge(int(m.DesiredReplicas()))
		if err != nil {
			return 0, errors.Wrap(err, "failed to calculate surge for the machine pool")
		}

		return surgeCount, nil
	}

	return 0, nil
}

// updateReplicasAndProviderIDs ties the Azure VMSS instance data and the Node status data together to build and update
// the AzureMachinePool replica count and providerIDList.
func (m *MachinePoolScope) updateReplicasAndProviderIDs(ctx context.Context) error {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scope.MachinePoolScope.UpdateInstanceStatuses")
	defer done()

	machines, err := m.getMachinePoolMachines(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to get machine pool machines")
	}

	var readyReplicas int32
	providerIDs := make([]string, len(machines))
	for i, machine := range machines {
		if machine.Status.Ready {
			readyReplicas++
		}
		providerIDs[i] = machine.Spec.ProviderID
	}

	m.AzureMachinePool.Status.Replicas = readyReplicas
	m.AzureMachinePool.Spec.ProviderIDList = providerIDs
	return nil
}

func (m *MachinePoolScope) getMachinePoolMachines(ctx context.Context) ([]infrav1exp.AzureMachinePoolMachine, error) {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scope.MachinePoolScope.getMachinePoolMachines")
	defer done()

	labels := map[string]string{
		clusterv1.ClusterLabelName:      m.ClusterName(),
		infrav1exp.MachinePoolNameLabel: m.AzureMachinePool.Name,
	}
	ampml := &infrav1exp.AzureMachinePoolMachineList{}
	if err := m.client.List(ctx, ampml, client.InNamespace(m.AzureMachinePool.Namespace), client.MatchingLabels(labels)); err != nil {
		return nil, errors.Wrap(err, "failed to list AzureMachinePoolMachines")
	}

	return ampml.Items, nil
}

func (m *MachinePoolScope) applyAzureMachinePoolMachines(ctx context.Context) error {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "scope.MachinePoolScope.applyAzureMachinePoolMachines")
	defer done()

	if m.vmssState == nil {
		log.Info("vmssState is nil")
		return nil
	}

	labels := map[string]string{
		clusterv1.ClusterLabelName:      m.ClusterName(),
		infrav1exp.MachinePoolNameLabel: m.AzureMachinePool.Name,
	}
	ampml := &infrav1exp.AzureMachinePoolMachineList{}
	if err := m.client.List(ctx, ampml, client.InNamespace(m.AzureMachinePool.Namespace), client.MatchingLabels(labels)); err != nil {
		return errors.Wrap(err, "failed to list AzureMachinePoolMachines")
	}

	existingMachinesByProviderID := make(map[string]infrav1exp.AzureMachinePoolMachine, len(ampml.Items))
	for _, machine := range ampml.Items {
		existingMachinesByProviderID[machine.Spec.ProviderID] = machine
	}

	// determine which machines need to be created to reflect the current state in Azure
	azureMachinesByProviderID := m.vmssState.InstancesByProviderID()
	for key, val := range azureMachinesByProviderID {
		if _, ok := existingMachinesByProviderID[key]; !ok {
			log.V(4).Info("creating AzureMachinePoolMachine", "providerID", key)
			if err := m.createMachine(ctx, val); err != nil {
				return errors.Wrap(err, "failed creating AzureMachinePoolMachine")
			}
			continue
		}
	}

	deleted := false
	// delete machines that no longer exist in Azure
	for key, machine := range existingMachinesByProviderID {
		machine := machine
		if _, ok := azureMachinesByProviderID[key]; !ok {
			deleted = true
			log.V(4).Info("deleting AzureMachinePoolMachine because it no longer exists in the VMSS", "providerID", key)
			delete(existingMachinesByProviderID, key)
			if err := m.client.Delete(ctx, &machine); err != nil {
				return errors.Wrap(err, "failed deleting AzureMachinePoolMachine to reduce replica count")
			}
		}
	}

	if deleted {
		log.V(4).Info("exiting early due to finding AzureMachinePoolMachine(s) that were deleted because they no longer exist in the VMSS")
		// exit early to be less greedy about delete
		return nil
	}

	if futures.Has(m.AzureMachinePool, m.Name(), ScalesetsServiceName) {
		log.V(4).Info("exiting early due an in-progress long running operation on the ScaleSet")
		// exit early to be less greedy about delete
		return nil
	}

	deleteSelector := m.getDeploymentStrategy()
	if deleteSelector == nil {
		log.V(4).Info("can not select AzureMachinePoolMachines to delete because no deployment strategy is specified")
		return nil
	}

	// select machines to delete to lower the replica count
	toDelete, err := deleteSelector.SelectMachinesToDelete(ctx, m.DesiredReplicas(), existingMachinesByProviderID)
	if err != nil {
		return errors.Wrap(err, "failed selecting AzureMachinePoolMachine(s) to delete")
	}

	for _, machine := range toDelete {
		machine := machine
		log.Info("deleting selected AzureMachinePoolMachine", "providerID", machine.Spec.ProviderID)
		if err := m.client.Delete(ctx, &machine); err != nil {
			return errors.Wrap(err, "failed deleting AzureMachinePoolMachine to reduce replica count")
		}
	}

	log.V(4).Info("done reconciling AzureMachinePoolMachine(s)")
	return nil
}

func (m *MachinePoolScope) createMachine(ctx context.Context, machine azure.VMSSVM) error {
	if machine.InstanceID == "" {
		return errors.New("machine.InstanceID must not be empty")
	}

	if machine.Name == "" {
		return errors.New("machine.Name must not be empty")
	}

	ampm := infrav1exp.AzureMachinePoolMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.Join([]string{m.AzureMachinePool.Name, machine.InstanceID}, "-"),
			Namespace: m.AzureMachinePool.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         infrav1exp.GroupVersion.String(),
					Kind:               "AzureMachinePool",
					Name:               m.AzureMachinePool.Name,
					BlockOwnerDeletion: to.BoolPtr(true),
					UID:                m.AzureMachinePool.UID,
				},
			},
			Labels: map[string]string{
				m.ClusterName():                 string(infrav1.ResourceLifecycleOwned),
				clusterv1.ClusterLabelName:      m.ClusterName(),
				infrav1exp.MachinePoolNameLabel: m.AzureMachinePool.Name,
			},
		},
		Spec: infrav1exp.AzureMachinePoolMachineSpec{
			ProviderID: machine.ProviderID(),
			InstanceID: machine.InstanceID,
		},
	}

	controllerutil.AddFinalizer(&ampm, infrav1exp.AzureMachinePoolMachineFinalizer)
	conditions.MarkFalse(&ampm, infrav1.VMRunningCondition, string(infrav1.Creating), clusterv1.ConditionSeverityInfo, "")
	if err := m.client.Create(ctx, &ampm); err != nil {
		return errors.Wrapf(err, "failed creating AzureMachinePoolMachine %s in AzureMachinePool %s", machine.ID, m.AzureMachinePool.Name)
	}

	return nil
}

// SetLongRunningOperationState will set the future on the AzureMachinePool status to allow the resource to continue
// in the next reconciliation.
func (m *MachinePoolScope) SetLongRunningOperationState(future *infrav1.Future) {
	futures.Set(m.AzureMachinePool, future)
}

// GetLongRunningOperationState will get the future on the AzureMachinePool status.
func (m *MachinePoolScope) GetLongRunningOperationState(name, service string) *infrav1.Future {
	return futures.Get(m.AzureMachinePool, name, service)
}

// DeleteLongRunningOperationState will delete the future from the AzureMachinePool status.
func (m *MachinePoolScope) DeleteLongRunningOperationState(name, service string) {
	futures.Delete(m.AzureMachinePool, name, service)
}

// setProvisioningStateAndConditions sets the AzureMachinePool provisioning state and conditions.
func (m *MachinePoolScope) setProvisioningStateAndConditions(v infrav1.ProvisioningState) {
	m.AzureMachinePool.Status.ProvisioningState = &v
	switch {
	case v == infrav1.Succeeded && *m.MachinePool.Spec.Replicas == m.AzureMachinePool.Status.Replicas:
		// vmss is provisioned with enough ready replicas
		conditions.MarkTrue(m.AzureMachinePool, infrav1.ScaleSetRunningCondition)
		conditions.MarkTrue(m.AzureMachinePool, infrav1.ScaleSetModelUpdatedCondition)
		conditions.MarkTrue(m.AzureMachinePool, infrav1.ScaleSetDesiredReplicasCondition)
		m.SetReady()
	case v == infrav1.Succeeded && *m.MachinePool.Spec.Replicas != m.AzureMachinePool.Status.Replicas:
		// not enough ready or too many ready replicas we must still be scaling up or down
		updatingState := infrav1.Updating
		m.AzureMachinePool.Status.ProvisioningState = &updatingState
		if *m.MachinePool.Spec.Replicas > m.AzureMachinePool.Status.Replicas {
			conditions.MarkFalse(m.AzureMachinePool, infrav1.ScaleSetDesiredReplicasCondition, infrav1.ScaleSetScaleUpReason, clusterv1.ConditionSeverityInfo, "")
		} else {
			conditions.MarkFalse(m.AzureMachinePool, infrav1.ScaleSetDesiredReplicasCondition, infrav1.ScaleSetScaleDownReason, clusterv1.ConditionSeverityInfo, "")
		}
		m.SetNotReady()
	case v == infrav1.Updating:
		conditions.MarkFalse(m.AzureMachinePool, infrav1.ScaleSetModelUpdatedCondition, infrav1.ScaleSetModelOutOfDateReason, clusterv1.ConditionSeverityInfo, "")
		m.SetNotReady()
	case v == infrav1.Creating:
		conditions.MarkFalse(m.AzureMachinePool, infrav1.ScaleSetRunningCondition, infrav1.ScaleSetCreatingReason, clusterv1.ConditionSeverityInfo, "")
		m.SetNotReady()
	case v == infrav1.Deleting:
		conditions.MarkFalse(m.AzureMachinePool, infrav1.ScaleSetRunningCondition, infrav1.ScaleSetDeletingReason, clusterv1.ConditionSeverityInfo, "")
		m.SetNotReady()
	default:
		conditions.MarkFalse(m.AzureMachinePool, infrav1.ScaleSetRunningCondition, string(v), clusterv1.ConditionSeverityInfo, "")
		m.SetNotReady()
	}
}

// SetReady sets the AzureMachinePool Ready Status to true.
func (m *MachinePoolScope) SetReady() {
	m.AzureMachinePool.Status.Ready = true
}

// SetNotReady sets the AzureMachinePool Ready Status to false.
func (m *MachinePoolScope) SetNotReady() {
	m.AzureMachinePool.Status.Ready = false
}

// SetFailureMessage sets the AzureMachinePool status failure message.
func (m *MachinePoolScope) SetFailureMessage(v error) {
	m.AzureMachinePool.Status.FailureMessage = pointer.StringPtr(v.Error())
}

// SetFailureReason sets the AzureMachinePool status failure reason.
func (m *MachinePoolScope) SetFailureReason(v capierrors.MachineStatusError) {
	m.AzureMachinePool.Status.FailureReason = &v
}

// SetBootstrapConditions sets the AzureMachinePool BootstrapSucceeded condition based on the extension provisioning states.
func (m *MachinePoolScope) SetBootstrapConditions(ctx context.Context, provisioningState string, extensionName string) error {
	_, log, done := tele.StartSpanWithLogger(ctx, "scope.MachinePoolScope.SetBootstrapConditions")
	defer done()

	switch infrav1.ProvisioningState(provisioningState) {
	case infrav1.Succeeded:
		log.V(4).Info("extension provisioning state is succeeded", "vm extension", extensionName, "scale set", m.Name())
		conditions.MarkTrue(m.AzureMachinePool, infrav1.BootstrapSucceededCondition)
		return nil
	case infrav1.Creating:
		log.V(4).Info("extension provisioning state is creating", "vm extension", extensionName, "scale set", m.Name())
		conditions.MarkFalse(m.AzureMachinePool, infrav1.BootstrapSucceededCondition, infrav1.BootstrapInProgressReason, clusterv1.ConditionSeverityInfo, "")
		return azure.WithTransientError(errors.New("extension is still in provisioning state. This likely means that bootstrapping has not yet completed on the VM"), 30*time.Second)
	case infrav1.Failed:
		log.V(4).Info("extension provisioning state is failed", "vm extension", extensionName, "scale set", m.Name())
		conditions.MarkFalse(m.AzureMachinePool, infrav1.BootstrapSucceededCondition, infrav1.BootstrapFailedReason, clusterv1.ConditionSeverityError, "")
		return azure.WithTerminalError(errors.New("extension state failed. This likely means the Kubernetes node bootstrapping process failed or timed out. Check VM boot diagnostics logs to learn more"))
	default:
		return nil
	}
}

// AdditionalTags merges AdditionalTags from the scope's AzureCluster and AzureMachinePool. If the same key is present in both,
// the value from AzureMachinePool takes precedence.
func (m *MachinePoolScope) AdditionalTags() infrav1.Tags {
	tags := make(infrav1.Tags)
	// Start with the cluster-wide tags...
	tags.Merge(m.ClusterScoper.AdditionalTags())
	// ... and merge in the Machine Pool's
	tags.Merge(m.AzureMachinePool.Spec.AdditionalTags)
	// Set the cloud provider tag
	tags[infrav1.ClusterAzureCloudProviderTagKey(m.ClusterName())] = string(infrav1.ResourceLifecycleOwned)

	return tags
}

// SetAnnotation sets a key value annotation on the AzureMachinePool.
func (m *MachinePoolScope) SetAnnotation(key, value string) {
	if m.AzureMachinePool.Annotations == nil {
		m.AzureMachinePool.Annotations = map[string]string{}
	}
	m.AzureMachinePool.Annotations[key] = value
}

// PatchObject persists the AzureMachinePool spec and status.
func (m *MachinePoolScope) PatchObject(ctx context.Context) error {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scope.MachinePoolScope.PatchObject")
	defer done()

	conditions.SetSummary(m.AzureMachinePool)
	return m.patchHelper.Patch(
		ctx,
		m.AzureMachinePool,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
			clusterv1.ReadyCondition,
			infrav1.BootstrapSucceededCondition,
			infrav1.ScaleSetDesiredReplicasCondition,
			infrav1.ScaleSetModelUpdatedCondition,
			infrav1.ScaleSetRunningCondition,
		}})
}

// Close the MachinePoolScope by updating the AzureMachinePool spec and AzureMachinePool status.
func (m *MachinePoolScope) Close(ctx context.Context) error {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "scope.MachinePoolScope.Close")
	defer done()

	if m.vmssState != nil {
		if err := m.applyAzureMachinePoolMachines(ctx); err != nil {
			log.Error(err, "failed to apply changes to the AzureMachinePoolMachines")
			return errors.Wrap(err, "failed to apply changes to AzureMachinePoolMachines")
		}

		m.setProvisioningStateAndConditions(m.vmssState.State)
		if err := m.updateReplicasAndProviderIDs(ctx); err != nil {
			return errors.Wrap(err, "failed to update replicas and providerIDs")
		}
	}

	return m.PatchObject(ctx)
}

// GetBootstrapData returns the bootstrap data from the secret in the MachinePool's bootstrap.dataSecretName.
func (m *MachinePoolScope) GetBootstrapData(ctx context.Context) (string, error) {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scope.MachinePoolScope.GetBootstrapData")
	defer done()

	dataSecretName := m.MachinePool.Spec.Template.Spec.Bootstrap.DataSecretName
	if dataSecretName == nil {
		return "", errors.New("error retrieving bootstrap data: linked MachinePool Spec's bootstrap.dataSecretName is nil")
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: m.AzureMachinePool.Namespace, Name: *dataSecretName}
	if err := m.client.Get(ctx, key, secret); err != nil {
		return "", errors.Wrapf(err, "failed to retrieve bootstrap data secret for AzureMachinePool %s/%s", m.AzureMachinePool.Namespace, m.Name())
	}

	value, ok := secret.Data["value"]
	if !ok {
		return "", errors.New("error retrieving bootstrap data: secret value key is missing")
	}
	return base64.StdEncoding.EncodeToString(value), nil
}

// GetVMImage picks an image from the AzureMachinePool configuration, or uses a default one.
func (m *MachinePoolScope) GetVMImage(ctx context.Context) (*infrav1.Image, error) {
	_, log, done := tele.StartSpanWithLogger(ctx, "scope.MachinePoolScope.GetVMImage")
	defer done()

	// Use custom Marketplace image, Image ID or a Shared Image Gallery image if provided
	if m.AzureMachinePool.Spec.Template.Image != nil {
		return m.AzureMachinePool.Spec.Template.Image, nil
	}

	svc := virtualmachineimages.New(m)

	var (
		err          error
		defaultImage *infrav1.Image
	)
	if m.AzureMachinePool.Spec.Template.OSDisk.OSType == azure.WindowsOS {
		runtime := m.AzureMachinePool.Annotations["runtime"]
		windowsServerVersion := m.AzureMachinePool.Annotations["windowsServerVersion"]
		log.V(4).Info("No image specified for machine, using default Windows Image", "machine", m.MachinePool.GetName(), "runtime", runtime, "windowsServerVersion", windowsServerVersion)
		defaultImage, err = svc.GetDefaultWindowsImage(ctx, m.Location(), to.String(m.MachinePool.Spec.Template.Spec.Version), runtime, windowsServerVersion)
	} else {
		defaultImage, err = svc.GetDefaultUbuntuImage(ctx, m.Location(), to.String(m.MachinePool.Spec.Template.Spec.Version))
	}

	if err != nil {
		return defaultImage, errors.Wrap(err, "failed to get default OS image")
	}

	return defaultImage, nil
}

// SaveVMImageToStatus persists the AzureMachinePool image to the status.
func (m *MachinePoolScope) SaveVMImageToStatus(image *infrav1.Image) {
	m.AzureMachinePool.Status.Image = image
}

// RoleAssignmentSpecs returns the role assignment specs.
func (m *MachinePoolScope) RoleAssignmentSpecs(principalID *string) []azure.ResourceSpecGetter {
	roles := make([]azure.ResourceSpecGetter, 1)
	if m.HasSystemAssignedIdentity() {
		roles[0] = &roleassignments.RoleAssignmentSpec{
			Name:          m.AzureMachinePool.Spec.RoleAssignmentName,
			MachineName:   m.Name(),
			ResourceGroup: m.ResourceGroup(),
			ResourceType:  azure.VirtualMachineScaleSet,
			PrincipalID:   principalID,
		}
		return roles
	}
	return []azure.ResourceSpecGetter{}
}

// RoleAssignmentResourceType returns the role assignment resource type.
func (m *MachinePoolScope) RoleAssignmentResourceType() string {
	return azure.VirtualMachineScaleSet
}

// HasSystemAssignedIdentity returns true if the azure machine pool has system
// assigned identity.
func (m *MachinePoolScope) HasSystemAssignedIdentity() bool {
	return m.AzureMachinePool.Spec.Identity == infrav1.VMIdentitySystemAssigned
}

// VMSSExtensionSpecs returns the VMSS extension specs.
func (m *MachinePoolScope) VMSSExtensionSpecs() []azure.ResourceSpecGetter {
	var extensionSpecs = []azure.ResourceSpecGetter{}
	bootstrapExtensionSpec := azure.GetBootstrappingVMExtension(m.AzureMachinePool.Spec.Template.OSDisk.OSType, m.CloudEnvironment(), m.Name())

	if bootstrapExtensionSpec != nil {
		extensionSpecs = append(extensionSpecs, &scalesets.VMSSExtensionSpec{
			ExtensionSpec: *bootstrapExtensionSpec,
			ResourceGroup: m.ResourceGroup(),
		})
	}

	return extensionSpecs
}

func (m *MachinePoolScope) getDeploymentStrategy() machinepool.TypedDeleteSelector {
	if m.AzureMachinePool == nil {
		return nil
	}

	return machinepool.NewMachinePoolDeploymentStrategy(m.AzureMachinePool.Spec.Strategy)
}

// SetSubnetName defaults the AzureMachinePool subnet name to the name of the subnet with role 'node' when there is only one of them.
// Note: this logic exists only for purposes of ensuring backwards compatibility for old clusters created without the `subnetName` field being
// set, and should be removed in the future when this field is no longer optional.
func (m *MachinePoolScope) SetSubnetName() error {
	if m.AzureMachinePool.Spec.Template.SubnetName == "" {
		subnetName := ""
		for _, subnet := range m.NodeSubnets() {
			subnetName = subnet.Name
		}
		if len(m.NodeSubnets()) == 0 || len(m.NodeSubnets()) > 1 || subnetName == "" {
			return errors.New("a subnet name must be specified when no subnets are specified or more than 1 subnet of role 'node' exist")
		}

		m.AzureMachinePool.Spec.Template.SubnetName = subnetName
	}

	return nil
}

// UpdateDeleteStatus updates a condition on the AzureMachinePool status after a DELETE operation.
func (m *MachinePoolScope) UpdateDeleteStatus(condition clusterv1.ConditionType, service string, err error) {
	switch {
	case err == nil:
		conditions.MarkFalse(m.AzureMachinePool, condition, infrav1.DeletedReason, clusterv1.ConditionSeverityInfo, "%s successfully deleted", service)
	case azure.IsOperationNotDoneError(err):
		conditions.MarkFalse(m.AzureMachinePool, condition, infrav1.DeletingReason, clusterv1.ConditionSeverityInfo, "%s deleting", service)
	default:
		conditions.MarkFalse(m.AzureMachinePool, condition, infrav1.DeletionFailedReason, clusterv1.ConditionSeverityError, "%s failed to delete. err: %s", service, err.Error())
	}
}

// UpdatePutStatus updates a condition on the AzureMachinePool status after a PUT operation.
func (m *MachinePoolScope) UpdatePutStatus(condition clusterv1.ConditionType, service string, err error) {
	switch {
	case err == nil:
		conditions.MarkTrue(m.AzureMachinePool, condition)
	case azure.IsOperationNotDoneError(err):
		conditions.MarkFalse(m.AzureMachinePool, condition, infrav1.CreatingReason, clusterv1.ConditionSeverityInfo, "%s creating or updating", service)
	default:
		conditions.MarkFalse(m.AzureMachinePool, condition, infrav1.FailedReason, clusterv1.ConditionSeverityError, "%s failed to create or update. err: %s", service, err.Error())
	}
}

// UpdatePatchStatus updates a condition on the AzureMachinePool status after a PATCH operation.
func (m *MachinePoolScope) UpdatePatchStatus(condition clusterv1.ConditionType, service string, err error) {
	switch {
	case err == nil:
		conditions.MarkTrue(m.AzureMachinePool, condition)
	case azure.IsOperationNotDoneError(err):
		conditions.MarkFalse(m.AzureMachinePool, condition, infrav1.UpdatingReason, clusterv1.ConditionSeverityInfo, "%s updating", service)
	default:
		conditions.MarkFalse(m.AzureMachinePool, condition, infrav1.FailedReason, clusterv1.ConditionSeverityError, "%s failed to update. err: %s", service, err.Error())
	}
}
