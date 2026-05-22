/*
Copyright The Kubernetes Authors.

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

package scheduling

import (
	"unique"

	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
)

// nodeClaimDRAAdapter adapts the scheduler's NodeClaim to the dynamicresources.NodeClaim interface.
type nodeClaimDRAAdapter struct {
	id             dynamicresources.NodeClaimID
	nodePoolID     dynamicresources.NodePoolID
	requirements   scheduling.Requirements
	instanceTypes  []dynamicresources.InstanceTypeID
	resourceSlices map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice
}

func (a *nodeClaimDRAAdapter) ID() dynamicresources.NodeClaimID            { return a.id }
func (a *nodeClaimDRAAdapter) NodePoolID() dynamicresources.NodePoolID     { return a.nodePoolID }
func (a *nodeClaimDRAAdapter) Requirements() scheduling.Requirements       { return a.requirements }
func (a *nodeClaimDRAAdapter) InstanceTypes() []dynamicresources.InstanceTypeID { return a.instanceTypes }
func (a *nodeClaimDRAAdapter) ResourceSlices() map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice {
	return a.resourceSlices
}

// newNodeClaimDRAAdapter builds a dynamicresources.NodeClaim adapter from the scheduler's NodeClaim state.
// The adapter is constructed with the current requirements and instance types at the point of DRA evaluation.
func newNodeClaimDRAAdapter(hostname string, nodePoolName string, requirements scheduling.Requirements, instanceTypes []*cloudprovider.InstanceType) *nodeClaimDRAAdapter {
	itIDs := make([]dynamicresources.InstanceTypeID, len(instanceTypes))
	resourceSlices := make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice, len(instanceTypes))

	for i, it := range instanceTypes {
		itID := unique.Make(it.Name)
		itIDs[i] = itID
		for _, tmpl := range it.DynamicResources.ResourceSliceTemplates {
			resourceSlices[itID] = append(resourceSlices[itID], dynamicresources.NewTemplateSlice(tmpl))
		}
	}

	return &nodeClaimDRAAdapter{
		id:             unique.Make(hostname),
		nodePoolID:     unique.Make(nodePoolName),
		requirements:   requirements,
		instanceTypes:  itIDs,
		resourceSlices: resourceSlices,
	}
}

// newExistingNodeDRAAdapter builds a dynamicresources.NodeClaim adapter from an ExistingNode.
// Existing nodes have a single known instance type and all ResourceSlices are already in-cluster.
func newExistingNodeDRAAdapter(n *ExistingNode) *nodeClaimDRAAdapter {
	instanceTypeName := n.Labels()[corev1.LabelInstanceTypeStable]
	nodePoolName := n.Labels()["karpenter.sh/nodepool"]

	var itIDs []dynamicresources.InstanceTypeID
	if instanceTypeName != "" {
		itIDs = []dynamicresources.InstanceTypeID{unique.Make(instanceTypeName)}
	}

	return &nodeClaimDRAAdapter{
		id:             unique.Make(n.HostName()),
		nodePoolID:     unique.Make(nodePoolName),
		requirements:   n.requirements,
		instanceTypes:  itIDs,
		resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
	}
}
