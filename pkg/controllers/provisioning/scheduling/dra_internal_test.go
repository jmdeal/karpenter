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
	"testing"
	"unique"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func TestResourceClaimName(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		claim    corev1.PodResourceClaim
		wantName string
		wantOK   bool
	}{
		{
			name:     "direct claim name",
			pod:      &corev1.Pod{},
			claim:    corev1.PodResourceClaim{Name: "ref", ResourceClaimName: lo.ToPtr("claim-a")},
			wantName: "claim-a",
			wantOK:   true,
		},
		{
			name: "template generated name from status",
			pod: &corev1.Pod{Status: corev1.PodStatus{ResourceClaimStatuses: []corev1.PodResourceClaimStatus{
				{Name: "ref", ResourceClaimName: lo.ToPtr("generated-a")},
			}}},
			claim:    corev1.PodResourceClaim{Name: "ref"},
			wantName: "generated-a",
			wantOK:   true,
		},
		{
			name: "template with nil generated name is skipped",
			pod: &corev1.Pod{Status: corev1.PodStatus{ResourceClaimStatuses: []corev1.PodResourceClaimStatus{
				{Name: "ref", ResourceClaimName: nil},
			}}},
			claim:  corev1.PodResourceClaim{Name: "ref"},
			wantOK: false,
		},
		{
			name:   "no matching status entry",
			pod:    &corev1.Pod{},
			claim:  corev1.PodResourceClaim{Name: "ref"},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotOK := resourceClaimName(tt.pod, &tt.claim)
			if gotOK != tt.wantOK {
				t.Fatalf("resourceClaimName() ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotName != tt.wantName {
				t.Fatalf("resourceClaimName() name = %q, want %q", gotName, tt.wantName)
			}
		})
	}
}

func TestDRANodeClaimAdapter(t *testing.T) {
	it1 := &cloudprovider.InstanceType{
		Name: "it-1",
		DynamicResources: cloudprovider.DynamicResources{
			ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("driver-a")}},
		},
	}
	it2 := &cloudprovider.InstanceType{Name: "it-2"}
	nc := &NodeClaim{hostname: "hostname-1"}
	nc.NodePoolName = "np-a"
	nc.Requirements = scheduling.NewRequirements()
	nc.InstanceTypeOptions = cloudprovider.InstanceTypes{it1, it2}

	adapter := &draNodeClaim{nc: nc}
	if adapter.ID() != unique.Make("hostname-1") {
		t.Errorf("ID() = %v, want hostname-1", adapter.ID().Value())
	}
	if adapter.NodePoolID() != unique.Make("np-a") {
		t.Errorf("NodePoolID() = %v, want np-a", adapter.NodePoolID().Value())
	}
	its := lo.Map(adapter.InstanceTypes(), func(id unique.Handle[string], _ int) string { return id.Value() })
	if len(its) != 2 || its[0] != "it-1" || its[1] != "it-2" {
		t.Errorf("InstanceTypes() = %v, want [it-1 it-2]", its)
	}
	slices := adapter.ResourceSlices()
	if got := len(slices[unique.Make("it-1")]); got != 1 {
		t.Errorf("it-1 template slices = %d, want 1", got)
	}
	if got := len(slices[unique.Make("it-2")]); got != 0 {
		t.Errorf("it-2 template slices = %d, want 0", got)
	}
}

func TestDRAExistingNodeAdapterInitialized(t *testing.T) {
	it := &cloudprovider.InstanceType{
		Name: "it-1",
		DynamicResources: cloudprovider.DynamicResources{
			ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{{Driver: unique.Make("driver-a")}},
		},
	}

	// Initialized node: ResourceSlices() must be empty (devices published in-cluster).
	initialized := newDRAExistingNode(true, it)
	if got := len(initialized.ResourceSlices()); got != 0 {
		t.Errorf("initialized ResourceSlices() = %d entries, want 0", got)
	}

	// Pre-initialized node: ResourceSlices() returns the full template set for the instance type.
	preInitialized := newDRAExistingNode(false, it)
	slices := preInitialized.ResourceSlices()
	if got := len(slices[unique.Make("it-1")]); got != 1 {
		t.Errorf("pre-initialized it-1 template slices = %d, want 1", got)
	}

	// Pre-initialized node with unresolved instance type: ResourceSlices() empty.
	noIT := newDRAExistingNode(false, nil)
	if got := len(noIT.ResourceSlices()); got != 0 {
		t.Errorf("pre-initialized with nil IT ResourceSlices() = %d entries, want 0", got)
	}
}

// newDRAExistingNode builds a draExistingNode backed by a StateNode whose initialization state and labels are set for
// testing the adapter's ResourceSlices() behavior.
func newDRAExistingNode(initialized bool, it *cloudprovider.InstanceType) *draExistingNode {
	labels := map[string]string{
		corev1.LabelInstanceTypeStable: "it-1",
		v1.NodePoolLabelKey:            "np-a",
	}
	if initialized {
		labels[v1.NodeInitializedLabelKey] = "true"
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: labels}}
	nodeClaim := &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
	en := &ExistingNode{StateNode: &state.StateNode{Node: node, NodeClaim: nodeClaim}}
	en.requirements = scheduling.NewLabelRequirements(labels)
	return &draExistingNode{en: en, instanceType: it}
}
