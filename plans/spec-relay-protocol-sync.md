# Spec — Relay protocol re-sync and a drift gate that can detect staleness

Status: ABANDONED 2026-07-27. Retained for the conclusion in Further Notes, not as work.
Date: 2026-07-27
Repositories touched: none. The work was not carried out.

> **Abandoned, and why the document survives.** This specification proposed repairing a
> drift gate in the .NET implementation. That implementation is now frozen, so the fix would
> have improved a maintenance property of something being retired, and Go consumers avoid
> the failure entirely — `go.mod` records what was taken and `go.sum` verifies it, and
> neither is derived from the copy being checked. The vendored copy stays pinned to an older
> Relay revision; the divergence is a Go-specific generation option, so the generated C# and
> the wire contract are unaffected. The reasoning is recorded in the language decision
> record.
>
> The document is kept for the conclusion in Further Notes, which generalises beyond this
> instance: a consistency check whose reference is derived from the thing being checked
> cannot detect drift in both together, and proves only that the artefact agrees with
> itself. Every mention of "this repository" below means the frozen .NET reference.

## Problem Statement

The Relay's Protobuf contract is authoritative in the Relay repository. This repository
keeps a vendored copy of it, a SHA-256 manifest, and a committed descriptor baseline so the
.NET control plane can generate C# from the same source without depending on the other
repository at build time.

The Relay's Go module path moved, which rewrote the `go_package` option in all three proto
files and therefore changed the descriptor. The copy here is now stale in all four
artefacts, and nothing noticed.

Nothing noticed because the check verifies the copy against its **own** manifest. Both are
stale, so they agree, and the test passes. The mechanism detects tampering with the copy;
it cannot detect the copy falling behind its source, which is the failure that actually
happens. A gate that reports green while the thing it guards has drifted is worse than no
gate, because it converts an open question into a false answer.

Measured 2026-07-27 — every artefact differs:

| Artefact | Source | Vendored copy |
| --- | --- | --- |
| `kubernetes_workload_runtime.proto` | `8181de61…` | `91a45e96…` |
| `registration.proto` | `b704e557…` | `9432f097…` |
| `session.proto` | `ba33db71…` | `fa04d968…` |
| descriptor set | `e2493ef1…` | `d45fd763…` |

The wire contract did not change. Only a language-specific option did, so nothing is
broken at runtime today — which is precisely why this can sit undetected until something
subtler drifts behind the same green check.

## Solution

Re-synchronise the four artefacts, and change what the manifest records so that staleness
becomes visible rather than invisible.

The manifest gains the identity of the source it was taken from. The check then asserts two
things instead of one: that the copy matches its manifest (unchanged, catches tampering)
**and** that the manifest was taken from the source commit this repository expects (new,
catches staleness). Falling behind now fails a test with a message naming the commit to
re-sync from.

Synchronisation becomes one operation that updates copy, manifest, descriptor and recorded
source identity together. Updating any subset is what produced this situation.

## User Stories

1. As an engineer working on the .NET control plane, I want the vendored proto copy to match
   the contract the Relay actually speaks, so that generated C# describes the real wire
   format.
2. As an engineer, I want a stale copy to fail a test, so that I learn about drift from CI
   rather than from a runtime mismatch during an integration run.
3. As an engineer, I want the failure message to name the source commit to re-sync from, so
   that fixing it is mechanical rather than an investigation.
4. As an engineer, I want re-synchronising to be one command, so that I cannot update the
   copy and forget the descriptor.
5. As an engineer, I want the recorded source commit to be part of the diff, so that a
   re-sync is a reviewable change rather than an invisible one.
6. As a reviewer, I want a proto change to arrive with its manifest and descriptor updated
   in the same commit, so that I can see the whole contract move at once.
7. As a reviewer, I want to know which Relay commit this repository is pinned to, so that I
   can tell whether a reported protocol behaviour applies here.
8. As an engineer proving the end-to-end relay path, I want both sides generated from the
   same contract, so that a failure is a real defect rather than a version skew.
9. As a security reviewer, I want the copy verified against a recorded source rather than
   trusted, so that a modified vendored contract cannot pass unnoticed.
10. As an engineer, I want the check to keep catching tampering with the copy, so that the
    new capability does not replace the old one.
11. As an engineer, I want the descriptor regenerated from the synchronised copy rather than
    copied separately, so that the two cannot disagree.
12. As a maintainer, I want the sync to refuse to run against a dirty source tree, so that a
    recorded commit always describes content that actually exists at that commit.
13. As an engineer, I want the check to fail loudly when the manifest lists a file that is
    absent, so that a partial sync is not silently tolerated.
14. As an engineer, I want the check to fail when a proto file exists that the manifest does
    not list, so that an added contract file cannot slip in unverified.
15. As a future maintainer, I want the reasoning recorded where the check lives, so that the
    next person understands why internal consistency alone was insufficient.

## Implementation Decisions

**The manifest records provenance, not just content.** It gains the source repository's
commit identity alongside the per-file hashes. Hashes answer "has this been altered"; the
commit answers "what was it taken from". Only the second detects staleness.

**The expected source commit is pinned in this repository** and asserted by the check. This
makes advancing the pin a deliberate act with a diff, in the same spirit as pinning a
dependency version. A re-sync that forgets to move the pin fails; a pin moved without a
re-sync fails on hashes.

**Synchronisation is one operation.** It copies the proto files, regenerates the descriptor
from the copy it just wrote, recomputes hashes, and records the source commit — all or
nothing. The descriptor is generated from the synchronised copy rather than transferred, so
the two cannot disagree by construction.

**The sync refuses a dirty source tree.** Recording a commit identity for content that
includes uncommitted edits produces a manifest that describes something unreproducible.

**Scope is bounded by the fact that this consumer is temporary.** This repository is a
frozen reference implementation. The Go control plane will consume the Relay's contract as
an ordinary module dependency, where the vendoring problem does not exist at all — the
compiler resolves the version and the module checksum database verifies it. Building
elaborate cross-repository CI for a consumer scheduled for retirement is effort spent on
the wrong side. The gate therefore lives entirely inside this repository and needs no
network access, no access to the Relay repository, and no CI coordination between the two.

**The wire contract is unchanged.** This re-sync alters a language-specific generation
option and the descriptor that embeds it. No message, field, service or semantic changes.
Anything that does change the wire contract is a different change with its own review.

## Testing Decisions

**What makes a good test here.** The test asserts a property an engineer could verify by
hand — does this copy match what it claims to be a copy of — and says what to do when it
does not. It does not assert how synchronisation is implemented, so replacing the sync
mechanism does not break it.

**The seam is the existing protocol-sync test.** It already reads the manifest and hashes
the copy; it gains the provenance assertion. No new seam is introduced, and no test double
appears anywhere: the inputs are files on disk.

**Scenarios.**

- A synchronised tree passes.
- A modified proto file fails, naming the file (existing behaviour, must not regress).
- A manifest recording a source commit other than the pinned one fails, and the message
  names both commits.
- A proto file present on disk but absent from the manifest fails.
- A manifest entry naming a file that does not exist fails.
- The descriptor being stale relative to the proto copy fails.

**Prior art.** The existing sync test in this repository establishes the shape — read the
manifest, hash the listed files, compare, and additionally assert no unlisted proto exists.
The Relay repository's generated-code drift gate establishes the discipline: regenerate,
then fail if anything differs from what is committed.

## Out of Scope

- Any change to the wire contract: messages, fields, services, or their semantics.
- Automating the sync in CI, or having either repository trigger the other. The sync is run
  deliberately by a person when the contract moves.
- Publishing the contract to a schema registry. That was considered and deferred because it
  puts a hosted dependency in the build path.
- Making the Go control plane consume the contract. It will use a module dependency, which
  is a different mechanism with none of these problems.
- Retiring the vendored copy. That happens when this repository is archived.

## Further Notes

This is the cheapest item outstanding and it removes a false signal rather than adding a
feature. It should land before the end-to-end relay proof, because that work depends on both
sides speaking the same contract and would otherwise debug a version skew as if it were a
protocol defect.

The general lesson is worth recording separately from the fix: a consistency check whose
reference is derived from the thing being checked cannot detect drift in both together. The
reference has to be external to the artefact, or the check only proves the artefact agrees
with itself.
