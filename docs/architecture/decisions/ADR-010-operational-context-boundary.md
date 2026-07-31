# Persist what decays; read live what does not

Status: ACCEPTED (2026-07-30 — founder decision in the architecture grilling session)

OpenCluster continuously persists exactly one class of operational context: **workload revision
and configuration-change history**. Everything else about a customer's infrastructure —
containment, placement, current state — is resolved live at investigation time. This is the line
that stops an investigation product from becoming a monitoring platform by accident.

The criterion is decay, not usefulness. Kubernetes containment is fully recoverable from a live
read at any moment, so caching it buys nothing and costs staleness. Change history is not:
Events expire on a default one-hour TTL, `revisionHistoryLimit` defaults to ten ReplicaSets, and a
ConfigMap change leaves no history at all. At 03:40, investigating something that began at 03:00,
"what changed?" — the most productive question in incident investigation — is unanswerable from a
live read unless somebody was recording. That is the whole justification for continuous
synchronization, and it applies to nothing else.

The change ledger travels the inventory-synchronization path of ADR-004, not `relay_job`.

## Topology is four graphs, not one

Containment (owner references, node and instance placement) is read live. Change (what deployed,
what configuration moved, when) is persisted. Ownership (which team, who is on call) is deferred.
Dependency — service A calls service B — is deferred entirely, and when it arrives it is **read
from an authoritative source the customer already runs** — distributed traces, a service mesh, an
APM service map — carrying that source's provenance and freshness.

Dependency edges are never inferred from Kubernetes Services, EndpointSlices or network
structure. Those objects describe what *can* be called, not what *is* called, and the cases where
they diverge — a dependency exercised only on the failure path, a client addressing a pod IP
directly, a queue that makes two coupled services look unrelated — are precisely the cases an
investigation turns on. A wrong dependency edge feeding a hypothesis ranker is a confident wrong
conclusion, which ADR-011 makes a design failure.

## Two rules

**Synchronize identity and meaningful change; never mirror runtime telemetry or mutable state.**
That a pod exists and that its image changed at 14:02 is identity and change. That it is currently
using 400 MiB is state. State is read on demand and persisted only once it becomes investigation
evidence. Every monitoring platform in history began by deciding to cache one state field.

**Persisted topology is a navigation index, never a citable fact.** It tells the planner where to
look. It never tells the investigator what is true. Any relationship a conclusion depends on is
revalidated live at investigation time and recorded as an Observation with its own timestamp and
provenance.

## Consequences

- A cached edge can never produce a false EvidenceItem carrying valid provenance, because a cached
  edge is not evidence. This closes the same failure class ADR-003 addresses, arriving by a
  different route.
- Containment resolution costs a Relay round trip on every investigation. That is the price of
  guaranteed freshness and it is cheap.
- A resource inventory kept fresh by delta sync is not built now. It becomes worth building when a
  second Connection exists and scope resolution has to span sources.
- Coverage — what an Environment can currently investigate — is a property of Connection
  capability and freshness, not of a mirrored resource table.
