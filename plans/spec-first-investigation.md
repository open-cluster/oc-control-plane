# Spec — The first investigation

Status: READY FOR IMPLEMENTATION. This is the slice the product exists for.
Date: 2026-07-31
Repository: the Go control plane (capabilities in the Relay — see
`spec-capabilities-kubernetes-events-and-logs.md`; evaluation in `spec-scenario-harness.md`)
Decision records: ADR-008 (investigator first), ADR-009 (brief then bounded adaptive planner),
ADR-010 (persist what decays), ADR-011 (abstention standard), ADR-012 (untrusted evidence and
redaction), ADR-003 as amended (Connection is the environment authority), ADR-001 (typed bounded
capabilities)
Glossary: `CONTEXT.md`

## Problem Statement

Nothing investigates.

The control plane can dispatch a typed bounded read to a real cluster and record its result
durably. It can turn a customer's alert into a Signal. Between those two facts there is nothing:
no Investigation, no evidence, no timeline, no hypothesis, no conclusion. Across the frozen .NET
reference and both Go repositories the only implementation of an investigation executor is one
that throws.

So the product's single load-bearing claim — that bounded typed reads plus reasoning can produce an
explanation an on-call engineer finds useful at 03:00 — has never been tested. Every further
refinement of the truth model is a prediction about how that reasoning will fail, and no amount of
design distinguishes the right predictions from the wrong ones. Only a run does.

The engineer this is for has been paged, has a namespace and a workload name, and has nothing else.
Today they open a terminal. The question is whether OpenCluster can hand them a supported
explanation before they finish typing the first command — or, when it cannot, say so plainly enough
that they trust it the next time.

## Solution

An engineer names a Connection, a resource scope and a time window. The control plane derives the
Environment from that Connection and opens an Investigation.

It first assembles an **Investigation brief**: what is being looked at, when, what changed
recently, what the live topology around it is, which capabilities are available in that
Environment, and what coverage is missing. The brief is orientation. It states what may be asked
and deliberately does not prescribe what will be.

From the brief the planner forms competing hypotheses and requests typed read-only evidence. Each
request is validated against organization, Environment, Connection, resource scope, time range,
cost, result size, step count and timeout before any job is dispatched. Results are validated into
EvidenceItems or recorded as CoverageGaps. Up to **two adaptive rounds** follow, each driven by the
hypotheses currently held.

The run then terminates in exactly one of two ways. Either a **most supported explanation**, where
every claim cites the EvidenceItems that support it and the surviving alternatives and
contradictions are shown alongside it — or an **abstention** that names what was missing, what
contradicted what, and what could not be checked. A confident conclusion without sufficient support
is not a permitted outcome.

Everything the run intended, requested, received and concluded is recorded, so the whole
investigation replays from its own record with no access to live sources.

## User Stories

1. As an on-call engineer, I want to ask for an investigation by naming a workload and a time
   window, so that I can start one from a page without configuring anything.
2. As an on-call engineer, I want the investigation to run without further input from me, so that I
   can read the result rather than drive a tool.
3. As an on-call engineer, I want to see a timeline of what happened, so that I can understand
   ordering rather than a pile of facts.
4. As an on-call engineer, I want to see more than one candidate explanation, so that I am not
   anchored on the first plausible one.
5. As an on-call engineer, I want each explanation to show the evidence supporting it, so that I can
   check the reasoning rather than trust it.
6. As an on-call engineer, I want each explanation to show the evidence contradicting it, so that I
   can see what the system had to argue past.
7. As an on-call engineer, I want to be told when the system could not determine the cause, so that
   I stop reading and start working.
8. As an on-call engineer, I want to be told what the system could not check, so that I know where
   to look myself.
9. As an on-call engineer, I want to know when a field was masked by my own organization's
   redaction policy, so that I do not mistake a policy hole for an absence of evidence.
10. As an on-call engineer, I want to see what changed recently around the affected resource, so
    that the first question I would have asked is already answered.
11. As an on-call engineer, I want a result quickly enough that waiting beats typing, so that using
    it is not slower than not using it.
12. As an on-call engineer, I want to cancel a running investigation, so that a run I no longer
    need stops costing money.
13. As an on-call engineer, I want to open an investigation that is still running and see what it
    has found so far, so that I am not blocked on a terminal state.
14. As an on-call engineer, I want to re-run an investigation over the same scope, so that I can
    ask again once the situation has moved.
15. As an on-call engineer, I want a re-run to be a new run rather than an edit of the old one, so
    that what I read the first time still exists.
16. As an engineer reviewing afterwards, I want the whole investigation reconstructable, so that a
    surprising conclusion can be examined rather than argued about.
17. As an engineer reviewing afterwards, I want to see which requests the planner made and why, so
    that I can tell a bad conclusion from bad evidence selection.
18. As an engineer reviewing afterwards, I want to see requests that returned nothing useful, so
    that the record shows what was tried, not only what worked.
19. As an engineer, I want an uncited claim to be impossible rather than discouraged, so that
    citation discipline does not depend on review.
20. As an engineer, I want facts, hypotheses and conclusions kept distinct in the record, so that a
    hypothesis cannot quietly become a fact.
21. As an engineer, I want the Environment derived from the named Connection, so that no caller can
    assert a scope.
22. As an engineer, I want an investigation spanning Connections in different Environments to be
    refused, so that the boundary is enforced at the point of request.
23. As an engineer, I want every capability request checked against the investigation's scope before
    dispatch, so that a request outside it never reaches a customer's cluster.
24. As an engineer, I want an investigation to survive a control-plane restart, so that a deploy
    does not lose a run.
25. As an engineer, I want a run that has lost its lease to be recoverable rather than duplicated,
    so that two workers cannot investigate the same run at once.
26. As a security reviewer, I want evidence text treated as untrusted for its whole life, so that
    text from a customer's systems cannot become an instruction.
27. As a security reviewer, I want the planner to be structurally unable to derive a request from
    evidence text, so that injected log content cannot steer execution.
28. As a security reviewer, I want every request bounded in time range, result size, cost and step
    count, so that one investigation cannot become an unpriced loop.
29. As a security reviewer, I want an investigation authorised against the organization that owns
    the Connection and no other, so that a run cannot read across tenants.
30. As a security reviewer, I want no evidence content in logs or traces, so that diagnosis is not a
    disclosure channel.
31. As an operator, I want to see what an investigation cost, so that I can price the feature before
    enabling it broadly.
32. As an operator, I want investigations to fail closed when the model provider is unavailable, so
    that an outage produces an honest failure rather than a guess.
33. As an operator, I want an investigation to be attributable to whoever started it, so that the
    record says who asked.
34. As a platform engineer, I want the same investigation to be replayable from its record without a
    live cluster, so that I can debug one long after the cluster changed.
35. As a platform engineer, I want CI to exercise the whole path without calling a paid model, so
    that the pipeline stays fast, free and deterministic.
36. As a platform engineer, I want recorded model transcripts versioned by model, prompt, output
    schema and investigator version, so that a stale recording is detected rather than silently
    replayed.
37. As the founder, I want the system to abstain rather than guess, so that one wrong answer at
    03:00 does not end the product's credibility with a team.
38. As the founder, I want evidence selection and conclusion quality scored separately, so that I
    can tell which half is failing.
39. As the founder, I want the first slice narrow and honest about what is hard-coded, so that a
    demo is not mistaken for a general capability.

## Implementation Decisions

**One trigger in this slice: a human naming a Connection, a scope and a window.** Signal-triggered
investigation is not built here. Intake already produces Signals and nothing consumes them; that
remains true after this slice and is the natural next one. The manual path is chosen because it
does not depend on a customer's alert quality, which is exactly the property that makes the product
demonstrable.

**Scope resolution is deliberately narrow and hard-coded.** A scope is a Kubernetes namespace plus
a workload kind and name, resolved through one Kubernetes Connection. It is not a general resource
resolver and no canonical identity is involved. **This code is expected to be discarded.** What is
owed is a written list of what is hard-coded, not an early generalisation.

**The Environment is derived, never accepted.** The request names a Connection; the control plane
reads that Connection's Environment and persists it on the Investigation. A request naming several
Connections is refused unless they share one. A client may send an Environment for navigation; it
is ignored as an authority.

**Investigation lifecycle.** A closed vocabulary, with every terminal state stamped:
`pending → briefing → reasoning → gathering → (reasoning → gathering)* → concluded | abstained |
cancelled | failed`. `concluded` and `abstained` are both successful outcomes and are distinguished
because they mean different things to a reader. `failed` is reserved for the platform failing —
model provider unavailable, budget exhausted before any round completed, storage error — and never
for "no explanation was found", which is `abstained`.

**Durability follows the job table's shape, not a new one.** A run is claimed under a server-clock
lease fenced by session and epoch, exactly as `relay_job` is. A control-plane restart returns the
run to claimable; a worker that lost its lease cannot write. This is proven machinery and inventing
a second concurrency model for the same problem would be the mistake ADR-004 warns about in a
different domain.

**The Investigation brief is assembled deterministically, before any hypothesis exists.** It
carries:

- resource identity as the cluster reports it, including UID, so a recreated object is not confused
  with its predecessor;
- the trigger, who or what started the run, and the time window;
- **recent changes** around the resource;
- **live topology context** — owner references and node placement, resolved live per ADR-010;
- **available capabilities** in that Environment, so the planner cannot propose a read that does
  not exist;
- **coverage** — what is not reachable and why.

**In this slice, "recent changes" is a live read, not the change ledger.** ADR-010's continuously
persisted change ledger is a separate slice and is not a prerequisite. What is available live is
ReplicaSet revision history, which is bounded by the cluster's `revisionHistoryLimit`, and
Kubernetes Events, which expire on the cluster's own TTL. **Where that horizon truncates the window
the engineer asked for, the brief records a CoverageGap saying so.** This is the honest version and
it is also the argument the change-ledger slice will be built on: the gap will show up in the
harness as scenarios the system cannot explain because the evidence had already expired.

**The brief is orientation and is not expected to contain the answer.** Assembling it never
constitutes an investigation. A run that terminated at the end of the brief has abstained.

**The planner does two things and they are recorded separately: it proposes hypotheses, and it
proposes requests.** A request is always justified by a hypothesis it would support or falsify, and
that justification is persisted. This is the property that makes evidence selection separately
scorable, and it is also the containment mechanism from ADR-012: **the planner may never derive a
request from evidence text.** The chain from "a log line said X" to "therefore read Y" passes
through a typed hypothesis a human can read.

**Two adaptive rounds, hard-capped.** Per run: a maximum number of capability requests, a maximum
total result size, a wall-clock deadline, a token/cost ceiling, and per-request timeouts. Exhausting
a budget is not a failure — it terminates the reasoning and the run concludes or abstains on what it
has, recording which budget was exhausted as a CoverageGap. A budget exhausted before the first
round completes is `failed`.

**Every request is validated before dispatch** against organization, Environment, Connection,
resource scope, time range, declared cost, result size and remaining budget. Validation happens in
the control plane, and the Relay re-validates on receipt as it already does. A request that fails
validation is recorded with its reason and never dispatched.

**Results become EvidenceItems through validation, never directly.** A result arrives, is validated
for grounding, provenance, scope and completeness, and produces either an EvidenceItem or a
CoverageGap. A Relay-attested completeness claim is recorded as such and carries the trust class
ADR-001 defines; it is never promoted to centrally verified.

**Absence is only a fact with a completeness certificate.** A read that found nothing and completed
without a continuation token can support an absence claim. A read that was truncated, bounded, or
partially authorised cannot, and produces a CoverageGap instead. This is already the shape the
`kubernetes.workload.runtime` result carries and it must not be weakened by convenience.

**The timeline is derived, not authored.** It is built from EvidenceItems carrying source-observed
timestamps, and it distinguishes source time from receipt time. An item with no defensible time is
not placed on the timeline; it is listed beside it.

**The output schema makes an uncited claim impossible.** Each claim in a conclusion carries at
least one EvidenceItem reference; the schema rejects a claim without one before it is persisted, so
citation is a structural property rather than a review obligation. A model response that fails the
schema is retried once and then abstains.

**Abstention is a first-class outcome and carries content.** It names what was missing, which
hypotheses were live and unresolved, what contradicted what, and which coverage gaps mattered. An
abstention with no explanation of why is a defect.

**The model provider sits behind one boundary, and CI never crosses it.** Transcripts are recorded
from real runs and replayed in commit CI, keyed and versioned by model identifier, prompt version,
output schema version and investigator version. A key mismatch fails loudly rather than replaying a
stale recording. **Recorded fixtures contain synthetic scenario data only** — no customer evidence
is ever committed. Live-model evaluation is the scenario harness's job and runs manually, before
releases, and periodically to detect model drift.

**Evidence content never appears in logs, traces or metrics.** Identifiers, counts, outcomes and
reasons do. This continues the discipline intake already established and tested.

**Reasoning artifacts are persisted as data.** Hypotheses, their stance towards each EvidenceItem,
transitions between states, the requests proposed and their justifications, and the resolved brief.
These are the case pack. A run replays from them with no live source access.

**Cost and token consumption are recorded per run**, because the founder cannot price a feature
whose unit cost is unknown and an operator cannot enable one.

## Testing Decisions

**What makes a good test here.** It asserts what an engineer could observe — an investigation
reached a terminal state, its conclusion cites evidence that exists, a request outside scope was
refused before dispatch, an abstention names what was missing. It does not assert how hypotheses
were represented or which prompt was used, because both will change.

**Two existing seams, one new one. No others.**

**Seam 1 — the composition root**, real HTTP and a real Postgres, whole process. Everything about
the investigation lifecycle, scope derivation, budget enforcement, request validation, evidence
validation, the output schema, abstention and replay is tested here, with the model boundary
replaying transcripts.

**Seam 2 — the end-to-end process harness in `test/e2e`**, which already runs a real control plane,
a real TLS terminator, a real Relay, a real single-node Kubernetes and a real Postgres with nothing
faked. The first investigation is proven here: a real cluster is broken, an investigation runs
against it through the real protocol, and the conclusion is asserted against durable state. Its
`doc.go` already says it enqueues a job "the way an investigation would"; this slice makes that
literal. The exhaustive not-proven list in that file is extended, not quietly left behind.

**Seam 3 — the model boundary, new.** The only new seam, and the only component in the program that
is faked in CI. `test/e2e/doc.go` states the rule this deviates from: nothing here is a test double,
because every component that could be faked is one whose behaviour is in question. The model is the
first genuine exception — it is nondeterministic and priced per call, and what is in question about
it is answered by the scenario harness rather than by a commit gate. The deviation is recorded at
the seam itself so the next reader does not think the rule was forgotten.

**Live-model runs are never a commit gate.** They run in the harness. A CI job that fails on
ordinary model variance is a gate that gets ignored within two weeks.

**Scenarios.**

- An investigation is created from a Connection, scope and window, and reaches a terminal state.
- Its Environment matches the named Connection's, and an Environment sent by the caller is ignored.
- A request naming Connections in two Environments is refused.
- A request naming another organization's Connection is refused, with both organizations on the
  same placement.
- The brief is assembled and persisted before any hypothesis exists.
- A window extending past the cluster's event TTL produces a CoverageGap naming the truncation.
- A capability request outside the investigation's scope is refused before dispatch and no
  `relay_job` row becomes dispatchable.
- A run exhausting its request budget terminates and records which budget was exhausted.
- A run exhausting its budget before the first round completes is `failed`, not `abstained`.
- A conclusion's every claim resolves to an EvidenceItem that exists in the same run.
- A model response containing an uncited claim is rejected and does not reach storage.
- A scenario whose decisive evidence is unavailable produces an abstention naming what was missing.
- A truncated read does not produce an absence claim.
- A Relay-attested completeness claim is recorded with its trust class and is not promoted.
- A cancelled investigation stops, is terminal, and dispatches nothing further.
- An investigation survives a control-plane restart and completes.
- A worker whose lease expired cannot write to the run that outlived it.
- A run replays from its case pack with the database's live tables unavailable.
- A log line containing an injection attempt does not change which capabilities were requested.
- No log line, trace attribute or metric label contains evidence content.
- The model provider being unavailable produces `failed`, not a conclusion.

**Prior art.** `test/e2e` is the harness and needs extending rather than replacing. The relay job
store establishes the lease, fence and enumerated-refusal shapes the run leasing copies. Intake
establishes the discipline of logging a rejection with its reason and never its payload. The frozen
.NET `OpenCluster.Investigations` is worth reading for storage shapes and refusal taxonomies and
for nothing else: its executor throws, so it is an oracle for the ledger and not for anything that
reasons.

## Out of Scope

- Signal-triggered investigation. Intake produces Signals; consuming them is the next slice.
- Incidents and grouping.
- Canonical resource identity beyond one Kubernetes Connection.
- The persisted change ledger (ADR-010) — this slice reads changes live and records the gap.
- Relay-side redaction (ADR-012). **This is a blocking prerequisite for any non-synthetic cluster**
  and is deliberately not a prerequisite for the harness, which runs on clusters that contain no
  real data. No design-partner installation happens before it exists.
- Postmortem generation. The case pack makes it cheap later; it is not built here.
- More than two adaptive rounds, planner self-critique, or multi-agent structures.
- Any frontend. The read model that a UI needs is a separate spec.
- Notification of investigation outcomes.
- Cost attribution and quotas per organization beyond recording what a run consumed.

## Further Notes

The single most important property of this slice is not that it produces good explanations. It is
that it produces **inspectable** ones, quickly enough that the harness can be run many times as
the planner changes. Everything above optimises for that: separately recorded selection and
conclusion, transcripts in CI, replay without live sources, and abstention as a real outcome rather
than a fallback.

The second most important property is honesty about what is hard-coded. Scope resolution, the
hypothesis vocabulary and the brief's contents are all narrow, and a demo will make them look
general. The written list of hard-coded assumptions is part of the deliverable, not an afterthought.

Two risks worth stating rather than discovering. Adaptive rounds make ADR-011's standard harder,
because a system that can keep looking can also look in the wrong direction twice and then conclude
from a picture that feels complete — the exhaustion path is where that will show up first. And the
harness's scenarios are chosen by the person who built the system, so they measure whether it works
on failures he already understands. Both are accepted; neither is solved here.
