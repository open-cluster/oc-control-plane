# OpenCluster domain glossary

Use these terms in code, APIs, tests, and documentation. Each term names one product
concept; do not introduce a synonym for the same record.

## Organization

The tenant boundary. Every durable customer-owned record has an explicit Organization,
and every query predicates on its identifier. An Organization is never inferred from a
credential, Integration, or request body.

## User

A person who can sign in. A User may belong to several Organizations with a fixed Admin,
Editor, or Viewer role in each.

## Principal

The authenticated actor for one request. It carries the User or automation identity,
Organization memberships, source address, and request identifier used for authorization
and audit attribution.

## Integration

One configured installation belonging to an Organization, such as Production
Alertmanager or the platform Slack workspace. It holds non-secret configuration,
verification facts, and sealed or digested credentials where required.

## Tool

A bounded operation offered by an Integration. Its definition declares when it
is useful, when it is not, its arguments, permissions, and result shape. Every external
call states an operator-visible purpose and may name the visible hypothesis it tests.

## Alert Event

One normalized alert occurrence received through a webhook. Its text remains untrusted.
Redelivery is idempotent, and its source timestamps remain distinct from receipt time.

## Webhook Delivery

One accepted inbound webhook request and the operator-visible unit of retry. Its state is
accepted, processing, succeeded, or failed based on all durable work created by that
request. Internal work leases and attempts are not separate operator resources.

## Incident

The operational occurrence Alert Events belong to using the alert source’s own grouping
identity. An Incident is provisional grouping, not a causal claim. It resolves when none
of its Alert Events is firing.

## Investigation

One bounded evidence-gathering turn about an Incident or operator question. It progresses
from queued to investigating and terminates as needs_input, concluded, partial,
cancelled, or failed. It never resumes; a follow-up opens another immutable Investigation.

## Conversation

The multi-turn context a person talks to. It contains ordered Messages and the immutable
Investigations those messages opened. Prior cited Findings provide continuity without
copying raw source payloads or private model reasoning.

## Finding

A factual statement established by an Investigation and supported by Tool Run references.
Its kind is cause, trigger, contributing_factor, symptom, propagation, ruled_out,
unresolved, or observation. Causal Findings state the mechanism that produced impact.

## Tool Run

One attempted Tool execution, successful or failed. Its one-based ordinal is the citation
used within its Investigation. It records Integration, purpose, optional hypothesis,
arguments, bounded window, outcome, truncation, summary, and source identifiers.

## Action Proposal

A suggested mitigation, rollback, verification, permanent fix, or monitoring step. It
states rationale, risk, reversibility, approval requirement, verification procedure, and
supporting Tool Runs. OpenCluster does not execute it.

## Relay

The customer-side process that opens an outbound session to the control plane and executes
the closed Kubernetes capability protocol. It belongs to one Organization and keeps
cluster credentials inside the customer boundary.

## Relay Job

One durable Relay capability execution. It is leased and fenced so it is neither lost nor
recorded twice, and it names the Integration and Relay involved.

## Postmortem

One operator-triggered draft per resolved Incident. It draws from Alert Events,
Investigation events, Tool Runs, cited Findings, Conversation testimony, resolution state,
and explicit operator input. It remains draft until reviewed, and missing impact, owners,
deadlines, or resolution facts read `Needs human input.`
