// Package e2e runs the Relay and the control plane as real processes. It is two things, and
// the second one reuses the first wholesale rather than standing up a second cluster.
//
// The first is the protocol proof: a job crosses between the two halves and durable truth is
// asserted. That is what the `_test.go` files here are.
//
// The second is the SCENARIO HARNESS — a fixed set of Kubernetes clusters broken in known ways,
// on purpose, with the cause written down before the system ever sees them. It provisions each
// one, verifies the cluster actually reached its declared broken state, investigates it through
// the real control plane and a real Relay, and files an artifact a scorer reads apart from the
// ground truth they must not. It is a PROGRAM rather than a test — see cmd/scenario — because it
// has a human in the loop, calls a paid model, and produces a judgement rather than a pass.
// Under `go test` it would either weaken CI or misrepresent what it proves.
//
// The two share this package because they share provisioning. A second way to stand up a
// cluster, a control plane and a Relay is a second thing to drift.
//
// Nothing here is a test double. Every component that could be faked — the transport, the
// database, the cluster, either implementation — is exactly a component whose behaviour is
// in question. Two halves each tested against a model of the other is the classic place a
// protocol misunderstanding hides, and no amount of further testing on either side finds it.
// The one exception is the fake clientset used by the harness's own tests, which check that a
// cluster failing to reach its declared state is discarded — a property that is about the
// harness rather than about either implementation.
//
// # What one run does
//
// A Postgres and a single-node Kubernetes start as containers. The control plane is built
// and run as a child process against that database, serving its relay endpoint as plaintext
// HTTP/2. A TLS terminator generated for this run fronts that endpoint, because the Relay
// dials TLS and validates by pinned public key with no plaintext path — so the two halves
// cannot connect directly, and a harness that made them would remove the layer this exercise
// exists to test. The Relay is then built and run as its own child process, with its own
// credential file and its own client to the cluster.
//
// From there the harness only writes and reads durable state. It enqueues a job the way an
// investigation would and waits for the outcome to be recorded, asserting on the database
// rather than on messages: asserting on messages would prove they were sent, not that truth
// was written.
//
// # What is proven, and what is not
//
// Proven here, and nowhere else:
//
//   - A bootstrap token issued by the control plane is consumed by a real Relay, and the
//     durable credential that enrolment returns authenticates the session that follows.
//   - A spent token is refused a second identity.
//   - A typed job crosses the real transport, executes against a real Kubernetes API, and
//     its result is recorded durably — carrying the completeness basis intact, which is what
//     the central certificate logic depends on.
//   - A read of a workload that is not there is recorded as a typed outcome rather than a
//     failure, and does not claim to have been complete. The distinction is the one certified
//     absence rests on.
//   - A job naming a capability version no half of the fleet has is refused with a typed
//     failure and never executed. This is the one that found something twice: before it
//     existed the Relay dispatched every assignment to its single executor without reading the
//     capability or the version at all, so a job at any version would have been run and
//     answered under v1 semantics; and when central pre-dispatch validation landed it recorded
//     the refusal as malformed arguments, which would have sent an operator to inspect a
//     payload that was never the problem.
//   - The cluster's own account of what it did, and a container's own words, cross the
//     protocol with their completeness bases intact — the returned counts, the completeness
//     flags, which bound bound, the effective bounds applied, and the attested event-retention
//     horizon. The executors' unit tests prove those are computed; only this proves they
//     survive encoding, the stream, the recording transaction and the column they land in,
//     which is what the central certificate depends on.
//   - The container that DIED is readable rather than the one that replaced it, and a
//     previous-container read on a container that never restarted is a distinct typed outcome
//     from a container that is not there.
//   - A SECRET PRINTED BY A REAL CONTAINER DOES NOT REACH THE CONTROL PLANE'S DURABLE STATE.
//     This is the property Relay-side redaction exists for and the one that cannot be unit
//     tested, because the claim is about what leaves a cluster rather than about what a function
//     returns. The assertion sweeps every text and binary column of every table read from the
//     catalogue, so a table added later is covered without anyone remembering; it was verified to
//     FAIL with the enforcement point removed, which is the only way to know a negative assertion
//     is load-bearing. What was not a secret on the same line survives, so a rule that masked
//     everything could not pass it either. The masked counts and rule identifiers cross the
//     protocol intact, which is what the control plane turns into a CoverageGap.
//   - A missing pod, a missing container, and an empty namespace are typed outcomes, and only
//     the last of them reports a complete read. That distinction is the one certified absence
//     rests on.
//   - Work enqueued while no Relay is connected is delivered when one connects.
//   - A Relay whose control plane is killed mid-life reconnects on its own and keeps working.
//
// Not proven here. The list is exhaustive against the specification's scenarios, because a
// harness that ran green while quietly covering half of what it appeared to would be worse
// than no harness — the claim it licenses is "the protocol works".
//
// Needing a seam this harness does not have:
//
//   - Fencing: a result arriving under a superseded lease epoch. Needs a second Relay or a
//     way to hold an execution open, and the one compiled capability finishes in about eleven
//     milliseconds, so there is no window to act in.
//   - Cancellation reaching an executing job. Same missing window.
//   - Idempotent resend of an already-recorded result. Needs a way to make an acknowledgement
//     go missing.
//   - A connection dropped mid-execution. Same missing window.
//   - A control-plane restart between leasing a job and sending it. What is proven below is a
//     restart and a reconnect; the gap between the durable claim and the delivery is never
//     entered, because entering it means stopping the process inside a few milliseconds.
//   - A Relay reconnecting while work is genuinely in flight, so that its hello reports a
//     roster. The roster is exercised, but always empty.
//   - A job carrying another organization's identity refused by the Relay. The control plane
//     builds every assignment from the session it is serving, so it cannot be made to send
//     one; producing it would need a hostile control plane, which is a fake, which is what
//     this harness exists not to have.
//   - The RELAY refusing an assignment naming a capability it never advertised. Both halves
//     compile the same contract, so there is no version one has and the other does not, and
//     the control plane now refuses such a job before it is sent. It is proven in the Relay's
//     own suite, where the advertised set can be varied.
//   - A namespace outside the Relay's local allowlist being refused, and a local cap lowering
//     an effective bound. Both are customer-authored configuration this harness runs with
//     unset, and setting it would prove the Relay reads its own environment rather than
//     anything about the protocol.
//   - A session presenting a wrong or absent credential.
//
// Needing work this harness has not had yet:
//
//   - AN INVESTIGATION RUN END TO END, AS A TEST. The mechanism now exists and is the scenario
//     harness above: the control plane's model boundary arrives by configuration, its operator
//     surface is bound and given a token, a Kubernetes Connection is created through that surface,
//     and an investigation is opened against a workload broken on purpose. The obstacle this list
//     used to record — that a recorded transcript proposes a log read naming a POD, and a
//     Deployment's pod name is generated by the cluster — is resolved: every scenario workload is
//     a StatefulSet, whose pod is deterministically <name>-0.
//
//     What is still missing is a TEST rather than a program. The harness produces a judgement
//     scored by a human, so nothing here asserts that a conclusion was right; it asserts only
//     that a case reached a terminal state and that the artifact carries what a scorer needs. A
//     commit-gate assertion over an investigation's CONTENT would have to be written against a
//     recorded transcript, which makes it a test of the replay rather than of the reasoning.
//     Whether that is worth having is an open question and not an oversight.
//
//   - A LIVE MODEL PROVIDER. The harness is specified to call the real provider and record the
//     transcript as a by-product. This build has no provider: the Reasoner seam carries a
//     recorded replay and an unavailable stub and nothing else, so a run asked for a live
//     provider is REFUSED with that said plainly rather than quietly replaying a recording and
//     being scored as though a model had answered. Until it exists, every artifact says
//     "recorded transcript" as its model source, and a scorer reading one is reading a
//     reproduction.
//
//     This is recorded here rather than left silent because the specification requires this list
//     to be exhaustive, and a harness that ran green while quietly covering less than it appeared
//     to would be worse than no harness.
//
// Excluded by decision rather than by difficulty:
//
//   - Graceful drain on either side. Stopping a process here means killing it, which is a
//     crash and is the more adversarial model — the durable guarantees exist for exactly
//     that. Drain has its own tests in each side's suite, where a signal can be delivered
//     portably.
//   - Multi-Relay scenarios, credential rotation, and chart-based deployment, all of which
//     the specification puts out of scope.
//
// Everything in the first list is covered on the control-plane side against a programmable
// stream. None of it has met a real Relay.
//
// # Running it
//
// The Relay is built from its own working tree, which is not vendored here — the two
// repositories share no history and no file is copied between them. The tree is found beside
// this repository by default, or named by OC_E2E_RELAY_SOURCE. Without it the tests skip
// with that said plainly, rather than passing on a proof that did not run.
//
// A missing container runtime skips too, and nothing else does. A runtime that is present but
// could not start a container has found something — a pinned image that no longer exists, a
// daemon out of disk — and that fails, because reporting it as a skip would let a run that
// proved nothing read as a run that passed.
//
// In CI the proof needs a credential for the Relay's private repository. Until one is
// configured the job says, in the run summary, that the proof did not run.
package e2e
