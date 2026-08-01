# Spec — Investigation read models

Status: IMPLEMENTED 2026-08-01, alongside `spec-first-investigation.md`. The frontend cannot begin
its case file without these, and they are cheaper to design with the writer than after it.
Date: 2026-07-31
Repository: the Go control plane. Consumed by the frontend, whose specifications live in
`oc-frontend/plans/`.
Decision records: ADR-013 (an Investigation is a durable case; rounds are its executions), ADR-014
(one renderer, one permission-aware read model), ADR-011 (abstention standard), ADR-015 (resolved
control snapshot per run), ADR-012 (redaction produces coverage gaps), ADR-003 as amended
(environment derived from the Connection)
Glossary: `CONTEXT.md`

## Problem Statement

The investigator will produce a case and nothing can read it.

`spec-first-investigation.md` writes a great deal: a brief, requests with their justifications,
evidence items, coverage gaps, hypotheses with stances, a timeline, per-round outcomes, resolved
plan and control snapshots, and component versions. All of it exists to be inspected — that is the
product — and no contract exposes any of it.

Three consumers need it and they are usually built as three things, which is the mistake this
specification exists to prevent. The live view needs the current state of a running case, refreshed.
The case file needs the same information settled, at a permalink, shareable and exportable. The
scenario harness needs it as a scored artifact. If those become three assemblies they will diverge,
and a customer will eventually notice that the shared version says something the application does
not.

There is also a known defect to avoid repeating. The frozen .NET frontend contract audit recorded
that run detail's `version` tracked only the latest lifecycle transition, so growth in evidence,
hypotheses and steps was invisible to a polling client. It also recorded that cursor lists carried
no totals, so the incident list fanned out one request per row. Both are in the historical record
with their consequences already measured.

## Solution

One logical aggregate, several transport shapes, and one version that governs all of them.

An Investigation is one identity, one permission boundary and one monotonic **case version** that
advances whenever anything within the case changes — a round opening, evidence arriving, a gap being
recorded, a hypothesis moving, an outcome being reached or superseded.

A client reads a small **summary** and polls it by version. Large collections — timeline, evidence,
hypotheses, gaps, activity — are separate, paginated reads, each stamped with the case version it
represents, so a client can tell when what it holds is stale. Unopened sections are never fetched.

For sharing, export and harness evaluation the server assembles the **complete case file at a pinned
case version**, in one pass, without the browser retrieving every section first.

Every one of these is permission-aware and derives its Environment from the Connection. None of them
computes a number that is not backed by an evidence item.

## User Stories

1. As a client, I want one request that tells me everything needed to render the top of a case, so
   that the first paint is not blocked on five round trips.
2. As a client, I want one monotonic version covering the whole case, so that I can ask "has
   anything changed" and get a truthful answer.
3. As a client, I want that version to advance when evidence grows, not only when the lifecycle
   transitions, so that a polling client is not blind to the thing it is watching for.
4. As a client, I want a cheap negative answer when nothing has changed, so that polling costs little.
5. As a client, I want every response to carry the case version it represents, so that I can tell a
   stale section from a current one.
6. As a client, I want to fetch a section only when it is opened, so that an unopened tab costs
   nothing.
7. As a client, I want counts for each section in the summary, so that tabs can be labelled without
   fetching their contents.
8. As a client, I want large collections paginated with a stable order, so that a growing case does
   not produce an unbounded response.
9. As a client, I want evidence filterable by capability, source and stance, so that a case with
   hundreds of items is navigable.
10. As a client, I want bounded evidence content fetched separately from the list, so that a listing
    is not the size of its contents.
11. As a client, I want the lifecycle state and current round in the summary, so that I can choose a
    polling rate that matches what is happening.
12. As an on-call engineer, I want the current outcome — supported, caveated or abstained — with its
    stated basis, so that the answer arrives with what it rests on.
13. As an on-call engineer, I want every claim to carry the identifiers of the evidence supporting
    it, so that checking is a lookup rather than a search.
14. As an on-call engineer, I want superseded outcomes retained with their round and time, so that I
    can see the finding changed rather than being told a different thing than a colleague was.
15. As an on-call engineer, I want coverage expressed per capability with its state and reason, so
    that I know what was checked, what was not, and why.
16. As an on-call engineer, I want a complete read that found nothing distinguishable from a read
    that never happened, so that a certified negative is usable.
17. As an on-call engineer, I want affected scope backed by evidence identifiers, so that no figure
    on the page is uncited.
18. As an engineer reviewing afterwards, I want each capability request with the hypothesis that
    justified it, so that I can judge evidence selection separately from the conclusion.
19. As an engineer reviewing afterwards, I want requests that returned nothing useful retained, so
    that the record shows what was tried.
20. As an engineer reviewing afterwards, I want each round's resolved control snapshot, plan
    snapshot, planner version and model version, so that "why did this stop early" is answerable
    without current configuration.
21. As an engineer, I want the complete case file assembled server-side at a pinned version, so that
    a share, an export and a harness artifact are the same bytes.
22. As an engineer, I want an export to identify which rounds it includes, so that a document read
    next quarter carries its own scope.
23. As an engineer, I want the investigation list to carry precomputed counts and summary fields, so
    that rendering it does not fan out one request per row.
24. As an engineer, I want the list ordered by state and recency, with attributed severity as a
    secondary signal, so that the ordering is defensible in a review.
25. As a security reviewer, I want every read scoped to the organization and the investigation's
    Environment, so that no client can read across a boundary.
26. As a security reviewer, I want a request combining one organization's identity with another's
    investigation refused, so that path parameters cannot be composed to cross tenancy.
27. As a security reviewer, I want redacted evidence to arrive as a declared gap rather than as
    empty content, so that masking is visible rather than silent.
28. As a security reviewer, I want evidence content bounded on the read path as well as the write
    path, so that a large item cannot be used to exhaust a client or a proxy.
29. As a security reviewer, I want no read model to expose a secret, a credential digest or a raw
    unredacted payload, so that the read surface is not a disclosure path.
30. As an operator, I want the cost and duration a case has consumed, so that the feature is
    priceable before it is enabled broadly.
31. As an operator, I want to know when a case is waiting rather than running, so that a quiet case
    is distinguishable from a stalled one.
32. As the founder, I want payload boundaries set by measurement rather than by estimate, so that a
    long-running case does not discover them in production.

## Implementation Decisions

**One aggregate, several reads.** The logical aggregate means one case identity, one permission
boundary, one versioning model and one vocabulary. It does not mean one response containing every
section — a case with hundreds of rounds would make that unbounded. The reads are:

- **Summary** — case identity and brief, current trigger, lifecycle state, current round, current
  outcome or abstention with its stated basis, affected-scope summary, per-section counts, cost and
  duration to date, and the whole-case version. This is the only endpoint a client polls.
- **Timeline, Hypotheses, Evidence, Coverage gaps, Activity** — separate reads, paginated where
  unbounded, each stamped with the case version it represents.
- **Evidence item** — full bounded content for one item, fetched on demand.
- **Case file assembly** — the complete case at a pinned version, assembled server-side.

**The case version advances on any change within the case.** Not only on lifecycle transitions. A
round opening, an evidence item arriving, a gap recorded, a hypothesis stance moving, an outcome
reached or superseded — each advances it. This is the specific defect the .NET frontend audit
recorded, and it is the reason polling can be cheap: a client sends the version it holds and a
matching version is answered without assembling anything.

**Conditional reads are first-class.** The summary supports a conditional request keyed on the case
version, and answers "unchanged" without touching the section tables. Polling is state-aware on the
client — a few seconds while investigating, tens of seconds while waiting, stopping when terminal —
so the server should expect a low rate rather than optimise for a high one.

**Every response carries the case version it represents.** A client that refreshes the summary and
finds a newer version invalidates only what is visible; unopened sections are marked stale rather
than fetched. A section response older than the summary a client holds is detectable rather than
silently mixed.

**The summary carries the outcome and its basis, never a score.** Supporting findings, contradicting
findings, relevant checks not made, independent sources, and the reasons alternatives were set aside.
No confidence number, no coverage percentage, no calibrated probability. Internal ranking may order
hypotheses; the read model exposes the ordinal, not the score.

**Claims carry evidence identifiers.** A claim in an outcome references the evidence items supporting
it. The write path already makes an uncited claim impossible; the read model must preserve the
reference rather than flattening the claim to prose, because flattening it is what turns an
inspectable artifact into an assertable one.

**Superseded outcomes are retained with their round and time.** ADR-013's projection: the case has a
present tense and the rounds are immutable. A superseded outcome is readable, attributed, and
ordered.

**Coverage is per typed capability, with a state and a reason.** The five states are checked with
evidence, checked and empty with a completeness certificate, checked but incomplete, relevant but
unavailable, and not applicable. A capability the customer's stack does not provide is not a gap and
must not be reported as one. A field withheld by redaction is a gap with its cause.

**Affected scope is a set of cited statements**, each resolving to evidence. No request counts, no
user counts, no derived percentages. If a figure has no evidence behind it, the read model has no
field for it.

**Each round exposes its resolved control snapshot, plan snapshot, planner version and model
version.** This is what makes "why did this round stop after two requests" answerable from the case
alone, and it is what the harness scores.

**Requests are exposed with their justifying hypothesis, including those that returned nothing
useful.** Evidence selection is scored independently of the conclusion (ADR-009), which is only
possible if what was asked and why survives into the read model.

**Server-side assembly is one code path for three consumers.** The shared route, both export formats
and the harness artifact all take the assembled case file at a pinned version. Assembly names the
rounds included and stamps the case version, so a document read later carries its own scope. The
browser never assembles a case file by fetching every section.

**The list read carries precomputed counts and summary fields.** Ordering is by lifecycle state then
recency, with attributed severity available as a secondary sort. The .NET audit recorded the
alternative — cursor lists with no totals, one request per row — and its consequence.

**Every read is tenant-scoped and Environment-scoped, derived from the Connection.** A request
naming one organization's identity and another's investigation is refused. Nothing accepts an
Environment from the caller.

**Payload boundaries are set by measurement.** Database time, query count, response bytes and p95
latency are measured against small, medium and long-running cases before the boundaries are fixed.
An estimate here is how a summary endpoint quietly becomes the slow one.

**Optional inclusion is bounded.** A field mask or include parameter may attach a small section to
the summary for a consumer that needs it. It may never attach an unbounded collection — timeline,
activity or raw evidence content.

## Testing Decisions

**What makes a good test here.** It asserts what a consumer can observe: the version advanced when
evidence arrived, a conditional read answered unchanged without work, a superseded outcome is still
readable, a request naming another organization's investigation was refused. It does not assert
query shape or table layout, because those will change when the case grows.

**The seam is unchanged: the composition root**, whole process, real HTTP, real Postgres. Every
slice in this repository has used it since the foundation and no new seam is proposed.

**The version test is the load-bearing one and must be written to fail if the defect returns.**
Specifically: recording an evidence item, a coverage gap and a hypothesis stance each advance the
case version, with no lifecycle transition involved. That is the exact regression the .NET
implementation shipped.

**Scale is asserted rather than assumed.** A case with many rounds and many evidence items is
constructed, and the summary read's query count and response size are asserted bounded. Without
this the measurement instruction becomes a comment.

**Scenarios.**

- The summary returns identity, brief, state, current round, outcome, basis, counts and version in
  one request.
- Recording evidence advances the case version with no lifecycle transition.
- Recording a coverage gap advances the case version.
- A hypothesis stance change advances the case version.
- A conditional read at the current version answers unchanged without assembling sections.
- Each section response carries the case version it represents.
- A section fetched before an update is detectably older than a summary fetched after it.
- Paginated sections return a stable order across pages while the case is growing.
- Evidence is filterable by capability, source and stance.
- Evidence content is bounded on the read path.
- A superseded outcome is readable with its round and time.
- Coverage reports the five states, and a capability not applicable to the stack is not a gap.
- A redacted field appears as a gap with its cause, never as empty content.
- Every claim in an outcome resolves to evidence items that exist in the same case.
- Affected scope statements each carry evidence identifiers.
- Each round exposes its resolved control snapshot, plan snapshot and component versions.
- Requests that returned nothing useful appear with their justifying hypothesis.
- Server-side assembly at a pinned version produces the same content as the summary plus sections at
  that version.
- Two assemblies at the same pinned version are identical.
- The list read carries counts and requires one request regardless of row count.
- A request naming one organization's identity and another's investigation is refused, with both
  organizations on the same placement.
- No read model exposes a secret, a credential digest or an unredacted payload.
- Summary read query count and response size stay bounded on a case with many rounds.

**Prior art.** The intake and relay suites establish the composition-root seam, the tenant-scoped
read shape and the enumerated refusal. The frozen .NET `OpenCluster.Investigations` read stores are
worth reading for the projection shapes they arrived at — conclusion detail with citations,
hypothesis stance rows, audit step lists — and for the two defects its own frontend audit recorded,
which are the reason two of the decisions above exist.

## Out of Scope

- Streaming or server-sent events. Polling with a change token is the decision; streaming is a later
  swap behind the same read models.
- The frontend itself, specified in `oc-frontend/plans/`.
- Export rendering. The read models supply the content; producing HTML and Markdown is frontend work
  against the assembled case file.
- Authentication and the identity model (ADR-006). These reads assume a resolved principal.
- Incident grouping, merging and splitting. `IncidentEpisode` is exposed only far enough that an
  Investigation can name the episode it belongs to.
- Search across investigations beyond the list read's filters.
- Aggregate analytics over investigations.
- Notification of outcomes.

## Further Notes

The single most consequential decision here is the smallest to state: the case version advances on
anything, not on lifecycle transitions. Everything else — cheap polling, stale-section detection,
pinned assembly, identical exports — follows from it, and the alternative has already been built once
in this program and recorded as a defect.

The second is that server-side assembly exists at all. It is tempting to let the browser compose a
case file from the sections it already has, and it works until the first share link renders
differently from the page that produced it. One assembly, three consumers, no divergence — which is
also what gives the scenario harness a scored artifact for free rather than as a separate exporter.
