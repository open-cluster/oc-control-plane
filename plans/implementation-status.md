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

**It has answered one live scenario, on GLM-5, and the red herring did not work.** Run
2026-08-02 via `cmd/redherring` (`scripts/live-model.sh scenario`). The model reached the planted
cause — a rejected database credential — and explicitly exonerated the deploy that happened thirty
minutes earlier. Every claim cited evidence, the output schema admitted the draft, and there was no
refusal, retry, fallback or contract failure. Cost was about $0.03 for three calls, 14,188 billable
tokens, 3m20s wall clock with the conclusion alone taking 2m12s.

**Three findings from it are worth more than the pass.** First, **reasoning was 78% of output
tokens** (6,130 of 7,859) — on this model the cost of a round is set by how hard it thinks, so
effort is the lever to tune before anything else. Second, **prompt caching works on this vendor**
(1,280 tokens served warm, 20% of input) and it reports no cache-write count at all, exactly as its
capability matrix declares — which is the matrix doing real work rather than describing. Third, and
most important: **the conclusion it reached was never one of its own hypotheses.** All four were
falsified or set aside, and the outcome was still admitted as `supported`, because the output
schema requires a supporting claim and never requires the explanation to be a hypothesis anyone
proposed. That is a real hole in the falsification machinery and it is a domain question, not a
provider one.

**What remains unproved.** Whether the gathering pipeline works end to end — `cmd/redherring`
serves pre-baked evidence and touches no cluster or Relay, so the ten-scenario harness is still the
instrument for that. Anthropic has never been called: the adapter is exercised only against a
canned transport. And one scenario on one model is one data point, not a measurement.

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
| 4 — Scenario harness | `spec-scenario-harness.md` | Go control plane, as a program not a test | ⚠️ Built 2026-08-01, **not yet runnable**: no provider and no transcripts |
| 4 — Live model provider | `spec-live-model-provider.md` | Go control plane | ✅ Built 2026-08-02 (revision 2, provider-neutral; Anthropic and Z.AI adapters) and **proved on one live GLM-5 scenario**. Anthropic itself still uncalled |
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
