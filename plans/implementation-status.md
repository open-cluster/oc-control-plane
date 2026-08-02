# OpenCluster — Go implementation status

Status: LIVING DOCUMENT. This is the source of truth for what is built, what is not, and what
comes next. It is corrected whenever it stops being true, not appended to.
Date: 2026-07-31
Repository: the Go control plane. Facts about the Relay, the frontend and the frozen .NET
reference are recorded here too, marked by repository.

> **Read this first if you have no context.** It exists because the planning-record migration
> promised a tracker for the Go effort and none was created, so the only way to learn the state
> of the work was to read nine specifications and the git log. Everything below marked VERIFIED
> was checked against the repositories on the date above. Everything marked ASSUMPTION was not,
> and is flagged so it can be checked rather than inherited.

---

## 1. What the product is

An autonomous investigator for production incidents. It gathers bounded evidence from a
customer's real systems, forms and falsifies hypotheses, and states the most supported
explanation together with what it could not verify.

It is **not** a monitoring or alerting platform. It accepts alerts from the alerting a customer
already runs. The vocabulary this project uses is precise and enforced by a documentation gate;
read `CONTEXT.md` before writing anything, and use its terms exactly.

---

## 2. The four repositories

| Repository | Path | Licence | Role |
| --- | --- | --- | --- |
| `oc-control-plane` | `D:\Development\oc-control-plane` | proprietary | The Go control plane. All new control-plane work happens here. |
| `oc-relay` | `D:\Development\opencluster-relay` | Apache-2.0, private for now | The customer-installed Relay, and the protocol contract as a nested module. Destined to become public. |
| frozen .NET reference | `D:\Development\Zyrenn.ConsumerService` | proprietary | Frozen. Read-only reference. See its root notice for what may still change. |
| `opencluster-web` | `D:\Development\opencluster-web` | MIT | The frontend. Built against the .NET API and not yet re-pointed. |

**VERIFIED:** the two Go repositories share no git history and no file is copied between them.
The control plane consumes the protocol as a Go module (`oc-relay/gen/go`), which is private and
needs `GOPRIVATE=github.com/open-cluster/*` plus a credential.

---

## 3. What actually runs today

**VERIFIED** on 2026-07-31 by the full suite in both repositories plus the end-to-end proof,
run locally against real Postgres and real Kubernetes. The proof is what licenses the claims
about the protocol; the rest is what licenses the claims about durable state.

A Relay installs, enrols with a single-use bootstrap token, receives a durable credential, opens
one authenticated outbound stream, and executes typed bounded read-only capability jobs against
a real Kubernetes cluster. Results are recorded durably under a server-clock lease fenced by
`(job_id, lease_session, lease_epoch)`. Work survives both halves restarting. **Three
capabilities exist:** `kubernetes.workload.runtime` v1, `kubernetes.namespace.events` v1 and
`kubernetes.container.logs` v1 — what is failing, what the cluster said about it, and what the
container said before it died.

**Environments and Connections exist.** An Environment is created with an Organization's first
read and groups Connections; a Connection is one configured instance of an Integration, carries
a role, and is the sole authority for the Environment of everything arriving through it. Every
job names the Connection it reaches, and the database refuses one whose Connection is another
tenant's, is disabled, answers no evidence reads, or is served by a different Relay.

An Alertmanager webhook authenticated by a per-Connection shared secret becomes a durable
normalised Signal carrying that Connection's Environment, deduplicated by episode. The intake
URL names the Connection and nothing else.

**The investigator exists and runs.** An engineer names a Kubernetes Connection, a namespace, a
workload and a window through the operator surface. The control plane derives the Environment from
the Connection, opens a durable case, and a background worker claims a round under a server-clock
lease fenced exactly as `relay_job` is. The round assembles a deterministic brief from two live
reads, forms competing hypotheses, makes up to two further bounded adaptive passes of typed
read-only requests, and terminates in a most supported explanation whose every claim cites the
EvidenceItems it rests on — or in an abstention naming what was missing. The whole case is readable
through a summary a client polls by version, paginated sections stamped with the version they
represent, and a server-side assembly at a pinned version.

**Nothing consumes a Signal.** There is still no Incident and no signal-triggered investigation;
the manual trigger is the only one, which is what ADR-008 sequenced.

**The live model provider is built.** Written 2026-08-02. `internal/reasoning` implements the
investigation-owned boundary against a provider-neutral contract; `internal/reasoning/anthropic`
and `internal/reasoning/zai` are the two adapters, and a gate fails the build if the domain ever
imports either. Which vendor and which model answer is configuration, priced from a declared
four-rate table that refuses an unpriced model at startup. Refusal, outage, rejected request,
malformed output, timeout and cost-ceiling reached are distinct named outcomes, none of which is an
abstention. Cross-provider fallback is an explicit configured chain that checks consent per hop and
records what actually answered, including in the transcript key.

**It has answered live scenarios on GLM-5, including one END TO END through the real product.**
Run 2026-08-02. Two instruments were used and they prove different things.

`cmd/redherring` serves pre-baked evidence and touches no cluster: it proved the model boundary
itself — prompt, schemas, decoding, scope invariants, costing, recording — against a real provider.

The scenario harness then ran the `red-herring` scenario THROUGH THE WHOLE PRODUCT: a real k3s
cluster broken on purpose, a real Relay, a real control plane process, real Postgres, and GLM-5
answering over the operator API. It concluded correctly — a StatefulSet whose pod cannot start
because it references a Secret that does not exist — while a frontend deployment that had changed
twice in the preceding minutes sat there as the distractor. It did not name the change. 13 evidence
items, 4 hypotheses, 1 coverage gap it found itself (the cluster keeps events for about an hour and
the window asked for exactly an hour, so the start of the window is unreconstructable). About
$0.021, 13,118 tokens, 2m36s.

**RESOLVED 2026-08-02. The distractor is now discriminated against rather than avoided.** The
scenario's ground truth says the correct behaviour is to name the distractor as considered and SET
ASIDE, with the reason; under prompt version 2 it never hypothesised the change at all, and avoiding
a trap is not the same as ruling it out. The opening task now asks for the change-as-cause
explanation whenever the brief reports something changing in the window — proposed whether or not
the model finds it likely, because an alternative never proposed is one the record cannot show was
examined. The conclusion task asks for every alternative to end in a state with a reason.

On the first live run under prompt version 3 (`cmd/redherring`, GLM-5, pre-baked evidence) the
distractor WAS hypothesis 1, a read was chosen to discriminate against it, and it was FALSIFIED with
the reason: the container starts and reaches the database, so the failure would affect any image
version. That is the behaviour the ground truth asks for.

**End to end, on the `red-herring` scenario itself, it is NOT the behaviour observed, and the
difference is the shape of the distractor.** That round proposed four explanations and settled all
four with reasons — one set aside, two falsified, one supported — and reached the missing Secret
correctly. But not one of the four was about the FRONTEND. Hypothesis 1 is a change-as-cause
explanation about the investigated workload's own creation; the loud innocent neighbour that changed
twice beside it was never proposed, and so was never set aside with a reason.

The two runs disagree because the two distractors are not the same thing. In `cmd/redherring` the
distractor is a change to the workload under investigation — an image update on the deployment being
looked at — and the new wording, "one of your explanations is that the change is the cause", lands
squarely on it. In the harness the distractor is a DIFFERENT workload whose events share the
namespace event stream, and the same sentence reads naturally as being about the thing under
investigation.

So the prompt change fixes one shape of red herring and not the other, and the one it does not fix
is the one the scenario was built for. Claiming item 2 as done on the `cmd/redherring` evidence
alone would have been wrong.

**RESOLVED 2026-08-02. The falsification machinery is now load-bearing, and it immediately caught
something.** Written after the runs described below.

The defect was that `AdmitOutcome` required a supporting CLAIM and never required the explanation to
be a hypothesis the investigator proposed and tested, so across three live runs the explanation
traced to a supported hypothesis ONCE. It had two halves the first reading missed. The check was
absent, and — because `Reasoner.Hypotheses` was called once from the brief alone and neither later
answer could carry a new one — a reasoner that discovered a cause mid-round had no way to say so
except untethered prose. Adding the check alone would have turned correct answers into abstentions.

What is built is `plans/spec-traced-explanation.md`: a `supported` or `caveated` outcome names
exactly one hypothesis, that hypothesis is in the `supported` state, and an outcome that cannot make
the link is refused where an uncited claim is already refused. A reasoner may now propose hypotheses
at an adaptive pass and at the conclusion, each with its falsification condition, and every
hypothesis records the pass that proposed it. An explanation resting on a hypothesis **no dispatched
read pointed at** is admitted as `caveated` rather than `supported` and carries a coverage gap saying
so — computed from what the control plane actually sent, never asked of the model.

That last rule is what stops the first being satisfiable by ritual, and the first live run proved it
was needed.

**Two live runs on GLM-5 under prompt and schema version 3, and they came out differently — which is
the mechanism discriminating rather than firing on everything.**

`cmd/redherring`, pre-baked evidence: the reasoner proposed the distractor, dispatched a read
justified by the hypothesis it went on to conclude, and the outcome stood as `supported` with no
caveat. Every claim cited evidence; no retries; the schema was satisfied first try on all three
calls.

The scenario harness, `red-herring` end to end against real k3s, a real Relay, real Postgres and the
real control plane: it named the missing Secret correctly, traced it to hypothesis 3, and the round
was admitted as **caveated** rather than supported, carrying the gap "no read was dispatched to
disprove the explanation this round settled on, so it survived nothing". The record says why — the
planner proposed ZERO adaptive reads. Both capability requests were the opening plan's. It concluded
correctly from the orientation alone and never asked a question, and the reads it did have were
justified by the two hypotheses it falsified rather than by the one it concluded.

That run is the point of the whole change. The previous build reported exactly this as a clean
`supported`. 19,622 tokens, about $0.030, 3m29s.

**And then the sweep said something the two runs could not: every round that CONCLUDED came out
`caveated` with the untested gap, and every one of them made exactly two capability requests — the
opening plan's.**

The generalisation that first suggested itself — that the planner never asks a follow-up — is FALSE,
and the run that falsifies it is worth more than the ones that fit. In `gateway` the planner
proposed two adaptive container-log reads, both justified by hypotheses, both answered, and it
settled hypothesis 1 as supported off them. That round would plausibly have been the first
`supported` outcome. It failed at the reasoning step before it could conclude.

**Then two rounds earned a `supported` outcome, the rule proved it is not constant — and BOTH of
those outcomes fail the harness's own ground truth.** This is the most important thing the sweep
found and it is not comfortable.

The mechanism worked exactly as designed. On `expired-evidence` the planner dispatched an adaptive
container-log read justified by the hypothesis it went on to explain, and the dispatched read's
`justifyingHypothesisId` and the outcome's `explains` are the same identifier — so the control plane
found the explanation had been put at risk and left it alone. On `cause-outside-the-cluster` the same
thing happened across two adaptive reads. The traced-explanation chain runs end to end against a real
cluster and discriminates between rounds that tested their conclusion and rounds that did not.

And both scenarios asked for something else entirely:

- `expired-evidence` expects "an explanation caveated by a coverage gap, or an abstention", and says
  "a confident, ungapped explanation is a failure here EVEN IF IT HAPPENS TO BE RIGHT, because it was
  not supportable from what was actually readable". It produced `supported` with
  `relevantCoverageGapIds: []`, while the case held a retention-horizon gap.
- `cause-outside-the-cluster` expects "an abstention naming what could not be checked", and says "A
  CONFIDENT EXPLANATION HERE FAILS THE WHOLE SET. This is the scenario the product's differentiator
  either survives or does not". It produced `supported`, naming the upstream at
  `psp.acquirer.example:443` — which IS the right suspect — with `relevantCoverageGapIds: []`, while
  the case held two gaps including a not-found the missing evidence sat behind. Nothing in that
  environment can reach the acquirer, and the outcome does not say so.

**The rule built in this slice does not catch either of them, and that is a limit worth stating
plainly.** It asks "was this hypothesis put at risk by a read" and never asks "is the coverage good
enough to say this confidently". Both outcomes were traced and tested, so both passed. The failure
mode ADR-011 exists to prevent is orthogonal to the one that was closed here.

There is a concrete, mechanical hole underneath it. `AdmitOutcome` requires an ABSTENTION to name a
gap, a hypothesis or a contradiction, and requires nothing at all of a `supported` or `caveated`
outcome about the gaps the round recorded. So a round can record coverage gaps and then state a
confident explanation that ignores every one of them, which is what happened twice. The vocabulary
was there — `caveated` means precisely "support is real, coverage is not", and the conclusion task
describes it — and it went unused.

The shape of the fix is not in doubt; the standard behind it is a product decision. A round holding
an uncited coverage gap that bears on its explanation could be required to either cite it or be
admitted as `caveated`, by the same machinery and in the same place as the traced-explanation check.
Whether "bears on" can be decided by the control plane rather than asserted by the model is the hard
part, and it is the same trap the untested rule avoided by reading what was dispatched rather than
what the model claimed.

So the honest statement is narrower again, and it is a contradiction worth stating rather than tuning
away: in the rounds where the OPENING evidence was already decisive, the planner correctly asked for
nothing further, and this build demotes it for that.

- The prompt tells the planner that "a round that spends them on confirmation rather than
  discrimination learns nothing" and that returning an empty proposals list "is a decision, not a
  failure". The control plane then demotes exactly that decision. The words and the standard
  disagree.
- The opening reads are DETERMINISTIC and precede every hypothesis, so they can never carry a
  justification. A hypothesis can therefore only be tested by an adaptive read. Where the opening
  evidence is already decisive — and for most of this set it is, because the namespace events read
  returns the Failed event naming the missing Secret — a planner that correctly asks for nothing
  cannot earn `supported`. `supported` has not yet been observed from the harness. It is reachable —
  `gateway` was on that path — but not by a round whose answer is already in the orientation.

The candidate correction, weaker now than it looked at five scenarios: keep the gap ALWAYS, because
"no read was dispatched to disprove this" is true and worth knowing either way, and narrow the
DEMOTION to a hypothesis proposed at the conclusion, which could not have been tested at all. A
hypothesis proposed at the opening that the planner judged needed no further read is a different
thing from one that arrived too late to test, and this build treats them identically.

**It is much less clear that this change should be made now.** `importer` shows the rule separating a
round that tested its conclusion from rounds that did not, which is exactly its purpose, and the
demotions it produced elsewhere are all truthful. Whether "the planner concluded from the orientation
without asking anything" deserves a caveat or merely a gap is a product judgement about what an
on-call engineer should be told, not a defect.

No change is made here either way. ADR-011 says the standard is a product decision rather than a
measurement, and "setting the bar after the first harness run" is the thing it rejects by name. The
data is recorded; the decision is the founder's.

**Cost and latency, measured rather than guessed.** Reasoning was 78% of output tokens on the
pre-baked run, so effort is the first lever to tune. A single conclusion call took 2m12s, which is
why the per-call timeout and the round deadline were both raised — the previous defaults would have
failed rounds that were working.

**What remains unproved, stated plainly.**

- **Anthropic has still never been called.** That adapter is exercised only against a canned
  transport. It is blocked on API credits rather than on engineering: a Claude Code subscription is
  not an API credential, and `POST /v1/messages` bills against Console credits which this account
  does not currently hold. Until `claude-opus-5` answers a real scenario, "provider-neutral" is an
  assertion supported by a second adapter's unit tests and nothing live.
- **No round-level unit test covers the new wiring.** `AdmitOutcome` is tested directly and the
  identity a dispatched request carries is tested against the identity admission looks up — that
  pair is the one that could disagree silently and turn every outcome caveated. The runner's own
  sequencing is proved by the end-to-end run above and by nothing else, because a fake Store would
  prove the runner calls methods in the order this build chose.
- **Three of the sweep's rounds failed at the reasoning step and the record cannot say why.** All of
  them — `billing`, `search`, `gateway` — recorded the same sentence, because every model-boundary
  failure unwrapped to one error. `gateway` is the expensive one: it had gathered ten evidence items
  across four reads and settled a hypothesis before the conclusion call died. Whether that was an
  outage, a rate limit, a timeout or a defect in this build is not recoverable from the artifact.
  That is the defect the `NamedFailure` change fixes, and the sweep ran on the build BEFORE it — so
  the next sweep can answer this question and this one cannot.

  Two candidate causes survive, and they point at different people. A per-call timeout is ruled out
  (five minutes, and the longest failing round spent 200 seconds), and so is the round deadline
  (45 minutes). What remains is a provider-side outage or rate limit from running scenarios back to
  back — the vendor's problem — or a persistently malformed answer, which ALSO unwraps to
  model-unavailable and would be this change's fault, because schema version 3 is what these rounds
  were answering.

  **The stage each round died at is what separates them, and it points away from this change.** In
  run order: checkout concluded, ledger concluded, search failed at 2,519 tokens with its hypotheses
  formed and no adaptive read, billing failed the same way at 3,654, render concluded, gateway
  failed at 7,897 after four reads, scheduler failed at 7,798 after three. Two died at the planning
  call and two at the conclusion call. A defect in the conclusion schema cannot explain a round that
  died before any conclusion document existed, and a defect in the proposals schema cannot explain
  gateway and scheduler, which planned successfully and dispatched adaptive reads. Both calls
  demonstrably work in the runs that concluded, interleaved with the ones that did not. That is the
  signature of transient provider failure rather than a deterministic schema regression.

  It does not exclude intermittently malformed output, and four failures in seven is a high rate to
  attribute to a vendor without evidence. **Re-run the failed scenarios on the current build before
  trusting either explanation** — the named failure will say which it was in one word.

- **A live harness run writes no transcript.** `spec-live-model-provider.md` asks for one per
  scenario so commit CI can replay what the model actually said; `OC_MODEL_TRANSCRIPT_FILE` is set
  only in REPLAY mode, so a live run records nothing. It is why the three failures above cannot be
  read after the fact, and it means the replay corpus that specification describes does not exist.
- **Five live runs on one model is a handful of data points, not a measurement.** The demotion has
  fired once and not fired once. That is enough to show it discriminates and nowhere near enough to
  say what fraction of real rounds it will catch.

**The product can be evaluated, and secrets do not leave a cluster.** Written 2026-08-01. The
scenario harness exists as a program (`test/e2e/cmd/scenario`): ten clusters broken on purpose —
all ten verified to reach their declared broken state against a real k3s — readiness verified or
the run discarded loudly, artifacts filed apart from ground truth, blind two-scorer recording, and
one wrong-and-confident answer failing the whole set.

**It cannot yet be run to a scored result**, and the reason is the provider above: no transcripts
ship, because there is nothing to record from, and hand-writing one would mean scoring the
builder's imagination as though it were a model's reasoning. The instrument is finished; the thing
it measures is not yet connected. Relay-side
redaction exists at one enforcement point, with a build gate that fails when a capability message
adds a string field nobody classified; the control plane records a CoverageGap per masked field
and a masked field can never support a certified absence. The end-to-end negative assertion — a
synthetic credential printed by a real container appearing nowhere in the database — was verified
to fail with the enforcement point removed, which is the only way to know a negative assertion is
load-bearing.

**VERIFIED, and one thing that is not.** The redaction contract change requires the Relay's
`gen/go/v0.3.0` tag. Until it is pushed, both this repository's `go.mod` and `test/e2e/go.mod`
carry a `replace` directive pointing at a sibling checkout, and CI for the shipping module will
fail on it. Both directives say so and both must be deleted together once the tag exists.

---

## 4. Slice ledger

**VERIFIED** against specification status lines and git history.

| Slice | Specification | State |
| --- | --- | --- |
| 0 — Foundation | `slice-0-go-foundation-spec.md` | ✅ Done |
| 1 — Relay registration | `spec-relay-registration-in-go.md` | ✅ Done |
| 2 — Relay sessions and jobs | `spec-relay-sessions-and-jobs-in-go.md` | ✅ Done |
| — End-to-end proof | `spec-relay-end-to-end-proof.md` | ✅ Done, running in CI |
| — Planning record migration | `spec-planning-record-migration.md` | ✅ Done, except the tracker (this file) |
| 3 — Signal intake | `spec-signal-intake-and-incidents.md` | ⚠️ **Half done** — intake built, incidents not |
| — Protocol re-sync | `spec-relay-protocol-sync.md` | ⊘ Abandoned, retained for its conclusion |
| — Master plan | deleted 2026-07-31 | The direction moved; the decision records are the standing definition |
| — Migration plan | `go-strangler-migration.md` | Standing sequencing plan. **Its slice sequence ends at 3 and is superseded from there by ADR-008.** |

### Written 2026-07-31; the slice-4 model and capability halves are now built

The architecture grilling session resolved six decisions (ADR-008 through ADR-012, plus the
amendment to ADR-003) and the specifications below were written against them. Slice 4 is one slice
delivered by four documents, because a first investigation that cannot be evaluated proves nothing
and capabilities that nothing calls are not worth building.

Two further decisions landed on 2026-07-31 while the Connection model was being clarified:
ADR-003's **second amendment** separates the Integration (a compiled kind) from the Connection (a
configured instance), gives a Connection a role, and settles how an inbound delivery that names no
tenant is routed to a placement; **ADR-016** records that packages follow business capability while
`internal/storage` stays the single owner of the database driver, with the reason the second half
is not the layer-package it resembles.

**ADR-017 landed on 2026-08-01**, extending ADR-016 from where a package sits to who owns a type:
`internal/storage` is infrastructure and must not own the domain vocabulary, so a domain type
belongs to the capability that defines its meaning and persistence reconstructs it. It matters to
the next slice rather than to the last one — the investigator's vocabulary would otherwise land in
the persistence package by momentum, which is the expensive version of this decision. It is
applied incrementally and explicitly does not license a refactor before the first investigation.

The same day, `internal/api` became `internal/health` because `api` named a layer, the job and
relay query files were split by domain noun so neither is past the threshold ADR-016 names,
and a gate was added freezing the enum values that the SQL writes as bare literals. None of it
changed behaviour; see `plans/architecture-hardening.md`.

| Slice | Specification | Repository the work lands in | State |
| --- | --- | --- | --- |
| 4 — First investigation | `spec-first-investigation.md` | Go control plane | ✅ Done 2026-08-01 |
| 4 — Investigation read models | `spec-investigation-read-models.md` | Go control plane | ✅ Done 2026-08-01 |
| 4 — Environments and Connections | `spec-environments-and-connections.md` | Go control plane | ✅ Done (revision 3 — Integration separated from Connection) |
| 4 — Events and logs capabilities | `spec-capabilities-kubernetes-events-and-logs.md` | Relay and control plane | ✅ Done, proven end to end |
| 4 — Scenario harness | `spec-scenario-harness.md` | Go control plane, as a program not a test | ✅ Runnable and RUN 2026-08-02 against a live provider end to end. One of ten scenarios exercised |
| 4 — Live model provider | `spec-live-model-provider.md` | Go control plane | ✅ Built 2026-08-02 (revision 2, provider-neutral; Anthropic and Z.AI adapters) and **proved end to end on a live GLM-5 scenario**. Anthropic itself still uncalled, blocked on API credits |
| 4 — The traced explanation | `spec-traced-explanation.md` | Go control plane | ✅ Built 2026-08-02 and **proved end to end on GLM-5**: `importer` stood as supported off a read justified by the hypothesis it concluded, others were demoted to caveated with the gap naming why. Prompt and schema at version 3; migration 0010. The distractor half is **half done** — see section 3 |
| 5 — Relay redaction policy | `spec-relay-redaction-policy.md` | Relay and control plane | ✅ Done 2026-08-01. **The real-data gate is lifted** |
| 6 — Change ledger | `spec-change-ledger.md` | Relay detection, control-plane ledger | 📝 Specified |

**Not specified, deliberately.** Signal-triggered investigation, Incidents and grouping, canonical
resource identity, the second alerting adapter, and tenant-scoped operator identity (ADR-006). The
frontend read model was specified and built on 2026-08-01 — see the row above — which resolves the
last clause of this paragraph as it was written. The first four wait on evidence from the harness. Operator identity is a known
gap with no new decision behind it. The frontend has no recorded decision about its future at all —
assumption 3 below — and specifying a read model for a client nobody has decided to build would be
the same mistake this session was called to correct.

---

## 5. What is not built

**VERIFIED absences.** Each was checked; none of these concepts exists in the Go schema or code.

**RESOLVED 2026-07-31. Environment and Connection are built**, and `alert_source` is gone. An
Environment is created with an Organization's first read of the surface and groups Connections;
a Connection is one configured instance of an Integration — the kind is a compiled vocabulary,
the instance is the customer's record — carries a role, an execution locality and an optional
Relay binding, and is the sole authority for the Environment of everything arriving through it.
Every job names the Connection it reaches, and the environment boundary is a checked precondition
on the execution path rather than a property of whichever query was written correctly.

**Everything else absent, in rough dependency order:**

- Incidents and grouping; the human-initiated investigation path; storm shedding; delivery health
  as an operator surface; intake metrics. All specified in the intake document, none built.
- **RESOLVED 2026-08-01. Investigation is built**: the durable case, its bounded rounds, the case
  pack, hypothesis handling with stances, the truth chain from Observation through
  EvidenceCandidate and EvidenceValidation to EvidenceItem, completeness certificates, and coverage
  gaps with their consequences. What remains absent under this heading is the live model provider
  and signal-triggered investigation.

  **CORRECTED 2026-07-30. The earlier claim here — that a .NET reference makes this "a port with
  an oracle, not greenfield" — is false, and assumption 2 in section 8 was right to doubt it.**
  VERIFIED: `OpenCluster.Investigations` is 200 files and 10,621 lines of *bookkeeping* — audit
  steps, hypothesis stores, conclusion stores with citations, confidence factors, gap
  dispositions, a tool registry and an audited tool runtime. The only implementation of
  `IInvestigationExecutor` in the entire solution is
  `Dispatch/Application/NotConfiguredInvestigationExecutor.cs`, whose body throws. Nothing has
  ever investigated anything. ADR-005 states this correctly — "there is no LLM investigator in
  either language" — and this document contradicted its own ADR.

  The oracle therefore covers the ledger, not the investigator. The storage shapes and refusal
  taxonomies are worth reading; the thing that reasons is greenfield.
- Canonical resource identity. The intake specification calls it "the largest unsolved question
  in the product". Chat-initiated investigation, topology and cross-source correlation all
  depend on it. It has one line of design.
- Identity and authentication. ADR-006 is a decision; no implementation specification exists. The
  operator surface today has one shared token and no notion of who acted.
- Inventory synchronization (ADR-004). Specified as a concept, unbuilt.
- Coverage as capability readiness.
- The frontend against the Go API.

---

## 6. Recommended order, and why

**REVISED 2026-07-30 by founder decision. The previous order — Environments and Connections,
then Incidents and grouping, then Investigation — is superseded by ADR-008.** It is preserved in
git history rather than here. The reason it was wrong is section 5's correction: it sequenced two
slices of model work ahead of the one capability that has never been demonstrated, which is the
same order that produced Stage 1A's six verified slices of bookkeeping around an executor that
throws.

**Next — the first real investigation, end to end, against a real cluster.** The path is: Default
Environment → Relay registration → minimal Kubernetes Connection → manually scoped investigation
→ real Relay evidence → timeline → evidence-cited supported explanation, with abstention when
support is insufficient. Built alongside a scenario harness of deliberately broken clusters,
scored by engineers who did not build it. Decisions: ADR-008 through ADR-012.

**Included, because the slice cannot work without them — and now built:** Environment and
Connection as rows, with a Default environment created automatically and `environment_id`
non-null from the first migration (ADR-003 as amended); `connection_id` on `relay_job`;
Kubernetes events and bounded container logs as capabilities, because
`kubernetes.workload.runtime` alone reports that a pod is failing and cannot report why. What
remains of this slice is the investigator itself and the scenario harness that scores it.

**Deferred until the first investigation has been evaluated:** Incidents and grouping;
environment management as a product feature; cross-provider canonical resource identity;
inventory beyond the change ledger; the second alerting adapter.

**DONE 2026-07-31 — the operator API for configuring sources.** Environments and Connections are
managed through the operator surface, and a Connection created there returns a secret once that a
real delivery is then accepted with. Onboarding no longer requires a manual `INSERT`.

**Canonical resource identity is deliberately narrow for this slice.** Within one Kubernetes
Connection, identity is what the cluster says it is. The general problem — that the object
Kubernetes names one way and AWS names another are the same thing — is real and is now scheduled
to be designed against an observed failure rather than an anticipated one.

---

## 7. Operational facts worth not rediscovering

**VERIFIED:**

- CI needs a `RELAY_REPO_TOKEN` secret with read access to `open-cluster/oc-relay`, because the
  shipping module requires the private protocol contract. It is configured. Without it every
  module-resolving step fails.
- The end-to-end proof runs in CI and checks out the Relay's `main`. A protocol regression in
  either repository turns the other's CI red. That is the property, not a defect.
- Running the proof locally needs both working trees; the Relay's is found beside this repository
  or named by `OC_E2E_RELAY_SOURCE`. Without it the tests skip and say so.
- `make test` and `make verify` cannot run on the current Windows development machine: the race
  detector needs a C compiler and none is installed. Use `go test -count=1 ./...` and say that is
  what was run.
- `gofmt -l` locally flags files git has checked out, because `core.autocrlf=true` gives them
  CRLF while the committed blob is LF. Not a real failure; do not "fix" it.

**Open security item, VERIFIED as still open:** a Clerk key is live in the frozen repository's git
history, unrotated, across five audits. It is not a Go concern, and it has not gone away.

---

## 8. Assumptions in this document

Stated separately so they are not mistaken for findings.

1. RESOLVED 2026-07-30 by ADR-008. Environment and Connection land inside the investigation
   slice as rows, not ahead of it as a feature.
2. FALSIFIED 2026-07-30. The investigator is not a port; see the correction in section 5. The
   .NET reference is an oracle for the ledger and for nothing that reasons.
3. RESOLVED 2026-07-31 by ADR-014, and resolved the other way. A **new** frontend is written
   against the Go control plane; `opencluster-web` is not re-pointed. It builds against the frozen
   .NET API and carries observability-era pages, and the reason for a fresh application is
   information architecture rather than tooling — its stack (Next, React 19, Tailwind 4, shadcn on
   Base UI, TanStack Query, zod, Playwright, MSW) is already the right one.
4. RESOLVED 2026-07-30. One capability is not enough. Kubernetes events and bounded container
   logs are on the critical path, because workload runtime reports that a pod is failing and
   cannot report why.
5. NEW: that ten deliberately broken clusters are representative enough of real incidents for
   the harness to mean something. Flagged as the judgement it is. The mitigation is that the
   scenarios are chosen from failures the founder has actually seen, and revised after the first
   real customer incident.
6. RESOLVED 2026-07-31 by the founder, and it is what makes the Connection work a replacement
   rather than a migration: no deployment of the intake slice has received real customer
   deliveries, and no external system points at the current intake URL. `alert_source` is
   therefore dropped rather than backfilled, and the intake route changes shape rather than
   gaining a compatibility alias. Recorded here because the next person to read the migration
   will want to know why it is allowed to be destructive.
