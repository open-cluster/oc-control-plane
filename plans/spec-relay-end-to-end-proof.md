# Spec — Prove the Relay end to end against the reference control plane

Status: READY FOR IMPLEMENTATION
Date: 2026-07-27
Repositories touched: the frozen .NET reference repository (OCluster/Zyrenn.ConsumerService) and the Relay repository

## Problem Statement

Both halves of the customer-execution path are built and neither has met the other.

The Go Relay has a validated configuration loader, durable credential custody, key-pinned
TLS, a read-only Kubernetes port, one compiled capability, a session runtime with a
single-writer stream discipline, an unacked-result buffer, and reconnect with jittered
backoff. Every one of those is proven against a programmable fake stream or a typed fake
Kubernetes client. None is proven against a real control plane.

The .NET control plane has bootstrap registration with single-use token consumption, a
durable job store with server-clock leases, lease epochs, stale-result fencing, idempotent
recording, and gRPC registration and session services on an isolated port. Every one of
those is proven against tests that stand in for a Relay. None is proven against a real one.

Two halves each tested against a model of the other is the classic place where a protocol
misunderstanding hides. Both sides can be individually correct and still disagree about
what the contract means — message ordering on first connect, what a duplicate acknowledgement
implies, whether a fenced result is an error or a no-op. No amount of further unit testing
on either side finds that.

The work queued behind this makes it urgent rather than merely desirable: the next step is
to reimplement the control-plane half in Go. Doing that before the protocol has ever carried
a real job means building a second unproven half against the first, and a failure would be
attributable to either. The reference implementation is only useful as an oracle if it has
been shown to work.

## Solution

One end-to-end run, exercised as a test, that proves the sentence the walking skeleton was
defined by: an installed Relay registers with the control plane, establishes an
authenticated outbound session, receives a typed job, executes it against a real Kubernetes
cluster through the read-only port, returns a bounded typed result, and the control plane
records that result durably.

Then the same harness is pointed at the failure modes that only exist when two real
processes are involved: the connection dropping mid-job, the control plane restarting
between leasing and delivering, a result arriving under a superseded epoch, and a Relay
reconnecting with work still in flight.

The output is evidence, not a feature. What it produces is the ability to say the protocol
works, and a harness that the Go control plane must satisfy identically.

## User Stories

1. As an engineer, I want a Relay to register against a real control plane, so that bootstrap
   token consumption is proven rather than modelled.
2. As an engineer, I want the durable credential issued by registration to authenticate the
   subsequent session, so that the two halves of identity are proven to fit together.
3. As an engineer, I want a second registration with the same token to be refused, so that
   single-use consumption holds against a real client.
4. As an engineer, I want a Relay to establish an authenticated session and be accepted, so
   that the handshake ordering is proven against a real peer.
5. As an engineer, I want a typed job dispatched to a connected Relay to arrive, so that
   delivery works over the real transport rather than a fake stream.
6. As an engineer, I want that job executed against a real cluster, so that the capability
   is proven against a real Kubernetes API rather than a typed fake.
7. As an engineer, I want the result recorded durably by the control plane, so that the
   whole path from dispatch to truth is closed.
8. As an engineer, I want the recorded result to carry the completeness basis the central
   certificate logic depends on, so that the transport is proven not to lose the fields
   that decide whether an absence may be stated as fact.
9. As an engineer, I want a job dispatched while no Relay is connected to be delivered when
   one connects, so that work is not lost to a disconnected window.
10. As an engineer, I want a connection dropped mid-execution to leave the job recoverable,
    so that a network blip does not lose or duplicate work.
11. As an engineer, I want a result produced after its lease expired to be refused, so that
    fencing holds between two real processes.
12. As an engineer, I want a result resent after it was already recorded to be answered
    idempotently, so that the Relay's resend buffer cannot create duplicate truth.
13. As an engineer, I want a control-plane restart between leasing a job and sending it to
    end with the job delivered, so that the gap between durable claim and delivery is proven
    recoverable.
14. As an engineer, I want a Relay reconnecting with work in flight to report it, so that
    the roster on reconnect is proven to carry what the control plane needs.
15. As an engineer, I want a cancellation to reach an executing job and produce a terminal
    outcome, so that the cancel path is proven end to end rather than in isolation.
16. As an engineer, I want a job carrying another organization's identity to be refused by
    the Relay, so that tenant isolation is proven at the point it is enforced.
17. As an engineer, I want the harness to run unattended in CI, so that the proof is
    repeatable rather than a one-time demonstration.
18. As an engineer, I want the harness to pin the Kubernetes version it runs against, so
    that a cluster upgrade cannot silently change what was proven.
19. As an engineer, I want a failure to say which half failed, so that debugging starts from
    evidence rather than from bisecting two codebases.
20. As an engineer about to reimplement the control plane in Go, I want this harness to be
    reusable against the new implementation, so that the reference becomes a differential
    oracle rather than a one-off.
21. As a reviewer, I want the proof to exercise the transport as deployed rather than
    in-process shortcuts, so that what passes resembles what ships.
22. As a reviewer, I want to see which failure modes are covered and which are not, so that
    the claim made about the protocol is bounded honestly.
23. As the founder, I want to know the protocol works before a second implementation of
    either half is written, so that a later failure has one candidate cause rather than two.
24. As the founder, I want this to stop short of a formal exit review, so that review effort
    is not spent on code scheduled for replacement.

## Implementation Decisions

**The harness drives real processes over the real transport.** The control plane runs with
its gRPC endpoint listening; the Relay runs as its own process with its own configuration,
credential file and Kubernetes client. Substituting an in-process stream would remove
exactly the layer this exercise exists to test.

**The cluster is a disposable single-node Kubernetes with a pinned version.** The capability
under test reads workloads and pods, which needs a real API server; both repositories already
use disposable containerised dependencies, so this is an extension of established practice
rather than a new one.

**Fixtures are quiesced before reads.** Endpoint mirroring and pod status settle
asynchronously, so a workload is created and allowed to reach a terminal state before a job
reads it. Racing the cluster produces flakes that look like protocol defects.

**Durable state is asserted in the database, not inferred from messages.** The control plane
records job outcomes transactionally; the proof reads that state directly. Asserting on
protocol messages would prove the messages were sent, not that truth was recorded.

**Failure modes are induced deterministically, not by timing.** A dropped connection is
induced by severing it, not by waiting. A lease expiry is induced by manipulating the
control plane's clock or the lease deadline, not by sleeping. A restart is induced by
stopping and starting the process. Tests that depend on wall-clock races are the ones that
get disabled six months later.

**Scope stops at the walking-skeleton sentence plus the distributed-systems interleavings.**
The formal exit review and the edge-compatibility gate are deliberately excluded. The edge
gate tests middleware behaviour, protocol negotiation and keepalive tuning specific to the
current server stack — findings about a server that is being replaced. Edge feasibility is
re-run against the Go implementation, where the answers will apply.

**The harness is written to be pointed at either control plane.** Its configuration names an
endpoint; nothing in it assumes which implementation is listening. That is what turns it
from a one-time proof into the differential oracle the reimplementation needs.

## Testing Decisions

**What makes a good test here.** It asserts an observable outcome — a credential was issued,
a job reached a terminal recorded state, a stale result was refused — and does not reach
into either implementation to check how. A test that inspects internal state of either side
would break when one is reimplemented, which defeats the purpose.

**The seam is the pair of running processes.** There is no test double anywhere in this
exercise: real control plane, real Relay, real cluster, real transport, real database. Every
component that could be faked is exactly a component whose behaviour is in question.

**This is deliberately a small number of expensive tests.** Each run starts several
containers and two processes. Cheap properties belong in the per-side suites that already
exist; only properties that require both sides belong here.

**Scenarios**, grouped by what they prove:

- *The sentence.* Register, session, dispatch, execute, record. One test, asserted at each
  stage so a failure localises.
- *Identity.* Token reuse refused. Session with a wrong or absent credential refused. A job
  carrying a foreign organization refused by the Relay and audited centrally.
- *Delivery.* Job enqueued while disconnected is delivered on connect. Redelivery of an
  already-executing job is acknowledged without re-execution.
- *Fencing.* A result under a superseded epoch is refused. A result resent after recording
  is answered as already recorded. A restart between claim and send ends with delivery.
- *Lifecycle.* Cancellation reaches an executing job and yields a terminal outcome. A Relay
  reconnecting reports in-flight work.
- *Fidelity.* The recorded result carries every completeness-basis field intact and
  correctly typed, since the central certificate logic depends entirely on them.

**Prior art.** The Relay repository's session tests establish the interleavings to cover and
the discipline of driving them deterministically rather than by timing. Its capability tests
establish the fault cases a healthy cluster never produces. This repository's relay tests
establish how durable job state is asserted. The existing disposable-cluster tests establish
the container harness. Nothing here needs inventing; it needs connecting.

## Out of Scope

- The formal exit review for the walking skeleton, and the edge-compatibility gate, both
  deliberately skipped for the reasons above.
- Any new capability. One capability is enough to prove the path.
- Performance, latency, or load characteristics. This proves correctness of the path.
- Multi-Relay scenarios, active-active replicas, or fan-out across clusters.
- Credential rotation. It is specified but not required by the sentence being proven.
- Deploying the Relay by chart. The harness runs it as a process; packaging is separate.
- Any change to either implementation beyond what the harness reveals as broken.

## Further Notes

The most valuable outcome of this work may be a defect list rather than a green suite. Two
independently built halves meeting for the first time usually disagree somewhere, and every
disagreement found here is one that would otherwise have been found later with a second Go
implementation in the picture and two candidate causes.

The harness has a second life after this: it becomes the differential oracle the Go control
plane is measured against, which is why it is written to be pointed at an endpoint rather
than wired to an implementation.
