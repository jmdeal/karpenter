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

package dynamicresources

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	dracel "k8s.io/dynamic-resource-allocation/cel"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const allocateTimeout = 5 * time.Second

// ClaimAllocationMetadata records per-claim allocation state from a prior pod's allocation
// within the same scheduling loop. This enables in-memory claim reuse when multiple pods
// reference the same ResourceClaim.
type ClaimAllocationMetadata struct {
	// NodeClaimID is the NodeClaim that this claim was allocated for.
	NodeClaimID NodeClaimID
	// UsedTemplateDevices is true if the allocation used any template (potential) devices,
	// making the claim node-local to the original NodeClaim.
	UsedTemplateDevices bool
	// Requirements contains the accumulated topology requirements from the allocation.
	// Nil if no topology requirements were produced (e.g., all devices were AllNodes).
	Requirements scheduling.Requirements
}

// Allocator manages DRA device allocation across a single scheduling loop. It is shared across
// all per-pod allocation requests and is read-only during Allocate() calls. Mutation occurs only
// during initialization and Commit().
type Allocator struct {
	inClusterSlices          []ResourceSlice
	allocatedDevices         sets.Set[DeviceID]
	inFlightAllocatedDevices map[NodeClaimID]map[InstanceTypeID]sets.Set[DeviceID]
	inMemoryAllocations      map[string]*ClaimAllocationMetadata // keyed by claim name
	attributeBindings        AttributeBindings
	poolCache                map[NodeClaimID][]*Pool
	kubeClient               client.Client
}

// NewAllocator constructs an Allocator for a single scheduling loop.
func NewAllocator(
	inClusterSlices []ResourceSlice,
	allocatedDevices sets.Set[DeviceID],
	attributeBindings AttributeBindings,
	kubeClient client.Client,
) *Allocator {
	return &Allocator{
		inClusterSlices:          inClusterSlices,
		allocatedDevices:         allocatedDevices,
		inFlightAllocatedDevices: make(map[NodeClaimID]map[InstanceTypeID]sets.Set[DeviceID]),
		inMemoryAllocations:      make(map[string]*ClaimAllocationMetadata),
		attributeBindings:        attributeBindings,
		poolCache:                make(map[NodeClaimID][]*Pool),
		kubeClient:               kubeClient,
	}
}

// AllocationResult contains the output of a successful Allocate() call.
type AllocationResult struct {
	// InstanceTypes is the set of instance types whose allocation succeeded.
	InstanceTypes []InstanceTypeID
	// Requirements contains the accumulated topology requirements from all sources:
	// already-allocated claims (in-cluster and in-memory) and newly allocated devices.
	Requirements scheduling.Requirements
	// Allocation is the opaque handle used to commit the allocation to the Allocator's state.
	Allocation Allocation
}

// Allocation is the interface for committing a successful allocation to the Allocator's shared state.
type Allocation interface {
	Commit()
}

// allocation commits per-instance-type device allocations (both in-cluster and template).
type allocation struct {
	allocator           *Allocator
	nodeClaimID         NodeClaimID
	deviceIDsByIT       map[InstanceTypeID][]DeviceID
	deviceIDsByClaimIT  map[int]map[InstanceTypeID][]DeviceID // per-claim, per-IT device IDs
	pools               []*Pool
	claimMetadata       map[string]*ClaimAllocationMetadata // per-claim metadata to record on commit
}

func (a *allocation) Commit() {
	for itID, deviceIDs := range a.deviceIDsByIT {
		ncDevices, ok := a.allocator.inFlightAllocatedDevices[a.nodeClaimID]
		if !ok {
			ncDevices = make(map[InstanceTypeID]sets.Set[DeviceID])
			a.allocator.inFlightAllocatedDevices[a.nodeClaimID] = ncDevices
		}
		itDevices, ok := ncDevices[itID]
		if !ok {
			itDevices = sets.New[DeviceID]()
			ncDevices[itID] = itDevices
		}
		for _, id := range deviceIDs {
			itDevices.Insert(id)
		}
	}
	a.allocator.poolCache[a.nodeClaimID] = a.pools
	for claimName, meta := range a.claimMetadata {
		a.allocator.inMemoryAllocations[claimName] = meta
	}
}

// ReleaseInstanceType removes all device allocations for a specific instance type on a NodeClaim.
// Called by the scheduler when an instance type is pruned from a NodeClaim's candidate set.
// Once all instance types referencing a device are released, the device becomes available
// to other NodeClaims.
func (a *Allocator) ReleaseInstanceType(nodeClaimID NodeClaimID, itID InstanceTypeID) {
	if ncDevices, ok := a.inFlightAllocatedDevices[nodeClaimID]; ok {
		delete(ncDevices, itID)
		if len(ncDevices) == 0 {
			delete(a.inFlightAllocatedDevices, nodeClaimID)
		}
	}
}

// matchKey is used to cache CEL selector evaluation results per (device, claim, request) tuple.
type matchKey struct {
	DeviceID     DeviceID
	ClaimIndex   int
	RequestIndex int
}

// reqPoolSnapshot captures the incremental requirements and pool set at a point during the DFS,
// enabling restoration on backtrack when a non-node-local device tightens requirements.
type reqPoolSnapshot struct {
	reqs  scheduling.Requirements
	pools []*Pool
}

// deviceAllocation records a single device allocation during the DFS.
type deviceAllocation struct {
	claimIndex   int
	requestIndex int
	slotIndex    int
	deviceID     DeviceID
}

// allocator is the per-Allocate() child struct that holds mutable state for the current DFS.
type allocator struct {
	*Allocator
	ctx                  context.Context
	nodeClaim            NodeClaim
	pools                []*Pool
	claimData            []*ClaimData
	templateDevicesByIT  map[InstanceTypeID][]DeviceWithID
	celCache             *dracel.Cache
	allocatingDevices    sets.Set[DeviceID]
	deviceMatchesRequest map[matchKey]bool
	incrementalReqs      scheduling.Requirements
	baselineReqs         scheduling.Requirements
	reqPoolSnapshots     []reqPoolSnapshot
	allocated            []deviceAllocation
	// itID is the current instance type being evaluated in the DFS.
	itID InstanceTypeID
}

// Allocate attempts to satisfy the given ResourceClaims for the specified NodeClaim. It returns
// an AllocationResult on success or an error if allocation is not possible.
//
// All claims are passed regardless of allocation state. The allocator classifies each claim:
//   - Allocated (in-cluster): status.allocation is set. Topology requirements are extracted
//     and merged into the baseline. No DFS needed.
//   - Allocated (in-memory): a prior pod in this scheduling loop allocated this claim.
//     Metadata is used to validate compatibility and merge topology.
//   - Unallocated: proceeds through validation and DFS.
func (a *Allocator) Allocate(
	ctx context.Context,
	nodeClaim NodeClaim,
	claims []*resourcev1.ResourceClaim,
) (*AllocationResult, error) {
	if len(claims) == 0 {
		return &AllocationResult{
			InstanceTypes: nodeClaim.InstanceTypes(),
			Requirements:  scheduling.NewRequirements(),
		}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, allocateTimeout)
	defer cancel()

	// Phase 1: Classify claims and build effective requirements from already-allocated claims.
	// Effective requirements start from the NodeClaim's base requirements and are progressively
	// tightened as each already-allocated claim contributes topology. Each claim is checked
	// against the effective requirements at the time it is processed, so mutually incompatible
	// claims (e.g., one pinned to zone A and another to zone B) are detected immediately.
	effectiveReqs := copyRequirements(nodeClaim.Requirements())
	var unallocatedClaims []*resourcev1.ResourceClaim
	newClaimMetadata := make(map[string]*ClaimAllocationMetadata)

	for _, claim := range claims {
		// In-cluster allocated: status.allocation is set.
		if claim.Status.Allocation != nil {
			reqs, err := nodeSelectorsToRequirements(claim.Status.Allocation.NodeSelector)
			if err != nil {
				return nil, fmt.Errorf("claim %q: %w", claim.Name, err)
			}
			if reqs != nil {
				if !effectiveReqs.IsCompatible(*reqs, scheduling.AllowUndefinedWellKnownLabels) {
					return nil, fmt.Errorf("claim %q: in-cluster allocation topology is incompatible with NodeClaim requirements", claim.Name)
				}
				effectiveReqs.Add(reqs.Values()...)
			}
			continue
		}

		// In-memory allocated: a prior pod already allocated this claim in this loop.
		if meta, ok := a.inMemoryAllocations[claim.Name]; ok {
			if meta.UsedTemplateDevices {
				// Template-allocated claims are node-local to the original NodeClaim.
				if meta.NodeClaimID != nodeClaim.ID() {
					return nil, fmt.Errorf("claim %q: template-allocated claim is bound to a different NodeClaim", claim.Name)
				}
				// Same NodeClaim — claim is already satisfied, no requirements to add.
			} else {
				// In-cluster only — check topology compatibility.
				if meta.Requirements != nil {
					if !effectiveReqs.IsCompatible(meta.Requirements, scheduling.AllowUndefinedWellKnownLabels) {
						return nil, fmt.Errorf("claim %q: in-memory allocation topology is incompatible with NodeClaim requirements", claim.Name)
					}
					effectiveReqs.Add(meta.Requirements.Values()...)
				}
			}
			continue
		}

		// Unallocated — will proceed through DFS.
		unallocatedClaims = append(unallocatedClaims, claim)
	}

	// Compute the baseline additions (topology from already-allocated claims, excluding
	// the NodeClaim's original requirements). This is what gets merged into the AllocationResult.
	baselineReqs := scheduling.NewRequirements()
	for _, v := range effectiveReqs.Values() {
		baselineReqs.Add(v)
	}

	// If there are no unallocated claims, return early with baseline requirements.
	if len(unallocatedClaims) == 0 {
		return &AllocationResult{
			InstanceTypes: nodeClaim.InstanceTypes(),
			Requirements:  baselineReqs,
		}, nil
	}

	// Phase 2: Pool gathering with cache, using the tightened effective requirements.
	var pools []*Pool
	if cached, ok := a.poolCache[nodeClaim.ID()]; ok {
		pools = FilterPools(cached, effectiveReqs)
	} else {
		pools = GatherPools(a.inClusterSlices, effectiveReqs)
	}

	// Build template devices by instance type.
	resourceSlices := nodeClaim.ResourceSlices()
	templateDevicesByIT := make(map[InstanceTypeID][]DeviceWithID, len(resourceSlices))
	for itID, slices := range resourceSlices {
		for _, s := range slices {
			for _, d := range s.Devices() {
				templateDevicesByIT[itID] = append(templateDevicesByIT[itID], DeviceWithID{
					Device: d,
					ID: DeviceID{
						Driver: s.Driver(),
						Pool:   s.Pool().Name,
						Device: d.Name,
					},
				})
			}
		}
	}

	// Create child allocator.
	child := &allocator{
		Allocator:            a,
		ctx:                  ctx,
		nodeClaim:            nodeClaim,
		pools:                pools,
		templateDevicesByIT:  templateDevicesByIT,
		celCache:             dracel.NewCache(0, dracel.Features{}),
		allocatingDevices:    sets.New[DeviceID](),
		deviceMatchesRequest: make(map[matchKey]bool),
		incrementalReqs:      scheduling.NewRequirements(),
		baselineReqs:         baselineReqs,
	}

	// Validate unallocated claims and build ClaimData. Binding fallback is nil here — it is
	// set per-IT before each DFS run since it depends on the instance type.
	child.claimData = make([]*ClaimData, len(unallocatedClaims))
	for i, claim := range unallocatedClaims {
		cd, err := ValidateClaimRequest(ctx, a.kubeClient, claim, i, pools, templateDevicesByIT, child.celCache, nil)
		if err != nil {
			return nil, fmt.Errorf("validating claim %q: %w", claim.Name, err)
		}
		child.claimData[i] = cd
	}

	result, err := child.allocate(nodeClaim.InstanceTypes())
	if err != nil {
		return nil, err
	}

	// Merge baseline requirements (from already-allocated claims) into the DFS result.
	result.Requirements.Add(baselineReqs.Values()...)

	// Build per-claim metadata for newly allocated claims.
	// Build a lookup set of template device IDs for UsedTemplateDevices detection.
	templateDeviceIDs := sets.New[DeviceID]()
	for _, devices := range templateDevicesByIT {
		for _, d := range devices {
			templateDeviceIDs.Insert(d.ID)
		}
	}
	alloc := result.Allocation.(*allocation)
	for i, claim := range unallocatedClaims {
		usedTemplate := false
		if claimITs, ok := alloc.deviceIDsByClaimIT[i]; ok {
			for _, ids := range claimITs {
				for _, id := range ids {
					if templateDeviceIDs.Has(id) {
						usedTemplate = true
						break
					}
				}
				if usedTemplate {
					break
				}
			}
		}
		newClaimMetadata[claim.Name] = &ClaimAllocationMetadata{
			NodeClaimID:         nodeClaim.ID(),
			UsedTemplateDevices: usedTemplate,
			Requirements:        result.Requirements,
		}
	}
	alloc.claimMetadata = newClaimMetadata

	return result, nil
}

// nodeSelectorsToRequirements extracts scheduling requirements from a NodeSelector.
// Returns nil if the NodeSelector is nil (no topology constraints).
func nodeSelectorsToRequirements(ns *corev1.NodeSelector) (*scheduling.Requirements, error) {
	if ns == nil {
		return nil, nil
	}
	reqs := scheduling.NewRequirements()
	for _, term := range ns.NodeSelectorTerms {
		termReqs := scheduling.NewNodeSelectorRequirements(term.MatchExpressions...)
		reqs.Add(termReqs.Values()...)
	}
	return &reqs, nil
}

// allocate runs a per-instance-type DFS over in-cluster and template devices.
// In-cluster devices are iterated first so the DFS naturally prefers them, minimizing
// variance across instance types. Each IT gets a full DFS; ITs whose DFS fails are pruned.
func (a *allocator) allocate(instanceTypes []InstanceTypeID) (*AllocationResult, error) {
	var survivingITs []InstanceTypeID
	deviceIDsByIT := make(map[InstanceTypeID][]DeviceID)
	deviceIDsByClaimIT := make(map[int]map[InstanceTypeID][]DeviceID)
	var resultReqs scheduling.Requirements

	// Snapshot initial state for restoration between IT attempts.
	initPools := a.pools

	for _, itID := range instanceTypes {
		select {
		case <-a.ctx.Done():
			return nil, a.ctx.Err()
		default:
		}

		// Restore to initial state.
		a.restoreState(initPools)

		// Set binding fallback for this IT on all constraints.
		a.setBindingFallback(&AttributeBindingFallback{
			Bindings:       a.attributeBindings,
			NodePool:       a.nodeClaim.NodePoolID().Value(),
			InstanceTypeID: itID,
		})

		a.itID = itID
		if a.dfs(0, 0, 0) {
			survivingITs = append(survivingITs, itID)
			// Collect device IDs globally and per-claim for this IT.
			ids := make([]DeviceID, len(a.allocated))
			for i, da := range a.allocated {
				ids[i] = da.deviceID
				if deviceIDsByClaimIT[da.claimIndex] == nil {
					deviceIDsByClaimIT[da.claimIndex] = make(map[InstanceTypeID][]DeviceID)
				}
				deviceIDsByClaimIT[da.claimIndex][itID] = append(deviceIDsByClaimIT[da.claimIndex][itID], da.deviceID)
			}
			deviceIDsByIT[itID] = ids
			resultReqs = a.incrementalReqs
		}

		// Clear binding fallback.
		a.setBindingFallback(nil)
	}

	if len(survivingITs) == 0 {
		return nil, fmt.Errorf("no instance type can satisfy the allocation")
	}

	return &AllocationResult{
		InstanceTypes: survivingITs,
		Requirements:  resultReqs,
		Allocation: &allocation{
			allocator:          a.Allocator,
			nodeClaimID:        a.nodeClaim.ID(),
			deviceIDsByIT:      deviceIDsByIT,
			deviceIDsByClaimIT: deviceIDsByClaimIT,
			pools:              a.pools,
		},
	}, nil
}

// dfs runs the depth-first search over claims, requests, and device slots. Devices are
// iterated lazily from the current pools and template devices rather than from a prebuilt
// candidate list, so pool re-filtering during requirement tightening is automatically
// reflected in subsequent iterations.
func (a *allocator) dfs(claimIdx, reqIdx, slotIdx int) bool {
	select {
	case <-a.ctx.Done():
		return false
	default:
	}

	// Base case: all claims processed.
	if claimIdx >= len(a.claimData) {
		return true
	}

	cd := a.claimData[claimIdx]

	// Advance past completed requests/claims.
	if reqIdx >= len(cd.Requests) {
		return a.dfs(claimIdx+1, 0, 0)
	}
	rd := &cd.Requests[reqIdx]
	numSlots := a.numSlots(rd)
	if slotIdx >= numSlots {
		return a.dfs(claimIdx, reqIdx+1, 0)
	}

	if rd.AllocationMode == resourcev1.DeviceAllocationModeAll {
		return a.dfsAllMode(claimIdx, reqIdx, slotIdx, cd, rd)
	}
	return a.dfsExactCount(claimIdx, reqIdx, slotIdx, cd, rd)
}

// numSlots returns the number of device slots to fill for a request.
func (a *allocator) numSlots(rd *RequestData) int {
	if rd.AllocationMode == resourcev1.DeviceAllocationModeAll {
		return len(rd.AllDevices) + len(rd.AllTemplateDevicesByIT[a.itID])
	}
	return rd.NumDevices
}

// dfsExactCount handles a single slot for an ExactCount request by iterating devices from
// the current pools (in-cluster) and, if enabled, template devices.
func (a *allocator) dfsExactCount(claimIdx, reqIdx, slotIdx int, cd *ClaimData, rd *RequestData) bool {
	// In-cluster devices from pools (reflects current pool state after any requirement tightening).
	for _, pool := range a.pools {
		for _, d := range pool.Devices {
			if a.tryDevice(claimIdx, reqIdx, slotIdx, cd, rd, d) {
				return true
			}
		}
	}
	// Template devices for the current instance type.
	for _, d := range a.templateDevicesByIT[a.itID] {
		if a.tryDevice(claimIdx, reqIdx, slotIdx, cd, rd, d) {
			return true
		}
	}
	return false
}

// dfsAllMode handles a single slot for an All-mode request. Each slot maps to a specific
// predetermined device: in-cluster devices first, then template devices.
func (a *allocator) dfsAllMode(claimIdx, reqIdx, slotIdx int, cd *ClaimData, rd *RequestData) bool {
	inClusterCount := len(rd.AllDevices)
	if slotIdx < inClusterCount {
		d := rd.AllDevices[slotIdx]
		return a.tryDevice(claimIdx, reqIdx, slotIdx, cd, rd, d)
	}
	// Template device slot.
	templateIdx := slotIdx - inClusterCount
	templateDevices := rd.AllTemplateDevicesByIT[a.itID]
	if templateIdx < len(templateDevices) {
		d := templateDevices[templateIdx]
		return a.tryDevice(claimIdx, reqIdx, slotIdx, cd, rd, d)
	}
	return false
}

// tryDevice attempts to allocate a single device at the given position in the DFS tree.
// Returns true if the subtree rooted at this device leads to a complete solution.
func (a *allocator) tryDevice(
	claimIdx, reqIdx, slotIdx int,
	cd *ClaimData,
	rd *RequestData,
	dw DeviceWithID,
) bool {
	deviceID := dw.ID

	// 1. Already allocated?
	if a.isDeviceAllocated(deviceID) {
		return false
	}
	if a.allocatingDevices.Has(deviceID) {
		return false
	}

	// 2. Selector match?
	mk := matchKey{DeviceID: deviceID, ClaimIndex: claimIdx, RequestIndex: reqIdx}
	matched, cached := a.deviceMatchesRequest[mk]
	if !cached {
		var err error
		matched, err = DeviceMatchesSelectors(a.ctx, dw.Device, deviceID, rd.Selectors, a.celCache)
		if err != nil {
			return false
		}
		a.deviceMatchesRequest[mk] = matched
	}
	if !matched {
		return false
	}

	// 3. Constraint satisfaction.
	constraintsAdded := 0
	for _, con := range cd.Constraints {
		if !con.Add(rd.Name, dw.Device, deviceID) {
			for j := constraintsAdded - 1; j >= 0; j-- {
				cd.Constraints[j].Remove(rd.Name, dw.Device, deviceID)
			}
			return false
		}
		constraintsAdded++
	}

	// 4. Requirement compatibility (devices with topology requirements only).
	pushedSnapshot := false
	if dw.TopologyRequirements != nil {
		if !a.incrementalReqs.IsCompatible(*dw.TopologyRequirements, scheduling.AllowUndefinedWellKnownLabels) {
			for j := constraintsAdded - 1; j >= 0; j-- {
				cd.Constraints[j].Remove(rd.Name, dw.Device, deviceID)
			}
			return false
		}
		// Push snapshot and update.
		a.reqPoolSnapshots = append(a.reqPoolSnapshots, reqPoolSnapshot{
			reqs:  copyRequirements(a.incrementalReqs),
			pools: a.pools,
		})
		a.incrementalReqs.Add(dw.TopologyRequirements.Values()...)
		a.pools = FilterPools(a.pools, a.incrementalReqs)
		pushedSnapshot = true
	}

	// Record allocation.
	a.allocatingDevices.Insert(deviceID)
	a.allocated = append(a.allocated, deviceAllocation{
		claimIndex:   claimIdx,
		requestIndex: reqIdx,
		slotIndex:    slotIdx,
		deviceID:     deviceID,
	})

	// Recurse.
	if a.dfs(claimIdx, reqIdx, slotIdx+1) {
		return true
	}

	// Backtrack — undo in reverse order of application: allocation, then requirements/pools,
	// then constraints.
	a.allocated = a.allocated[:len(a.allocated)-1]
	a.allocatingDevices.Delete(deviceID)

	if pushedSnapshot {
		snapshot := a.reqPoolSnapshots[len(a.reqPoolSnapshots)-1]
		a.reqPoolSnapshots = a.reqPoolSnapshots[:len(a.reqPoolSnapshots)-1]
		a.incrementalReqs = snapshot.reqs
		a.pools = snapshot.pools
	}

	for j := constraintsAdded - 1; j >= 0; j-- {
		cd.Constraints[j].Remove(rd.Name, dw.Device, deviceID)
	}

	return false
}

// isDeviceAllocated checks whether a device is unavailable for allocation.
//
// Device allocation constraints:
//  1. Blocked if allocated in real cluster state (seed allocatedDevices set).
//  2. Blocked if allocated for a different NodeClaim (any IT on that NC).
//  3. Allowed if allocated for the same NodeClaim on a different IT (only one IT
//     will be provisioned, so the device is only actually consumed once).
//  4. Blocked if allocated for the same NodeClaim on the same IT (prior pod).
func (a *allocator) isDeviceAllocated(deviceID DeviceID) bool {
	if a.allocatedDevices.Has(deviceID) {
		return true
	}
	for ncID, ncDevices := range a.inFlightAllocatedDevices {
		if ncID == a.nodeClaim.ID() {
			// Same NC: only blocked if the current IT already allocated this device.
			if itDevices, ok := ncDevices[a.itID]; ok && itDevices.Has(deviceID) {
				return true
			}
		} else {
			// Different NC: blocked if any IT allocated this device.
			for _, itDevices := range ncDevices {
				if itDevices.Has(deviceID) {
					return true
				}
			}
		}
	}
	return false
}

// restoreState resets the child allocator's mutable DFS state for a new IT attempt.
func (a *allocator) restoreState(pools []*Pool) {
	a.allocated = nil
	a.incrementalReqs = scheduling.NewRequirements()
	a.pools = pools
	a.allocatingDevices = sets.New[DeviceID]()
	a.reqPoolSnapshots = nil
}

// copyRequirements creates a shallow copy of a Requirements map.
func copyRequirements(reqs scheduling.Requirements) scheduling.Requirements {
	cp := scheduling.NewRequirements()
	cp.Add(reqs.Values()...)
	return cp
}

// setBindingFallback sets or clears the AttributeBindingFallback on all MatchAttributeConstraints.
func (a *allocator) setBindingFallback(fallback *AttributeBindingFallback) {
	for _, cd := range a.claimData {
		for _, c := range cd.Constraints {
			if mac, ok := c.(*MatchAttributeConstraint); ok {
				mac.AttributeBindingFallback = fallback
			}
		}
	}
}
