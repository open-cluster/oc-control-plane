# Architecture

The control plane owns durable Organizations, Integrations, Signals, incidents,
Investigations, findings, and audit records. One HTTP surface serves operator and inbound
integration requests; a separate gRPC listener accepts enrolled Relays.

An alerting integration produces a Signal. The control plane associates that Signal with
an incident, opens an Investigation, selects authorized read-only integration tools, and
records each read before producing findings that cite their evidence.

Slack and GitHub integrations contact their respective services from the control plane.
Kubernetes reads are dispatched to an Organization-scoped Relay installed inside the
customer boundary; the control plane contains no Kubernetes client and never receives
cluster credentials.

PostgreSQL is the only deployment database, reached through the storage boundary. Operator
authorization, file-backed secrets, sealed recoverable credentials, provider consent, and
Organization predicates remain explicit trust boundaries. See
[CONTEXT.md](./CONTEXT.md) for terminology and
[SECURITY.md](./SECURITY.md) for the threat and disclosure model.
