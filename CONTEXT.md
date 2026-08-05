# OpenCluster

An autonomous investigator for production incidents. It gathers bounded evidence from a
customer's real systems, forms and falsifies hypotheses, and states the most supported
explanation together with what it could not verify.

## Language

### Truth chain

**Observation**:
Something a source reported or exposed, at a time and scope. Carries no causal authority.
_Avoid_: reading, sample, datapoint

**EvidenceCandidate**:
A potential factual claim extracted from a bounded result, before validation.
_Avoid_: finding, fact

**EvidenceValidation**:
The reproducible check of a candidate's grounding, provenance, scope and completeness.
Its outcome decides whether and how the candidate may be used.

**EvidenceItem**:
An immutable validated factual claim with provenance and an incident-time snapshot. May
support or contradict a hypothesis; never establishes cause on its own.
_Avoid_: evidence (unqualified), proof

**Completeness certificate**:
The recorded basis on which an absence may be stated as fact — searched scopes, source
freshness, pagination completion, authorization coverage. Without one, absence is a
coverage gap, not evidence.
_Avoid_: negative result

**CoverageGap**:
Something the investigation could not check, and the consequence of not checking it.
Missing capability, stale source, denied authorization, exhausted budget, or a field a
customer's redaction policy masked.
_Avoid_: error, failure, unknown

**Abstention**:
A terminal investigation outcome stating that no explanation is sufficiently supported,
with the missing evidence and contradictions that prevented one. A first-class result, not
a failure.
_Avoid_: inconclusive, no result, low confidence

**Hypothesis**:
A proposed explanation under active investigation, carrying a falsification condition.

**Most supported explanation**:
The strongest explanation surviving alternatives and contradictions. Deliberately not
called a cause.
_Avoid_: root cause, likely cause, verified cause

**Traced explanation**:
An explanation that IS one of the hypotheses on the record, named by it and settled as supported.
An outcome that cannot name one is refused: a cause corresponding to nothing the investigator
proposed is a conclusion, not a finding, and the record cannot show what it beat.
_Avoid_: linked hypothesis, root hypothesis, primary hypothesis

**Untested explanation**:
A traced explanation resting on a hypothesis no dispatched read pointed at, so nothing was read
that could have disproved it. Real and weaker: it is admitted as caveated rather than supported and
carries the coverage gap saying so. Decided from what the platform sent, never from what the
reasoner claims about its own rigour.
_Avoid_: unverified explanation, low-confidence explanation, weak hypothesis

**VerifiedCause**:
An explanation meeting a defined verification standard, with the verification basis
shown. Confidence alone never produces one.

**OutcomeAssessment**:
A signal about what ultimately happened, attributed to its source. Human acceptance,
human correction, remediation performed, observed recovery, and deterministic
verification are separate signals and are never conflated.

### Execution

**Control plane**:
The central private service owning reasoning, truth, correlation, and tenancy. Also an
execution locality: work that runs against native data or public SaaS APIs.

**Relay**:
The customer-installed execution runtime that performs typed, bounded, read-only capability
jobs against customer-private sources over one outbound stream. Also an execution locality.
Organization-scoped: it carries no Environment and may serve Connections in several. Not an
agent, not a runtime, not a collector — it hosts no reasoning and accepts no commands. It is
not a Connection and never stands in for one: a Relay is where work runs, a Connection is
what the work reaches.
_Avoid_: agent, investigation runtime, collector, direct, connection

**Execution locality**:
Where a job runs — `control_plane` or `relay`. A property of a Connection, not of a
Capability, so the same capability may run centrally for one customer and through a Relay
for another.
_Avoid_: direct

**Capability**:
A named, versioned, frozen contract for one bounded read a Relay can perform, identified
general-to-specific (`kubernetes.workload.runtime`). A released capability message is
never edited; a semantic change mints a new version.
_Avoid_: command, action, task, plugin

**Job**:
One durable, leased, fenced instance of a capability execution. PostgreSQL owns its
truth; the stream only delivers it.

**Trust class**:
How a result's completeness was established — centrally verified, or attested by a Relay.
Relay-attested results carry confidence caps.

**Inventory synchronization**:
Relay-side scheduled change detection that pushes a delta only when something changed.
Separate from jobs: at-least-once with a dedup key, never leased or fenced. Only change
detection is synchronized — metrics, logs, and traces stay on demand.
_Avoid_: capture policy, monitoring, polling, collection

**Change ledger**:
The continuously persisted record of workload revisions and configuration changes. The only
context persisted continuously, because it decays at the source and cannot be recovered by a
later read.
_Avoid_: inventory, resource cache, topology store

**Navigation index**:
Persisted context used to decide where to look. Never citable: a relationship a conclusion
depends on is revalidated live and recorded as an Observation.
_Avoid_: topology graph (as a source of truth), cached evidence

### Tenancy

**Organization**:
The tenant boundary. Every durable record belongs to exactly one.
_Avoid_: account, customer, workspace

**Environment**:
A customer-named scope grouping Connections, and the boundary evidence may never cross. A
relevance and correctness boundary, not an execution-isolation one. The subject that
investigation policy, coverage readiness, and access control attach to. Assigned only when a
Connection is created; everything else inherits from the Connection it came through. Implies
no dedicated database or deployment.
_Avoid_: stage, tier, cluster, project

**Label**:
Optional metadata used for grouping, filtering and selection. Never an authorization,
credential or tenant boundary, and never a substitute for an Environment.
_Avoid_: tag (as a boundary), scope

**Integration**:
A kind of external system OpenCluster knows how to speak to — Alertmanager, PagerDuty,
Zabbix, Kubernetes, Prometheus, Nomad, Proxmox. A closed vocabulary compiled into the
product, never a customer record: it names what an adapter exists for. Many Connections may
share one Integration, and adding a second installation of a system a customer already runs
adds a Connection and no code.
_Avoid_: connector, provider, plugin, datasource type, integration instance

**Connection**:
One configured instance of an Integration — "Production Alertmanager", "EU Zabbix",
"Staging Prometheus", "Production Proxmox". The member of an Environment, and the sole
authority for the Environment of everything that arrives through it. Carries its role, its
execution locality and, where relevant, its Relay binding.
_Avoid_: integration (the instance), datasource, alert source, connector.
"Source" is not banned outright — it is the ordinary word for the customer's own system at
the far end of a Connection, and intake talks about a source retrying or being told to slow
down. What it may never do is stand in for the Connection record itself: the platform
configures Connections, not sources.

**Connection role**:
What a Connection is used for: `trigger`, `evidence`, or both. A **Trigger Connection**
delivers SignalUpdates inbound and owns its verification secret, replay window, rate limit
and deduplication state. An **Evidence Connection** answers bounded capability reads
outbound and owns its execution locality and Relay binding. One Connection may be both; an
Alertmanager Connection is usually trigger-only and needs no Relay, and a Kubernetes
Connection is usually evidence-only and names one.
_Avoid_: direction, mode, connection type, kind

**Placement**:
Where an organization's data physically lives — database, object storage, region, model
provider. Resolved from the organization, never ambient. Independent of Environment.
_Avoid_: shard, instance, deployment

### Investigation

**IncidentEpisode**:
One durable operational episode, grouping repeated notifications about the same failure.
Provisional grouping, not causal truth; may be merged, split, or superseded without
rewriting history. Keyed conservatively on organization, environment, trigger source,
source-provided identity and affected target.

As built (2026-08-05) the key is organization, Connection and the **source-provided grouping
identity**, and the affected target appears only inasmuch as the source put it there —
Alertmanager's group key embeds the labels a customer grouped by. Deriving the target ourselves is
deliberately not done: reading it out of a Signal's labels is canonical resource identity, which
does not exist in this product, and a grouping built on that inference would merge two failures a
customer's own alerting kept apart. A delivery supplying no grouping identity produces one episode
per notification, which is a split rather than a merge and is the failure worth having.
_Avoid_: incident (unqualified), alert group

**Grouping basis**:
Who decided that the notifications in an IncidentEpisode belong together — the source's own
grouping, or none at all. Recorded on every episode and shown to an operator, because a grouping
nobody can explain is one they argue with rather than act on, and because the honest answer to
"why are these one incident" is sometimes "your alerting said so".
_Avoid_: grouping rule, correlation, group reason

**SignalUpdate**:
One firing, updated or resolved notification from an external source. Repeated updates
about the same episode append to it; they never create a second Investigation.
_Avoid_: alert, event, notification

**Investigation**:
The durable case for one operational episode: its evidence, hypotheses, timeline, coverage
gaps and current outcome. One stable identity, URL and permalink for the episode's whole
life. A current projection across its rounds, in which superseded explanations stay visible
and attributable.
_Avoid_: analysis, diagnosis, case (as the user-facing noun)

**InvestigationRound**:
One bounded, immutable execution of the investigator under pinned planner, model and policy
versions. Reinvestigation adds a round to the same Investigation; it never creates a new
one. Owns the case pack.
_Avoid_: run, attempt, pass

**Evidence plan**:
The declared, versioned set of capability calls an investigation intends to make. Its
resolved snapshot is pinned to the run, so what a run meant to read is recoverable without
reading the source that produced it.
_Avoid_: playbook, runbook, workflow

**Investigation brief**:
The deterministic orientation assembled before any hypothesis exists: resource identity,
trigger, time window, recent changes, live topology context, available capabilities and
coverage. It states what is being looked at and what may be asked. It never prescribes the
investigation.
_Avoid_: evidence floor, bootstrap context (bootstrap names relay enrolment), default plan,
baseline collection

**Adaptive pass**:
One cycle within a round in which the planner proposes further typed read-only requests from
the hypotheses it currently holds. Bounded in steps, cost, time and result size. Distinct
from an InvestigationRound, which contains passes.
_Avoid_: adaptive round (collides with InvestigationRound), agent step, iteration, tool loop

**Case pack**:
The immutable closed-world record of one InvestigationRound's inputs, evidence, gaps,
hypotheses, outcome, execution controls and component versions — sufficient to replay that
round without any access to live sources or current state. An internal audit and replay
term, not user-facing.

**Investigation controls**:
The customer-authored guardrails constraining what OpenCluster may do: permitted connections
and capabilities, exclusions, execution limits, redaction and retention requirements,
automatic-start permission. They may only restrict; they never prescribe what to inspect or
in which order.
_Avoid_: policy (unqualified), data collection plan, guardrails (in contracts), budgets

**Execution limits**:
The numeric bounds on an investigator execution — steps, duration, concurrency, result size,
evidence window, optional cost ceiling. Applied per round and cumulatively per Investigation.
_Avoid_: budgets, quotas, credits

**Coverage**:
What the platform can currently investigate in an environment, expressed as capability
readiness — available, degraded, stale, unauthorized, absent.
_Avoid_: connection health, integration status
