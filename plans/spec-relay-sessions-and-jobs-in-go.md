# Spec — Relay sessions and durable jobs in the Go control plane

Status: BLOCKED on relay registration in Go
Date: 2026-07-27
Repository: the Go control plane

## Problem Statement

Registration gets a Relay an identity. It does not get it any work.

The remaining half of the customer-execution path is the part where correctness is hard: a
long-lived bidirectional stream carrying job assignments outward and results inward, over a
connection that will drop, to a process that will restart, on both sides, indefinitely. The
guarantee that has to survive all of that is narrow and absolute — a job is never lost and
never silently completed twice.

That guarantee is not provided by the stream. It is provided by durable state: every job is
leased, every lease is fenced by the session that owns it and the generation of that lease,
and the clock that decides expiry is the control plane's. The stream is only a delivery
channel. A disconnect loses a delivery attempt, never a job.

This is where a reimplementation is most likely to be subtly wrong, because the failure modes
do not appear in normal operation. A healthy run never produces a result arriving under a
superseded lease, a restart between claiming a job and sending it, or an acknowledgement for
work the sender has forgotten. Those paths are reached only by inducing them, and an
implementation that has never had them induced is untested rather than correct.

## Solution

Sessions and durable jobs served by the Go control plane, with the same guarantee: the
database owns job truth, leases are fenced, and the stream delivers.

A Relay establishes an authenticated session and is issued a session identity. Jobs are
claimed durably before they are delivered. Delivery is attempted over the stream and retried
by a sweep and an on-connect catch-up, so nothing depends on the stream being up at the
moment work becomes available. Results are recorded in one guarded transaction and only then
acknowledged. A result whose lease has been superseded is refused rather than recorded, and a
result resent after recording is answered idempotently.

Correctness is established the same way as registration: the same Relay driven against both
implementations, with the interleavings induced deterministically on both.

## User Stories

1. As an operator, I want a Relay to hold one outbound connection, so that no inbound access
   to my infrastructure is required.
2. As an operator, I want a dropped connection to re-establish on its own with backoff, so
   that a network blip does not require intervention.
3. As an operator, I want reconnection to be jittered, so that a control-plane restart does
   not bring every Relay back simultaneously.
4. As an operator, I want work queued while my Relay was offline to arrive when it returns,
   so that an outage delays investigation rather than losing it.
5. As an operator, I want a Relay to refuse work for another organization, so that a
   compromised control plane cannot use my Relay against my cluster.
6. As an engineer, I want a job claimed durably before it is delivered, so that a delivery
   failure cannot lose work that was already scheduled.
7. As an engineer, I want no database transaction held for the life of a stream, so that a
   long-lived connection cannot exhaust the connection pool.
8. As an engineer, I want lease expiry decided by the control plane's clock, so that a Relay
   with a skewed clock cannot extend or shorten its own lease.
9. As an engineer, I want every job fenced by the session that leased it and the generation of
   that lease, so that a result from a superseded execution cannot be recorded.
10. As an engineer, I want a result arriving under a stale lease to be refused and audited,
    so that the outcome of the current execution is not overwritten by an older one.
11. As an engineer, I want result recording guarded by the job's terminal status, so that a
    job already recorded cannot be recorded again.
12. As an engineer, I want the acknowledgement sent only after the recording transaction
    commits, so that a Relay never stops resending a result that was not durably stored.
13. As an engineer, I want a resent result for an already-recorded job answered definitively,
    so that the Relay's buffer drains instead of growing.
14. As an engineer, I want expired leases swept back to pending, so that a Relay that
    disappears mid-job does not strand work forever.
15. As an engineer, I want a catch-up on connect as well as a periodic sweep, so that delivery
    does not depend on an event notification that is not durable.
16. As an engineer, I want a duplicate session for one Relay to supersede the previous one,
    so that two connections cannot both believe they own the same work.
17. As an engineer, I want job-level fencing to remain authoritative even when sessions are
    superseded, so that session churn cannot corrupt job truth.
18. As an engineer, I want per-Relay concurrency bounded, so that one Relay cannot be handed
    more work than it advertised it can run.
19. As an engineer, I want message sizes bounded below the transport limit, so that an
    oversized result fails as a typed outcome rather than as a transport error.
20. As an engineer, I want a cancellation to reach an executing job and produce a terminal
    outcome, so that abandoned work does not hold a lease until it expires.
21. As an engineer, I want a drain instruction to let in-flight work finish before the
    connection ends, so that a deployment does not abandon running jobs.
22. As an engineer, I want terminal job rows retained under a policy, so that job history does
    not grow without bound.
23. As an on-call engineer, I want queue depth, lease expiries, sweep activity and stale-result
    refusals visible as metrics, so that a stuck pipeline is diagnosable.
24. As an on-call engineer, I want a job's lifecycle traceable from dispatch to recording, so
    that a slow investigation can be attributed to a stage.
25. As an on-call engineer, I want stale-result refusals logged with enough context to tell a
    benign race from a compromise, so that an alarming event can be triaged.
26. As an engineer, I want the interleavings induced deterministically rather than by timing,
    so that the suite does not become flaky and get disabled.
27. As an engineer, I want the same interleavings run against both implementations, so that
    correctness is compared rather than assumed.
28. As a reviewer, I want the deletion criteria for the reference stated before the overlap
    window opens, so that the second implementation has a defined end.
29. As the founder, I want the hardest part of the customer-execution path proven against a
    working reference, so that the reimplementation is measured rather than trusted.

## Implementation Decisions

**Durable state is the source of truth; the stream is a delivery channel.** Every rule below
follows from that and none of them can be relaxed without losing the guarantee.

**A job is claimed durably before delivery is attempted.** The transition from pending to
leased commits first; the assignment message is sent afterwards. A crash between the two
leaves a leased job whose lease expires and is swept, which is recoverable. Sending first and
claiming after would leave work delivered but unrecorded, which is not.

**The lease is fenced by the session identity that owns it and the generation of the lease.**
Both travel on the wire — the assignment carries them, the result echoes them — so the
control plane can validate ownership on every job-scoped message without inferring it.

**Lease duration exceeds the maximum execution budget plus margin**, so a job that is running
normally cannot have its lease expire underneath it.

**Delivery liveness has three independent mechanisms**: a periodic sweep for pending and
expired work, an unconditional catch-up scan when a session connects, and event notification
as an optimisation only. The notification mechanism is not durable, so it may accelerate
delivery but may never be the only thing that causes it.

**Recording is one transaction guarded by terminal status**, and the acknowledgement is sent
only after it commits. A result for an already-terminal job is answered as already recorded
rather than treated as an error, because the Relay resending is correct behaviour.

**A result under a superseded generation is refused, audited, and not recorded.** Where the
successor execution is still running, the refusal says so, so the Relay stops resending
immediately rather than waiting for its buffer to overflow.

**Caps are checked inside the short claim transaction, and no lock is held across a stream
write.** Holding a database lock while writing to a network stream couples database
availability to network latency.

**No database transaction spans the life of a stream.** Sessions are long-lived; transactions
are not.

**Session supersession does not weaken job fencing.** A duplicate session for one Relay
supersedes the earlier one, but jobs remain fenced at the job level, so session churn cannot
cause a job to be executed twice or recorded under the wrong owner.

**The wire contract is unchanged**, which is what makes differential comparison meaningful.

**Deletion criteria, stated before the overlap window opens**: every production path reaches
sessions and jobs through Go; the full interleaving suite passes identically against both
implementations; no reference code writes the job or session tables; the semantic contract
tests survive the deletion; and an independent review closes it.

## Testing Decisions

**What makes a good test here.** It asserts durable outcomes — what state the job reached,
what was recorded, what was refused — rather than which messages were exchanged. Message
assertions prove a conversation happened; only the database proves the guarantee held.

**The seam is unchanged: the composition root**, with a real Relay as the client and a real
database. The reference implementation participates as an external peer. Faults are induced
at the edges — sever the connection, stop the process, manipulate the lease deadline — rather
than by injecting doubles into either implementation.

**Faults are induced deterministically.** Connection loss is severing, not waiting. Lease
expiry is deadline manipulation, not sleeping. Restart is stopping and starting. A suite that
depends on winning a timing race is a suite that gets disabled.

**Scenarios**, each of which is a way the guarantee could break:

- Work enqueued while disconnected is delivered on connect.
- An assignment acknowledgement lost in transit results in one recording and bounded
  execution, not two executions.
- A result under a superseded generation, racing a result from the current one, does not
  overwrite the current outcome.
- A control-plane restart in the gap between the durable claim and the stream write ends with
  the job delivered.
- A Relay crashing with unacknowledged results in its buffer resends them, and recording is
  idempotent.
- A duplicate session supersedes the earlier one without disturbing job ownership.
- An expired lease is swept back to pending and redelivered exactly once.
- A cancellation reaches an executing job and yields a terminal outcome, and a result already
  produced wins over the cancellation.
- A drain lets in-flight work finish and ends the connection; abandoned work recovers by
  lease expiry.
- Per-Relay concurrency is respected; excess work is shed rather than queued at the Relay.
- Differential: every scenario above produces the same durable outcome against both
  implementations.

**Concurrent scenarios run under race detection**, and the concurrency must be genuine.
Running the detector over serial tests proves nothing.

**Prior art.** The reference implementation's job store, dispatch worker and session registry
define the semantics to preserve. The Relay's session package establishes the interleavings
and the discipline of driving them deterministically. The end-to-end proof provides the
harness. The registration work establishes the differential method.

## Out of Scope

- New capabilities. This moves the machinery that carries work, not the work.
- Continuous inventory synchronisation, which deliberately does not travel this path because
  its volume is wrong for durable per-job truth.
- Streaming or watch-mode extensions to the session.
- Multiple concurrently active Relays per organization, and the identity questions that
  raises.
- Credential rotation and mutual TLS.
- Any change to the wire contract.
- Deleting the reference implementation.

## Further Notes

This is the largest correctness risk in the migration and the last part of the
customer-execution path still to move. Everything after it is either new work with no
reference to compare against, or the truth layers, which are bigger but far less concurrent.

The interleaving suite is the deliverable that outlives the code. It encodes what the
protocol guarantees under failure, and it should survive the deletion of the implementation
it was written to check, because the next question anyone asks about this path is whether
some new change still satisfies it.
