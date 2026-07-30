# Spec — Prove the Relay end to end against the control plane

Status: IMPLEMENTED 2026-07-29, and running in CI since 2026-07-30. The walking-skeleton
sentence and five interleavings are proven; eight named failure modes are not, and are listed
below rather than left to be discovered. It found one protocol defect, fixed in the Relay.
Date: 2026-07-27, revised 2026-07-29
Repositories touched: this one, and the Relay — which was modified, once, for a defect this
exercise found. Both halves take part as running processes.

> **Revision 2 — the counterparty changed.** This document was written when the .NET
> implementation was the only control plane that existed, so it named that as the process
> under test and the Go rewrite as the thing queued behind. Registration, sessions and
> durable jobs are now implemented in Go, and the .NET implementation is frozen and
> scheduled for deletion. So the proof runs Relay against the **Go** control plane.
>
> Nothing this document argues depends on which implementation answers. The value was never
> "the .NET code works" — it was "two halves built against models of each other have never
> met", and that is exactly as true of the Go half. What is lost is the differential
> oracle: the proof no longer compares two implementations, it establishes one. That is a
> genuine reduction in what a green run means, and it is the consequence of building
> registration and sessions ahead of this document rather than behind it.

> **What the implemented harness proves, and what it does not.** Proven: enrolment against a
> real control plane with a real single-use token; the durable credential authenticating the
> session that follows; a spent token refused a second identity, with the refusal audited; a
> typed job crossing the real transport, executing against a real Kubernetes API, and being
> recorded durably with its completeness basis intact; an absent workload recorded as a typed
> outcome rather than a failure, and not claiming completeness; work enqueued while no Relay
> is connected delivered on connect; and a Relay reconnecting to a control plane that was
> killed.
>
> Not proven, for want of a seam rather than a decision — and this list is exhaustive against
> the Scenarios above, because a partial honesty ledger is the failure this section exists to
> avoid: fencing under a superseded epoch; cancellation reaching an executing job; idempotent
> resend of an already-recorded result; a connection dropped mid-execution; a restart in the
> gap between leasing a job and sending it (what is proven is a restart and a reconnect, not
> that gap); a reconnect whose hello reports a non-empty in-flight roster; a job carrying
> another organization's identity; and a session presenting a wrong or absent credential.
>
> Most need a window that does not exist: the one compiled capability finishes in about eleven
> milliseconds, so there is nothing to interrupt. Getting them needs a deliberately slow test
> capability or an administrative way to expire a lease. The foreign-identity case is
> different — the control plane builds every assignment from the session it is serving, so it
> cannot be made to send one, and producing it would need a hostile control plane, which is a
> fake, which is what this harness exists not to have. All of it is covered on the
> control-plane side against a programmable stream.
>
> Graceful drain is also unproven here and deliberately so: stopping a process in the harness
> means killing it, which is the more adversarial model and the one the durable guarantees
> exist for. Drain is tested in each side's own suite, where a signal can be delivered
> portably.
>
> **It runs in CI as of 2026-07-30**, once a credential for the Relay's private repository was
> provisioned. The first run against relay `main` failed on the capability-version refusal —
> correctly, because the fix for it was still an unmerged pull request. That is the harness
> doing its job rather than a flake, and it is worth recording as the moment the proof first
> earned its place: a protocol regression in either repository now turns the other's CI red.

## Problem Statement

Both halves of the customer-execution path are built and neither has met the other.

The Go Relay has a validated configuration loader, durable credential custody, key-pinned
TLS, a read-only Kubernetes port, one compiled capability, a session runtime with a
single-writer stream discipline, an unacked-result buffer, and reconnect with jittered
backoff. Every one of those is proven against a programmable fake stream or a typed fake
Kubernetes client. None is proven against a real control plane.

The Go control plane has bootstrap registration with single-use token consumption, a
durable job store with server-clock leases, lease epochs, stale-result fencing, idempotent
recording, and gRPC registration and session services on an isolated port. Every one of
those is proven against tests that stand in for a Relay. None is proven against a real one.

Two halves each tested against a model of the other is the classic place where a protocol
misunderstanding hides. Both sides can be individually correct and still disagree about
what the contract means — message ordering on first connect, what a duplicate acknowledgement
implies, whether a fenced result is an error or a no-op. No amount of further unit testing
on either side finds that.

The work queued behind this makes it urgent rather than merely desirable: signal intake is
what finally gives jobs a reason to exist, and every slice after it assumes the customer
execution path carries work. Building on a contract that has never carried a real job means
each later failure has two candidate causes instead of one.

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
20. As an engineer, I want the harness to name the endpoint it drives rather than link
    against an implementation, so that it survives the control plane being replaced.
    *(Not met — see the coupling decision below. The proofs are portable; the setup is not.)*
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

One assertion does wait, and the exception is worth stating rather than hiding: proving that
work enqueued with no Relay connected is *not* delivered means observing that nothing happened,
and there is no event for that. The harness watches for longer than the control plane's
delivery round, so anything that was going to be dispatched has had its chance. It is a
negative observation, not an induced failure mode, and it is the only sleep in the suite.

**Scope stops at the walking-skeleton sentence plus the distributed-systems interleavings.**
The formal exit review and the edge-compatibility gate are deliberately excluded. The edge
gate tests middleware behaviour, protocol negotiation and keepalive tuning specific to the
current server stack — findings about a server that is being replaced. Edge feasibility is
re-run against the Go implementation, where the answers will apply.

**The harness is coupled to this control plane, and that is a cost worth naming.** The
decision here was originally that nothing in the harness would assume which implementation
was listening, so it could serve as a differential oracle. The implementation does not honour
it: the harness builds `./cmd/controlplane` from this repository, configures it through this
implementation's environment variables, polls this implementation's readiness path, and reads
two of its log lines as assertions.

Each of those could have been configuration and none was, because the reason for the decision
went away — the .NET implementation is frozen and scheduled for deletion, so there is no
second control plane to point at. What survives is weaker but real: the proofs themselves
assert on durable state and protocol behaviour, never on internals, so aiming the harness at a
replacement would be a day of work in the setup layer rather than a rewrite.

The two log-line assertions are deliberate exceptions. Both name events that on purpose write
nothing else down — a refused enrolment, which must not tell a caller whether a token was
unknown or spent, and a session being accepted after a restart. For those the audit line is
the observable there is.

**The harness is written in Go, in this repository.** When this specification was drafted it
assumed the harness would live with the .NET implementation, which was then still being
worked on. That implementation is now frozen, so a new test suite there would be built into a
repository scheduled for deletion and then rewritten here — and the retirement criteria say
plainly that a test surviving only there dies with it. The harness also has a second life as
the differential oracle for the Go control plane, which is here. Nothing about this changes
what is proven: both implementations still take part as external processes over the real
transport, which the specification already required.

**It does not live in the Relay repository**, despite the existing harnesses there being the
closest prior art. That repository is Apache-2.0 and intended to become public; a harness that
starts a proprietary control plane does not belong in it.

**The harness is a nested module.** It needs to build or run the Relay, and requiring the
Relay's module from the shipping module would pull the Kubernetes dependency graph into a
service that must not have it — which a gate here already forbids. A separate module under the
test tree may depend on whatever it needs while the shipping module's requirements stay clean.

**The control plane runs as an ordinary process.** It needs a Postgres connection string and
a relay port; it applies its own migrations at startup and serves the relay endpoint on a
dedicated listener. Nothing is added to it for the harness's benefit — a control plane that
had to be modified to be testable end to end would be proving a different binary from the one
that ships.

**The harness must terminate TLS, because the two halves do not connect directly.** The Relay
dials TLS and validates the server by pinned public key, with no plaintext path — deliberately,
since the pin is its whole trust decision. The control plane serves h2c, behind a
TLS-terminating edge in production. So the harness generates a certificate per run, derives its
pin the way the Relay derives it, and forwards to the control plane over h2c. This is not a
workaround: connecting the two directly would remove the layer the exercise exists to test, and
running with a fixed certificate would let a run pass by trusting a key an earlier run left
behind.

**HTTP/2 must survive the proxy.** gRPC needs it end to end, so the terminator forwards over
h2c rather than downgrading, and streams responses through rather than buffering them.
Buffering would break the session stream, which is most of the protocol.

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
establish the fault cases a healthy cluster never produces. The frozen implementation's relay
tests establish how durable job state is asserted. The existing disposable-cluster tests establish
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

**It did.** The first run of the harness passed, and the assertion that mattered turned out
not to discriminate: dispatching a job at a capability version no Relay has was executed
anyway and answered as a success. The Relay was handing every assignment to its single
compiled executor without reading `capability_id` or `capability_version` at all, so
`KIND_UNSUPPORTED_CAPABILITY_VERSION` — a value in the contract, with a comment on the
Relay's side saying it re-validates on receipt — could never be produced.

Nothing misbehaves today: there is one capability at one version. The defect is what happens
the day there are two. Schema versions are frozen, so a v2 means different semantics, and an
old Relay would have run a v2 job through its v1 implementation and returned an answer under
the wrong contract, with nothing downstream able to tell. It is exactly the shape this
exercise was written to catch — a disagreement about what the contract means that neither
side's own tests could see, because each was tested against a model of the other that shared
its assumption.

The Relay now refuses an assignment it did not advertise, tested there against a fake stream
and here against the real one.

The harness has a second life after this: it is the regression suite for the protocol
itself, which is why it is written to be pointed at an endpoint rather than wired to an
implementation. Every capability, every new message and every change to the fencing rules
arrives with the question "does the wire still carry it", and this is where that is answered.
