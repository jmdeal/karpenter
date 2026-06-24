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

package provisioning

import (
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

func TestNodeOwnerName(t *testing.T) {
	tests := []struct {
		name      string
		owners    []metav1.OwnerReference
		wantName  string
		wantOwned bool
	}{
		{
			name:      "node owner reference",
			owners:    []metav1.OwnerReference{{Kind: "Node", Name: "node-a"}},
			wantName:  "node-a",
			wantOwned: true,
		},
		{
			name:      "no owner references",
			owners:    nil,
			wantOwned: false,
		},
		{
			name:      "non-node owner reference",
			owners:    []metav1.OwnerReference{{Kind: "Pod", Name: "pod-a"}},
			wantOwned: false,
		},
		{
			name:      "node owner among others",
			owners:    []metav1.OwnerReference{{Kind: "Pod", Name: "pod-a"}, {Kind: "Node", Name: "node-b"}},
			wantName:  "node-b",
			wantOwned: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slice := &resourcev1.ResourceSlice{ObjectMeta: metav1.ObjectMeta{OwnerReferences: tt.owners}}
			gotName, gotOwned := nodeOwnerName(slice)
			if gotOwned != tt.wantOwned {
				t.Fatalf("nodeOwnerName() owned = %v, want %v", gotOwned, tt.wantOwned)
			}
			if gotName != tt.wantName {
				t.Fatalf("nodeOwnerName() name = %q, want %q", gotName, tt.wantName)
			}
		})
	}
}

func TestAllConsumersDeleting(t *testing.T) {
	deleting := sets.New[types.UID]("a", "b")
	tests := []struct {
		name     string
		podUIDs  []types.UID
		expected bool
	}{
		{name: "all deleting", podUIDs: []types.UID{"a", "b"}, expected: true},
		{name: "subset deleting", podUIDs: []types.UID{"a"}, expected: true},
		{name: "one non-deleting", podUIDs: []types.UID{"a", "c"}, expected: false},
		{name: "none deleting", podUIDs: []types.UID{"c"}, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allConsumersDeleting(tt.podUIDs, deleting); got != tt.expected {
				t.Fatalf("allConsumersDeleting() = %v, want %v", got, tt.expected)
			}
		})
	}
}
