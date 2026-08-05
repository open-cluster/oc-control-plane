# Spec — The live model provider

Status: IMPLEMENTED. The outstanding requirement — a live run writing a transcript — was closed on
2026-08-05, and the paragraph recording it as outstanding is preserved below with the correction
beneath it rather than deleted, because what it says about the cost of the gap is still the reason
the mechanism exists.
Revision 2 made the slice provider-neutral: the direction of revision 1 was approved, but its
architecture was Anthropic-shaped throughout, which would have made a second vendor a rewrite
rather than a package.

**Built.** `internal/reasoning` implements the investigation-owned boundary against a
provider-neutral contract; `internal/reasoning/anthropic` and `internal/reasoning/zai` are the two
adapters, and a gate fails the build if the domain ever imports either. Vendor and model are
configuration, priced from a declared four-rate table that refuses an unpriced model at startup.
Refusal, outage, rejected request, malformed output, timeout and cost-ceiling reached are distinct
named outcomes and none of them is an abstention. Cross-provider fallback is an explicit configured
chain checking consent per hop and recording what actually answered, including in the transcript
key. A model has now been asked to investigate, five times, against a live cluster.

**WAS outstanding: a live run wrote no transcript.** This specification asks for one per scenario so
that commit CI replays what the model actually said. `OC_MODEL_TRANSCRIPT_FILE` was read only in
replay mode, so a live run recorded nothing, and the replay corpus described here did not exist.
The consequence was not theoretical: three failures observed in the 2026-08-02 sweep cannot be read
after the fact.

**CLOSED 2026-08-05.** `OC_MODEL_TRANSCRIPT_DIR` names where a live round files what the model said,
one document per round, named by the case and the round's ordinal so a reinvestigated case's rounds
sort into the order they ran in. The recorder that existed all along is what makes them; what was
missing was only the wiring, and it is per ROUND rather than per process because a recorder shared
between two concurrent rounds would accumulate both into a transcript that replays as neither.

Four decisions in it are worth stating, because each has an alternative that looks equivalent:

- **A failed round files too.** Filing only on a conclusion would have missed exactly the rounds
  somebody goes looking for. What a recording of a failed round shows is the stage it reached —
  hypotheses and no passes, or passes and no conclusion — which is the question the sweep could not
  answer.
- **A replaying deployment records nothing**, and says so at startup rather than skipping silently.
  Re-recording a recording produces a copy of the file being replayed, and a corpus of this build's
  own echoes is worse than an empty one because it looks like evidence.
- **The recording is keyed on the ROUND's pinned versions**, not on what the build carries when it
  is filed. They are the same today and stop being the same the moment a prompt moves while a round
  is in flight.
- **A directory that cannot be written to is a refusal to START.** A round is the expensive thing in
  this system, and discovering at the end of one that its recording has nowhere to go means the
  money is spent and the answer is already unrecoverable — which is the failure this closes.

The scenario harness files them under `transcripts/` beside `artifacts/` rather than inside it. A
blind scorer is handed the artifact directory and reads what the PRODUCT concluded; handing them the
model's raw turns would be asking them to score the reasoning rather than the result, which is not
what blind scoring measures.
Date: 2026-08-02 (status corrected 2026-08-04)
Repository: the Go control plane
Glossary: `CONTEXT.md`

## Problem Statement

Nothing has ever asked a model to investigate anything.

The model boundary exists as an interface with two implementations: a recorded transcript that
replays what a provider once said, and a stub that reports the provider is unavailable. Neither
has ever called one. Every claim this product makes about explanations — that they are supported,
that abstention happens when it should, that evidence selection is sensible — rests on recordings
that were written by hand rather than produced by a model.

The scenario harness was built to settle exactly those questions and cannot run. It provisions ten
broken clusters, verifies each reached its declared state, and drives a real investigation through
the real control plane and a real Relay; then it needs a transcript per scenario and there are
none, because there is nothing to record from. Hand-writing one would mean scoring the builder's
imagination as though a model had reasoned it, which is the failure blind scoring exists to
prevent. The instrument is finished and the thing it measures is not connected.

This is not a gap that closes by choosing a vendor. The boundary has four properties that a naive
client would break, and each of them was deliberate: a reasoner never names a row, only an ordinal
among what it was shown; it never emits bytes, only typed Arguments the control plane validates; a
provider outage must produce an honest failure rather than a guess; and what a round cost must be
recorded in integers a second run can be compared against.

There is a fifth property that revision 1 missed, and it is why this revision exists. **Which
vendor answers is an operational fact, not an architectural one.** A model is a deployment choice
that changes with price, availability, regional obligation and what a tenant will consent to. An
architecture that names one vendor in its domain types makes every one of those an engineering
project.

## Solution

Three layers, and the vendor appears in exactly one of them.

**The investigation capability keeps the boundary it already owns.** `investigation.Reasoner`, its
three methods, its ordinal-only citations, its typed Arguments and its scope validation are
unchanged by this slice. Nothing in the domain learns that a provider exists.

**`internal/reasoning` is the shared orchestration**, and it depends on `internal/investigation`
rather than the reverse. It owns the prompt, the output schemas, the decoding, the cost
accounting, the recorder and the fallback policy. It talks to vendors through one internal
contract that mentions no vendor.

**`internal/reasoning/<vendor>` is one adapter per provider.** It speaks that vendor's wire
protocol, normalizes what comes back into the shared vocabulary, and declares what it can and
cannot do. Anthropic and Z.AI are built in this slice. A later OpenAI, Gemini or OpenRouter
adapter is a new directory and a configuration entry, and touches neither the domain nor the
orchestration.

Around all of it sits a recorder that implements the same domain interface, delegates every call,
and writes the transcript the harness and commit CI replay. Recording is a decorator rather than a
feature of any provider, so recording a live run is the same code path in the harness as in a
developer's terminal.

## User Stories

1. As the founder, I want the scenario harness to run against a real model, so that I learn
   whether the product works before a design partner does.
2. As the founder, I want each run's cost recorded in integers, so that two runs that cost the same
   report the same number and the feature can be priced.
3. As the founder, I want a cost ceiling that actually stops a round, so that an investigation
   cannot spend without bound.
4. As the founder, I want to change which vendor and which model answers by changing configuration,
   so that a price change or an outage is an operational response rather than a release.
5. As the founder, I want the account I already hold with a second vendor to be usable, so that the
   first live evidence does not depend on one commercial relationship.
6. As an on-call engineer, I want a provider outage to fail the round honestly, so that I am never
   shown a guess produced because the model was unreachable.
7. As an on-call engineer, I want a refusal by the provider's own safeguards to be distinguishable
   from an abstention, so that "the model declined" is never presented as "no explanation was
   supported".
8. As an engineer investigating a security incident, I want a refusal to be survivable, so that the
   one class of incident most likely to trip a safety classifier is not the one the product cannot
   investigate.
9. As an engineer changing the planner, I want the prompt to be a file with a version, so that a
   change to it is a diff I can review rather than a string buried in a function.
10. As an engineer, I want a prompt change to invalidate recordings made against the old prompt, so
    that a replayed test cannot pass against a prompt that no longer exists.
11. As an engineer, I want the rendered prompt for a fixed brief to be byte-stable, so that prompt
    caching works and an accidental change is visible.
12. As an engineer, I want the adapter to be the only component that talks to a provider, so that
    there is one place to look when the answer is wrong.
13. As an engineer, I want a malformed answer retried once against the schema, so that a single bad
    generation does not lose a round.
14. As an engineer, I want a persistently malformed answer to fail the round, so that a retry loop
    cannot spend a budget quietly.
15. As an engineer, I want the round's deadline respected, so that a hung request cannot outlive the
    investigation that asked for it.
16. As an engineer, I want to know what a prompt will cost before sending it where the provider can
    tell me, so that an oversized brief is refused rather than discovered as an error.
17. As an engineer adding a fourth provider, I want to write one directory, so that the domain and
    the orchestration are untouched by the addition.
18. As a platform engineer, I want the API credential supplied by file path, so that it cannot leak
    through a process listing or a diagnostic dump of the environment.
19. As a platform engineer, I want the credential to appear in no log line, no error message and no
    case file, so that the record of an investigation is safe to share.
20. As a platform engineer, I want a deployment with no provider configured to keep working exactly
    as it does today, so that adding this changes nothing for an installation that has not asked
    for it.
21. As a platform engineer, I want prompt caching to be measurable, so that I can tell whether it
    is working rather than assume it.
22. As a platform engineer, I want the cost of a cached round computed from the cache rates, so
    that the cheapest rounds are not reported as the most expensive.
23. As a platform engineer, I want an unpriced model to fail loudly, so that a cost ceiling cannot
    be silently disabled by a model nobody added a rate for.
24. As a platform engineer, I want to know what each provider can and cannot do, so that a
    capability one vendor lacks degrades visibly rather than silently.
25. As a security reviewer, I want evidence text to reach the model as data and never as
    instruction, so that a container's log line cannot redirect the investigation.
26. As a security reviewer, I want the model structurally unable to name a stored row, so that a
    prompt-injected identifier cannot become a citation.
27. As a security reviewer, I want the model structurally unable to emit a capability request this
    control plane would not have validated anyway, so that adaptivity does not widen the execution
    surface.
28. As a security reviewer, I want the adapter unable to reach anything but its own provider's API,
    so that the component holding the credential has the smallest possible blast radius.
29. As a compliance officer, I want a tenant's evidence to leave the platform only when that tenant
    has consented to that vendor, so that a model deployment change cannot become an undisclosed
    subprocessor change.
30. As an engineer running the harness, I want a live run to write a transcript per scenario, so
    that commit CI can replay what the model actually said rather than what someone imagined.
31. As an engineer running the harness, I want a recorded transcript to carry what the run cost, so
    that a replayed round can be compared against the priced run it came from.
32. As a reviewer, I want the adapters' own tests never to call the real API, so that the suite
    stays free, offline and deterministic.

## Implementation Decisions

### The boundary and who owns what

**The investigation-owned `Reasoner` interface is unchanged, and so is every invariant behind
it.** A reasoner cites hypotheses, evidence and gaps by ordinal among what it was shown; it emits
typed `Arguments` and never bytes; a proposed read is validated against the case's scope before
dispatch and re-validated by the Relay on receipt. This slice adds an implementation. It does not
get to renegotiate the contract, and a change to the contract is a different specification.

**`internal/reasoning` depends on `internal/investigation`, never the reverse.** The boundary, its
types and its invariants belong to the capability that defines their meaning; this package is
infrastructure that satisfies it, in the same relationship storage has to the same domain.

**The domain must not be able to name a vendor.** No type in `internal/investigation` mentions
Anthropic, OpenAI, Gemini, Z.AI or OpenRouter, imports an adapter package, or varies by which one
is configured. This is checkable rather than aspirational: a gate asserts the domain's import
graph, so the next person to reach for a vendor type from the domain gets a failing build.

### The internal adapter contract

**One provider-neutral contract, expressed in this system's vocabulary rather than any vendor's.**
An adapter is asked to complete a *deliberation* — a system preamble, a rendered brief, a declared
output schema, an output ceiling, an effort level and a deadline — and answers with a *completion*:
the document, which model actually produced it, the provider's own request identifier, why
generation stopped, and normalized token counts.

**The contract carries no vendor concepts in either direction.** No message-role arrays, no
vendor stop-reason strings, no vendor usage shapes, no beta headers, no SDK types. An adapter
translates in both directions and is the only place either vocabulary is known. What crosses the
boundary is a small, closed set of types this repository defines.

**Cache breakpoints are expressed structurally, not as vendor markers.** The contract describes
which blocks are stable enough to cache; whether and how that becomes a wire-level instruction is
the adapter's problem, and a provider without caching simply ignores it.

### Provider and model as configuration

**Anthropic answers first, on `claude-opus-5`, and neither string is a constant scattered through
the package.** The provider name and the exact model identifier are one configuration value —
a *deployment* — resolved at startup and pinned into the round. No date suffix is appended to the
model identifier: it is complete as written, and a constructed one is a 404.

**A second vendor is built in this slice because the founder already holds the account.** Z.AI's
GLM models are reachable today, and building the second adapter now is what proves the contract is
actually neutral rather than Anthropic's shape with a different name on it. One adapter cannot
demonstrate that, and the cost of discovering it later is a rewrite.

**Which provider answers a given round is never inferred.** It is the configured deployment, and
it is recorded on the round. There is no automatic selection by price, latency or availability in
this slice; a router is a later decision with its own evidence.

### The capability matrix

**Every adapter declares what it can do, and the orchestration reads that declaration rather than
assuming.** The matrix covers strict structured output, token counting, streaming, caching,
refusal detection, provider-side fallback, and regional or zero-retention operation.

**A missing capability degrades visibly.** Where a provider cannot enforce an output schema at the
wire level, the orchestration still validates the document against the same schema and still fails
the round on a persistent violation — the guarantee is preserved, the enforcement point moves, and
the round records which it got. Where a provider cannot count tokens before sending, the
pre-flight size check is skipped and recorded as skipped rather than silently passing. A
capability that is absent is never reported as satisfied.

**The two adapters in this slice differ, and that difference is the point.** Anthropic enforces a
declared JSON schema, counts tokens ahead of a request, and reports both cache-write and cache-read
tokens. Z.AI offers a JSON output mode rather than schema enforcement, and reports cached input
tokens with no separate cache-write figure. Recording that honestly is what stops a cache-write
figure of zero from reading as a cache that stopped working.

### The prompt and the output schemas

**The prompt is a committed file with a version, and the version is this package's to own.** The
prompt version currently sits as a literal in the composition root; it moves here, because the
component that owns the words is the component that must bump the number when they change. The
same applies to the output schema version. Bumping either invalidates every recording made against
the old one through the transcript key that already exists — which is the mechanism working, not a
cost.

**The schemas make the boundary's structural rules unstateable rather than forbidden.** A
hypothesis reference is an integer ordinal; an evidence citation is an integer ordinal; a
capability request is a capability identifier, a version and the typed `Arguments` fields. There is
no field in any schema that accepts an identifier, a query, a selector, a path, a command or free
text that becomes an instruction. A prompt-injected identifier has nowhere to go.

**The schemas are provider-neutral documents.** They are JSON Schema this repository declares once
and every adapter renders into whatever its provider accepts. A vendor-specific schema dialect
would put the output contract inside the adapter, where a second vendor could quietly diverge from
it.

**Evidence text is presented as data, at a boundary, and the prompt says what it is.** It is
untrusted for its whole life: written by software an attacker may control, read by a model. The
containment is structural — whatever the text says, the only thing it can produce is a typed
proposal or a draft the control plane then validates — and the prompt wording is the second layer
rather than the first.

**A golden file pins the rendered prompt for a fixed brief.** Prompt caching is a prefix match: a
byte that changes anywhere in the prefix invalidates everything after it, so an accidental change
is a silent cost regression rather than a visible defect. The golden file makes it a diff.

**Two cache breakpoints, placed where the stability boundaries actually are.** The frozen system
preamble is the first: it is identical across every investigation in every organization. The
rendered brief is the second: it is identical across the calls one round makes, so a round pays for
it once. Nothing volatile — no timestamp, no identifier, no per-request value — may appear before
the last breakpoint, and the ordering is a design rule rather than a preference.

### What is recorded

**Every completion is normalized into one record, whatever answered it.** The record carries the
provider, the requested model, the model that actually answered, the provider's request
identifier, input tokens, output tokens, cache-write tokens, cache-read tokens, reasoning tokens
where the provider reports them, whether a fallback was used, the stop reason, the integer cost,
the latency, the prompt version and the schema version.

**A field a provider does not report is absent, not zero.** Zero is a measurement; absent is the
lack of one, and a cache-read figure of zero means the cache missed while an absent one means the
provider never said. Collapsing them is how a broken cache and an unreported cache look identical.

**The round's pinned model is the model that ANSWERED, not the one that was asked for.** It
matters because of fallbacks, and because a transcript keyed on the requested model would replay
against a recording a different model produced.

**Money stays in integer micro-cents throughout**, as the existing `Spend` type requires. Cost is
computed from four rates — input, output, cache write, cache read — because a cache read is about a
tenth of input and a cache write more than it. Costing a round from input and output alone reports
the cheapest rounds as the most expensive.

**Rates are a declared table keyed by provider and model, and an unpriced model is a refusal.** A
model with no entry must fail loudly rather than report a cost of zero: zero cost silently disables
the cost ceiling, and the failure would first be noticed as a bill.

### Failure is a closed set of named outcomes

**These are told apart because they call for different things, and none of them is an abstention.**

- **Refusal** — the provider's own safeguards declined. A successful response carrying a refusal
  stop reason, not an error. The stop reason is checked before the content is read, because reading
  first is the defect that presents an empty response as a conclusion.
- **Outage** — unreachable, rate-limited past the retry budget, or a provider-side error. Retried
  with backoff, and a failed round when exhausted.
- **Rejected request** — the provider says this request is malformed. A defect in this build, and
  it must not be disguised as an outage or the wrong person is paged.
- **Malformed output** — the document did not satisfy the schema. Retried exactly once, then a
  failed round, because an unbounded retry loop spends a budget quietly.
- **Timeout** — the deadline was reached. The per-request bound sits inside the round's deadline,
  chosen so the bound multiplied by the attempts allowed still fits.
- **Cost ceiling reached** — a spending limit was reached. Refused before a request is sent. The
  word is the glossary's: a budget is something you spend down and a limit is something you may
  not cross, and this is the second.
- **Investigation abstention** — a finding about the evidence, produced by a model that answered
  correctly. It is not a failure and never shares a code path with one.

**A refusal is a failed round and never an abstention.** An abstention says no explanation was
supported; a refusal says nobody looked. Conflating them would report the first when the second is
true, which is the single most damaging thing this component could do.

**Every terminal outcome above still fails the round honestly.** They are distinguishable in
telemetry, audit records and logs, and they all produce a failed round with the reasoning step
named as the gap — never a conclusion, never a guess.

### Fallback is explicit configuration

**Cross-model and cross-provider fallback is a configured, ordered chain, or it is absent.** There
is no implicit substitution. An operator who configured one deployment gets exactly that deployment
and an honest failure.

**A vendor is never switched silently.** Crossing a provider boundary is permitted only when the
configuration says so for that tenant, and every hop is recorded: what was tried, why it gave way,
and what answered. Consent is checked per hop rather than once at the head of the chain, because a
tenant who consented to one vendor has not thereby consented to the next.

**Fallback is attempted only for outcomes another deployment could plausibly answer** — a refusal,
an outage, a timeout. A rejected request is this build's defect and would be rejected identically
by every hop; a malformed output is retried within the deployment that produced it.

**Provider-side fallback is a capability, not a default.** Where a vendor can re-serve a declined
request internally, that is declared in the matrix and recorded when it fires — and the answering
model is read from the response rather than assumed either way. The Anthropic Go SDK pinned by this
build does not expose the server-side fallback parameter, so in this slice every fallback is the
configured chain above, which is the honest position regardless.

## Production Requirements

These are the conditions under which this component may carry a real tenant's evidence. They are
part of the specification rather than a hardening backlog, because each one is cheaper to build now
than to retrofit around a live integration.

**Evidence redaction.** Evidence reaching a provider has already passed the Relay's redaction
enforcement point, and what was masked is already recorded as a coverage gap. This component adds
the last mile: a bounded per-item and per-request size, and no evidence content in any log, span,
metric or error. What travels to a vendor is the same text the case already holds — never more.

**Tenant consent.** Evidence may reach a vendor only where that vendor is recorded as consented
to. Absent a recorded consent the round fails rather than defaulting to permitted, and consent is
per provider rather than per feature, because the question a customer answered was about a
subprocessor. Consent is checked when a deployment is selected and again at every fallback hop,
because consenting to one vendor is not consenting to the next.

**Consent is deployment-wide in this slice, and that is a stated limitation rather than a
finished feature.** Per-tenant consent needs the organization at the point the model is called,
and the model boundary deliberately carries no organization — this repository refuses ambient
tenancy, so the identifier is passed explicitly or not at all, and the boundary's three methods
pass evidence rather than tenants. Reaching per-tenant consent therefore means widening the
domain interface, which this slice may not do. What is built is the enforcement point and the
per-hop check against a configured permitted set; what is not built is resolving that set per
organization. A single-tenant deployment is fully covered by this; a shared deployment serving
organizations with different subprocessor agreements is not, and must not be pointed at a live
provider until the interface question is settled.

**Bring-your-own-key and rotation.** A credential is read from a file path and never from an
environment value. A tenant may supply their own, in which case theirs is used for their rounds and
the platform's is not a fallback. Rotation is a file change picked up without a restart, and the
credential is held in memory, never logged, never placed in an error, never written to a case file.

**Egress restriction.** An adapter reaches its own provider's API host and nothing else. The
allowed host is derived from the configured deployment rather than from anything a response
contains, so a redirect cannot move where the credential is sent.

**Concurrency and rate limiting.** Concurrent in-flight requests are bounded per provider and per
tenant, so one investigation cannot consume the capacity of every other. Provider rate-limit
responses are honoured with backoff rather than retried immediately.

**Circuit breaking.** Repeated provider failure opens a breaker that fails rounds immediately with
the outage outcome instead of queueing behind a dead dependency, and closes again on a probe. A
breaker that is open is visible, because a silently open breaker looks exactly like a vendor that
has stopped being called.

**Telemetry and audit.** Every completion emits the normalized record as telemetry and, for a
tenant-attributable round, an audit entry: which tenant, which provider, which model, what it cost,
what stopped it. Identifiers, counts and outcomes travel; evidence content does not.

**Model deprecation.** A model identifier is configuration, and a deployment naming one that no
longer exists must fail at startup with the name in the message rather than at 03:00 on the first
round. An unpriced model is refused by the same mechanism.

**Tenant and round cost ceilings.** A round is bounded by the existing execution limit. Above it
sits a ceiling across rounds, checked before a request is sent, producing the cost-ceiling outcome
rather than a partial investigation.

The cross-round ceiling is a CHECK rather than a reservation, and the difference is stated because
it bounds what the ceiling promises: nothing is held between the check answering and the cost being
recorded, so calls already in flight can carry the total past the limit by up to what they
collectively cost. Concurrency per deployment is bounded separately, which bounds the overshoot.
What the ceiling guarantees is that no request is sent once the limit is known to have been
reached — which is what stops a runaway, not what makes the figure exact. It is also deployment-wide
rather than per tenant, for the same reason consent is.

## Testing Decisions

**What makes a good test here.** It asserts what this package sends and what it does with what
comes back — not whether the model's answer was any good. Whether the explanations are worth
reading is the scenario harness's question, answered by humans against a live provider, and no
assertion in this suite may pretend to answer it.

**The seam is the HTTP round-tripper, and the suite never reaches the network.** Every adapter is
constructed against a transport the test supplies, so every case is a canned response. This is the
one component in this program that is faked in CI and the deviation is already recorded at the
boundary; these packages are where that fake now lives, and their own tests are the reason it is
allowed. A test that called a real API would be non-deterministic, priced and offline-hostile.

**Scenarios, orchestration.**

- A well-formed answer for each of the three methods becomes the typed value the interface returns,
  with hypotheses, proposals, weighings, settlings and a draft carried intact.
- An answer citing an ordinal outside what was shown is refused before anything is returned.
- No schema admits an identifier, a query, a selector, a path or a command; the assertion is over
  the schemas themselves, so a field added later fails it.
- A schema-invalid answer is retried once; a second invalid answer fails the round, and the request
  count proves the retry was bounded.
- Cost is computed from all four rates, and a round served largely from cache costs less than the
  same round served cold.
- A model with no rate entry is refused rather than costed at zero.
- The rendered prompt for a fixed brief matches its golden file, and the prompt version is part of
  what the golden covers.
- Nothing volatile appears before the last cache breakpoint, asserted over the rendered prefix.
- The credential appears in no log line, no returned error and no rendered prompt.
- A cancelled context stops the call and returns promptly.
- The recorder produces a transcript the existing replay accepts, and one made under a different
  prompt version is refused by the existing key check.
- The model named in a round's pinned versions is the model the response says answered, including
  when a fallback answered it.
- A configured fallback chain crosses to the next deployment on a refusal, an outage and a timeout,
  and does not on a rejected request.
- Crossing a provider boundary without recorded consent fails rather than falling back.
- Every failure outcome is distinguishable from every other, and none of them is an abstention.

**Scenarios, per adapter.**

- The declared outcome for each of that provider's failure shapes: its refusal signal, its rate
  limiting, its rejected-request status, its server errors.
- Rate limiting followed by success returns the answer; rate limiting throughout becomes an outage.
- Usage normalization from that provider's own usage shape, including a field it does not report
  arriving as absent rather than zero.
- The capability matrix it declares matches what its code actually does, asserted rather than
  documented.

**What these packages are not tested for.** Whether the prompt elicits good reasoning. Whether the
effort level is right. Whether the schemas ask for the right things. All three are empirical, none
is knowable from a unit test, and the instrument that answers them already exists.

**Unit tests passing is not the bar for calling this done.** The deliverable includes exactly one
live red-herring scenario against a real provider, producing the real transcript, human scoring,
provider and model attribution, the token and cost breakdown, latency, cache effectiveness, every
refusal, retry and fallback, the investigation result, and any contract or prompt failures. Until
that exists, this component is unproven no matter how green the suite is.

## Out of Scope

- A provider router that chooses a vendor per round by price, latency or load. The contract makes
  one possible; choosing between vendors automatically before either has answered a scenario is a
  policy with no evidence behind it.
- Streaming partial reasoning to a user. The round is a background job whose result is a durable
  case; there is no reader waiting on a token.
- An agentic tool-use loop, in which the model calls capabilities directly. The investigator owns
  the loop, and every proposed read is validated against the case's scope before dispatch — a model
  driving the tools would remove exactly that step. This is not a performance question.
- Server-managed agents, hosted sandboxes and multi-agent orchestration. All of them move the loop
  to the provider, which is the same objection.
- Fine-tuning, embeddings and retrieval over past investigations.
- Prompt experimentation infrastructure — A/B assignment, per-tenant prompts, automatic prompt
  optimisation. The prompt is one committed artifact with one version until there is evidence that
  a second is needed.
- Per-provider prompt variants. One prompt, one schema set, every vendor. A variant per vendor
  would make cross-provider comparison meaningless, which is half of why a second adapter exists.
- Automatic effort tuning. Effort is configuration; which value is right is what the harness
  measures.
- Model-graded scoring of an investigation's output. A model judging a model's output on "was this
  supported" is the same failure mode being measured, and produces a number with no information in
  it.

## Further Notes

The order of work matters more here than usual. The provider should be pointed at one scenario
before it is pointed at ten — the red-herring scenario, which is where a change-aware investigator
most plausibly fails, and where a wrong answer is most informative. Ten scenarios against an
unproven prompt spends money to learn one thing badly.

The first live run will produce transcripts, and those transcripts immediately become the commit
CI's replay corpus. That is the moment the deviation recorded at the model boundary stops being a
hand-written approximation and starts being a recording of something that actually happened, which
is worth more than any assertion this specification could add.

There is one honest caveat about cost that should be stated before anyone is surprised by it. A
1M-token context window and adaptive thinking mean the cost of a round is bounded by the controls
rather than by the shape of the problem, and the default controls were chosen when every round was
free. The first priced runs are the evidence for what those numbers should be; until they exist,
the cost ceiling is a guess with an integer in it.

The second adapter earns its place in this slice for one reason worth stating plainly: a
provider-neutral contract with one implementation is an assertion, and a provider-neutral contract
with two is a demonstration. The two chosen differ in the ways that matter — one enforces output
schemas and the other does not, one reports cache writes and the other does not — so the contract
is tested against real disagreement rather than against a second vendor that happens to look like
the first.
