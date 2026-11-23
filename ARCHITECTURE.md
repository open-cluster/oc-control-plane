# Architecture

OpenCluster has two execution boundaries:

- The control plane owns durable Organization-scoped product state, the public API,
  webhook intake, Investigation coordination, and audit events. It runs AgentScope for
  model calls, Tool scheduling, context compaction, and reply recovery.
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
   creates the Incident and, when an Agent is available, its automatic Investigation.
   Alert intake does not create a Conversation. Resolution occurs when no grouped Alert
   Event remains firing.
4. **Conversation and Investigation** separate continuity from execution. Web and Slack
   Messages still open Investigation turns during their Chat cutovers. Native checkpoints
   carry Tool results, compacted context, and the accepted visible reply across those turns.
   Independent Investigations own their execution checkpoints.
5. **AgentScope** selects reads from a Toolkit built from current authorization and
   verified Integration capabilities. OpenCluster checks current access on Tool admission.
   Tool arguments carry provider-specific input and an optional purpose.
6. **Relay or provider read** dispatches the selected Tool. Slack and GitHub reads leave
   from the control plane. Kubernetes reads become a Relay Job executed through the
   customer’s outbound session.
7. **Tool Run** receives a stable UUID before dispatch and records the Integration,
   purpose, stripped provider input, applied window, outcome, truncation, summary, and
   source identifiers. Agent execution work links Relay Jobs through their Tool Runs.
   The execution lease fences completion of those observations and scopes cancellation.
8. **Operational events** expose durable startup, Tool activity, and terminal outcomes.
   Tool completion activity follows the commit of its Tool Run. Incremental public model
   output is tracked in #234.
9. **Conclusion** generation uses a buffered structured request after the native reply
   and its checkpoint complete. An assigned Conversation turn needs its matching native
   assistant Message to publish an accepted reply. The transaction commits the Conclusion,
   terminal event, and any linked reply and promoted checkpoint under the execution lease.
10. **Cited Findings** refer to successful Tool Runs. Causal Findings state a mechanism.
    New citations use Tool Run UUIDs. Historical ordinal links continue to resolve while
    their evidence is retained.
11. **Action Proposals** contain a title, rationale, verification step, and optional
    evidence citations. They are recommendations for the operator.
12. **Delivery** exposes the structured Conclusion through the Investigation API.
    Slack presents the accepted summary and activity in its thread.

## Postmortem workflow

Resolving an Incident only marks it eligible; no draft and no model execution happens
automatically. An authorized operator requests the first draft. The separate Postmortem
domain gathers the Incident’s Alert Events and timestamps, Tool Runs, cited Findings,
Investigation events, linked Conversation messages, resolution state, and explicit
operator input.

The draft contains title, executive summary, impact, detection, timeline, root causes,
contributing factors, resolution, what went well, what went wrong, detection gaps, action
items, and open questions. Attributed user Messages remain labeled testimony. Missing
human facts appear as `Needs human input.` Differing eligible Investigation summaries
leave the executive summary for human review and add a reconciliation question.

Regeneration replaces the current draft with a higher audited revision. Human corrections
are audited and preserve explicitly supplied owners and deadlines. Review marks the draft
reviewed and records the reviewer and time.

## AI contract

The investigative reply uses a bounded task, authorized read Tools, and native context.
When several authorized Integrations offer an operation, its arguments require an
integrationId from the eligible sources. AgentScope receives the Conclusion schema in a
separate structured request. Recovery can repeat that request after a crash before commit
while reusing the completed investigative reply. Reads can repeat if interrupted before
AgentScope checkpoints a Tool batch.

OpenCluster validates the report shape, assessment and Finding combinations, and the
presence of a mechanism for causal Findings. PostgreSQL checks cited Tool Run ownership,
availability, and outcome in the completion transaction. These checks establish citation
provenance and report consistency. The operator reviews whether the evidence supports the
explanation.

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
