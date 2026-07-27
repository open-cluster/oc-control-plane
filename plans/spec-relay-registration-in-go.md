# Spec — Relay registration in the Go control plane

Status: BLOCKED on the end-to-end proof
Date: 2026-07-27
Repository: the Go control plane

## Problem Statement

The Go control plane starts, resolves an organization's placement, migrates, observes
itself and shuts down cleanly — and does nothing. Every capability the product needs still
lives in the .NET reference, which is frozen and receives only critical fixes.

Something has to move first, and the choice determines how much is learned versus how much
is risked. Registration is the smallest piece of real behaviour that exercises the whole
foundation: it takes a request over a network transport, authenticates a bootstrap
credential, writes durable tenant-scoped state through the placement seam, issues a secret,
and must never leak why it refused.

It is also the piece with the most favourable conditions for a first move. Its contract is
Protobuf, so the interface is language-neutral and already fixed. Its peer is already Go.
It has no web-frontend consumer, so no external contract is pinned to it. It owns exactly
one table. And a complete implementation exists to compare against, so correctness is
measurable rather than argued.

Moving anything larger first — sessions with leases and fencing, or intake with no
reference at all — means learning whether the foundation is sound at the same time as
learning whether complicated logic is right.

## Solution

Registration served by the Go control plane: a Relay presents a single-use bootstrap
credential, receives a durable identity bound to its organization, and can then authenticate
future sessions with it. The bootstrap credential is consumed atomically so it works exactly
once, and every refusal looks identical from outside.

Correctness is established by comparison rather than assertion. The same Relay is driven
against both implementations, and the observable outcomes must match: what was issued, what
was persisted, what was refused, and what the refusal revealed. Where they differ, the
reference is presumed right until the difference is understood.

## User Stories

1. As an operator installing a Relay, I want it to register with one bootstrap credential, so
   that installation does not require handling a long-lived secret.
2. As an operator, I want a successful registration to yield a durable identity the Relay
   stores itself, so that a restart does not require a new bootstrap credential.
3. As an operator, I want a bootstrap credential to work exactly once, so that a leaked or
   reused installation manifest cannot enrol a second Relay.
4. As an operator, I want a registered Relay to survive a control-plane restart without
   re-registering, so that a deployment does not require touching every cluster.
5. As an operator, I want a clear terminal failure when my credential is invalid, so that I
   stop retrying and fix the installation.
6. As an operator, I want a retryable failure to be distinguishable from a terminal one, so
   that transient unavailability does not look like a rejected credential.
7. As a security reviewer, I want every refusal reason to be indistinguishable from outside,
   so that an attacker cannot learn whether a credential is unknown, expired, consumed or
   revoked.
8. As a security reviewer, I want the durable credential stored only as a hash, so that a
   database disclosure does not yield working Relay identities.
9. As a security reviewer, I want the issued credential returned exactly once and never
   retrievable, so that there is no endpoint that hands out identities.
10. As a security reviewer, I want credentials kept out of logs, traces and error messages,
    so that observability does not become a disclosure channel.
11. As a security reviewer, I want registration flooding shed before the credential is
    examined, so that shedding carries no signal about validity and the check cannot be used
    as an oracle.
12. As a security reviewer, I want a registration to be bound to the organization it was
    issued for, so that a credential cannot enrol a Relay into another tenant.
13. As an engineer, I want the registration row written through placement resolution, so that
    a tenant on a dedicated database has its registration there and not in the shared one.
14. As an engineer, I want concurrent presentations of the same credential to result in
    exactly one registration, so that a retrying client cannot create two identities.
15. As an engineer, I want the consumption and the issuance to commit together, so that a
    failure cannot consume a credential without issuing anything.
16. As an engineer, I want the behaviour compared against the reference implementation, so
    that correctness is measured rather than asserted.
17. As an engineer, I want the comparison to include refusals, not just successes, so that
    the paths most likely to differ are the ones most carefully checked.
18. As an engineer, I want a real Relay used in the comparison, so that what is proven is
    what will happen.
19. As an engineer, I want the reference implementation to keep running in CI throughout, so
    that it cannot rot into an invalid oracle while it is still being used as one.
20. As an on-call engineer, I want a registration attempt to appear in logs with its outcome
    and organization, so that a failing installation can be diagnosed without a debugger.
21. As an on-call engineer, I want registration outcomes counted as metrics without a
    per-organization label, so that a spike is visible without a cardinality problem.
22. As an on-call engineer, I want a registration to produce a trace reaching its database
    write, so that latency can be attributed without adding instrumentation during an
    incident.
23. As a reviewer, I want the criteria for deleting the reference implementation stated before
    the work starts, so that two implementations cannot quietly become permanent.
24. As the founder, I want the first migrated domain to be the one with the most favourable
    conditions, so that the strangler pattern is validated before it is trusted with harder
    work.

## Implementation Decisions

**The wire contract is unchanged.** The Protobuf service, its messages and their semantics
are fixed. This is a reimplementation behind a stable interface, which is what makes
differential comparison meaningful at all. Any contract change is a separate matter with its
own review.

**Registration state is written through the placement seam** like all tenant data. The
organization determines the database. This is the first real use of the foundation for
something a customer depends on.

**Consumption and issuance commit in one transaction.** A credential must not be consumed
without an identity being issued, and an identity must not exist without the credential being
spent. The use case owns that transaction and it is visible in the signature.

**Concurrency is resolved by the database, not by application locking.** Two simultaneous
presentations of one credential are made to serialise on the row, so exactly one wins. An
application-level guard would be a second source of truth for something the database already
decides.

**Every refusal produces one indistinguishable response.** Unknown, expired, already
consumed, revoked, and organization mismatch all look the same to the caller and are
distinguished only in server-side audit. Intake shedding under flood is deliberately a
*different*, retryable response, and is applied before the credential is examined so it
carries no information about validity.

**The durable credential is stored hashed** and returned exactly once, in the response that
creates it. There is no path that reads it back.

**Verification on the hot path avoids a deliberately slow function.** The stored value is
high-entropy and server-generated, so the reason to use a slow key-derivation function —
resisting offline attack on a low-entropy human secret — does not apply, while the cost
would apply to every session establishment.

**Correctness is established by differential comparison** against the reference, using a real
Relay driven against both. The comparison covers what was issued, what was persisted, what was
refused, and what the refusal disclosed. The reference stays in CI for the whole overlap
window so drift breaks the build visibly.

**The deletion criteria are stated now, before the overlap window opens**: every production
path reaches registration through Go; differential comparison is clean including refusal
paths; no reference code writes the registration table; the contract tests survive the
deletion; and an independent review closes it. An overlap window without a declared end is
how a strangler becomes a permanent second implementation.

## Testing Decisions

**What makes a good test here.** It asserts what a Relay or an operator could observe: an
identity was issued, a second attempt was refused, the refusal revealed nothing, the row
landed in the right database. It does not assert how consumption is implemented, so the
implementation can change without rewriting the suite.

**The seam is unchanged: the composition root.** The process is started in-process against a
real database with a real listener, and a real Relay is the client. There is no mock Relay
and no mock database. The reference implementation participates as an external peer over the
network, not as a test double — which keeps the count at one behavioural seam.

**Scenarios.**

- A Relay registers and receives a usable identity; the row is persisted under the right
  placement.
- The same credential presented twice yields one identity and one refusal.
- Concurrent presentations of one credential yield exactly one identity.
- Unknown, expired, consumed, revoked and organization-mismatched credentials all produce
  byte-identical refusals.
- Flood shedding produces a *different*, retryable response, and does so without examining
  the credential.
- The issued credential appears in no log line, trace attribute or error message.
- The stored form is not the issued value.
- A registration for an organization on a dedicated placement lands in that database and is
  absent from the shared one.
- Differential: for each scenario above, the reference and the Go implementation produce the
  same observable outcome.

**Prior art.** The foundation's composition-root suite establishes the harness shape —
process in-process, real database, real listener. The reference implementation's registration
tests establish the refusal taxonomy and the flood-control behaviour to preserve. The Relay's
own enrolment tests establish the client side. The end-to-end proof that precedes this work
provides the harness this suite extends.

## Out of Scope

- Sessions, job dispatch, leases and result recording. Separate and larger.
- Credential rotation. Specified in the protocol, not required to register.
- Mutual TLS. Recorded as a later upgrade to the identity model.
- Any change to the wire contract.
- Deleting the reference implementation. Its criteria are stated here; meeting them is a
  later, separately reviewed act.
- Registration user interface or self-service credential issuance.
- Multi-Relay-per-organization semantics beyond what the contract already defines.

## Further Notes

This work is blocked until the protocol has carried a real job between the existing two
halves. Reimplementing one half of a protocol that has never run is how a protocol defect
and a reimplementation defect get discovered together, with no way to tell which is which.

The comparison is more valuable than the code. If the two implementations agree on every
scenario including the refusal paths, the strangler approach is validated for everything
that follows. If they disagree, the disagreement is worth more than a green suite would have
been, because it is a real ambiguity in a contract that two independent readings interpreted
differently.
