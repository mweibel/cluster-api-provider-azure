/*
Copyright 2022 The Kubernetes Authors.

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
	"fmt"
	"strings"

	"github.com/Azure/go-autorest/autorest/to"
	"github.com/pkg/errors"
	infrav1 "sigs.k8s.io/cluster-api-provider-azure/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-azure/azure"
	infrav1exp "sigs.k8s.io/cluster-api-provider-azure/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-azure/util/futures"
	"sigs.k8s.io/cluster-api-provider-azure/util/tele"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	expv1 "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagedMachinePoolScopeParams defines the input parameters used to create a new managed
// control plane.
type ManagedMachinePoolScopeParams struct {
	ManagedMachinePool
	Client                   client.Client
	Cluster                  *clusterv1.Cluster
	ControlPlane             *infrav1exp.AzureManagedControlPlane
	ManagedControlPlaneScope azure.ManagedClusterScoper
}

type ManagedMachinePool struct {
	InfraMachinePool *infrav1exp.AzureManagedMachinePool
	MachinePool      *expv1.MachinePool
}

// NewManagedMachinePoolScope creates a new Scope from the supplied parameters.
// This is meant to be called for each reconcile iteration.
func NewManagedMachinePoolScope(ctx context.Context, params ManagedMachinePoolScopeParams) (*ManagedMachinePoolScope, error) {
	_, _, done := tele.StartSpanWithLogger(ctx, "scope.NewManagedMachinePoolScope")
	defer done()

	if params.Cluster == nil {
		return nil, errors.New("failed to generate new scope from nil Cluster")
	}

	if params.ControlPlane == nil {
		return nil, errors.New("failed to generate new scope from nil ControlPlane")
	}

	helper, err := patch.NewHelper(params.InfraMachinePool, params.Client)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init patch helper")
	}

	return &ManagedMachinePoolScope{
		Client:               params.Client,
		Cluster:              params.Cluster,
		ControlPlane:         params.ControlPlane,
		MachinePool:          params.MachinePool,
		InfraMachinePool:     params.InfraMachinePool,
		patchHelper:          helper,
		ManagedClusterScoper: params.ManagedControlPlaneScope,
	}, nil
}

// ManagedMachinePoolScope defines the basic context for an actuator to operate upon.
type ManagedMachinePoolScope struct {
	Client      client.Client
	patchHelper *patch.Helper

	azure.ManagedClusterScoper
	Cluster          *clusterv1.Cluster
	MachinePool      *expv1.MachinePool
	ControlPlane     *infrav1exp.AzureManagedControlPlane
	InfraMachinePool *infrav1exp.AzureManagedMachinePool
}

// PatchObject persists the cluster configuration and status.
func (s *ManagedMachinePoolScope) PatchObject(ctx context.Context) error {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scope.ManagedMachinePoolScope.PatchObject")
	defer done()

	conditions.SetSummary(s.InfraMachinePool)

	return s.patchHelper.Patch(
		ctx,
		s.InfraMachinePool,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
			clusterv1.ReadyCondition,
		}})
}

// Close closes the current scope persisting the cluster configuration and status.
func (s *ManagedMachinePoolScope) Close(ctx context.Context) error {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "scope.ManagedMachinePoolScope.Close")
	defer done()

	return s.PatchObject(ctx)
}

// AgentPoolAnnotations returns a map of annotations for the infra machine pool.
func (s *ManagedMachinePoolScope) AgentPoolAnnotations() map[string]string {
	return s.InfraMachinePool.Annotations
}

// AgentPoolSpec returns an azure.AgentPoolSpec for currently reconciled AzureManagedMachinePool.
func (s *ManagedMachinePoolScope) AgentPoolSpec() azure.AgentPoolSpec {
	return buildAgentPoolSpec(s.ControlPlane, s.MachinePool, s.InfraMachinePool)
}

func buildAgentPoolSpec(managedControlPlane *infrav1exp.AzureManagedControlPlane,
	machinePool *expv1.MachinePool,
	managedMachinePool *infrav1exp.AzureManagedMachinePool) azure.AgentPoolSpec {
	var normalizedVersion *string
	if machinePool.Spec.Template.Spec.Version != nil {
		v := strings.TrimPrefix(*machinePool.Spec.Template.Spec.Version, "v")
		normalizedVersion = &v
	}

	replicas := int32(1)
	if machinePool.Spec.Replicas != nil {
		replicas = *machinePool.Spec.Replicas
	}

	agentPoolSpec := azure.AgentPoolSpec{
		Name:          to.String(managedMachinePool.Spec.Name),
		ResourceGroup: managedControlPlane.Spec.ResourceGroupName,
		Cluster:       managedControlPlane.Name,
		SKU:           managedMachinePool.Spec.SKU,
		Replicas:      replicas,
		Version:       normalizedVersion,
		OSType:        managedMachinePool.Spec.OSType,
		VnetSubnetID: azure.SubnetID(
			managedControlPlane.Spec.SubscriptionID,
			managedControlPlane.Spec.ResourceGroupName,
			managedControlPlane.Spec.VirtualNetwork.Name,
			managedControlPlane.Spec.VirtualNetwork.Subnet.Name,
		),
		Mode:              managedMachinePool.Spec.Mode,
		MaxPods:           managedMachinePool.Spec.MaxPods,
		AvailabilityZones: managedMachinePool.Spec.AvailabilityZones,
		OsDiskType:        managedMachinePool.Spec.OsDiskType,
		EnableUltraSSD:    managedMachinePool.Spec.EnableUltraSSD,
	}

	if managedMachinePool.Spec.OSDiskSizeGB != nil {
		agentPoolSpec.OSDiskSizeGB = *managedMachinePool.Spec.OSDiskSizeGB
	}

	if len(managedMachinePool.Spec.Taints) > 0 {
		nodeTaints := make([]string, 0, len(managedMachinePool.Spec.Taints))
		for _, t := range managedMachinePool.Spec.Taints {
			nodeTaints = append(nodeTaints, fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect))
		}
		agentPoolSpec.NodeTaints = nodeTaints
	}

	if managedMachinePool.Spec.Scaling != nil {
		agentPoolSpec.EnableAutoScaling = to.BoolPtr(true)
		agentPoolSpec.MaxCount = managedMachinePool.Spec.Scaling.MaxSize
		agentPoolSpec.MinCount = managedMachinePool.Spec.Scaling.MinSize
	}

	if len(managedMachinePool.Spec.NodeLabels) > 0 {
		agentPoolSpec.NodeLabels = make(map[string]*string, len(managedMachinePool.Spec.NodeLabels))
		for k, v := range managedMachinePool.Spec.NodeLabels {
			agentPoolSpec.NodeLabels[k] = to.StringPtr(v)
		}
	}

	return agentPoolSpec
}

// SetAgentPoolProviderIDList sets a list of agent pool's Azure VM IDs.
func (s *ManagedMachinePoolScope) SetAgentPoolProviderIDList(providerIDs []string) {
	s.InfraMachinePool.Spec.ProviderIDList = providerIDs
}

// SetAgentPoolReplicas sets the number of agent pool replicas.
func (s *ManagedMachinePoolScope) SetAgentPoolReplicas(replicas int32) {
	s.InfraMachinePool.Status.Replicas = replicas
}

// SetAgentPoolReady sets the flag that indicates if the agent pool is ready or not.
func (s *ManagedMachinePoolScope) SetAgentPoolReady(ready bool) {
	s.InfraMachinePool.Status.Ready = ready
}

// SetLongRunningOperationState will set the future on the AzureManagedControlPlane status to allow the resource to continue
// in the next reconciliation.
func (s *ManagedMachinePoolScope) SetLongRunningOperationState(future *infrav1.Future) {
	futures.Set(s.ControlPlane, future)
}

// GetLongRunningOperationState will get the future on the AzureManagedControlPlane status.
func (s *ManagedMachinePoolScope) GetLongRunningOperationState(name, service string) *infrav1.Future {
	return futures.Get(s.ControlPlane, name, service)
}

// DeleteLongRunningOperationState will delete the future from the AzureManagedControlPlane status.
func (s *ManagedMachinePoolScope) DeleteLongRunningOperationState(name, service string) {
	futures.Delete(s.ControlPlane, name, service)
}

// UpdateDeleteStatus updates a condition on the AzureManagedControlPlane status after a DELETE operation.
func (s *ManagedMachinePoolScope) UpdateDeleteStatus(condition clusterv1.ConditionType, service string, err error) {
	switch {
	case err == nil:
		conditions.MarkFalse(s.InfraMachinePool, condition, infrav1.DeletedReason, clusterv1.ConditionSeverityInfo, "%s successfully deleted", service)
	case azure.IsOperationNotDoneError(err):
		conditions.MarkFalse(s.InfraMachinePool, condition, infrav1.DeletingReason, clusterv1.ConditionSeverityInfo, "%s deleting", service)
	default:
		conditions.MarkFalse(s.InfraMachinePool, condition, infrav1.DeletionFailedReason, clusterv1.ConditionSeverityError, "%s failed to delete. err: %s", service, err.Error())
	}
}

// UpdatePutStatus updates a condition on the AzureManagedControlPlane status after a PUT operation.
func (s *ManagedMachinePoolScope) UpdatePutStatus(condition clusterv1.ConditionType, service string, err error) {
	switch {
	case err == nil:
		conditions.MarkTrue(s.InfraMachinePool, condition)
	case azure.IsOperationNotDoneError(err):
		conditions.MarkFalse(s.InfraMachinePool, condition, infrav1.CreatingReason, clusterv1.ConditionSeverityInfo, "%s creating or updating", service)
	default:
		conditions.MarkFalse(s.InfraMachinePool, condition, infrav1.FailedReason, clusterv1.ConditionSeverityError, "%s failed to create or update. err: %s", service, err.Error())
	}
}

// UpdatePatchStatus updates a condition on the AzureManagedControlPlane status after a PATCH operation.
func (s *ManagedMachinePoolScope) UpdatePatchStatus(condition clusterv1.ConditionType, service string, err error) {
	switch {
	case err == nil:
		conditions.MarkTrue(s.InfraMachinePool, condition)
	case azure.IsOperationNotDoneError(err):
		conditions.MarkFalse(s.InfraMachinePool, condition, infrav1.UpdatingReason, clusterv1.ConditionSeverityInfo, "%s updating", service)
	default:
		conditions.MarkFalse(s.InfraMachinePool, condition, infrav1.FailedReason, clusterv1.ConditionSeverityError, "%s failed to update. err: %s", service, err.Error())
	}
}
