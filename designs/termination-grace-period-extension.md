# TerminationGracePeriod Extension Design

**Note:** This is an excerpt from a larger RFC focusing on Karpenter's tainting design for v1. These are the required
changes to `terminationGracePeriod` to meet some of the goals in that RFC. I'm not currently planning on opening a PR
with this RFC upstream.

## Background

The original `terminationGracePeriod` RFC gave the following motivating examples for the feature:

- Cluster admins who want to ensure that nodes are cycled after a given period of time, regardless of user-defined
  disruption controls (such as PDBs or preStop hooks) that might prevent eviction of a pod beyond the configured limit.
  This could be to satisfy security requirements or for convenience.
- Cluster admins who want to allow users to protect long-running jobs from being interrupted by node disruption, up to a
  configured limit.

The current implementation meets these demands by ensuring that once a node begins terminating, the node will be
terminated within a bounded period of time. However, this does not go far enough due to the design of Karpenter's
disruption controller. This is because blocking PDBs and `do-not-disrupt` pods can prevent nodes from ever becoming
candidates for disruption, and hence never begin termination.

This design will propose the following changes to Karpenter's disruption and termination controllers respectively to
better meet the original goals of `terminationGracePeriod`:

- Ensure all nodes marked for eventual disruption (expiration / drift) are eventually disrupted
- Align the treatment of pods with the `karpenter.sh/do-not-disrupt` annotation with PDBs

## Proposal 1: Updated Disruptable Critera for Eventual Disruption

Currently blocking PDBs and pods with the `karpenter.sh/do-not-disrupt` annotation can prevent Karpenter from disrupting
nodes indefinitely. In these cases, nodes never begin termination because they are not considered candidates by the
disruption controller. In order to ensure eventual disruption with `terminationGracePeriod`, we must consider these
nodes candidates. This change will effect both expiration and drift, but not consolidation.

It is important to acknowledge some of the limitations of this approach. The total disruption time, i.e. the time from a
node being expired or drifted to the time it is terminated, is not bounded. Node disruption budgets would still be
respected, and the `terminationGracePeriod` would only take effect once node termination begins.

## Proposal 2: Updated Eviction Behavior

First, a brief summary of Karpenter’s current eviction behavior:

- The node is cordoned with the `karpenter.sh/disruption=disrupting:NoSchedule` taint
- Pods are evicted using the eviction API, respecting PDBs
- If a `terminationGracePeriod` is set on the owning NodePool, pods may be manually deleted bypassing PDBs to ensure
  individual pod `terminationGracePeriodSeconds` are respected
- Once all evictable pods are drained from the node, the finalizer is removed and the node is deleted

What’s missing here is handling for `do-not-disrupt` pods. Today Karpenter treats them as any other pod and will evict
them immediately, leveraging the eviction API. This is due to an assumption that, unless a node is forcefully deleted,
nodes with `do-not-disrupt` pods should not be disrupted. This invariant is altered by proposal 1. To address this
issue, we should think about `do-not-disrupt` pods as pods with a permanently blocking PDB. These pods should not be
evicted unless the annotation is removed, and can be deleted at the appropriate time if a `terminationGracePeriod` is
set.

## Summary

To address the problems laid out in the background this design includes two proposals:

- Updating the disruption criteria for eventual disruption (expiration and drift)
- Updating the eviction behavior for `do-not-disrupt` pods

Together, these updates ensure that cluster operators have an opt-in mechanism to ensure eventual disruption through
`terminationGracePeriod`.
