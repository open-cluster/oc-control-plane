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
Missing capability, stale source, denied authorization, exhausted budget.
_Avoid_: error, failure, unknown

**Hypothesis**:
A proposed explanation under active investigation, carrying a falsification condition.

**Most supported explanation**:
The strongest explanation surviving alternatives and contradictions. Deliberately not
called a cause.
_Avoid_: root cause, likely cause, verified cause

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
The customer-installed component that executes typed, bounded, read-only capability jobs
against customer-private sources over one outbound stream. Also an execution locality.
Not an agent, not a runtime, not a collector — it hosts no reasoning and accepts no
commands.
_Avoid_: agent, investigation runtime, collector, direct

**Execution locality**:
Where a job runs — `control_plane` or `relay`. A property of a connection, not of a
capability.
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

### Tenancy

**Organization**:
The tenant boundary. Every durable record belongs to exactly one.
_Avoid_: account, customer, workspace

**Environment**:
A customer-named scope grouping connections, and the boundary evidence may never cross.
The subject that investigation policy, coverage readiness, and access control attach to.
Resources, topology, incidents and investigations inherit theirs from the connection that
discovered them and are never assigned one directly. Implies no dedicated database or
deployment.
_Avoid_: stage, tier, cluster, project

**Connection**:
One configured integration with a customer system, carrying its execution locality and,
where relevant, its Relay binding. The member of an Environment.
_Avoid_: integration (the type), datasource

**Placement**:
Where an organization's data physically lives — database, object storage, region, model
provider. Resolved from the organization, never ambient. Independent of Environment.
_Avoid_: shard, instance, deployment

### Investigation

**Incident**:
A grouped operational problem. Provisional grouping, not causal truth; may be merged,
split, or superseded without rewriting history.

**Investigation**:
One run against an incident, under pinned policy and investigator versions. Immutable
once terminal; re-investigating always creates a new run.
_Avoid_: analysis, diagnosis

**Case pack**:
The immutable closed-world record of an investigation's inputs, sufficient to replay it
without any access to live sources or current state.

**Coverage**:
What the platform can currently investigate in an environment, expressed as capability
readiness — available, degraded, stale, unauthorized, absent.
_Avoid_: connection health, integration status
