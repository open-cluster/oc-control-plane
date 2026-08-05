# Spec — Acceptance through the browser

Status: **READY FOR IMPLEMENTATION. Not started.** Its one blocking dependency —
`spec-operator-api-identity-and-rbac.md` — was built on 2026-08-05, so increment 1 is now
unblocked: no browser has yet rendered a live investigation.

Repo: `oc-control-plane`, extending `plans/spec-scenario-harness.md`. Spans `oc-frontend` and
references `opencluster-relay`.
Depends on `plans/spec-operator-api-identity-and-rbac.md`.
Audit basis: `oc-frontend/plans/audit-2026-08-04-enterprise-forensic.md` §1 B1, B7, B8.

**This spec deliberately does not re-specify the scenario harness, the live model provider, or the
Relay end-to-end proof.** All three exist and have run.
`spec-scenario-harness.md` (BUILT 2026-08-01, run against a live provider 2026-08-02) provisions ten
deliberately broken k3s scenarios from code and has taken `red-herring` end to end through the real
control plane and a real Relay. `spec-live-model-provider.md` (IMPLEMENTED 2026-08-02) records five
live investigations. `spec-relay-end-to-end-proof.md` has been in CI since 2026-07-30. Writing a
second k3s acceptance specification would duplicate working instruments and is the wrong move.

What this spec adds is the one hop none of them cover.

## Problem Statement

A live model has investigated a real broken cluster through the real control plane and a real Relay.
No human has ever watched it happen in a browser.

`plans/implementation-status.md:506` lists **"The frontend against the Go API"** among verified
absences, and the audit measured what that costs: the frontend sends a cookie session
(`oc-frontend/shared/lib/transport.ts`, `credentials: 'include'`), the operator surface requires a
shared static bearer token (`internal/operator/operator.go:97-111`), and sixteen of the frontend's
thirty-one declared operations are absent or differently shaped. Against a real control plane the
application renders its unauthorized state on every route.

So the product has two halves that each work and have never met. That is precisely the failure
`spec-relay-end-to-end-proof.md` was written about — *"two halves built against models of each other
have never met"* — repeated one layer up, between the control plane and the interface.

Three further gaps sit alongside it, recorded in the specs that own them and repeated here because
they bound what an acceptance run can currently claim:

- A live run writes **no transcript file**, so CI cannot replay what the model said
  (`spec-live-model-provider.md`, outstanding requirement).
- **One scenario of ten** has run end to end (`spec-scenario-harness.md`).
- **No blind two-scorer scoring** has happened; the runs were read by the person who built the
  system.

And one path the harness cannot exercise at all: intake does not reach Investigations. Signals
persist (`internal/intake/intake.go:88`, migration `0007_signal_intake.sql`) and nothing opens an
Investigation from one — deferred by ADR-008, not broken by accident. The harness opens
investigations through the operator API instead, which is why it works.

## Solution

Extend the existing harness with a browser hop and a transcript, then close the alert path.

**Increment 1 — the browser hop.** Once the control plane issues a real session
(`spec-operator-api-identity-and-rbac.md`), the harness's existing `red-herring` run gains a final
stage: a Playwright spec signs in with a real session, opens the Investigation the harness just
created by its real id, watches the timeline advance while the case runs, opens full evidence, and
asserts the rendered verdict form matches the harness's recorded outcome. The run report gains the
browser hop with the same real-or-stubbed flag every other hop carries.

**Increment 2 — the transcript.** `OC_MODEL_TRANSCRIPT_FILE` is honoured on live runs, so what the
model actually said is a committed artifact and CI replays it. This is already an outstanding
requirement of `spec-live-model-provider.md` and is listed here because the browser assertion is
worth much less without it: replaying a recorded transcript is what makes the frontend assertion
cheap enough to run on every commit rather than nightly.

**Increment 3 — the alert path.** Wire intake to Investigations so the chain starts where a customer
starts it: Prometheus fires, Alertmanager posts to a real Trigger Connection, an IncidentEpisode is
keyed, an Investigation opens under the Organization's Investigation controls. This closes the one
hop the current end-to-end story skips.

**Increment 4 — the set, and honest scoring.** The remaining nine scenarios end to end, and blind
two-scorer scoring by engineers who did not build the system. Both are already this repository's own
stated requirements and neither is frontend work; they are listed so the acceptance claim is not
made before they land.

## User Stories

1. As an engineer, I want a Playwright spec to sign in with a real session and open a live
   Investigation, so that the frontend-to-control-plane path is proven rather than assumed.
2. As an engineer, I want the browser to watch a running investigation advance, so that the
   conditional-read and polling path is exercised against a real case version.
3. As an engineer, I want the browser to open full evidence and see the bytes the Relay returned, so
   that the citation chain is verified end to end.
4. As an engineer, I want the rendered verdict form asserted against the harness's recorded outcome,
   so that a supported, caveated or abstained result cannot be mis-rendered silently.
5. As an engineer, I want a run in which the evidence is insufficient to be rendered as an
   abstention in the browser, so that the product's most distinctive output is proven in the
   interface and not only in the database.
6. As an engineer, I want a CoverageGap produced by a real authorization denial to appear in place
   of the fact it replaces, so that `GapInPlace` is verified against a real denial.
7. As an engineer, I want the live run to write a transcript file, so that CI replays what the model
   actually said instead of asking it again.
8. As an engineer, I want the replayed transcript to drive the frontend assertions on every commit,
   so that a UI regression against a real case is caught in minutes rather than nightly.
9. As an engineer, I want a real Alertmanager alert to open an Investigation, so that the chain
   starts where a customer's chain starts.
10. As an engineer, I want repeated firings of one alert to append to one IncidentEpisode, so that a
    flapping alert does not produce forty Investigations.
11. As an engineer, I want automatic start gated on the Organization's Investigation controls, so
    that a customer who has not authorized automatic investigation gets an episode and no round.
12. As an operator, I want to record a remediation I performed, so that the outcome assessment has a
    stated human origin rather than an inferred one.
13. As an operator, I want recovery verified by a fresh read rather than by the alert clearing, so
    that "it stopped firing" is not mistaken for "it was fixed".
14. As an auditor, I want every hop of the run to appear in the audit log with an actor, so that the
    run is reconstructible from the record alone.
15. As an engineer, I want the run report to name which hop failed, so that a red run is a diagnosis
    rather than a mystery.
16. As an engineer, I want a run with any stubbed hop to be incapable of reporting success, so that
    a passing run cannot mean a fixture ran.
17. As an engineer, I want all ten scenarios exercised end to end before acceptance is claimed, so
    that one working scenario is not read as a working product.
18. As an engineer, I want blind two-scorer scoring by people who did not build the system, so that
    the quality claim is not self-assessment.
19. As a design partner, I want a machine-readable run report I can reproduce on my own cluster, so
    that "show me it working on something real" has an artifact.

## Implementation Decisions

**Increment 1 is blocked on the session and on nothing else.** Do not attempt a browser hop against
the shared static token by teaching Playwright to send it: that would prove a path no operator can
use and would leave the real defect in place. The order is session first.

**The browser hop lives in `oc-frontend/tests/e2e/`, driven by the harness.** The harness owns
provisioning and the investigation id; the Playwright spec receives the id and asserts against it.
Prior art: `oc-frontend/tests/e2e/case-file.spec.ts` and `control-plane.ts` already have the shape,
including the discipline of reporting *which* of "not running", "wrong process", "refused" or "not
implemented yet" applied. That discipline is kept — with one change: **in the scheduled acceptance
job a skip fails the build.** A skip that reads as a pass is what let the sixteen-operation contract
divergence go unnoticed, and the same mechanism must not be allowed to hide this.

**Three scenario variants carry the frontend assertions**, chosen from the harness's existing ten
rather than newly written: one whose evidence supports an explanation, one whose evidence is
insufficient and must abstain, and one with a capability denied so a CoverageGap is produced by a
real denial. The second and third are not optional. A harness that only proves the happy path proves
the least interesting claim, and abstention and coverage honesty are what this product is for.

**Transcript replay is the mechanism that makes this affordable.** Live runs are nightly and cost
real money and cluster time. Replayed runs drive the frontend on every commit. Both are real: a
replay asserts the interface against a real model's real output, and only the model call is
substituted. The run report must distinguish them, because "live" and "replayed" are different
claims.

**Intake wiring, dependency direction.** `internal/intake` must not import `internal/investigation`
wholesale. Introduce a narrow `EpisodeOpener` interface owned by `intake` and satisfied by
`investigation`, so `internal/gates/dependency_boundary_test.go` keeps enforcing the import graph
rather than being relaxed to accommodate this. Episode keying follows `CONTEXT.md` — organization,
environment, trigger source, source-provided identity, affected target, keyed conservatively.

**Run report.** Extends the harness's existing artifact rather than replacing it, adding: the
browser hop, per-hop real/replayed/stubbed, the transcript file identity, and the audit event ids
the run produced. `artifacts/` already holds run artifacts separated from ground truth and scores;
that layout stays.

**Relay side: nothing changes, and the run verifies it.** `opencluster-relay` already compiles
typed read-only capabilities, enforces masking at one point between a capability's result and the
wire with no opt-out, and keeps a local audit log of every job and result byte count. The harness
reads that local log and asserts it accounts for every job the control plane dispatched. A
disagreement there is worth more than the rest of the run, and today nothing checks it.

## Testing Decisions

The run is the test; these decisions are about making it trustworthy.

**It must be able to fail, and to say where.** Per-hop real/replayed/stubbed flags, any stub
forfeits success, and a named failing hop. Prior art for the naming discipline is
`oc-frontend/tests/e2e/control-plane.ts`.

**Cadence.** Live full run nightly on `main` and on demand. Replayed run on every commit. A failed
nightly blocks the next release.

**Contract drift runs in the same job.** `oc-frontend/tests/e2e/contract-drift.spec.ts` currently
skips at collection when nothing is listening and sends no credential. It gains a credential and
runs against the same seeded control plane the acceptance run uses. This is the cheapest test in the
whole programme and it catches the most expensive class of defect.

**Unit prerequisites stay in the repos that own them.** Episode keying is a control-plane table test
of near-miss signals that must and must not group. Automatic-start gating is a control-plane unit
test. Neither belongs in the harness — the harness proves integration, not logic.

## Out of Scope

Re-specifying the scenario harness, the live model provider, or the Relay end-to-end proof — all
built, all with their own specs. Model quality evaluation, which is the blind-scoring discipline
`spec-scenario-harness.md` owns; this spec proves a live round is well-formed, reproducible and
correctly rendered, and says nothing about whether the reasoning is good. Multi-cluster scenarios.
Non-Kubernetes providers. Load and performance testing.

## Further Notes

**What may and may not be claimed, stated plainly so it is not overstated later.**

Claimable today: a live model has investigated a real broken k3s cluster through the real control
plane and a real Relay, five times.

Not claimable today: that the product works end to end. Nobody has seen it in a browser; nine of ten
scenarios have not run; no independent scorer has read the output; a real alert cannot start an
investigation.

Claimable after increments 1–2: an operator can sign in and follow a live evidence-backed
investigation to its verdict, and the run is reproducible from a transcript.

Claimable after increments 1–4: the acceptance question in the originating brief, in full.

Increment 1 is also what turns the run report into something showable. Until a human has watched a
live investigation in a browser, the only demonstration available is a database row and a log.
