// Package e2e runs the Relay and the control plane as real processes and proves the
// protocol carries a job between them.
//
// Nothing here is a test double. Every component that could be faked — the transport, the
// database, the cluster, either implementation — is exactly a component whose behaviour is
// in question. Two halves each tested against a model of the other is the classic place a
// protocol misunderstanding hides, and no amount of further testing on either side finds it.
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
