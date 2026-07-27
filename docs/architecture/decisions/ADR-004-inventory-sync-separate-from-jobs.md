# Inventory synchronization is separate from investigation jobs

Continuous inventory work — the Kubernetes workload revision ledger first, and later any
periodic change detection — does not travel the `relay_job` path. The control plane sends
an inventory synchronization policy once; the Relay schedules locally, diffs locally, and
pushes a delta only when something changed. Investigation jobs keep the durable, leased,
epoch-fenced path unchanged.

Routing both through `relay_job` was the simpler shape and was rejected on volume: at the
stated target of 100,000 Relays on a five-minute interval it produces roughly 1.2 million
fenced, leased, durably-recorded rows per hour, of which about 99 percent report no
change. Durable-truth-per-job is correct for investigation work — low volume, high value,
must never be lost or run twice — and wrong for work that is high volume and almost always
empty. Verified 2026-07-26: no periodic capture exists in either codebase, so this costs a
message type today and would have cost a schema and a scale incident later.

## Consequences

- Inventory deltas are at-least-once with a dedup key, not leased and fenced. Losing one
  tick is recoverable by the next full reconcile; losing an investigation job is not.
- Metrics, logs, and traces stay on demand during an investigation. Only change detection
  is synchronized.
- The Relay owns the schedule, so the control plane requests an interval and customer-
  authored local configuration sets a floor the server cannot go below — the same
  ownership rule already applied to destination allowlists and local hard caps.
- Configuration root is `inventory:`, not `discovery:`. `kubernetes.workload.discovery.v1`
  is an on-demand investigation capability, and an operator reading `discovery:` in a
  config file would reasonably expect it to configure that.
- Introducing nested configuration is a paradigm change for the Relay, which is env-var
  configured today. The committed golden-file configuration-reference gate applies either
  way.
