# Spec — The change ledger

Status: BUILT 2026-08-05, both halves. Relay detection, diff and delta push under an `inventory:`
configuration root; the control-plane ledger in migration 0015 with at-least-once dedup,
baseline-continuity boundaries, per-scope freshness off the heartbeat, retention pruning, and the
Investigation brief consuming it as navigation with ledger-horizon and ledger-staleness coverage
gaps. Implementation decisions this document left open, and two recorded deviations (the
event-derived change list is retained beside the ledger; the controller revision and pod template
hash are realised as generation, the Deployment revision annotation and a Relay-computed template
digest), are in `plans/change-ledger-implementation.md`.
Date: 2026-07-31 (built 2026-08-05)
Repositories: detection and delta push in the Relay (`oc-relay`); the ledger and its consumers in
the Go control plane
Decision records: ADR-010 (persist what decays; sync identity and change, never state), ADR-004
(inventory synchronization is separate from investigation jobs), ADR-003 as amended (environment
comes from the Connection), ADR-001 point 9 (periodic list-and-diff in v1; continuous watch
deferred)
Glossary: `CONTEXT.md`

## Problem Statement

"What changed?" is the most productive question in incident investigation, and at 03:40 it is
frequently unanswerable.

Kubernetes does not keep the answer. Events expire on a default one-hour TTL. ReplicaSet history is
bounded by `revisionHistoryLimit`, ten by default, and carries creation timestamps rather than a
description of what moved. A ConfigMap change leaves no history at all — the object is simply
different now, and nothing records that it used to be something else or when it stopped being that.

So an investigation into a failure that began forty minutes ago is reading a cluster that has
already forgotten why. The first-investigation slice handles this honestly by reading what survives
and recording a CoverageGap where the window it was asked about extends past what the cluster
retains. That is the correct behaviour and it is not a solution: the gap will appear in exactly the
scenarios where a change is the cause, which is most of them.

This is the one class of context that decays. Containment, placement and current state are all fully
recoverable from a live read at any moment, which is why ADR-010 persists none of them. Change
history is recoverable only if somebody was recording.

## Solution

The Relay detects changes locally and pushes deltas; the control plane keeps the ledger.

The control plane sends an inventory synchronization policy once — what to watch, at what interval.
The Relay schedules locally, lists and diffs locally, and sends a message only when something
actually changed. At the stated scale roughly ninety-nine percent of ticks report nothing, and
ADR-004 already decided that such work must not travel the durable, leased, epoch-fenced job path:
deltas are at-least-once with a dedup key, and losing one tick is recovered by the next full
reconcile.

What is recorded is **declared intent and identity**, never observed state. That a Deployment's
image moved from one tag to another at 14:02 is a change. That it currently has three ready replicas
is state, and state is read on demand during an investigation or not at all. This is the line that
keeps an investigation product from becoming a monitoring platform, and it is the reason this
specification is narrow.

The ledger then answers one question for the Investigation brief: **what changed around this
resource, in this window.**

## User Stories

1. As an on-call engineer, I want to know what changed around the failing workload before I was
   paged, so that the question I would have asked first is already answered.
2. As an on-call engineer, I want changes I can no longer see in the cluster, so that a deploy from
   three hours ago is still visible after the cluster has forgotten it.
3. As an on-call engineer, I want to know exactly which fields changed, so that "a deploy happened"
   becomes "the memory limit was halved".
4. As an on-call engineer, I want to know when the platform started watching, so that I do not read
   silence before that moment as nothing having happened.
5. As an on-call engineer, I want the ledger's freshness shown, so that I know whether it is current
   or the Relay has been offline.
6. As an investigator, I want changes scoped to the resource under investigation and its owners, so
   that a busy namespace does not drown the relevant one.
7. As an investigator, I want a window predating the ledger's start to produce a CoverageGap, so
   that an absence of recorded change is never reported as an absence of change.
8. As an investigator, I want a change recorded with both the time the cluster observed it and the
   time it was received, so that a delayed delivery is distinguishable from a delayed change.
9. As an investigator, I want the ledger to be a navigation index rather than citable evidence, so
   that a conclusion resting on a change is revalidated live before it is stated.
10. As an operator, I want to choose which namespaces are watched, so that synchronization does not
    reach further than I intend.
11. As an operator, I want to set a floor on the interval that the control plane cannot go below, so
    that a server-side change cannot increase load on my cluster.
12. As an operator, I want the Relay to survive being offline and reconcile when it returns, so that
    a restart does not permanently lose a window.
13. As an operator, I want to know that a gap exists after an outage, so that a quiet period is
    distinguishable from a period nobody was watching.
14. As an operator, I want the volume of synchronization traffic bounded, so that watching does not
    become the load I installed this to avoid.
15. As a security reviewer, I want no secret values in the ledger, so that watching a ConfigMap or a
    Secret reference does not become an exfiltration path.
16. As a security reviewer, I want Secret changes recorded by identity and version only, never by
    content, so that the fact of a rotation is visible and the value is not.
17. As a security reviewer, I want deltas attributed to the Connection that produced them, so that
    the Environment is inherited rather than declared.
18. As a security reviewer, I want the synchronization path to carry no capability of executing
    anything, so that widening it cannot widen execution.
19. As an engineer, I want deltas deduplicated by key, so that at-least-once delivery does not
    produce duplicate history.
20. As an engineer, I want the first synchronization recorded as a baseline rather than as a change,
    so that installing the Relay does not appear as everything changing at once.
21. As an engineer, I want the ledger retained on its own schedule, so that history useful for
    investigation does not grow without bound.
22. As an engineer, I want this path kept out of the job table, so that a high-volume, mostly-empty
    stream does not share a mechanism designed for low-volume, must-never-be-lost work.
23. As an engineer, I want the watched field set to be explicit and small, so that "what we record"
    is a decision rather than an accident of what the API returns.
24. As the founder, I want this to record change and not state, so that the product does not drift
    into monitoring one field at a time.

## Implementation Decisions

**Periodic list-and-diff in v1.** ADR-001 point 9 already deferred the continuous watch to a
streaming mode, and the gRPC session makes that a natural later extension rather than a second
protocol. Polling at a modest interval is enough to place a change within a window, and a watch
buys precision that the investigation does not yet need.

**What is watched is small, explicit, and about intent.** For each workload in scope: container
images, replica count as *declared*, resource requests and limits, the names and versions of
referenced ConfigMaps and Secrets, the controller revision, and the pod template hash. Nothing that
is a status field. No ready counts, no available replicas, no conditions, no pod phases.

**Secret and ConfigMap changes are recorded by identity and version, never by content.** That a
Secret named `db-credentials` moved to a new resource version at 14:02 is the fact an investigation
needs. Its content is not, and recording it would turn synchronization into the exfiltration path
this architecture exists to avoid.

**The first synchronization is a baseline, marked as such.** Otherwise installing the Relay appears
in the ledger as every workload changing simultaneously, and the first investigation after an
install would blame the install. The baseline carries the observation time and is excluded from
change queries.

**The ledger knows nothing before its baseline, and says so.** A brief whose window opens earlier
than the ledger's baseline for that scope records a CoverageGap naming the boundary. Same rule for
a window overlapping a period when the Relay was disconnected: a gap, not silence.

**Freshness is recorded per scope**, so the brief can state when the ledger was last confirmed
current rather than implying it is.

**Deltas are at-least-once with a dedup key, never leased or fenced.** ADR-004's reasoning is
unchanged: durable-truth-per-job is right for low-volume high-value work that must never be lost or
run twice, and wrong for high-volume work that is almost always empty. The dedup key is derived
from the Connection, the object identity including UID, and the observed revision, so a redelivery
collapses rather than duplicating.

**A delta names its Connection**, and the control plane derives the Environment from it. Nothing
about synchronization declares a scope.

**Object identity includes UID.** A Deployment deleted and recreated under the same name is not the
same object, and a ledger that treats it as one produces a change history that reads as a mutation
where there was a replacement.

**Interval is requested by the control plane and floored locally.** Same ownership rule as
destination allowlists, volume caps and redaction: the server asks, local configuration constrains.

**Configuration root is `inventory:`, not `discovery:`.** ADR-004 settled this; an on-demand
investigation capability named `...discovery` already exists and an operator reading `discovery:`
in a config file would reasonably expect it to configure that.

**The ledger is a navigation index, never citable.** ADR-010's rule applies without exception: the
brief may state that a change was recorded, and any conclusion resting on it revalidates the current
state live and cites the Observation that revalidation produced. A ledger row is never an
EvidenceItem.

**Retention is its own schedule**, expressed in days and independent of evidence retention, because
this is derived operational context rather than investigation record.

**Nothing on this path can execute anything.** The synchronization messages carry observations
outward only. There is no request-response, no arguments, and no capability invocation, so widening
what is watched can never widen what can be run.

## Testing Decisions

**What makes a good test here.** It asserts what an investigator could observe: a change made in a
real cluster appears in the ledger with the fields that moved; a window predating the baseline
produces a gap; a redelivered delta produces no second row. It does not assert the diff algorithm.

**Two seams, both existing.** The Relay's own suite for detection and diffing, and the end-to-end
process harness for the property that cannot be unit tested: **a change made in a real cluster
becomes a durable ledger row in the control plane, and a change to a status field does not.** That
second half is the test that keeps ADR-010's line honest, and it must be written so that widening
the watched field set makes it fail.

**Scale is asserted, not assumed.** A test that a tick with no change produces no message, and that
its cost is bounded, because the entire justification for this path being separate from `relay_job`
is volume.

**Scenarios.**

- A workload image change in a real cluster appears in the ledger naming the field that moved.
- A resource-limit change appears with its before and after values.
- A change to a status field produces no ledger row.
- A tick with no change produces no message.
- The first synchronization is recorded as a baseline and excluded from change queries.
- A brief window opening before the baseline records a CoverageGap.
- A brief window overlapping a Relay disconnection records a CoverageGap.
- A redelivered delta produces no second row.
- A Deployment deleted and recreated under the same name is recorded as a different object.
- A Secret rotation is recorded by name and version, and its content appears nowhere.
- A delta carries the Environment of its Connection, which nothing declared.
- A control-plane-requested interval below the local floor is refused and the floor is applied.
- A ledger row is not accepted as an EvidenceItem by evidence validation.

**Prior art.** The intake path establishes at-least-once handling deduplicated at the boundary,
which is the same shape as delta dedup and is already tested against real redelivery. The end-to-end
harness establishes assertion over durable control-plane state after a real cluster action. The
relay session establishes the outbound stream this path reuses.

## Out of Scope

- Continuous watch. Deferred by ADR-001 point 9 to a streaming mode; the interval is enough to
  place a change in a window.
- Any resource inventory beyond what is needed to attribute a change. ADR-010 reads containment live
  and this specification does not reopen that.
- Runtime state of any kind: replicas ready, pod phases, conditions, restart counts, metrics.
- Dependency or call topology. ADR-010 restricts it to authoritative customer sources and defers it.
- Changes from outside Kubernetes — ArgoCD, Flux, Terraform, CI, cloud audit logs. Each is a
  Connection kind of its own and a later slice; the ledger's shape is designed to accept them, and
  none is built here.
- Ownership and on-call mapping.
- Alerting on change. Detecting that something changed is not the same as deciding it matters, and
  deciding it matters is what an investigation does.

## Further Notes

The temptation this specification exists to resist is one field at a time. Ready replica count looks
harmless and would make a brief slightly better. Then pod phase. Then restart count. Each is
individually defensible and the sum is a monitoring platform with a worse data model than the ones
customers already run. ADR-010's rule — synchronize identity and change, never state — is the whole
defence, and the test that a status-field change produces no ledger row is what makes it real rather
than aspirational.

The honest sequencing note: this slice is what closes the CoverageGap the first investigation will
report constantly. If the harness shows that gap dominating the failures, this becomes urgent rather
than next. That is the intended way to learn it, and it is why the first slice reads changes live
and reports the gap instead of waiting for this.
