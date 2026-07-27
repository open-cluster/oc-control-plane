# OpenCluster — Autonomous AI SRE Master Plan

Status: DRAFT FOR FOUNDER APPROVAL — planning only; no implementation is authorized by this document.
Date: 2026-07-12
Supersedes: `plans/investigation-platform-master-plan.md` in the frozen .NET reference repository (OCluster/Zyrenn.ConsumerService)
Scope: product definition, autonomous investigation architecture, truth model, tools, topology, evaluation, frontend, API contracts, scalability, security, sequencing, competitive proof, and implementation gates.

---

## 0. Goal

OpenCluster becomes a read-only autonomous AI SRE that begins investigating production incidents before the on-call engineer starts from zero. It scopes the failure, forms and falsifies hypotheses, selects bounded investigation tools, queries the real environment, preserves validated evidence, narrows toward the most supported explanation, and states what remains unknown.

The product must prove more than evidence visualization or metric summarization. It must reliably investigate a cross-layer failure, replay that investigation against incident-time data, expose coverage gaps, and demonstrate measurable advantages over a generic HolmesGPT/Robusta-style agent connected to the same sources.

## 1. Founder decision summary

| Decision | Verdict |
| --- | --- |
| Product category | AI SRE externally; autonomous production investigator in product and API language. |
| Product promise | Alert in; a disciplined, inspectable investigation is already under way when the engineer arrives. |
| Strategic architecture | Model A+ remains: connector-first, native collector retained, OpenCluster-owned truth, context, and evaluation layers. |
| Native observability | Strategically frozen as a broad platform. Narrow additions are allowed only when they directly unlock investigation quality or reliability. |
| Agent architecture | One bounded adaptive investigator first. Multi-agent execution is deferred until evaluation proves a single agent is a bottleneck. |
| Safety posture | Stage 1 is strictly non-mutating: source reads plus DNS/TCP/HTTP checks from a pre-provisioned Relay probe. Creating temporary diagnostic resources and all remediation are outside Stage 1. |
| Truth posture | Observation, evidence candidate, validated evidence, hypothesis, conclusion, and verified outcome remain distinct. |
| Causal language | The product uses “most supported explanation.” “Verified cause” requires an explicit verification source, not confidence alone. |
| Human confirmation | Valuable but optional. Human acceptance, correction, remediation success, observed recovery, and deterministic verification are separate outcome signals. |
| `incident_signal` | Retained only as a legacy neutral correlation projection. Causal role labels are removed from investigator inputs and customer-facing claims. |
| Stage 1 proof | A real cross-layer deployment/configuration incident, not CPU-versus-memory metric classification. |
| Runtime scope | A thin Kubernetes read-only capability moves into Stage 1. Without runtime inspection, OpenCluster cannot prove senior-SRE behavior. |
| Topology | Local, temporal, provenance-carrying investigation context. No global graph explorer. |
| eBPF | Deferred. Reconsider only after repeated design-partner failures caused by missing dependency or network visibility that Kubernetes, traces, and existing sources cannot provide. |
| Compounding asset | Environment-specific investigation knowledge continuously evaluated against operational outcomes and historical replays. |
| Competitive standard | OpenCluster is not described as better than Resolve or Robusta until benchmark and design-partner evidence supports the claim. |

## 2. Exact product definition

OpenCluster is the autonomous first investigator for production incidents.

It is responsible for:

- Understanding the trigger and determining the affected scope.
- Establishing which sources and investigation capabilities are available and trustworthy.
- Generating multiple plausible hypotheses without treating the first hypothesis as privileged.
- Declaring a falsification condition for each material hypothesis.
- Choosing the next investigation question by expected information value.
- Querying bounded production sources through authorized read-only tools.
- Following runtime, dependency, deployment, and ownership relationships when supported by provenance.
- Creating new hypotheses when evidence changes the incident model.
- Preserving supporting, contradicting, and contextual evidence separately.
- Challenging the leading explanation before concluding.
- Producing a most supported explanation, contributing conditions, rejected hypotheses, unknowns, and next checks.
- Producing `INCONCLUSIVE` only after a serious bounded investigation or a clearly disclosed capability failure.

OpenCluster is not:

- A chatbot placed over dashboards.
- A deterministic engine that gathers every predefined fact and asks an LLM for a summary.
- A replacement for Prometheus, Loki, Datadog, cloud platforms, or Kubernetes.
- A global topology browser.
- An auto-remediation system in this plan.
- A system that converts temporal correlation into causal truth.
- A product that claims superiority based on architecture diagrams rather than evaluated investigations.

### 2.1 Architectural alternatives considered

| Decision | Alternative | Trade-off and ruling |
| --- | --- | --- |
| Agent execution | Multiple specialized agents from Stage 1 | May increase parallelism but multiplies coordination, cost, failure modes, and evaluation ambiguity. Rejected until one bounded agent is measured as insufficient. |
| Agent execution | One fixed runbook executor | Predictable but cannot respond to novel discoveries. Rejected as the primary investigator; retained only as optional guidance. |
| Evidence production | Deterministic templates only | Highly reproducible for metrics but too weak for novel logs, configuration, code, and operational text. Rejected as the universal model. |
| Evidence production | Model-authored evidence without validation | Flexible but cannot sustain an enterprise truth claim. Rejected. The chosen model permits AI-assisted candidates with source-grounding validators. |
| Stage 1 data | Native metrics only | Fastest implementation but proves a metric analyst rather than a senior SRE. Rejected. |
| Stage 1 data | Full connector platform before the investigator | Broad but delays the core learning loop and creates integration scope risk. Rejected. The chosen model adds only a thin Kubernetes/runtime slice needed by the target incident. |
| Topology | Global continuously rendered graph | Visually impressive but expensive, stale, and not necessary for the incident workflow. Rejected. |
| Topology | No persisted topology; discover everything during each run | Simpler storage but loses incident-time context, reuse, and replay. Rejected. The chosen model persists temporal provenance-carrying relations and captures traversed edges per run. |
| Frontend | One long incident page containing every section | Simple routing but weak prioritization and excessive scrolling during active response. Rejected. |
| Frontend | Separate incident and investigation applications | Clear object separation but duplicates context and splits the audit trail. Rejected for Stage 1. The chosen model is one incident workbench with immutable run permalinks and progressive disclosure. |
| Outcome | Mandatory human confirmation | Produces clean labels when used but creates workflow friction and sparse or biased data. Rejected. Multiple independently sourced outcome assessments are chosen. |
| Remediation | Autonomous writes in the initial wedge | Potentially reduces MTTR further but expands risk before investigation trust is earned. Rejected for this plan. |
| Active diagnostics | Create an ephemeral diagnostic pod per check | Flexible, but creation and deletion are production mutations and contradict a read-only Stage 1. Rejected for Stage 1. |
| Active diagnostics | No DNS or connectivity diagnostics | Preserves strict reads but leaves the target failure materially unverified. Rejected. A pre-provisioned Relay probe with fixed identity and policy is chosen. |
| Replay | Re-query live or emulated sources | Can answer new questions but is not the original incident and risks non-reproducible results. Rejected for Stage 1. Closed-world case-pack replay is chosen. |
| Replay | Build full source emulators | More flexible than a closed case pack but adds large source-specific scope. Deferred until replay evaluation proves it necessary. |
| Scheduling | Kafka-only investigation dispatch | Strong wake-up throughput but weak mutable priority and cross-tenant fairness semantics. Rejected as the source of scheduling truth. |
| Scheduling | Durable database priority scheduler feeding worker wake-ups | Easier transactional eligibility, priority, fairness, leases, and visible queue reasons at expected MVP scale. Chosen; Kafka may remain a wake-up mechanism after code audit. |
| Kubernetes history | Capture current state only when an incident opens | Simple but loses deleted pods, garbage-collected revisions, and mutable ConfigMap history. Rejected for supported configuration evidence. |
| Kubernetes history | Archive all Kubernetes objects continuously | Complete but creates excessive sensitive storage and platform scope. Rejected. A bounded allowlisted metadata and non-secret revision ledger is chosen. |

## 3. Product success contract

The first product milestone succeeds only if OpenCluster can demonstrate all of the following on a real environment:

1. An operational signal opens or attaches to the correct incident.
2. The investigator identifies the affected workload and uncertainty in scope.
3. It creates at least two plausible hypotheses with explicit falsification conditions.
4. It queries native metrics and Kubernetes runtime state.
5. It rejects at least one plausible but incorrect explanation.
6. A runtime or configuration discovery causes it to create or materially revise a hypothesis.
7. It checks deployment or configuration context connected to the incident window.
8. It performs at least one disconfirming check against its leading explanation.
9. Every factual claim in the conclusion cites validated evidence with provenance.
10. Missing, stale, unauthorized, failed, or unavailable sources are represented as coverage gaps.
11. The same investigation can be replayed against a closed incident-time case pack without live source access.
12. A newer investigator version can be compared with the original run without querying the live current state.

The target demonstration is:

- A deployment introduces an invalid dependency configuration.
- The alert initially makes CPU, memory, network, and dependency failure plausible.
- OpenCluster verifies that resource exhaustion is not supported.
- Runtime logs show repeated DNS-resolution failures.
- Workload configuration references a hostname that does not map to an observed Kubernetes Service.
- The supported non-secret configuration ledger shows when the configuration changed.
- A direct service-discovery or bounded in-cluster DNS check supports or contradicts the configuration hypothesis.
- OpenCluster challenges the conclusion by checking whether the expected service exists under another authorized namespace, alias, cluster, or supported Kubernetes discovery form.
- It returns the configuration mismatch as the most supported explanation, or `INCONCLUSIVE` if the required verification cannot be performed.

This demonstration must use real collected data and real tool execution. Fabricated fixtures may support tests but may not satisfy the founder acceptance gate.

The Stage 1 demonstration uses a non-secret `DB_HOST` value present as a workload-template literal or an allowlisted referenced ConfigMap key. Secret values are never captured. If the value was supplied only through a Secret, mutated before capture, or belongs to an unsupported configuration source, the investigator records a decisive configuration-history gap and may not claim the mismatch as evidence.

## 4. Truth and confidence model

### 4.1 Truth layers

| Concept | Meaning | Causal authority |
| --- | --- | --- |
| Observation | A source reported or exposed something at a time and scope. | None. |
| EvidenceCandidate | A potential factual claim extracted from a bounded result. | None until validation. |
| EvidenceValidation | The reproducible result of checking grounding, provenance, scope, completeness, and ambiguity. | Determines whether and how the candidate may be used. |
| EvidenceItem | An immutable validated factual claim with provenance and an incident-time snapshot. | May support or contradict hypotheses; does not independently establish cause. |
| Hypothesis | A proposed explanation under active investigation. | Provisional. |
| InvestigationConclusion | The strongest supported explanation after alternatives and contradictions are considered. | Supported judgment, not verified cause. |
| OutcomeAssessment | A human or system signal about what ultimately happened. | Depends on source and verification class. |
| VerifiedCause | An explanation meeting a defined verification standard. | Highest available operational truth, with the verification basis shown. |

### 4.2 Evidence validation classes

- Deterministic: derived through versioned bounded computation or exact structural comparison.
- Source-grounded: directly present in a source object, event, configuration, log record, or query result with bounded quotation or fields.
- AI-assisted validated: proposed by a model from bounded source material, then checked for exact source grounding and scope by validators.
- Human-provided: explicitly entered by an authorized person and attributed to that person.
- Historical: derived from a prior outcome and always labeled as historical relevance rather than current proof.

AI-assisted evidence is allowed because novel text and configuration analysis cannot always be template-generated. It may never hide its origin or bypass validation.

Negative evidence requires an explicit completeness certificate. The validator records the searched scopes, source freshness, list/watch reconciliation status, pagination completion, authorization coverage, cluster set, namespace set, supported discovery forms, and query result. “No matching Service was observed” may become evidence only when the authorized relevant inventory is complete and fresh. Partial RBAC, stale watches, failed pagination, unknown clusters, or incomplete namespaces produce a coverage gap instead.

Stage 1 Kubernetes service discovery accounts for ordinary Services, headless Services, `ExternalName`, EndpointSlices, FQDN and search-suffix behavior, cross-namespace names, and declared multi-cluster or service-mesh aliases when the corresponding capability is connected. Unsupported discovery mechanisms remain explicit gaps.

### 4.3 Confidence contract

The UI exposes Low, Medium, and High with structured reasons. Percentages are not shown.

Confidence expresses quality of support, not model emotion.

- An uncited hypothesis is Low.
- A conclusion with an unresolved material contradiction is capped at Medium.
- A conclusion requiring a missing critical capability is capped at Medium and may be forced to `INCONCLUSIVE` when the missing check is decisive.
- Multiple facts from one query or one source family do not count as independent corroboration.
- High normally requires independent evidence families or one exceptionally direct deterministic verification plus no viable contradictory explanation.
- Historical similarity may guide investigation but cannot raise a current conclusion to High by itself.
- Confidence never creates `VerifiedCause`.

Conclusion guards operate on atomic factual claims. Each material factual clause maps to one or more EvidenceItems; a paragraph-level citation is insufficient. Evidence claims remain observational and non-causal. Causal interpretation belongs only to hypotheses and conclusions.

### 4.4 Correction, retention, and deletion

- Accepted EvidenceItems and snapshots are immutable during their retained lifetime.
- A correction appends a superseding EvidenceItem and links the prior item; it never rewrites the original investigation record silently.
- Conclusions and outcome assessments are versioned or superseded with actor, reason, and time.
- Hashes establish post-capture integrity, not authenticity of the original source. Source authenticity depends on connector identity, transport, authorization, and provenance.
- Retention policies may remove snapshots before metadata only when the UI and replay case declare the missing basis.
- Tenant deletion and legal erasure override ordinary append-only retention through documented crypto-erasure or physical deletion, with a non-sensitive deletion audit marker.
- Shared caches, embeddings, and learned knowledge must be tenant-isolated and deleted or rendered irrecoverable with the source tenant.

## 5. Revised domain model

### 5.1 Investigation truth

- `Observation`: source, source identity, resource scope, time scope, observed time, bounded payload reference, collection status, freshness.
- `EvidenceCandidate`: proposed claim, producing observation or tool result, origin, extractor version, proposed evidence family.
- `EvidenceValidation`: candidate, status, validation class, validator version, grounding references, source completeness certificate where required, ambiguity, rejection reason.
- `EvidenceItem`: immutable accepted claim, evidence family, resource and time scope, provenance, validation reference, snapshot reference, source deep link.
- `EvidenceSnapshot`: immutable bounded incident-time basis, redaction metadata, hash, size, retention class, capture time.

### 5.2 Investigation control and audit

- `Investigation`: incident child, policy and investigator versions, status, budgets, queue metadata, start and terminal reasons.
- `InvestigationDecision`: structured goal, current focus, question, concise reason, hypothesis tested, expected information value class, selected capability and tool.
- `ToolExecution`: authorization scope, tool and schema version, sanitized arguments, timeout, output limit, idempotency key, attempts, result status, source freshness, cost, and evidence candidates produced.
- `InvestigationStep`: ordered customer-facing audit event that references decisions, executions, evidence, and hypothesis transitions without storing private chain-of-thought.
- `CoverageGap`: missing capability, stale data, insufficient retention, failed connector, authorization denial, unresolved identity, or budget boundary; includes consequence and suggested resolution.

### 5.3 Reasoning state

- `Hypothesis`: stable identity and normalized statement.
- `HypothesisTransition`: append-only state, rank, confidence, reason, supporting and contradicting references, supersession or merge relationship.
- `HypothesisEvidence`: evidence stance and concise grounded relevance statement.
- `InvestigationConclusion`: supported explanation, contributing conditions, observed symptoms, rejected alternatives, remaining viable alternatives, unknowns, next verification, confidence factors, evidence citations.

### 5.4 Outcome and learning

- `OutcomeAssessment`: assessment source, accepted or corrected explanation, remediation performed, observed recovery, verification class, actor or system, and time.
- `InvestigationKnowledge`: versioned environment-specific learned artifact with source cases, confidence, decay, applicability scope, and last successful use.
- `InvestigationEvaluation`: replay inputs, expected outcome, actual conclusion, quality labels, investigator/model/policy/tool/extractor versions, cost, duration, and reviewer annotations.

### 5.5 Customer Production Model

- Canonical resources and external identities.
- Temporal resource and service relationships.
- Source capabilities, health, permissions, retention, and freshness.
- Changes, deployments, configuration versions, and ownership.
- Investigations, evidence, outcomes, successful checks, failed checks, and coverage gaps.
- Environment-specific query patterns and investigation knowledge.
- A bounded Kubernetes revision ledger for supported resource metadata, rollout identity, workload-template literals, and allowlisted referenced non-secret ConfigMap keys.

After one year OpenCluster should know which checks are informative for each service, which source queries are reliable, which relationships are stable, which hypotheses are repeatedly false, which changes commonly affect a scope, which gaps repeatedly block diagnosis, and which conclusions were later corrected. Past correlation is never promoted automatically to current causal evidence.

### 5.6 Incident correlation and scope correction

Incident correlation is provisional operational grouping, not causal truth.

- Late and out-of-order signals retain source time and observed time and may extend an incident without rewriting history.
- Overlapping incidents may reference the same detection or evidence context without being automatically merged.
- A merge creates a new canonical grouping relationship while preserving original incident identifiers, investigations, and audit history.
- A split creates new incident scopes and marks which detections, observations, and investigations were reassigned or remain ambiguous.
- A superseded incident remains addressable and points to the canonical replacement.
- Investigations display correlation confidence and blast-radius uncertainty when attachment is not deterministic.
- Material scope changes trigger re-planning; previous evidence remains attributed to the scope in which it was collected.

## 6. Autonomous investigation lifecycle

### 6.1 Senior-SRE investigation strategy

The investigator deliberately considers these axes rather than querying sources randomly:

- Scope and cohort comparison: affected versus unaffected versions, instances, zones, tenants, nodes, and regions.
- Temporal ordering: trigger, deviation, change, restart, dependency degradation, propagation, mitigation, and recovery.
- Recent changes: deployments, configuration, feature flags, infrastructure changes, certificate rotation, and source changes.
- Runtime truth: current and incident-time workload state, lifecycle, scheduling, readiness, termination, and configuration identity.
- Dependency traversal: upstream, downstream, backing services, DNS, queues, databases, and external endpoints, constrained by reliable topology.
- Symptom propagation: whether the timing and affected cohorts match the proposed causal path.
- Baseline and recovery: before, during, and after windows, including missing data and sampling changes.
- Historical relevance: prior cases guide questions but never substitute for current evidence.
- Falsification: the next check should distinguish viable explanations or challenge the leader, not merely collect supporting detail.
- Source quality: freshness, completeness, authorization, retention, sampling, conflicts, and collection failures are checked before source absence is interpreted.

### 6.2 Lifecycle

1. Validate and normalize the trigger without assuming the trigger identifies the cause.
2. Correlate or attach conservatively and record uncertainty about incident grouping.
3. Establish service, workload, resource, environment, cluster, and time scope.
4. Inventory available capabilities, source health, permissions, freshness, and retention.
5. Build an initial incident model from neutral observations and already validated evidence.
6. Generate multiple plausible hypotheses and a falsification condition for each material hypothesis.
7. Rank investigation questions by expected ability to distinguish between viable hypotheses, cost, risk, and latency.
8. Select an authorized bounded tool and execute it.
9. Convert the bounded result into observations and evidence candidates.
10. Validate candidates and preserve incident-time snapshots.
11. Update support, contradiction, confidence, rank, and coverage gaps.
12. Decide whether to deepen the current hypothesis, test an alternative, traverse a dependency, inspect a change, narrow or widen scope, merge hypotheses, supersede a hypothesis, or create a new hypothesis.
13. Before concluding, perform a disconfirming check against the leading explanation when an available check could materially challenge it.
14. Stop when support is sufficient and alternatives are materially addressed, no allowed check is likely to change the conclusion, the incident is invalidated or superseded, or a declared budget/capability boundary is reached.
15. Persist the most supported explanation or `INCONCLUSIVE`, all contradictions and gaps, and the best next verification step.

The agent does not execute a fixed checklist. Runbooks and learned procedures are optional investigation guidance. The investigator may deviate when evidence shows that the procedure is irrelevant, and the deviation is recorded as a structured decision.

### 6.3 Deterministic stopping and conclusion guards

The model may propose that an investigation is complete, but policy decides whether completion is allowed.

- “Sufficient support” requires atomic claim citations, validated evidence, confidence-policy compliance, and no failed validation used as support.
- “Alternatives materially addressed” requires every still-viable high-ranked alternative to be tested, explicitly blocked by a decisive gap, or shown unable to explain the observed scope and timing.
- A decisive gap forces `INCONCLUSIVE` when the missing check could reasonably reverse the leader and no independent direct verification exists.
- Budget exhaustion never upgrades confidence. It produces the best bounded result with the exhausted boundary shown.
- Material contradictions cap confidence and must appear in the conclusion.
- A conclusion cannot use absence as evidence without a valid completeness certificate.
- The leading explanation must receive a disconfirming check unless no safe available check exists; that absence becomes a gap.
- Policy rejects a conclusion containing uncited factual clauses, causal language presented as evidence, or a scope broader than its cited evidence.

## 7. Investigation tool architecture

Every tool definition includes:

- Stable tool identifier and schema version.
- Human and model semantic description.
- Required capability and connector.
- Required resource, environment, and tenant scope.
- Authorization and data sensitivity.
- Argument schema and server-enforced bounds.
- Timeout, retries, concurrency, and output-size limits.
- Idempotency and cache policy.
- Source freshness and retention expectations.
- Structured result schema and error vocabulary.
- Evidence candidate producers and validators.
- Provenance and source deep-link strategy.
- Prompt-injection exposure class.

### 7.1 Stage 1 tool families

Incident and scope:

- Incident context and related neutral detections are injected as versioned seed context.
- Resource identity, workload membership, current lifecycle, and one-hop deterministic relations.
- Related incidents for the same resource, service, rule, or failure signature without semantic similarity in Stage 1.

Metrics:

- Discover available resource metrics.
- Query a bounded metric window.
- Compare baseline, incident, and recovery windows.
- Detect a bounded deviation and data-quality gaps.

Kubernetes runtime:

- Read workload, pod, container, Service, EndpointSlice, node, event, and rollout state.
- Retrieve bounded current and previous container logs.
- Compare selected non-secret configuration fields between workload revisions.
- Inspect restarts, termination reasons, readiness, scheduling, image, and resource limits.
- Maintain a bounded continuous revision ledger for Deployment, StatefulSet, DaemonSet, Service, EndpointSlice, rollout identity, workload-template literal environment values, and allowlisted referenced non-secret ConfigMap keys.
- Secret values are never captured. Secret name/key references may be captured as metadata only.
- Garbage-collected ReplicaSets, deleted pods, or pre-installation revisions are usable only when captured in the ledger; otherwise they create a configuration-history gap.

Stage 1 configuration-source support:

| Source | Stage 1 use |
| --- | --- |
| Workload-template literal environment value | Captured and comparable when non-secret. |
| Referenced non-secret ConfigMap key | Captured only when allowlisted and referenced by an observed workload. |
| ConfigMap bulk `envFrom` | Metadata and allowlisted referenced keys only; uncaptured keys are a gap. |
| Secret value or Secret bulk `envFrom` | Never captured or read; only non-sensitive name/key reference metadata may be recorded. |
| Downward API field | Reconstructed only from captured workload and pod metadata when complete. |
| CSI, Vault, sidecar, init-generated, or application-fetched configuration | Unsupported in Stage 1 unless a dedicated connector provides bounded provenance; otherwise a gap. |
| Mutated configuration before OpenCluster installation or before first capture | Unavailable; never inferred from current state. |

Diagnostics:

- Resolve a bounded hostname from a pre-provisioned OpenCluster Relay probe with a dedicated identity and no Kubernetes mutation permission.
- Test bounded TCP or HTTP connectivity from that probe to an approved incident-related destination.
- The probe is deployed during integration setup, not created by an investigation. It exposes a fixed diagnostic tool set and cannot execute arbitrary shell commands.
- Destination policy rejects loopback, link-local, metadata services, control-plane addresses, unauthorized external networks, and non-incident destinations across IPv4 and IPv6.
- DNS results are resolved, policy-checked, and pinned for execution to prevent rebinding. Redirects are disabled or revalidated at every hop.
- Stage 1 HTTP diagnostics use only approved safe methods with no request body, no model-selected credentials or headers, and allowlisted health or read paths configured by the organization. Where a safe application path is not configured, the probe performs only DNS, TCP, or TLS handshake checks.
- TLS certificate and hostname verification are enabled by default. Any organization-approved exception is configuration, never a model decision, and appears in provenance and the security audit.
- Diagnostic time, connection count, request rate, and response bytes use server-enforced tool bounds; response content is truncated and treated as untrusted data.
- Organizations may disable active diagnostics; the investigator records the resulting decisive gap when appropriate.

Changes:

- Stage 1 reads the captured Kubernetes rollout and supported configuration revision ledger.
- Git commit and CI/CD enrichment remain later connectors, but the domain contract exists from Stage 1.

### 7.2 Query evolution

- Level 1: safe platform metric intents and exact discovered metric names.
- Level 2: metric and log discovery with semantic metadata.
- Level 3: generated source queries parsed, bounded, cost-checked, and source-scoped before execution.
- Level 4: versioned per-environment query patterns learned from successful investigations and subject to decay and evaluation.

Generated PromQL, LogQL, trace, SQL, or cloud queries never execute solely because a model produced valid syntax. Authorization, cost, time, cardinality, tenant, and output bounds are enforced outside the model.

## 8. Topology collection and use

Topology exists to answer investigation questions. It is not a decorative global graph.

### 8.1 Collection sources

| Relationship | Primary source | Collection method |
| --- | --- | --- |
| Container to host | Native collector and OTLP resource attributes | Continuous observation plus reconciliation. |
| Pod to node, container to pod, pod to workload | Kubernetes API | LIST/WATCH with periodic full reconciliation. |
| Workload to Service | Kubernetes Service selectors and EndpointSlice membership | Deterministic comparison with temporal validity. |
| Service to service | OTel traces or service-graph metrics | Observed dependency with sampling and freshness metadata. |
| Service to database, queue, cache, or external endpoint | Traces, bounded configuration references, runtime observation, or explicit declaration | Source-specific relation with reliability class. |
| Deployment to workload or service | Kubernetes rollout metadata, GitOps, and CI/CD | Change relationship with revision and time. |
| Commit to deployment | GitHub or GitLab plus CI/CD or GitOps | Normalized change relation. |
| Team to service, namespace, or cluster | Explicit ownership configuration | Declared deterministic relation. |

### 8.2 Relationship contract

Every edge records type, direction, source, discovery method, valid-from, valid-to, first observed, last observed, last successfully checked, environment, reliability class, confidence where inferred, provenance, and conflict state.

- Absence of an edge is not proof that no relationship exists.
- Stale sources make edges stale; they do not silently remain current.
- Conflicting sources remain visible.
- Deterministic runtime edges may scope tools and evidence.
- Trace-observed edges may guide dependency checks and are labeled with sampling limits.
- Heuristic edges may suggest a check but may not support a causal conclusion until verified.
- Incident-time topology queries use temporal validity and source freshness at the incident time.
- Important traversed edges are captured in the investigation snapshot so later replay does not depend on current topology.

### 8.3 eBPF trigger

eBPF remains out of Stage 1. A proposal may be opened only when design-partner evidence shows repeated material failures caused by missing service dependency, DNS, TCP, or process visibility, and existing Kubernetes, trace, cloud, and declared sources cannot provide acceptable coverage. The proposal must compare building a collector, integrating an external eBPF source such as Coroot, and accepting the coverage gap.

## 9. API contract model

Persistence entities are never returned directly.

The API exposes semantic strings for workflow states and stable extensibility keys for connectors, capabilities, tools, providers, and source types. Numeric database enum values never cross the API boundary.

Required bounded views:

- Incident list item with current investigation projection.
- Incident investigation first-viewport view.
- Live investigation state with current focus, hypothesis tally, recent steps, capability state, gaps, budgets, and queue status.
- Investigation run summary and immutable run permalink.
- Hypothesis detail with transitions, evidence stances, gaps, and checks.
- Evidence detail with validation, provenance, snapshot, redaction, and source link.
- Tool execution detail with sanitized arguments, bounds, status, and evidence products.
- Local incident topology neighborhood with temporal and reliability metadata.
- Outcome assessment and evaluation summary.

Required action contracts:

- Investigate now creates a stable run identity under current policy and capability state. The run changes only through allowed append-only transitions until terminal, then becomes frozen.
- Cancel stops future decisions and executions while preserving completed artifacts.
- Re-investigate always creates a new run, optionally with a bounded human focus hint; it never mutates the prior run.
- Accept, correct, or mark insufficient information appends an OutcomeAssessment with explicit assessment source.
- Add human evidence creates a human-provided candidate that still passes scope, authorization, and provenance validation.
- Retry a failed capability check during an active run creates a new ToolExecution linked to the original failure. A terminal run is frozen; retry after terminal requires a new re-investigation run linked to the prior run.
- Resolve or acknowledge a coverage gap records the operational action without deleting the historical gap.

Every action defines the minimum role, tenant scope, idempotency key, optimistic concurrency version where mutable projections are affected, immutable-run behavior, audit event, and stable conflict or authorization errors. Cancellation, correction, gap resolution, and human evidence never rewrite prior audit events.

Secondary sections load independently. There is no giant incident graph response.

Steps, evidence, hypothesis transitions, tool executions, topology edges, and run history use bounded cursor pagination with stable ordering. The first-viewport aggregate contains only capped recent or leading items plus total counts.

Live updates use a resumable ordered event or delta contract when implemented. Polling remains an acceptable first transport, but clients must receive monotonic versions so delayed responses cannot regress displayed state.

Error contracts use stable machine-readable codes for unavailable capability, authorization denial, stale source, retention gap, rate limit, timeout, budget exhaustion, invalid scope, source conflict, unsafe query, output limit, and provider failure. Frontends never parse exception text.

Raw or redacted content returns an explicit permission state. A caller lacking permission receives metadata and a stable redaction reason when allowed, or a non-enumerating authorization response when the existence of the content is sensitive.

Every contract is versioned for additive evolution. `/api/v1` remains until an incompatible public change is genuinely required.

## 10. Frontend product specification

The interface must make the investigator's useful work obvious within fifteen seconds without presenting model theater.

### 10.1 Incident list

Primary desktop columns:

- Severity.
- Incident title with affected scope and environment.
- Operational status.
- Investigation state and current focus while running.
- Most supported explanation and confidence when terminal.
- Started time and duration.

Owner, trigger, gap count, and detailed policy state are secondary metadata or optional responsive columns. A running incident never shows a causal finding before one exists.

Live updates modify visible cells without reordering the list under the user. A separate affordance applies a refreshed sort.

Default sort is newest started incident first, with active incidents visually prioritized without moving rows during a reading session. Filters are operational status, investigation status, severity, environment, service or resource, owner, trigger source, coverage state, and time range. Search covers incident title, affected resource/service, trigger, current focus, and terminal explanation. Running rows show a compact hypothesis tally such as testing, supported, and rejected counts without implying a cause.

### 10.2 Incident detail: running

The first viewport contains:

- Compact incident header with scope, trigger, status, owner, and time.
- Full-width `INVESTIGATING` band with current focus, elapsed time, step budget, and degraded-state explanation when applicable.
- Main investigation narrative in the wider left column: meaningful completed checks, the current check, findings, and evidence created.
- Sticky right column: active hypotheses, leading provisional explanation, rejected hypotheses, checked and available capabilities, and material coverage gaps.

The running experience prioritizes work already completed, work in progress, what was rejected, and what cannot be checked. It does not display a half-empty conclusion card.

### 10.3 Incident detail: terminal

The first viewport contains:

- Most supported explanation or `INCONCLUSIVE`.
- Confidence and structured reasons.
- Independent supporting evidence families.
- Material contradictions and viable alternatives.
- Unknowns and next verification.
- Optional actions: Accept, Correct, Add evidence, Record remediation, and Re-investigate.

Acceptance is clearly labeled as human feedback, not deterministic confirmation.

### 10.4 Incident detail: pending, delayed, or unavailable

The page states the exact reason: waiting for an organization slot, provider unavailable, connector unhealthy, missing capability, policy disabled, unresolved identity, retention gap, or budget exhausted. The action is specific to the reason.

### 10.5 Progressive disclosure

The core page is an investigation workbench, not eleven equally prominent vertical sections.

- Overview contains the first-viewport investigation experience.
- Evidence and System Context contains evidence inspection, local topology, and bounded telemetry/log context.
- Timeline contains the complete investigation and operational audit.
- Run history contains immutable previous runs and evaluation comparisons when authorized.

Hypothesis and evidence details open side panels that preserve the user's place. Raw outputs require additional permission and a second deliberate expansion.

The hypothesis panel contains statement, current state, confidence and factors, rank history, supporting, contradicting, and contextual evidence, decisive gaps, checks performed, checks still available, transition history, supersession or merge relationships, and the current falsification condition.

The evidence panel contains atomic factual claim, evidence family, validation class and status, source and connector, resource and time scope, source freshness, completeness certificate where applicable, producing tool and sanitized query, extractor and validator versions, snapshot metadata, redaction state, hypotheses and stance, supersession history, and deep link to the source.

Local topology highlights the affected scope and every edge referenced by the selected hypothesis. Solid, dashed, and dotted styles are not the only distinction; each reliability class also has a text label and accessible pattern. Edge inspection shows type, direction, source, validity, freshness, reliability, conflicts, and provenance.

### 10.6 Live-state resilience and accessibility

- Live state carries monotonic versions; stale or out-of-order responses cannot regress the page.
- Loss of live updates shows last-updated time, reconnect state, and a manual refresh action without hiding existing evidence.
- Long traces use incremental pagination or virtualization while preserving keyboard navigation, anchors, and screen-reader order.
- Partial-permission states explain which detail is redacted without rendering broken empty panels.
- Desktop uses the two-column workbench; narrower desktop and tablet collapse the right column into a pinned summary drawer; phone layout remains readable for triage but is not the primary deep-investigation workspace.
- All states, evidence stances, edge types, and confidence factors are understandable without color.
- Focus moves predictably into and out of drawers; all actions, filters, hypothesis rows, evidence rows, and timeline anchors are keyboard accessible.
- Running updates use polite announcements and never repeatedly steal focus.

The Stage 1D usability gate uses at least three senior SREs who did not author the scenario. Within fifteen seconds they must identify the trigger, affected scope, current focus, leading provisional hypothesis, one rejected hypothesis, and the most material gap. Within two minutes they must locate the evidence basis, explain why confidence is capped, and identify the next verification. Task completion and critical interpretation errors are recorded; founder preference alone does not pass the gate.

### 10.7 Integrations and capability readiness

Integrations communicate investigative capability, not only connection health.

For each environment the product shows Metrics, Logs, Runtime, Changes, Traces, Topology, Knowledge, and Diagnostics as available, degraded, stale, unauthorized, or absent. Recommendations are driven only by observed investigation gaps.

## 11. Human feedback, verification, and memory

Human confirmation is not required for the product to remain useful or for every learning artifact.

Outcome sources are retained separately:

- Human accepted the explanation.
- Human corrected the explanation.
- Human reported insufficient information.
- A remediation was performed.
- Recovery followed a remediation within a stated window.
- A deterministic verification check succeeded.
- An incident resolved without review.

Only artifacts with a clear source and applicability may become investigation knowledge. Automatic reuse rules:

- Reuse successful source queries and check ordering as suggestions, not truth.
- Reuse confirmed identity and ownership mappings until invalidated.
- Reuse prior failure patterns as hypothesis seeds with decay.
- Never reuse a prior causal conclusion as current evidence.
- Lower confidence when source versions, service versions, topology, or environment have materially changed.
- Record when reused knowledge helped, was ignored, or misled the investigation.

## 12. Investigation quality evaluation

Evaluation is part of the Stage 1 substrate, not a Stage 9 afterthought.

Every run records investigator version, policy version, prompt contract version, model/provider, tool schema versions, connector versions, evidence extractor and validator versions, topology snapshot version, cost, duration, and budgets.

Quality labels include:

- Supported conclusion matched the verified or accepted outcome.
- Partially correct.
- Incorrect.
- Inconclusive and appropriate.
- Inconclusive despite sufficient evidence.
- Material cause missed.
- False causal claim.
- Unsupported claim.
- Useful evidence discovered.
- Useful hypothesis generated.
- Wrong hypothesis rejected efficiently.
- Important contradiction ignored.
- Repeated human work avoided.

Operational measures include time to first useful finding, time to terminal result, irrelevant tool calls, duplicate calls, failed calls, evidence-family coverage, investigation cost, queue delay, and human correction rate.

Offline replay uses preserved observations, tool results, evidence snapshots, topology snapshots, and incident-time capability state. It must not silently query current production state. Replay can compare model, policy, prompt contract, tool, and validator versions against the same historical case.

### 12.1 Stage 1 closed-world case-pack replay

Each replayable incident produces an immutable case pack containing the seed context, normalized observations, exact tool requests and results, evidence candidates and validations, topology and configuration revisions used, capability/permission/freshness state, source and observed timestamps, budgets, and the terminal outcome available for scoring.

- Replay has no production credentials or network route.
- A replay tool call matches a recorded result by tool schema version, normalized authorized scope, bounded time window, and semantic arguments.
- Equivalent narrower queries may receive a deterministically filtered recorded result when the tool contract permits it.
- A new or materially different query unavailable in the case pack returns `ReplayCoverageGap`; it never reads current state and is scored as an unanswered question rather than a tool failure.
- Recorded source ordering and timestamps are preserved. The replay clock is fixed to the incident timeline.
- Model randomness, seed where supported, model version, retry count, budgets, and repeated-run count are recorded.
- Evaluation distinguishes a useful new question blocked by the closed case from an irrelevant query and from failure to use available evidence.
- Expanding a case pack requires a new case-pack version and cannot rewrite prior evaluation results.

Live critical incidents are not uncontrolled A/B tests. Candidate versions graduate through scenario tests, historical replay, shadow evaluation where permitted, and controlled design-partner rollout.

### 12.2 Stage 1 blinded adaptivity suite

Before Stage 1D can claim senior-SRE-like investigation, evaluators run blinded holdout variants not present in investigator prompts or scenario-specific causal rules:

- Similar symptoms caused by resource exhaustion rather than bad configuration.
- The same configuration cause with a different temporal order and alert trigger.
- The target failure with active DNS diagnostics disabled.
- Stale or partial topology and incomplete RBAC.
- Materially contradictory log and metric evidence.
- A healthy dependency with an unrelated recent deployment.

Scenario authors separate fixtures and expected outcomes from investigator development. The investigator must adapt its questions, preserve uncertainty, and avoid forcing the target conclusion. Scenario-specific causal rules or a hardcoded winning hypothesis invalidate the gate.

## 13. Failure and disaster scenario matrix

Stage gates use multiple incident families so the product cannot overfit one demo.

| Incident family | Required evidence and behavior |
| --- | --- |
| Bad deployment or configuration | Change timing, rollout state, config comparison, runtime symptoms, alternative pre-existing failure check. |
| DNS or service discovery failure | Configured name, Service and EndpointSlice state, bounded DNS check, namespace and cluster scope, prior logs. |
| CPU, memory, OOM, disk, or node pressure | Baseline comparison, runtime termination and scheduling events, node/pod scope, causal-order challenge. |
| Database saturation, connection exhaustion, or lock | Database metrics or activity when available, application errors, dependency scope, change context, declared gaps when DB access is absent. |
| Downstream dependency degradation | Temporal dependency traversal, downstream health, propagation ordering, rejection when downstream is healthy. |
| Network partition, packet loss, TLS, or certificate failure | Bounded connectivity, DNS, endpoint and certificate state, network-source gaps, regional comparison. |
| Partial rollout or canary failure | Per-version and per-instance comparison, rollout distribution, image/config identity, affected cohort. |
| Regional or cloud-provider degradation | Cross-region scope comparison, provider status or cloud telemetry when connected, explicit inability when not connected. |
| Alert storm and cascading failure | Deduplication, conservative correlation, blast-radius grouping, fair queueing, no provider request storm. |
| Observability or connector outage | Freshness detection, alternate-source use, explicit gaps, no stale data presented as current. |
| Intermittent or flapping failure | Repeated windows, state transitions, uncertainty retention, no single-window overclaim. |
| Security event or suspected data corruption | Preserve evidence, avoid unsafe diagnostics, clearly escalate outside ordinary availability RCA, never propose destructive remediation. |

No single release must solve every family. Each release declares supported, partial, and unsupported families, and evaluation prevents regression in supported families.

## 14. Performance, scheduling, and failure behavior

Investigation load is isolated from alert evaluation, detection intake, notification delivery, and telemetry ingestion.

The scheduling path is detection intake, deduplication and correlation, investigation policy, durable priority queue, per-organization fair scheduling, per-connector concurrency, global provider concurrency, and bounded execution.

The Stage 1 scheduler uses a durable database queue as scheduling truth because eligibility, priority, fairness, delay reason, leases, and manual priority changes require transactional visibility. Kafka or Redpanda may wake workers, but a message is not the authoritative priority state. This decision must be revalidated against the current outbox and worker implementation before the slice plan is approved.

Required controls:

- Per-organization queued and running limits.
- Global provider request, token, and cost limits.
- Per-installation query rate and concurrency limits.
- Weighted fairness so one enterprise storm does not starve every other organization and small tenants cannot consume unbounded shared capacity.
- Priority based on severity, user-requested investigation, incident age, customer policy, and whether an active investigation already covers the signal.
- Storm clustering and duplicate suppression before model invocation.
- Backoff with jitter for provider and connector failures.
- Circuit breakers for repeatedly failing sources.
- Poison-result rejection before model context assembly.
- Idempotent tool execution and evidence creation.
- Lease recovery without repeating persisted successful work.
- Bounded snapshots, logs, query windows, context, and evidence growth.
- Visible queue and delay reasons; eligible investigations are never silently dropped.

When capacity is exhausted, OpenCluster continues durable detection and incident creation, queues eligible investigations, and shows the reason. Alerting remains operational even if every AI provider and connector is unavailable.

### 14.1 Stage 1 capacity assumptions and gates

The baseline storm test ingests 10,000 alert events in five minutes, producing up to 1,000 investigation-eligible incidents after normalization, deduplication, and correlation.

- Default global model concurrency is 20 and default per-organization investigation concurrency is 2; both are configuration with safe upper bounds.
- Connector concurrency and rate limits are installation-specific and must never be exceeded by retry or replay.
- Provider concurrency never exceeds the configured global maximum, including retries and fallback.
- When capacity exists, p95 scheduler dispatch overhead is below five seconds.
- A continuously eligible organization receives a scheduling opportunity within two fair-scheduling rounds when global capacity becomes available; priority may reorder work but may not create indefinite starvation.
- Queue position is not promised, but queued count, delay reason, queued-since time, and estimated service band are visible within ten seconds of eligibility.
- The 1,000-incident backlog drains at the configured sustainable throughput with no lost eligible request, duplicate active run, or unbounded retry amplification.
- Detection durability, alert evaluation, and notification delivery show zero correctness loss and no more than five percent latency regression from their pre-investigation storm baseline.
- Core alerting remains within its existing SLO when the provider is unavailable for the full test.
- Default bounds are 200 investigation steps, 100 accepted EvidenceItems, 1 MiB per bounded tool result before extraction, 256 KiB per evidence snapshot, and 10 MiB total retained snapshots per investigation. Exceptions require an explicit policy and storage-cost test.
- Load tests measure queue growth, database contention, connection-pool isolation, worker recovery, circuit breakers, snapshot growth, retention throughput, and backlog recovery after provider restoration.

The slice plan may revise numeric defaults after measurement, but it must preserve the stated storm shape, fairness property, visibility, and alerting-isolation gates and repeat plan review.

## 15. Security and trust boundaries

- The investigator receives tenant-scoped capabilities and never chooses its own authorization scope.
- Tool execution is read-only by default and server-enforced outside prompts.
- Investigation-created diagnostic pods are prohibited in Stage 1. Any future ephemeral diagnostic capability requires a separate plan, threat model, security review, and explicit change to the non-mutating product contract.
- Credentials and notification destinations are structurally unavailable to model context.
- Raw logs, labels, source objects, runbooks, and repository text are untrusted data and may contain prompt injection.
- Model-produced queries and evidence candidates pass validators and policy gates.
- Evidence snapshots are bounded, redacted, hashed, retention-classed, and access-controlled.
- Source deep links do not embed credentials.
- Cross-tenant retrieval, cache reuse, topology, and memory are prohibited by construction.
- Every model call, decision, tool execution, evidence validation, outcome change, and replay is auditable without storing private chain-of-thought.
- Data-egress behavior is documented separately for managed, self-hosted, evidence-only, and local-model deployments.

### 15.1 Model egress and untrusted-data contract

- Every tool-result field is classified as no-egress, metadata-only, redacted-content, bounded-content, or local-model-only.
- Deployment policy determines which classes may reach each configured provider and region.
- Pre-context processing removes or masks credentials, tokens, secret values, customer-defined PII patterns, and disallowed source fields before tokenization.
- Provider configuration records data-retention, training-use, regional processing, enterprise privacy, and fallback restrictions. A fallback provider may not receive data unless it independently satisfies the active policy.
- Source text remains tainted as untrusted through extraction and model output. Instructions, URLs, destinations, tool names, or authorization claims found in source text cannot become executable control fields.
- Model output is schema-validated and policy-checked. Any destination or query derived from logs, configuration, runbooks, tickets, or repository content is treated as attacker-controlled.
- Diagnostic destination enforcement resolves and pins addresses, validates IPv4 and IPv6, rechecks redirects, blocks DNS rebinding, and denies metadata, link-local, loopback, control-plane, private scopes outside the authorized environment, and unapproved public destinations.
- Network policy and Relay policy enforce destination restrictions independently of the model.

### 15.2 Stage 1 security acceptance

- Prompt-injection tests cover logs, labels, Kubernetes annotations, ConfigMaps, runbooks, source URLs, redirects, and model-generated queries.
- Secret and PII canaries prove disallowed values never enter provider requests, audit text, evidence claims, or source deep links.
- Cross-tenant authorization tests cover every seed, tool, cache, snapshot, topology, replay, and learned-knowledge path.
- Diagnostic SSRF tests cover IPv4, IPv6, alternate numeric forms, DNS rebinding, redirects, metadata services, and unauthorized external targets.
- Provider outage and fallback tests prove policy cannot be weakened silently.
- The Stage 1 threat model and data-flow review receive independent security approval before a design-partner deployment.

## 16. Revised stages

### Stage 0 — Existing trust blockers and plan approval

Objective: establish that alerting, delivery truth, ingestion, tenancy, retention, and worker recovery are trustworthy enough to host the investigator.

Acceptance:

- All unresolved P0 trust findings are re-audited against current code, not copied from stale plans.
- Current remediation plans are reconciled and missing references are corrected.
- No incident UI fabricates delivery or causal status.
- This plan is approved.

### Stage 1A — Truth, execution, and evaluation contracts

Objective: implement the domain boundaries before introducing model reasoning.

Acceptance:

- Observation through outcome layers are enforceable and API contracts are approved.
- `incident_signal` causal labels cannot enter investigator inputs as facts.
- Tool execution, evidence validation, hypothesis transitions, conclusion, versions, and replay inputs have explicit lifecycles.
- Queue, idempotency, recovery, authorization, retention, and snapshot bounds are tested.
- Incident correlation split, merge, supersession, late-signal, and scope-change behavior is approved.
- Correction, supersession, retention, tenant deletion, and crypto-erasure behavior is approved.
- No LLM is required for acceptance.

### Stage 1B — Real cross-layer evidence foundation

Objective: make the target configuration/DNS incident investigable without an LLM.

Scope:

- Native metric discovery and comparison.
- Thin Kubernetes runtime, logs, Service discovery, rollout, and selected configuration inspection.
- Continuous bounded Kubernetes revision capture for supported non-secret sources.
- Bounded DNS/connectivity through a pre-provisioned Relay probe.
- Evidence candidate production, validation, snapshots, provenance, and gaps.

Acceptance:

- The real target incident produces validated metric, runtime, log, topology/configuration, and change-timing evidence where available.
- At least one plausible hypothesis can be contradicted by evidence.
- Missing permissions or disabled diagnostics produce actionable coverage gaps.
- Negative Service-discovery evidence cannot be produced from partial, stale, unauthorized, or incomplete inventory.
- Secret values never enter revision capture, evidence snapshots, model context, or audit output.

### Stage 1C — Autonomous investigator

Objective: add one frontier reasoning model as the adaptive driver.

Acceptance:

- Meets every product success contract in section 3.
- Creates a new or materially revised hypothesis after a tool discovery.
- Challenges its leading explanation.
- Never concludes with uncited factual claims.
- Recovers from connector failure and can finish `INCONCLUSIVE` honestly.
- Investigator database and tool permissions are verified independently of prompts.
- Passes the prompt-injection, egress, cross-tenant, and diagnostic SSRF acceptance suite.

### Stage 1D — Live SRE experience and replay

Objective: make the active investigation immediately useful to a senior SRE and measurable by the team.

Acceptance:

- Running, pending, degraded, terminal, and inconclusive states pass the fifteen-second founder review.
- The target incident can be replayed without querying current production state.
- A deliberately weakened investigator version scores worse on the scenario rubric.
- The blinded adaptivity variants in section 12.2 pass without scenario-specific causal rules.
- Human acceptance, correction, remediation, recovery, and verification remain distinct.
- A senior SRE can inspect why every material claim and confidence label is shown.
- The senior-SRE usability protocol in section 10.7 passes.
- The capacity, fairness, storage, backlog, and alerting-isolation gates in section 14.1 pass.

### Stage 2 — Design-partner breadth and competitor benchmark

Objective: prove the investigator across real incident families and measure it against a Robusta/HolmesGPT baseline using equivalent source access and comparable model quality.

Acceptance:

- At least four materially different incident families are evaluated.
- At least two design partners run investigations on production or production-like incidents.
- The benchmark publishes scenario definitions, source access, model, time, cost, unsupported claims, evidence quality, gaps, and outcome accuracy.
- OpenCluster demonstrates at least one repeatable advantage in conclusion reliability, falsification discipline, replayability, or gap honesty.

### Later stages

- Connector framework hardening and Prometheus beyond native metrics.
- External trigger breadth.
- Loki and external log sources.
- Rich Kubernetes and multi-cluster runtime.
- Trace-derived service dependencies.
- GitHub, GitLab, CI/CD, and GitOps changes.
- Slack and incident-system delivery.
- Operational memory and learned investigation knowledge.
- Enterprise RBAC, SSO, audit, privacy modes, and air-gapped deployment.

Later sequencing is driven by design-partner coverage gaps and evaluation results, not a fixed integration checklist.

## 17. Competitive proof and moat

OpenCluster does not win because it is self-hosted, uses AI, shows evidence, or exposes tool calls. These are table stakes or reproducible features.

Potential compounding advantages:

1. A rigorous structured corpus connecting hypotheses, supporting and contradicting evidence, gaps, conclusions, outcomes, and versions.
2. A temporal Customer Production Model with source reliability and incident-time snapshots.
3. Environment-specific knowledge about informative checks, reliable queries, recurring false hypotheses, and useful investigation paths.
4. Outcome feedback from humans, remediation, recovery, and deterministic verification without conflating them.
5. Offline replay that measures whether investigator changes improve real historical incidents.
6. Private and self-hosted deployment that makes this loop available to regulated or sovereignty-sensitive customers.

The moat exists only if this loop operates continuously: incident, investigation, evidence, outcome, evaluation, improved policy, better future investigation.

### 17.1 Robusta comparison gate

OpenCluster may claim a specific advantage over Robusta only when an equivalent-access benchmark demonstrates it.

The benchmark must compare:

- Correctness of the most supported explanation.
- Material causes missed.
- Unsupported causal claims.
- Quality of rejected hypotheses.
- Whether the leading explanation was challenged.
- Coverage gaps disclosed.
- Incident-time evidence retained.
- Replayability.
- Time to first useful finding and terminal result.
- Tool calls, tokens, and cost.
- Human work repeated after receiving the result.

The initial intended advantage is not tool breadth. Robusta is currently stronger there. The intended advantage is investigation reliability: source-grounded evidence, explicit contradictions, falsification, incident-time replay, and honest gaps.

Before running the benchmark, the team preregisters:

- Robusta/HolmesGPT version, OpenCluster version, model/provider and model settings.
- Tool and source access, permissions, retention, topology, runbooks, and configuration available to each system.
- Equivalent time, token, cost, and tool-call budgets or the reason a difference is intrinsic to the product.
- Scenario truth, supported versus unsupported capability classification, blinded holdout cases, and prohibited scenario-specific hints.
- Repeated-run count and model-randomness handling.
- Primary metrics: false causal claims, material cause missed, supported-conclusion correctness, decisive gaps disclosed, and blinded senior-SRE usefulness.
- Secondary metrics: time to first useful finding, terminal time, cost, irrelevant calls, and replayability.
- Non-inferiority limits for time and cost so a small reliability gain cannot hide impractical latency or expense.
- A blinded review process in which reviewers do not know which system produced the artifact.

A superiority claim must name the tested incident families, environment, versions, and metric. “Better than Robusta” without scope is prohibited. A single favorable run or a cherry-picked scenario cannot pass the gate.

### 17.2 Resolve comparison gate

Resolve is treated as an established category leader with broader production proof, cross-system context, learning, and remediation. OpenCluster does not position as generally superior during the MVP. It competes first on inspectability, deployment control, evidence semantics, and measurable reliability for the supported incident families.

## 18. Ten-minute Staff Engineer demo

1. Show the real alert and affected production-like workload.
2. Open the incident while the investigation is running.
3. Show initial scope, available capabilities, hypotheses, and falsification conditions.
4. Show resource exhaustion rejected with metric and runtime evidence.
5. Show DNS failures discovered in current or previous container logs according to actual runtime state.
6. Show the configured hostname and absence or mismatch in Service discovery.
7. Show deployment timing and revision context.
8. Show the investigator testing an alternative namespace, alias, or dependency explanation.
9. Show the most supported explanation, contradictions, gaps, and exact provenance.
10. Replay the incident with a newer investigator version and compare quality without touching current production.

The demo fails if it relies on prewritten causal rules, fabricated evidence, hidden manual intervention, or a fixed hypothesis winner.

## 19. One-minute investor statement

OpenCluster is an autonomous AI SRE for teams that cannot trust a black box during production incidents. When an alert fires, it investigates across the real environment, tries to disprove its own hypotheses, preserves the evidence available at incident time, and gives the engineer the strongest supported explanation plus the gaps. Every outcome becomes an evaluated case that improves how OpenCluster investigates that customer's systems. The initial wedge is reliable read-only investigation; the compounding asset is a customer-specific production model and evaluation history that makes future investigations faster and more accurate.

## 20. Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Stage 1 becomes a connector-platform project | Implement only the thin runtime and diagnostics required by the target incident. |
| The agent confirms its first idea | Require falsification conditions and a disconfirming check before conclusion. |
| Deterministic validation becomes too rigid | Permit source-grounded and AI-assisted validated evidence with explicit origin. |
| AI-assisted evidence becomes untrustworthy | Require bounded source references, validators, snapshots, and visible validation class. |
| Wrong identity or topology creates a false causal path | Carry temporal provenance, freshness, conflict state, and verification requirements. |
| Human feedback is sparse or wrong | Keep multiple outcome sources separate and weight them by verification class. |
| Replay differs from the original incident | Use preserved inputs and capability state; never query current state silently. |
| Alert storms overwhelm providers | Deduplicate before investigation and enforce fair hierarchical concurrency. |
| Model or connector outages damage alerting | Isolate investigation compute and queues from core alerting and delivery. |
| Robusta closes planned feature gaps | Compete on evaluated reliability and customer-specific learning, not static feature claims. |
| The UI becomes an AI transcript | Persist structured decisions and render a focused investigation workbench. |
| Evidence storage grows without bound | Cap snapshots, classify retention, deduplicate content, and preserve hashes and summaries where appropriate. |

## 21. Context strategy

Files required before implementation planning:

- `plans/autonomous-ai-sre-master-plan.md`
- Current active remediation plan after Stage 0 reconciliation
- `docs/data-model.md` in the frozen .NET reference repository (OCluster/Zyrenn.ConsumerService)
- Current incident, evidence, signal, worker, API, and database migration modules
- Current frontend repository or branch containing Incident Detail and Incidents list
- `.codex/agents/tdd-guide.md` in the frozen .NET reference repository (OCluster/Zyrenn.ConsumerService)
- `.codex/agents/qa-engineer.md` in the frozen .NET reference repository (OCluster/Zyrenn.ConsumerService)
- `.codex/agents/security-reviewer.md` in the frozen .NET reference repository (OCluster/Zyrenn.ConsumerService)
- Relevant C#, API, performance, frontend, documentation, and git rules

Required searches before each slice:

- Existing incident, signal, evidence, worker lease, outbox, connector, and API contract patterns.
- Current migrations and enum mappings.
- Existing frontend DTO validation, state badges, query keys, polling, drawers, lists, and incident routes.
- Existing security grants, tenant scoping, retention jobs, payload redaction, and audit patterns.

## 22. Files and dependencies

This master plan authorizes no implementation files.

For each implementation slice, the planner must identify exact files after current-code inspection. No new package, model provider, connector library, Kubernetes client, graph library, or storage system may be added without updating the slice plan and repeating plan review.

The preferred architecture uses existing PostgreSQL, Kafka/Redpanda, worker, outbox, API envelope, frontend validation, and design-system patterns unless evidence shows they cannot meet the contract.

## 23. Test strategy for future implementation

The implementation must follow one failing test at a time.

Required behavior groups:

- Truth-layer state transitions and immutability.
- Evidence validation and rejected candidates.
- Tenant, authorization, redaction, and prompt-injection boundaries.
- Tool bounds, timeouts, output limits, idempotency, retries, and cleanup.
- Hypothesis creation, supersession, merging, contradiction, and falsification discipline.
- Confidence caps and conclusion citation guards.
- Queue fairness, storms, provider outages, connector outages, and recovery.
- Temporal topology, stale edges, conflicting sources, and incident-time snapshots.
- API semantic enums, bounded aggregates, ordered updates, and error codes.
- Running, terminal, pending, degraded, and inconclusive frontend states.
- Replay isolation from current production state.
- Disaster scenario evaluation and competitor-baseline comparison.

No logic implementation begins until the relevant behavior has a failing test and the slice plan is approved.

## 24. Ordered implementation planning steps

1. Re-audit Stage 0 trust findings against the current branch and reconcile active remediation plans.
2. Create and approve the Stage 1A slice plan with exact domain, migration, API, and test files.
3. Execute Stage 1A through RED, GREEN, and REFACTOR with independent security and plan-adherence review.
4. Create and approve the Stage 1B slice plan for the target cross-layer incident and thin Kubernetes capability.
5. Execute Stage 1B one evidence behavior at a time and prove the incident is deterministically inspectable without an LLM.
6. Create and approve the Stage 1C slice plan for one bounded investigator and one provider.
7. Execute Stage 1C one agent behavior at a time, including falsification, discovery-driven hypothesis creation, contradiction, and failure recovery.
8. Create and approve the Stage 1D slice plan for the investigation workbench, outcome signals, replay, and evaluation comparison.
9. Execute Stage 1D and run founder, senior-SRE, security, performance, and accessibility reviews.
10. Run the real target demo and historical replay acceptance gates.
11. Build the equivalent-access Robusta baseline and record measured strengths and weaknesses.
12. Update this master plan from evaluation and design-partner evidence before authorizing Stage 2.
13. Remove or archive the superseded `plans/investigation-platform-master-plan.md` in the frozen .NET reference repository (OCluster/Zyrenn.ConsumerService) after founder approval and decision-history migration so only one active implementation source remains.

## 25. Final founder verdict

Build OpenCluster, but judge it as an investigator rather than as an architecture.

The company has the beginning of a defensible product only when OpenCluster can reliably investigate the cross-layer configuration failure, reject reasonable alternatives, preserve and replay the evidence, disclose what it could not verify, and demonstrate through equivalent-access evaluation that its result is more trustworthy or more useful than Robusta's for the same supported incident family.

Until that gate passes, OpenCluster remains a strong architecture proposal behind established competitors. Product language, fundraising claims, pricing, and roadmap decisions must preserve that distinction.

---

## Plan review checklist

- Goal and product boundary are explicit.
- Stage 1 proves cross-layer adaptive investigation rather than metric summarization.
- Truth layers and causal authority are separated.
- Human feedback is useful but not mandatory confirmation.
- Tool, authorization, validation, and replay contracts are explicit.
- Topology collection and uncertainty are defined.
- Frontend first-viewport behavior is concrete.
- Disaster scenarios, storm behavior, and source outages are covered.
- Evaluation and competitor proof are acceptance gates.
- No code, pseudocode, function signatures, or implementation snippets are included.
