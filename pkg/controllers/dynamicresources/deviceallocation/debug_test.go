package deviceallocation_test

import (
	"unique"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	deviceallocation "sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// fullDeviceResult constructs a DeviceRequestAllocationResult with explicit driver and pool.
func fullDeviceResult(driver, pool, device string) resourcev1.DeviceRequestAllocationResult {
	return resourcev1.DeviceRequestAllocationResult{
		Request: "request",
		Driver:  driver,
		Pool:    pool,
		Device:  device,
	}
}

// fullResourceClaim constructs a ResourceClaim with explicit driver/pool device results.
func fullResourceClaim(name string, results ...resourcev1.DeviceRequestAllocationResult) *resourcev1.ResourceClaim {
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name:    "request",
						Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "test-class"},
					},
				},
			},
		},
	}
	if len(results) > 0 {
		claim.Status.Allocation = &resourcev1.AllocationResult{
			Devices: resourcev1.DeviceAllocationResult{
				Results: results,
			},
		}
	}
	return claim
}

var _ = Describe("DebugDump", func() {
	BeforeEach(func() {
		triggerHydration()
	})

	It("returns empty slices when no claims exist", func() {
		state, err := controller.DebugDump(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(state.Pods).To(BeEmpty())
		Expect(state.Claims).To(BeEmpty())
		Expect(state.Pools).To(BeEmpty())
	})

	It("populates all three views for a single claim with a single device reserved for a pod", func() {
		claim := withReservedFor(
			resourceClaim("claim-a", deviceResult("device-0")),
			podRef("pod-a", "uid-a"),
		)
		ExpectApplied(ctx, env.Client, claim)
		ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

		state, err := controller.DebugDump(ctx)
		Expect(err).ToNot(HaveOccurred())

		// Pods view
		Expect(state.Pods).To(HaveLen(1))
		Expect(state.Pods[0]).To(Equal(deviceallocation.DebugPod{
			UID:       "uid-a",
			Namespace: "default",
			Name:      "pod-a",
			Devices:   []string{"driver.example.com/pool-a/device-0"},
		}))

		// Claims view
		Expect(state.Claims).To(HaveLen(1))
		Expect(state.Claims[0]).To(Equal(deviceallocation.DebugClaim{
			Namespace:  "default",
			Name:       "claim-a",
			Devices:    []string{"driver.example.com/pool-a/device-0"},
			Releasable: true,
			Consumers: []deviceallocation.DebugConsumer{
				{UID: "uid-a", Namespace: "default", Name: "pod-a"},
			},
		}))

		// Pools view
		Expect(state.Pools).To(HaveLen(1))
		Expect(state.Pools[0]).To(Equal(deviceallocation.DebugPool{
			Driver: "driver.example.com",
			Pool:   "pool-a",
			Devices: []deviceallocation.DebugDevice{
				{
					Device:     "device-0",
					Claims:     []string{"default/claim-a"},
					Consumers:  []deviceallocation.DebugConsumer{{UID: "uid-a", Namespace: "default", Name: "pod-a"}},
					Releasable: true,
				},
			},
		}))
	})

	It("aggregates devices across multiple claims sharing a device", func() {
		claimA := withReservedFor(
			resourceClaim("claim-a", deviceResult("device-0")),
			podRef("pod-a", "uid-a"),
		)
		claimB := withReservedFor(
			resourceClaim("claim-b", deviceResult("device-0")),
			podRef("pod-b", "uid-b"),
		)
		ExpectApplied(ctx, env.Client, claimA, claimB)
		ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
		ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

		state, err := controller.DebugDump(ctx)
		Expect(err).ToNot(HaveOccurred())

		// Both pods should reference the same device
		Expect(state.Pods).To(HaveLen(2))
		Expect(state.Pods).To(ContainElement(deviceallocation.DebugPod{
			UID: "uid-a", Namespace: "default", Name: "pod-a",
			Devices: []string{"driver.example.com/pool-a/device-0"},
		}))
		Expect(state.Pods).To(ContainElement(deviceallocation.DebugPod{
			UID: "uid-b", Namespace: "default", Name: "pod-b",
			Devices: []string{"driver.example.com/pool-a/device-0"},
		}))

		// Two claims
		Expect(state.Claims).To(HaveLen(2))

		// One pool, one device, two claims
		Expect(state.Pools).To(HaveLen(1))
		Expect(state.Pools[0].Devices).To(HaveLen(1))
		Expect(state.Pools[0].Devices[0].Claims).To(ConsistOf("default/claim-a", "default/claim-b"))
	})

	It("groups devices by pool across multiple drivers", func() {
		claimA := withReservedFor(
			fullResourceClaim("claim-a", fullDeviceResult("driver-1.example.com", "pool-x", "dev-0")),
			podRef("pod-a", "uid-a"),
		)
		claimB := withReservedFor(
			fullResourceClaim("claim-b", fullDeviceResult("driver-2.example.com", "pool-y", "dev-0")),
			podRef("pod-b", "uid-b"),
		)
		ExpectApplied(ctx, env.Client, claimA, claimB)
		ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
		ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

		state, err := controller.DebugDump(ctx)
		Expect(err).ToNot(HaveOccurred())

		// Two pools from different drivers
		Expect(state.Pools).To(HaveLen(2))
		// Sorted by driver name
		Expect(state.Pools[0].Driver).To(Equal("driver-1.example.com"))
		Expect(state.Pools[1].Driver).To(Equal("driver-2.example.com"))
	})

	It("shows a claim with no reservation as not releasable with empty consumers", func() {
		claim := resourceClaim("claim-a", deviceResult("device-0"))
		ExpectApplied(ctx, env.Client, claim)
		ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

		state, err := controller.DebugDump(ctx)
		Expect(err).ToNot(HaveOccurred())

		Expect(state.Claims).To(HaveLen(1))
		Expect(state.Claims[0].Releasable).To(BeFalse())
		Expect(state.Claims[0].Consumers).To(BeEmpty())

		// No pods since no consumers
		Expect(state.Pods).To(BeEmpty())
	})

	It("sorts devices within a pool alphabetically", func() {
		claim := withReservedFor(
			resourceClaim("claim-a",
				deviceResult("device-2"),
				deviceResult("device-0"),
				deviceResult("device-1"),
			),
			podRef("pod-a", "uid-a"),
		)
		ExpectApplied(ctx, env.Client, claim)
		ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

		state, err := controller.DebugDump(ctx)
		Expect(err).ToNot(HaveOccurred())

		Expect(state.Pools).To(HaveLen(1))
		Expect(state.Pools[0].Devices).To(HaveLen(3))
		Expect(state.Pools[0].Devices[0].Device).To(Equal("device-0"))
		Expect(state.Pools[0].Devices[1].Device).To(Equal("device-1"))
		Expect(state.Pools[0].Devices[2].Device).To(Equal("device-2"))
	})

	It("returns consistent device strings across all views", func() {
		claim := withReservedFor(
			resourceClaim("claim-a", deviceResult("device-0")),
			podRef("pod-a", "uid-a"),
		)
		ExpectApplied(ctx, env.Client, claim)
		ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

		state, err := controller.DebugDump(ctx)
		Expect(err).ToNot(HaveOccurred())

		expectedDevice := cloudprovider.DeviceID{
			Driver: unique.Make("driver.example.com"),
			Pool:   unique.Make("pool-a"),
			Device: unique.Make("device-0"),
		}.String()

		// Pod view device string matches
		Expect(state.Pods[0].Devices[0]).To(Equal(expectedDevice))
		// Claim view device string matches
		Expect(state.Claims[0].Devices[0]).To(Equal(expectedDevice))
		// Pool view reconstructed device string matches
		poolDevice := state.Pools[0].Driver + "/" + state.Pools[0].Pool + "/" + state.Pools[0].Devices[0].Device
		Expect(poolDevice).To(Equal(expectedDevice))
	})
})
