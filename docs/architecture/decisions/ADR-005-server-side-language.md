# New backend work is Go; existing .NET domains move only on a measured trigger

Status: SUPERSEDED IN PART (2026-07-27 — the .NET implementation is frozen and the
measured-trigger rule no longer governs what moves next; the title records the original
decision. See "The .NET reference is frozen" below)

> **Superseded in part, 2026-07-27.** The .NET solution is frozen. New control-plane work
> happens in Go without waiting for a per-domain trigger, and this repository becomes a
> read-only reference rather than a codebase under development. The measured-trigger rule
> below still describes how the decision was reached and why a rewrite was rejected in
> July; it no longer governs what happens next. See **The .NET reference is frozen** at the
> end of this document for the current posture and the criteria for retiring it.

> **Amended 2026-07-26.** The founder scoped alerting and observability out of the product
> and moved the frontend to a separate rewrite. Both amendments strengthen the case for
> migrating the surviving control plane rather than weaken it: the frontend no longer pins
> the REST contract, and the surviving codebase is roughly 18,500 source lines across nine
> migrations rather than 46,329 across fifty-six. The ratio argument below was correct
> against the codebase as it stood and no longer governs the surviving subset. The rule it
> establishes — move on measured need, never on language preference — is unchanged, and
> now binds hardest on the investigation truth layers. See
> `plans/go-strangler-migration.md` revision 2.

All new backend work is written in Go. Existing .NET domains are not rewritten; each moves
only when a domain-specific trigger fires, in the order and under the gates recorded in
`plans/go-strangler-migration.md`. Alerting is expected never to move. The frontend stays
Next.js and TypeScript, which was never in question — no product ships a Go frontend, so
"one language" was never available and the real choice is between two backend languages
and one.

The decision rests on measurement rather than preference. On 2026-07-26 the working tree
held 46,329 lines of C# source with 38,858 lines of tests across 56 migrations, against
3,350 lines of Go. Exactly one domain is implemented twice — Kubernetes workload reads —
and that duplication is deliberate, serving as the differential-parity oracle that proves
the Go Relay correct, with deletion already gated as slice R3b. The pivot from
observability to investigation orphaned 4,064 lines, which is 8.8 percent of C# source and
can be deleted outright with no migration. There is no LLM investigator in either
language, so the largest remaining body of product work is greenfield and lands in Go
without moving an existing line.

## Considered: continue with .NET and Go indefinitely

**Benefits.** Zero migration cost and zero migration risk. The investigation truth
machinery keeps its test coverage and its migration history intact. The C# Kubernetes
readers remain available as the parity oracle for as long as the Relay needs them. Each
language stays where it is strongest: .NET for the transactional control plane and its
mature Npgsql and ASP.NET surface, Go for the in-cluster component where client-go is the
reference client and static binaries matter.

**Costs.** One maintainer carries two idiom sets, two toolchains, two dependency and
vulnerability surfaces, and two review modes. Contributor optics for a cloud-native
open-source component are poor in .NET. Hiring for this domain is harder. Every
cross-cutting concern — tracing, tenancy, error taxonomy — is implemented and reviewed
twice.

**Why it is rejected as a permanent posture but accepted as the current one.** The costs
are real and compound, so a permanent two-language commitment is not accepted. They are
not, however, worth 46,329 lines of destruction today, and the honest trigger for moving
any particular domain is measured friction rather than a language count. This ADR
therefore rejects "two languages forever" as the destination while accepting it as the
present state, with new work biased to Go so the ratio shifts without a rewrite.

## Considered and rejected: rewrite the control plane in Go

Rejected on ratio. It destroys 46,329 lines of source, 38,858 lines of tests, and 56
migrations to remove 4,064 orphaned lines — roughly ten lines destroyed per orphaned line
removed. It also deletes the parity oracle that the Relay's own correctness gate depends
on, and it does not reach one language: the count goes from three to three, since the
frontend stays TypeScript. A rewrite based on language preference alone is explicitly out
of scope for this decision.

## Consequences

- The Relay control plane is the first genuine strangle, triggered by R1 exit review and
  not before, because it is Protobuf-contracted, frontend-decoupled, and owns two tables.
- Deleting the orphaned observability code is independent of every language question and
  is sequenced first.
- Tenant-placement indirection lands before any domain moves; a strangled service must
  resolve the same placement as its predecessor or the isolation tiers silently diverge.
- Every strangled domain declares its deletion gate before its overlap window opens. An
  overlap window without a declared gate is how a strangler becomes a permanent second
  implementation.
- The `/api/v1` REST surface is frozen for the duration. The frontend is the anchor that
  pins it, which is the structural reason Alerting migrates last or never.

## The .NET reference is frozen

Decided 2026-07-27.

This repository is a reference implementation, not a codebase under development. It is not
deleted and not archived, because it is still the only working description of behaviour
several Go slices will be measured against.

**What may change here.** Security fixes. Defects that would corrupt data or invalidate the
implementation as an oracle. Nothing else — no features, no refactors, no dependency
upgrades except security ones, no test additions except those that pin behaviour a Go slice
is about to reproduce. Every change is a deliberate exception with a stated reason, not the
default.

**What happens elsewhere.** All new control-plane work is written in Go, in the control-plane
repository, without waiting for a per-domain trigger. The trigger rule existed to stop a
rewrite being justified by language preference. That question is now settled by a decision
rather than by a measurement, so the rule has nothing left to gate.

**What this repository is still needed for.** Three things, and they are the reason it stays
readable: the differential oracle for registration, sessions and job fencing; the parity
oracle for Kubernetes workload reads, which is what proves the Relay's own capability
correct; and the record of how the truth chain, the completeness basis and the refusal
taxonomies actually behave, which is documented most precisely by working code and its
tests.

**A known, deliberate inconsistency.** The vendored copy of the Relay protocol under
`OpenCluster.Relay.Protocol` is pinned to an older Relay revision and will stay there. The
Relay's Go module path moved, which rewrote the `go_package` option in all three proto files
and therefore the descriptor. Nothing detected it, because the drift test verifies the copy
against a manifest generated from that same copy; both went stale together and agreed. The
divergence is confined to a Go-specific generation option, so the C# generated from these
files and the wire contract are unaffected, and the reference remains a valid oracle. It is
recorded here rather than repaired because repairing it improves a maintenance property of
an implementation being retired. Anyone reading protocol behaviour out of this repository
should read `proto/` in the Relay repository instead.

The general lesson outlives the instance: a consistency check whose reference is derived
from the thing being checked cannot detect drift in both together. It proves only that the
artefact agrees with itself. Go consumers avoid the failure entirely, because `go.mod`
records what was taken and `go.sum` verifies it, and neither is derived from the copy.

### Criteria for retiring this repository

Archiving is a separate, reviewed act. All of the following must hold first, and each exists
because something would otherwise be lost rather than migrated:

1. Every production path — relay registration, sessions and job fencing, signal intake,
   incidents and investigations — is served by Go.
2. Differential comparison against this implementation is clean for each of the three
   domains that use it as an oracle — relay registration, relay sessions with job leasing
   and fencing, and Kubernetes workload reads — including the refusal paths, not only the
   successful ones. Any domain later added to this list is added here.
3. No .NET code writes any table the Go control plane owns.
4. The semantic contract tests exist in Go. A test that survives only here dies with the
   repository, and the next person to change that behaviour has nothing to check against.
5. The Relay's Kubernetes workload capability has a correctness gate that does not depend on
   the C# readers, since the parity oracle disappears with them.
6. The completeness-basis mapping and the refusal taxonomies are captured in Go tests or in
   documentation, not inferred from C# source when someone needs them.
7. Every table the Go control plane reads has its schema defined by Go migrations. No
   deployment of this implementation exists, so the set of data needing interpretation
   through the C# migration history is currently empty; if one is created before
   retirement, this criterion becomes a migration rather than a check.
8. A review closes it, conducted by someone who did not write the Go replacement, and
   accepted by the founder. Each criterion above is answered with evidence rather than
   asserted.

Until every one holds, the repository stays readable and frozen. An overlap window without a
declared end is how a strangler becomes a permanent second implementation; a reference
without retirement criteria is the same failure wearing a different name.
