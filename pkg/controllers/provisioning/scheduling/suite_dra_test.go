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

package scheduling_test

import (
	"unique"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"k8s.io/client-go/tools/record"
)

// -- DRA test helpers --

func makeDeviceClass(name string, selectors ...resourcev1.DeviceSelector) *resourcev1.DeviceClass {
	return &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       resourcev1.DeviceClassSpec{Selectors: selectors},
	}
}

func makeResourceClaim(name string, requests ...resourcev1.DeviceRequest) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: requests,
			},
		},
	}
}

func makeAllocatedResourceClaim(name string, nodeSelector *corev1.NodeSelector, results ...resourcev1.DeviceRequestAllocationResult) *resourcev1.ResourceClaim {
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{Name: "request", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "test-class"}},
				},
			},
		},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				NodeSelector: nodeSelector,
				Devices: resourcev1.DeviceAllocationResult{
					Results: results,
				},
			},
		},
	}
	return claim
}

func exactRequest(name, className string, count int64) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{
		Name: name,
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: className,
			Count:           count,
		},
	}
}

func exactRequestWithSelector(name, className string, count int64, expr string) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{
		Name: name,
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: className,
			Count:           count,
			Selectors: []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: expr}},
			},
		},
	}
}

func makeResourceSlice(name, driver, pool string, opts ...func(*resourcev1.ResourceSlice)) *resourcev1.ResourceSlice {
	s := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver: driver,
			Pool: resourcev1.ResourcePool{
				Name:               pool,
				Generation:         1,
				ResourceSliceCount: 1,
			},
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func withAllNodes() func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.Spec.AllNodes = ptr.To(true)
	}
}

func withNodeOwner(nodeName string) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.OwnerReferences = []metav1.OwnerReference{
			{Kind: "Node", Name: nodeName, APIVersion: "v1", UID: types.UID(nodeName + "-uid")},
		}
	}
}

func withSliceNodeSelector(key string, values ...string) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.Spec.NodeSelector = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: key, Operator: corev1.NodeSelectorOpIn, Values: values},
				}},
			},
		}
	}
}

func withSliceDevices(names ...string) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		for _, name := range names {
			s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{Name: name})
		}
	}
}

func withSliceDevicesWithAttrs(specs ...sliceDeviceSpec) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		for _, spec := range specs {
			d := resourcev1.Device{Name: spec.name}
			if len(spec.attrs) > 0 {
				d.Attributes = spec.attrs
			}
			s.Spec.Devices = append(s.Spec.Devices, d)
		}
	}
}

type sliceDeviceSpec struct {
	name  string
	attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute
}

func deviceSpec(name string, attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute) sliceDeviceSpec {
	return sliceDeviceSpec{name: name, attrs: attrs}
}

func makeTemplate(driver, pool string, deviceNames ...string) *cloudprovider.ResourceSliceTemplate {
	devices := make([]cloudprovider.Device, len(deviceNames))
	for i, name := range deviceNames {
		devices[i] = cloudprovider.Device{Name: unique.Make(name)}
	}
	return &cloudprovider.ResourceSliceTemplate{
		Driver:  unique.Make(driver),
		Pool:    cloudprovider.ResourcePool{Name: unique.Make(pool)},
		Devices: devices,
	}
}

func makeTemplateWithAttrs(driver, pool string, devices ...cloudprovider.Device) *cloudprovider.ResourceSliceTemplate {
	return &cloudprovider.ResourceSliceTemplate{
		Driver:  unique.Make(driver),
		Pool:    cloudprovider.ResourcePool{Name: unique.Make(pool)},
		Devices: devices,
	}
}

func makeDevice(name string, attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute) cloudprovider.Device {
	return cloudprovider.Device{
		Name:       unique.Make(name),
		Attributes: attrs,
	}
}

func makeInstanceTypeWithDRA(name string, templates ...*cloudprovider.ResourceSliceTemplate) *cloudprovider.InstanceType {
	it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: name})
	it.DynamicResources = cloudprovider.DynamicResources{
		ResourceSliceTemplates: templates,
	}
	return it
}

func makeInstanceTypeWithDRAAndOfferings(name string, offerings cloudprovider.Offerings, templates ...*cloudprovider.ResourceSliceTemplate) *cloudprovider.InstanceType {
	it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: name, Offerings: offerings})
	it.DynamicResources = cloudprovider.DynamicResources{
		ResourceSliceTemplates: templates,
	}
	return it
}

func makePodWithClaims(claimNames ...string) *corev1.Pod {
	var podClaims []corev1.PodResourceClaim
	var containerClaims []corev1.ResourceClaim
	for _, name := range claimNames {
		podClaims = append(podClaims, corev1.PodResourceClaim{
			Name:              name,
			ResourceClaimName: ptr.To(name),
		})
		containerClaims = append(containerClaims, corev1.ResourceClaim{Name: name})
	}
	return test.UnschedulablePod(test.PodOptions{
		ResourceClaims:          podClaims,
		ContainerResourceClaims: containerClaims,
	})
}

// getRequirementValues extracts the values for a given label key from a NodeClaim's requirements.
func getRequirementValues(reqs []v1.NodeSelectorRequirementWithMinValues, key string) []string {
	for _, req := range reqs {
		if req.Key == key {
			return req.Values
		}
	}
	return nil
}

// -- DRA Integration Tests --

var _ = FDescribe("DRA Integration", func() {
	BeforeEach(func() {
		if env.Version.Minor() < 34 {
			Skip("DRA ResourceClaim/ResourceSlice/DeviceClass APIs require K8s >= 1.34")
		}
	})

	Context("Basic Template Device Allocation", func() {
		It("should schedule a pod with a single claim requesting a single device", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			it := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule a pod requesting multiple devices via ExactCount", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 2))
			it := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0", "gpu-1", "gpu-2", "gpu-3"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule a pod with no DRA claims normally when allocator is configured", func() {
			it := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := test.UnschedulablePod()

			ExpectApplied(ctx, env.Client, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule a pod with multiple claims requesting devices from different drivers", func() {
			gpuClass := makeDeviceClass("gpu")
			nicClass := makeDeviceClass("nic")
			gpuClaim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			nicClaim := makeResourceClaim("nic-claim", exactRequest("req-1", "nic", 1))
			it := makeInstanceTypeWithDRA("multi-device",
				makeTemplate("gpu.example.com", "gpu-pool", "gpu-0"),
				makeTemplate("nic.example.com", "nic-pool", "nic-0"),
			)
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim", "nic-claim")

			ExpectApplied(ctx, env.Client, gpuClass, nicClass, gpuClaim, nicClaim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
	})

	Context("Basic Template Device Allocation (Negative)", func() {
		It("should not schedule a pod when no instance type has matching devices", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			// Instance type has no DRA devices
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-only"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule a pod requesting more devices than any instance type provides", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 4))
			it := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0", "gpu-1"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule a pod when the DeviceClass does not exist", func() {
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "nonexistent-class", 1))
			it := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
	})

	Context("Instance Type Pruning", func() {
		It("should only keep instance types with matching devices", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			gpuIT := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))
			cpuIT := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-only"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuIT, cpuIT}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			nodeClaim := cloudProvider.CreateCalls[0]
			// The NodeClaim should only have the gpu-instance type
			itValues := getRequirementValues(nodeClaim.Spec.Requirements, corev1.LabelInstanceTypeStable)
			Expect(itValues).ToNot(BeEmpty())
			Expect(itValues).To(ContainElement("gpu-instance"))
			Expect(itValues).ToNot(ContainElement("cpu-only"))
		})
		It("should filter instance types by CEL device attribute selectors", func() {
			deviceClass := makeDeviceClass("h100-gpu",
				resourcev1.DeviceSelector{CEL: &resourcev1.CELDeviceSelector{
					Expression: `device.attributes["gpu.example.com"].model == "H100"`,
				}},
			)
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "h100-gpu", 1))
			h100IT := makeInstanceTypeWithDRA("h100-instance",
				makeTemplateWithAttrs("gpu.example.com", "pool-h100",
					makeDevice("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/model": {StringValue: ptr.To("H100")},
					}),
				),
			)
			a100IT := makeInstanceTypeWithDRA("a100-instance",
				makeTemplateWithAttrs("gpu.example.com", "pool-a100",
					makeDevice("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/model": {StringValue: ptr.To("A100")},
					}),
				),
			)
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{h100IT, a100IT}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			nodeClaim := cloudProvider.CreateCalls[0]
			itValues := getRequirementValues(nodeClaim.Spec.Requirements, corev1.LabelInstanceTypeStable)
			Expect(itValues).ToNot(BeEmpty())
			Expect(itValues).To(ContainElement("h100-instance"))
			Expect(itValues).ToNot(ContainElement("a100-instance"))
		})
		It("should not schedule when all instance types are pruned by CEL selectors", func() {
			deviceClass := makeDeviceClass("h200-gpu",
				resourcev1.DeviceSelector{CEL: &resourcev1.CELDeviceSelector{
					Expression: `device.attributes["gpu.example.com"].model == "H200"`,
				}},
			)
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "h200-gpu", 1))
			a100IT := makeInstanceTypeWithDRA("a100-instance",
				makeTemplateWithAttrs("gpu.example.com", "pool-a100",
					makeDevice("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/model": {StringValue: ptr.To("A100")},
					}),
				),
			)
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{a100IT}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should prune instance types whose offerings are eliminated by DRA topology tightening", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			// In-cluster slice with NodeSelector constraining to test-zone-1
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withSliceNodeSelector(corev1.LabelTopologyZone, "test-zone-1"),
				withSliceDevices("gpu-0"),
			)
			// Instance type A: offerings in zone 1 and 2
			itA := fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "it-zone-1-and-2",
				Offerings: []*cloudprovider.Offering{
					{Available: true, Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: "on-demand", corev1.LabelTopologyZone: "test-zone-1"}), Price: 1.0},
					{Available: true, Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: "on-demand", corev1.LabelTopologyZone: "test-zone-2"}), Price: 1.0},
				},
			})
			// Instance type B: offerings only in zone 2
			itB := fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "it-zone-2-only",
				Offerings: []*cloudprovider.Offering{
					{Available: true, Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: "on-demand", corev1.LabelTopologyZone: "test-zone-2"}), Price: 1.0},
				},
			})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{itA, itB}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, slice, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			nodeClaim := cloudProvider.CreateCalls[0]
			itValues := getRequirementValues(nodeClaim.Spec.Requirements, corev1.LabelInstanceTypeStable)
			Expect(itValues).ToNot(BeEmpty())
			Expect(itValues).To(ContainElement("it-zone-1-and-2"))
			Expect(itValues).ToNot(ContainElement("it-zone-2-only"))
		})
		It("should schedule DRA pod to the correct NodePool when only one has matching devices", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			gpuIT := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))
			cpuIT := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-only"})

			npGPU := test.NodePool(v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "gpu-pool"}})
			npCPU := test.NodePool(v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "cpu-pool"}})

			cloudProvider.InstanceTypesForNodePool = map[string][]*cloudprovider.InstanceType{
				"gpu-pool": {gpuIT},
				"cpu-pool": {cpuIT},
			}
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, npGPU, npCPU, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			nodeClaim := cloudProvider.CreateCalls[0]
			Expect(nodeClaim.Labels[v1.NodePoolLabelKey]).To(Equal("gpu-pool"))
		})
	})

	Context("In-Cluster Device Allocation", func() {
		It("should schedule a pod to an existing node with in-cluster devices", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withAllNodes(),
				withSliceDevices("gpu-0"),
			)
			nodePool := test.NodePool()
			// Create instance types without DRA templates — rely on in-cluster devices
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-instance"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, slice, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should prefer in-cluster devices over template devices", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			// In-cluster slice available to all nodes
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withAllNodes(),
				withSliceDevices("gpu-0"),
			)
			// Instance types also have template devices
			itA := makeInstanceTypeWithDRA("it-a", makeTemplate("gpu.example.com", "pool-b", "gpu-tmpl-0"))
			itB := makeInstanceTypeWithDRA("it-b", makeTemplate("gpu.example.com", "pool-c", "gpu-tmpl-0"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{itA, itB}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, slice, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)

			// Both instance types should survive since in-cluster device satisfies the claim
			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			nodeClaim := cloudProvider.CreateCalls[0]
			itValues := getRequirementValues(nodeClaim.Spec.Requirements, corev1.LabelInstanceTypeStable)
			Expect(itValues).ToNot(BeEmpty())
			Expect(itValues).To(ContainElement("it-a"))
			Expect(itValues).To(ContainElement("it-b"))
		})
		It("should not schedule a pod to an existing node when no devices are available", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			// No ResourceSlice, no template devices
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-only"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
	})

	Context("ResourceSlice Filtering", func() {
		It("should exclude ResourceSlices owned by deleting nodes", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			// Slice owned by a node that will be deleting
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withAllNodes(),
				withSliceDevices("gpu-0"),
				withNodeOwner("deleting-node"),
			)
			// No template devices — rely on in-cluster only
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-only"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()

			// Create the node, then mark it as deleting
			node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Name: "deleting-node",
			}})
			nodeClaim := test.NodeClaim(v1.NodeClaim{
				Status: v1.NodeClaimStatus{
					NodeName:   "deleting-node",
					ProviderID: node.Spec.ProviderID,
				},
			})
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, slice, nodePool, node, nodeClaim, pod)
			ExpectDeletionTimestampSet(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should NOT exclude non-node-owned ResourceSlices when nodes are deleting", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			// Slice with no node owner reference (e.g., owned by a DaemonSet)
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withAllNodes(),
				withSliceDevices("gpu-0"),
				// No withNodeOwner — empty OwnerReferences
			)
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-only"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, slice, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
	})

	Context("Multiple Pods and Claim Sharing", func() {
		It("should schedule two pods sharing the same claim with template devices to the same NodeClaim", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("shared-claim", exactRequest("req-1", "gpu", 1))
			it := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()

			pod1 := makePodWithClaims("shared-claim")
			pod2 := makePodWithClaims("shared-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod1, pod2)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod1, pod2)
			ExpectScheduled(ctx, env.Client, pod1)
			ExpectScheduled(ctx, env.Client, pod2)

			// Both pods should be on the same NodeClaim since the claim used template devices
			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
		})
		It("should schedule two pods sharing the same claim with in-cluster devices potentially to different NodeClaims", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("shared-claim", exactRequest("req-1", "gpu", 1))
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withAllNodes(),
				withSliceDevices("gpu-0"),
			)
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-instance"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()

			pod1 := makePodWithClaims("shared-claim")
			pod2 := makePodWithClaims("shared-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, slice, nodePool, pod1, pod2)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod1, pod2)
			ExpectScheduled(ctx, env.Client, pod1)
			ExpectScheduled(ctx, env.Client, pod2)
		})
		It("should schedule two pods with separate claims competing for template devices on the same NodeClaim", func() {
			deviceClass := makeDeviceClass("gpu")
			claim1 := makeResourceClaim("claim-1", exactRequest("req-1", "gpu", 1))
			claim2 := makeResourceClaim("claim-2", exactRequest("req-1", "gpu", 1))
			it := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0", "gpu-1"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()

			pod1 := makePodWithClaims("claim-1")
			pod2 := makePodWithClaims("claim-2")

			ExpectApplied(ctx, env.Client, deviceClass, claim1, claim2, nodePool, pod1, pod2)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod1, pod2)
			ExpectScheduled(ctx, env.Client, pod1)
			ExpectScheduled(ctx, env.Client, pod2)

			// Both should fit on the same NodeClaim (2 devices available)
			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
		})
		It("should create separate NodeClaims when device contention exhausts a single instance", func() {
			deviceClass := makeDeviceClass("gpu")
			claim1 := makeResourceClaim("claim-1", exactRequest("req-1", "gpu", 1))
			claim2 := makeResourceClaim("claim-2", exactRequest("req-1", "gpu", 1))
			// Only 1 template device per instance
			it := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()

			pod1 := makePodWithClaims("claim-1")
			pod2 := makePodWithClaims("claim-2")

			ExpectApplied(ctx, env.Client, deviceClass, claim1, claim2, nodePool, pod1, pod2)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod1, pod2)
			ExpectScheduled(ctx, env.Client, pod1)
			ExpectScheduled(ctx, env.Client, pod2)

			// Each pod should get its own NodeClaim
			Expect(cloudProvider.CreateCalls).To(HaveLen(2))
		})
	})

	Context("Topology Constraints from DRA", func() {
		It("should tighten zone requirements when in-cluster device has NodeSelector", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withSliceNodeSelector(corev1.LabelTopologyZone, "test-zone-1"),
				withSliceDevices("gpu-0"),
			)
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "multi-zone"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, slice, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			nodeClaim := cloudProvider.CreateCalls[0]
			zoneValues := getRequirementValues(nodeClaim.Spec.Requirements, corev1.LabelTopologyZone)
			Expect(zoneValues).ToNot(BeEmpty())
			Expect(zoneValues).To(ConsistOf("test-zone-1"))
		})
		It("should not merge DRA requirements into existing node requirements", func() {
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withAllNodes(),
				withSliceDevices("gpu-0"),
			)
			nodePool := test.NodePool()
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "existing-type"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}

			// First, provision an initial pod to create the infrastructure (existing node)
			initialPod := test.UnschedulablePod(test.PodOptions{ResourceRequirements: corev1.ResourceRequirements{
				Limits: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU: resource.MustParse("10m"),
				},
			}})
			ExpectApplied(ctx, env.Client, deviceClass, claim, slice, nodePool, initialPod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
			node := ExpectScheduled(ctx, env.Client, initialPod)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			// Now schedule the DRA pod — it should land on the existing node
			pod := makePodWithClaims("gpu-claim")
			ExpectApplied(ctx, env.Client, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			// Pod should have scheduled to the existing node, not a new NodeClaim
			Expect(cloudProvider.CreateCalls).To(HaveLen(1)) // only the initial pod's NodeClaim
		})
		It("should schedule a pod with an already-allocated claim with compatible topology", func() {
			nodeSelector := &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-zone-1"}},
					}},
				},
			}
			claim := makeAllocatedResourceClaim("gpu-claim", nodeSelector)
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "multi-zone"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			nodeClaim := cloudProvider.CreateCalls[0]
			zoneValues := getRequirementValues(nodeClaim.Spec.Requirements, corev1.LabelTopologyZone)
			Expect(zoneValues).ToNot(BeEmpty())
			Expect(zoneValues).To(ConsistOf("test-zone-1"))
		})
		It("should not schedule a pod with an already-allocated claim with incompatible topology", func() {
			nodeSelector := &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-east-1a"}},
					}},
				},
			}
			claim := makeAllocatedResourceClaim("gpu-claim", nodeSelector)
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "multi-zone"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule a pod with two already-allocated claims with conflicting topologies", func() {
			claim1 := makeAllocatedResourceClaim("claim-1", &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-zone-1"}},
					}},
				},
			})
			claim2 := makeAllocatedResourceClaim("claim-2", &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-zone-2"}},
					}},
				},
			})
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "multi-zone"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("claim-1", "claim-2")

			ExpectApplied(ctx, env.Client, claim1, claim2, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
	})

	Context("Device Allocation Controller Integration", func() {
		var daController *deviceallocation.Controller

		BeforeEach(func() {
			daController = deviceallocation.NewController(env.Client)
		})

		It("should exclude already-allocated devices from scheduling", func() {
			deviceClass := makeDeviceClass("gpu")
			// An existing claim that has already allocated gpu-0
			existingClaim := makeAllocatedResourceClaim("existing-claim", nil,
				resourcev1.DeviceRequestAllocationResult{
					Request: "request",
					Driver:  "gpu.example.com",
					Pool:    "pool-a",
					Device:  "gpu-0",
				},
			)
			existingClaim.Status.ReservedFor = []resourcev1.ResourceClaimConsumerReference{
				{Resource: "pods", Name: "existing-pod", UID: "existing-pod-uid"},
			}
			// In-cluster slice with the same device
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withAllNodes(),
				withSliceDevices("gpu-0"),
			)
			// New pod wants from the same pool — no template devices available
			newClaim := makeResourceClaim("new-claim", exactRequest("req-1", "gpu", 1))
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-only"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("new-claim")

			ExpectApplied(ctx, env.Client, deviceClass, existingClaim, slice, newClaim, nodePool, pod)
			// Hydrate the device allocation controller so it tracks the existing claim
			daController.Hydrate(ctx)

			// Create a provisioner that uses this controller
			draProv := provisioning.NewProvisioner(env.Client, events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster, fakeClock, daController)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, draProv, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should treat devices reserved by deleting pods as available", func() {
			deviceClass := makeDeviceClass("gpu")
			// Claim allocated to a pod that will be deleting
			existingClaim := makeAllocatedResourceClaim("existing-claim", nil,
				resourcev1.DeviceRequestAllocationResult{
					Request: "request",
					Driver:  "gpu.example.com",
					Pool:    "pool-a",
					Device:  "gpu-0",
				},
			)
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withAllNodes(),
				withSliceDevices("gpu-0"),
			)
			newClaim := makeResourceClaim("new-claim", exactRequest("req-1", "gpu", 1))
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-only"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()

			// Create the deleting pod (create first, then mark as deleting)
			deletingPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deleting-pod",
					Namespace: "default",
				},
			})
			// Create a "deleting" node so the pod's UID ends up in the deleting set
			deletingNode := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name: "deleting-node",
				},
			})
			deletingNodeClaim := test.NodeClaim(v1.NodeClaim{
				Status: v1.NodeClaimStatus{
					NodeName:   deletingNode.Name,
					ProviderID: deletingNode.Spec.ProviderID,
				},
			})
			deletingPod.Spec.NodeName = deletingNode.Name

			pod := makePodWithClaims("new-claim")

			// Apply the pod first to get the auto-generated UID, then set ReservedFor
			ExpectApplied(ctx, env.Client, deviceClass, slice, newClaim, nodePool, deletingNode, deletingNodeClaim, deletingPod, pod)
			existingClaim.Status.ReservedFor = []resourcev1.ResourceClaimConsumerReference{
				{Resource: "pods", Name: "deleting-pod", UID: deletingPod.UID},
			}
			ExpectApplied(ctx, env.Client, existingClaim)
			// Only mark the node as deleting (not the pod). The pod must remain active
			// so that it appears in deletingNodePods and its UID is tracked as deleting.
			ExpectDeletionTimestampSet(ctx, env.Client, deletingNode)
			daController.Hydrate(ctx)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(deletingNode))
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(deletingNodeClaim))

			draProv := provisioning.NewProvisioner(env.Client, events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster, fakeClock, daController)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, draProv, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should keep devices allocated when not all consumers are deleting", func() {
			deviceClass := makeDeviceClass("gpu")
			existingClaim := makeAllocatedResourceClaim("existing-claim", nil,
				resourcev1.DeviceRequestAllocationResult{
					Request: "request",
					Driver:  "gpu.example.com",
					Pool:    "pool-a",
					Device:  "gpu-0",
				},
			)
			// Two consumers: one deleting, one not
			existingClaim.Status.ReservedFor = []resourcev1.ResourceClaimConsumerReference{
				{Resource: "pods", Name: "alive-pod", UID: "alive-pod-uid"},
				{Resource: "pods", Name: "deleting-pod", UID: "deleting-pod-uid"},
			}
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withAllNodes(),
				withSliceDevices("gpu-0"),
			)
			newClaim := makeResourceClaim("new-claim", exactRequest("req-1", "gpu", 1))
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-only"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("new-claim")

			ExpectApplied(ctx, env.Client, deviceClass, existingClaim, slice, newClaim, nodePool, pod)
			daController.Hydrate(ctx)

			draProv := provisioning.NewProvisioner(env.Client, events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster, fakeClock, daController)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, draProv, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should keep devices with no consumers allocated (Releasable=false)", func() {
			deviceClass := makeDeviceClass("gpu")
			// Claim with allocation but empty ReservedFor (Releasable=false)
			existingClaim := makeAllocatedResourceClaim("existing-claim", nil,
				resourcev1.DeviceRequestAllocationResult{
					Request: "request",
					Driver:  "gpu.example.com",
					Pool:    "pool-a",
					Device:  "gpu-0",
				},
			)
			// No ReservedFor set — Releasable will be false
			slice := makeResourceSlice("gpu-slice", "gpu.example.com", "pool-a",
				withAllNodes(),
				withSliceDevices("gpu-0"),
			)
			newClaim := makeResourceClaim("new-claim", exactRequest("req-1", "gpu", 1))
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-only"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("new-claim")

			ExpectApplied(ctx, env.Client, deviceClass, existingClaim, slice, newClaim, nodePool, pod)
			daController.Hydrate(ctx)

			draProv := provisioning.NewProvisioner(env.Client, events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster, fakeClock, daController)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, draProv, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
	})

	Context("Claim Resolution Edge Cases", func() {
		It("should return DRA error when IgnoreDRARequests is enabled", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{IgnoreDRARequests: ptr.To(true)}))
			// Use a provisioner without a device allocation controller
			ignoreProv := provisioning.NewProvisioner(env.Client, events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster, fakeClock, nil)
			deviceClass := makeDeviceClass("gpu")
			claim := makeResourceClaim("gpu-claim", exactRequest("req-1", "gpu", 1))
			it := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			pod := makePodWithClaims("gpu-claim")

			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, ignoreProv, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule a pod normally when the referenced ResourceClaim does not exist", func() {
			it := fake.NewInstanceType(fake.InstanceTypeOptions{Name: "cpu-instance"})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()
			// Pod references a claim that doesn't exist in the API server
			pod := makePodWithClaims("nonexistent-claim")

			ExpectApplied(ctx, env.Client, nodePool, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			// Should schedule normally since resolveClaimsForPod returns empty claims
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should resolve template-based claims via pod status", func() {
			deviceClass := makeDeviceClass("gpu")
			// The actual claim that exists in the API server with a generated name
			claim := makeResourceClaim("generated-claim-name", exactRequest("req-1", "gpu", 1))
			it := makeInstanceTypeWithDRA("gpu-instance", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			nodePool := test.NodePool()

			// Pod uses template-based claim: ResourceClaimTemplateName set, resolved via status
			pod := test.UnschedulablePod(test.PodOptions{
				ResourceClaims: []corev1.PodResourceClaim{
					{Name: "gpu-claim", ResourceClaimTemplateName: ptr.To("gpu-claim-template")},
				},
				ContainerResourceClaims: []corev1.ResourceClaim{
					{Name: "gpu-claim"},
				},
			})
			ExpectApplied(ctx, env.Client, deviceClass, claim, nodePool, pod)
			// Pod status (including ResourceClaimStatuses) must be updated via the status subresource
			pod.Status.ResourceClaimStatuses = []corev1.PodResourceClaimStatus{
				{Name: "gpu-claim", ResourceClaimName: ptr.To("generated-claim-name")},
			}
			Expect(env.Client.Status().Update(ctx, pod)).To(Succeed())
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
	})
})
