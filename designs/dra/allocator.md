# DRA Device Allocator

## Overview

The DRA (Dynamic Resource Allocation) device allocator is the component responsible for determining whether a set of ResourceClaims can be satisfied for a given NodeClaim. It simulates the upstream Kubernetes DRA scheduler's allocation logic, adapted for Karpenter's unique scheduling model where a NodeClaim represents a superposition of candidate instance types rather than a concrete node.

The allocator runs during Karpenter's scheduling loop. For each pod, the allocator receives all of the pod's ResourceClaims — whether already allocated or pending — determines which candidate instance types can satisfy them, accumulates topology requirements from all sources (already-allocated claims and newly allocated devices), and produces an opaque allocation handle that, when committed, reserves devices and records per-claim metadata for subsequent scheduling decisions within the same loop.

## Problem Statement

Karpenter must make provisioning decisions for pods that require DRA devices before a concrete node exists. This creates two challenges that the upstream scheduler does not face:

1. **Instance type superposition.** A NodeClaim is compatible with multiple instance types, each of which provides a different set of template devices. The allocator must evaluate each instance type independently and prune those that cannot satisfy the claims.

2. **Cross-NodeClaim device contention.** Multiple NodeClaims may compete for the same in-cluster devices. Since only one instance type will ultimately be provisioned per NodeClaim, the allocator must track device reservations at the correct granularity: globally across NodeClaims, but conditionally within a single NodeClaim's instance type candidates.

## Device Sources

Devices come from two sources, prioritized in this order:

### In-Cluster Devices

Published to the Kubernetes API server as `ResourceSlice` objects by DRA drivers. These represent real, existing devices on nodes in the cluster. In-cluster devices are organized into **pools**, where each pool is identified by a `(driver, poolName)` pair and may span multiple ResourceSlice objects.

In-cluster devices may be **node-local** (accessible only from nodes matching a `NodeSelector`) or **cluster-wide** (accessible from all nodes via `AllNodes: true`). Node-local devices carry topology requirements that constrain which nodes can use them.

### Template Devices

Provided by the cloud provider as `ResourceSliceTemplate` objects. These represent devices that *will exist* once an instance type is launched but are not yet published to the API server. Template devices are always node-local to the instance they will run on and carry no topology requirements (the instance type itself determines topology).

The allocator prefers in-cluster devices over template devices. This is a natural consequence of the DFS iteration order: in-cluster pool devices are iterated first, and the search commits to the first valid solution found. Preferring in-cluster devices minimizes variance across instance types, since in-cluster allocations are shared while template allocations diverge per instance type.

## ResourceClaim State Handling

All of a pod's ResourceClaims are passed to the allocator regardless of their current allocation state. The allocator classifies each claim and handles it internally before proceeding to the DFS.

Claims are processed sequentially. The allocator maintains **effective requirements** — the NodeClaim's base requirements progressively tightened by each already-allocated claim's topology. Each claim's compatibility is checked against these effective requirements, not just the original NodeClaim requirements. This ensures that mutually incompatible claims (e.g., one pinned to zone A and another to zone B) are detected immediately rather than producing a confusing downstream failure.

### Allocated (In-Cluster)

A claim that already has `status.allocation` set on the API server is fully allocated in the cluster. The allocator reads the topology requirements from `status.allocation.nodeSelector` and checks them for compatibility with the current effective requirements. If incompatible, the allocation fails immediately. If compatible, the requirements are merged into the effective requirements — tightening the baseline for subsequent claims and for pool gathering/filtering — and are included in the `AllocationResult`. No device reservation or DFS is needed for this claim; its devices are already committed.

### Allocated (In-Memory)

Multiple pending pods may reference the same ResourceClaim. When a claim that was not previously allocated is first allocated during the scheduling loop, the allocator records per-claim metadata: the associated NodeClaim ID, whether template devices were used, and the accumulated topology requirements. When a subsequent pod references that same claim, the allocator uses this metadata instead of re-running the DFS:

- **Template devices were used.** The claim is node-local to the original NodeClaim. If the current NodeClaim matches the original, the claim is already satisfied — it is skipped with no additional requirements. If the current NodeClaim is different, the allocation fails immediately; template-allocated claims cannot be satisfied from a different node.

- **In-cluster devices only.** The accumulated topology requirements are checked for compatibility with the current effective requirements. If incompatible, the allocation fails. If compatible, the requirements are merged into the effective requirements (same as in-cluster allocated claims) and included in the allocation result. The claim is skipped for DFS.

### Unallocated

A claim with no allocation — neither in-cluster nor in-memory — proceeds through normal validation and DFS. The DFS starts with effective requirements that include the NodeClaim's base requirements plus any topology constraints accumulated from already-allocated claims, so pool filtering and topology compatibility checks reflect the full set of constraints.

## Allocation Algorithm

### Structure

Allocation proceeds in three phases:

1. **Claim classification.** Each ResourceClaim is classified as allocated (in-cluster), allocated (in-memory), or unallocated (see [ResourceClaim State Handling](#resourceclaim-state-handling)). Already-allocated claims progressively tighten the effective requirements (starting from the NodeClaim's base requirements). Each claim's topology is validated against the effective requirements at the time it is processed, so mutually incompatible claims are detected immediately. Only unallocated claims proceed to the DFS. Pool gathering and filtering use the final effective requirements, so already-allocated claims may eliminate pools before the DFS begins.

2. **Per-instance-type evaluation.** Each candidate instance type is evaluated independently via a full depth-first search (DFS). Instance types whose DFS fails are pruned from the candidate set.

3. **DFS over claims, requests, and slots.** Within a single instance type evaluation, the DFS iterates over three nested dimensions:
   - **Claims** (outer): Each ResourceClaim in the pod's claim list.
   - **Requests** (middle): Each device request within a claim.
   - **Slots** (inner): Each device slot within a request (one slot per device to allocate).

For each slot, the algorithm tries candidate devices in iteration order (in-cluster pools first, then template devices for the current instance type). The first device that passes all checks is tentatively allocated and the search recurses to the next slot. If the subtree fails, the algorithm backtracks and tries the next candidate.

### Allocation Modes

Each device request specifies an allocation mode:

- **ExactCount**: Allocate exactly N devices matching the request's selectors. Devices are drawn from the current pool set and template devices, tried in order until N are found.

- **All**: Allocate *every* matching device. The eligible device set is pre-computed during request validation (all matching in-cluster devices plus all matching template devices for each instance type). Each slot maps to a specific predetermined device. Unlike ExactCount, there is no choice in which device fills each slot; the constraint is that every eligible device must pass allocation checks (not already allocated, constraints satisfied).

### Device Eligibility Checks

For each candidate device, four checks are performed in order:

1. **Already allocated?** The device is rejected if it is already allocated globally (seed set), already allocated for a different NodeClaim (any instance type on that NodeClaim), or already allocated for the same NodeClaim on the same instance type (by a prior pod). A device allocated for the same NodeClaim on a *different* instance type is allowed, since only one instance type will actually be provisioned.

2. **Selector match?** The device must match all CEL selectors from both the `DeviceClass` and the request. Selectors use AND semantics. Match results are cached per `(device, claim, request)` tuple to avoid redundant CEL evaluation across backtrack iterations.

3. **Constraint satisfaction?** The device must satisfy all inter-device constraints on the claim (see [Constraints](#constraints) below). Constraints are stateful; if the device fails a constraint, all previously applied constraints for this device are rolled back.

4. **Topology compatibility?** For non-node-local in-cluster devices with a `NodeSelector`, the device's implied topology requirements must be compatible with the NodeClaim's accumulated requirements. If compatible, the requirements are tightened and the pool set is re-filtered to reflect the narrower topology. This is snapshotted so it can be restored on backtrack.

### Backtracking

When the DFS fails at any point, it unwinds in exact reverse order:

1. The device allocation record is removed.
2. If topology requirements were tightened, the snapshot is restored (requirements and filtered pool set).
3. Constraint state is rolled back via `Remove()` calls in reverse order.

This ensures the allocator can explore the full search space without leaving stale state.

### Timeout

The DFS is bounded by a 5-second context timeout. If the timeout fires during search, the current branch is abandoned. This prevents pathological claim/device combinations from blocking the scheduling loop.

## Constraints

Constraints are inter-device rules evaluated during the DFS. They are **stateful**: each `Add()` call modifies internal state (e.g., pinning a value), and each `Remove()` call reverses exactly one successful `Add()`. This stack-based design enables backtracking.

### MatchAttribute

The only constraint type currently supported. A `MatchAttribute` constraint requires that all devices allocated for the constrained requests share a common value for a specified attribute.

**Behavior:**
- The first device to satisfy the constraint **pins** the attribute value.
- Subsequent devices must have the same value for that attribute.
- On backtrack, when the pinning device is removed, the constraint resets to unpinned.

**Scoping:** A constraint may be scoped to specific request names within a claim, or apply to all requests if no names are specified.

**Evaluation paths:** A constraint uses one of two mutually exclusive evaluation paths, determined by the first device added:

1. **Concrete path.** The device has the attribute in its template. The attribute value is read directly and compared. Once established via concrete values, devices without the attribute are rejected.

2. **Binding fallback path.** The device lacks the attribute (it is runtime-only). The constraint consults the `AttributeBindings` graph to determine whether devices are bound under the attribute for the current `(nodePool, instanceType)`. Once established via bindings, devices with concrete attribute values are rejected.

This mutual exclusivity prevents mixing concrete comparison with binding-based comparison within the same constraint evaluation, which could produce inconsistent results.

### Attribute Lookup

When looking up an attribute on a device, the allocator first tries the fully qualified attribute name. If not found and the attribute name has a domain prefix matching the device's driver name, it retries with just the ID portion (driver-qualified fallback). This accommodates devices that store attributes without the driver domain prefix.

## Attribute Bindings

Attribute bindings model runtime-only attributes that share a value across devices on an instance, where the concrete value is not known at scheduling time. They are declared by cloud providers as part of instance type metadata.

**Example:** A ResourceClaim requires a GPU and NIC that share a PCI root complex. The PCI root ID is only known after the node launches, but the cloud provider declares that specific GPU-NIC pairs on a given instance type will share this attribute.

### Structure

Bindings are indexed by `(attribute, nodePool, instanceType)` and map each device to the set of devices it is bound with.

### Transitivity

Bindings are transitive. If device A is bound to device B and device B is bound to device C under the same `(attribute, nodePool, instanceType)` triple, then A is also bound to C. Transitivity is computed during construction via BFS closure over the direct binding graph.

### Construction

`BuildAttributeBindings` processes cloud provider instance type metadata:
1. For each declared binding group (a set of devices sharing an attribute), symmetric pairs are created between all devices in the group.
2. After all direct bindings are established, a BFS transitive closure is computed per `(attribute, nodePool, instanceType)` triple. The closure is computed from a snapshot of the original direct bindings to avoid contaminating mid-pass results.

Binding groups with fewer than 2 devices are ignored.

## Pool Management

### Pool Gathering

Pools are built from in-cluster `ResourceSlice` objects published to the API server. The gathering process:

1. Groups slices by `(driver, poolName)`.
2. Tracks the highest generation per pool. When a newer generation is encountered, all previously accumulated slices for that pool are discarded.
3. Determines **completeness** by comparing the total slice count at the current generation against the pool's declared `ResourceSliceCount`. Completeness is a global property computed across all slices (matching and non-matching).
4. Filters slices by node affinity compatibility with the NodeClaim's requirements. Only matching slices contribute devices; non-matching slices still participate in generation tracking and completeness checks.
5. Detects **invalid** pools with duplicate device names across slices.

### Pool Filtering

When topology requirements are tightened during the DFS (a non-node-local device narrows the NodeClaim's requirements), the pool set is re-filtered against the new requirements. This is an incremental operation on the cached pool superset, not a full rebuild. Pools with no matching slices after filtering are dropped.

### Pool Caching

After a successful allocation is committed, the resulting pool set is cached by NodeClaim ID. Subsequent allocations for the same NodeClaim reuse the cached pools (re-filtered against current requirements) instead of rebuilding from scratch.

## Request Validation

Before the DFS begins, each ResourceClaim is validated and parsed into internal structures:

1. **DeviceClass resolution.** Each request's `DeviceClassName` is resolved by fetching the `DeviceClass` from the API server. Missing classes cause validation failure.

2. **Selector combination.** Selectors from the DeviceClass and the request are merged (both must be CEL-based). All CEL expressions are compiled and validated upfront.

3. **All-mode pre-computation.** For `All` mode requests, the eligible device set is computed eagerly: all matching in-cluster pool devices and all matching template devices per instance type. Pools must be complete and valid for All-mode allocation; incomplete or invalid pools cause validation failure.

4. **AllocationResultsMaxSize enforcement.** The total device count across all requests is checked against the Kubernetes limit of 32 devices per allocation result. For All-mode requests with template devices, instance types whose template device count would push the total over the limit are pruned. If all instance types are pruned, validation fails.

5. **Unsupported features.** `FirstAvailable` (subrequest) requests are rejected. Only `Exactly` requests are supported. Only CEL selectors are supported. Only `MatchAttribute` constraints are supported.

## Commit Protocol

Allocation results are not applied to the allocator's shared state until explicitly committed. This two-phase approach allows the caller to inspect the result, decide whether to proceed, and only then modify shared state.

### Commit Behavior

When `Commit()` is called on an allocation result:

1. **Device reservation.** All device IDs from the allocation are recorded in the allocator's in-flight tracking, indexed by `(nodeClaimID, instanceTypeID)`. This makes them visible to `isDeviceAllocated()` checks in subsequent allocations.

2. **Pool cache update.** The pool set used during allocation is cached for the NodeClaim, enabling faster pool resolution in subsequent allocations.

3. **Per-claim allocation metadata.** For each newly allocated claim, the allocator records: the associated NodeClaim ID, whether template devices were used, and the accumulated topology requirements. This metadata enables in-memory allocated claim handling when subsequent pods reference the same ResourceClaim (see [ResourceClaim State Handling](#resourceclaim-state-handling)).

### Instance Type Release

When the scheduler prunes an instance type from a NodeClaim's candidate set, `ReleaseInstanceType` removes all device allocations for that instance type on that NodeClaim. Once all instance types referencing a device are released, the device becomes available to other NodeClaims.

## Interaction with the Scheduling Loop

The allocator is instantiated once per scheduling loop and shared across all pod allocation requests. The lifecycle is:

1. **Construction.** The allocator is created with the current set of in-cluster ResourceSlices, the set of globally allocated device IDs, attribute bindings from cloud provider metadata, and a Kubernetes client for DeviceClass resolution.

2. **Per-pod allocation.** For each pod, `Allocate()` is called with the NodeClaim and **all** of the pod's ResourceClaims, regardless of their allocation state. The allocator classifies each claim internally — already-allocated claims (in-cluster or in-memory) contribute topology requirements and may cause early failure, while unallocated claims proceed through validation and DFS. The result is an `AllocationResult` containing the surviving instance types, accumulated topology requirements from all sources (already-allocated claims and newly allocated devices), and an opaque allocation handle.

3. **Commit.** If the caller accepts the result, `Commit()` is called to update the allocator's shared state, including per-claim allocation metadata for in-memory claim reuse.

4. **Release.** If the scheduler later prunes an instance type, `ReleaseInstanceType` is called to free devices that are no longer needed.

The allocator itself is read-only during `Allocate()` calls. Mutation occurs only during `Commit()` and `ReleaseInstanceType()`, which are called by the scheduling loop between pod evaluations.

## NodeClaim Abstraction

The allocator operates on a `NodeClaim` interface that abstracts over three lifecycle phases:

- **Existing initialized nodes.** Have a single known instance type. ResourceSlices returns empty (all devices are already published in-cluster).
- **Pre-initialized nodes.** Have a single known instance type but outstanding template devices not yet published. ResourceSlices returns templates under that instance type.
- **In-flight NodeClaims.** Have multiple candidate instance types. ResourceSlices returns templates for all candidates.

This abstraction allows the allocator to use identical logic regardless of whether it is evaluating an existing node, a node being set up, or a node that does not yet exist.
