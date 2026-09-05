# Architecture

OpenCluster has two execution boundaries:

- The control plane owns durable Organization-scoped product state, the public API,
  webhook intake, investigation coordination, model calls, and audit events.
- The Relay runs inside the customer boundary, opens an outbound gRPC session, and performs
  the closed Kubernetes read capability protocol. The control plane has no Kubernetes
  library and receives no cluster credential.

PostgreSQL is the only deployment database. Only the storage adapter accesses it, and
every customer-owned statement keeps the Organization predicate explicit.

## Alert Event to proposed action

The following path is the primary product walkthrough.

1. **Webhook intake** authenticates an Alertmanager delivery, bounds its body, normalizes
   it, and deduplicates redelivery by digest. Connected text remains untrusted data.
2. **Alert Event** records what fired, its source identity, source timestamps, labels, and
   annotations. Intake never decides a cause.
3. **Incident** groups Alert Events using the source’s grouping identity. The first alert
   creates the Incident, Conversation, and initial Investigation. Resolution occurs when
   no grouped Alert Event remains firing.
4. **Conversation and Investigation** separate continuity from execution. A Conversation
   keeps messages and prior cited Findings; each turn opens a new immutable Investigation.
5. **Offered tools** derive from enabled Integrations and their verified grants. There is
   no provider switch inside the investigation loop. Every external call uses a common
   envelope containing purpose, optional hypothesis ID, and provider-specific input.
6. **Selective preflight** runs only for incident work with exact safe identifiers. An
   exact namespace permits a Kubernetes event read; exact namespace, workload kind, and
   workload name permit a runtime read. OpenCluster never guesses identifiers or broadly
   prefetches Slack or GitHub.
7. **Relay or provider read** dispatches the selected Tool. Slack and GitHub reads leave
   from the control plane. Kubernetes reads become a Relay Job executed through the
   customer’s outbound session.
8. **Tool Run** records the attempt before it can support a conclusion: Integration,
   ordinal, purpose, hypothesis ID, stripped provider input, applied window, outcome,
   truncation, summary, and source identifiers. Results count against investigation
   budgets and may be cited.
9. **Visible hypotheses** are complete bounded snapshots published through a local
   semantic tool. Snapshots are versioned Investigation events, not private chain of
   thought and not a separate table. Progress events describe the operational read in
   flight.
10. **Cited Finding** names supporting Tool Run ordinals. A verified cause requires a
    confirmed causal Finding with a mechanism. Contradictions, missing telemetry, missing
    access, and unresolved assumptions remain explicit limitations.
11. **Action Proposal** distinguishes mitigation, rollback, verification, permanent fix,
    and monitoring. It states risk, reversibility, approval need, verification, and cited
    support. The API has no production-change execution route.
12. **UI or Slack reply** receives the same structured conclusion: status, summary,
    impact, Findings, hypotheses, Action Proposals, limitations, and whether human
    confirmation is required.

## Postmortem workflow

Resolving an Incident only marks it eligible; no draft and no model execution happens
automatically. An authorized operator requests the first draft. The separate Postmortem
domain gathers the Incident’s Alert Events and timestamps, Tool Runs, cited Findings,
Investigation events, linked Conversation messages, resolution state, and explicit
operator input.

The draft contains title, executive summary, impact, detection, timeline, root causes,
contributing factors, resolution, what went well, what went wrong, detection gaps, action
items, and open questions. Conversation messages remain labeled testimony unless system
evidence corroborates them. Missing human facts are rendered as `Needs human input.`

Regeneration replaces the current draft with a higher audited revision. Human corrections
are audited and preserve explicitly supplied owners and deadlines. Review marks the draft
reviewed and records the reviewer and time.

## AI contract

The reasoning input is split into independently versioned safety policy, task instruction,
structured Investigation bundle, native read tools, local hypothesis-update tool, and
result schema.

The conclusion decoder rejects invalid status combinations, nonexistent citations,
causal Findings without mechanisms, unsafe action approval claims, and invented impact.
Evaluation hard gates require zero secret leakage, missing citations, fabricated causes,
unsupported execution claims, and false verified-cause outcomes.

## Trust boundaries

- Authentication resolves a Principal; authorization checks its membership and compiled
  role permission before a domain handler runs.
- File-backed secrets are never environment values. Presentable credentials are sealed;
  inbound secrets are digests.
- Credential-shaped fields are mechanically removed from events, audit details, and logs.
- Provider content, webhook text, and Conversation messages remain untrusted throughout
  storage, prompt rendering, and UI responses.
- Model providers receive only the bounded Investigation context and tools authorized for
  that Organization.
- Application audit events and operational Tool Run provenance are distinct records: one
  attributes human mutations, the other explains what evidence the investigator read.

See [CONTEXT.md](./CONTEXT.md) for the product vocabulary and
[SECURITY.md](./SECURITY.md) for threat reporting and deployment guidance.
