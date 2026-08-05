# Implementation plan — the change ledger

Status: ACTIVE, written 2026-08-05 against `plans/spec-change-ledger.md` (READY FOR
IMPLEMENTATION). This document records the implementation decisions the specification left
open, and the two places this build deliberately deviates from what earlier documents
promised. It is corrected in place if the build deviates further.
Repositories: `opencluster-relay` (detection, diff, delta push) and this one (the ledger).
Decision records: ADR-004, ADR-010, ADR-003 as amended, ADR-016, ADR-017.

## The shape

The Relay detects changes locally and pushes deltas; the control plane keeps the ledger.
Nothing on this path is leased or fenced; deltas are at-least-once with a dedup key and a
redelivery collapses. The ledger answers one question for the Investigation brief: what
changed around this resource, in this window — as navigation, never as evidence.

## Protocol (Relay repository, `proto/opencluster/relay/v1/inventory_synchronization.proto`)

- New envelope variants, additive: `ControlToRelay.inventory_synchronization_policy` (field
  9) and `ControlToRelay.inventory_delta_ack` (field 10); `RelayToControl.inventory_delta`
  (field 10). `Heartbeat` gains a repeated `InventoryScopeStatus` (field 4).
- `InventorySynchronizationPolicy`: connection id, revision, requested namespaces (empty
  means everything local configuration allows), requested interval. The Relay floors the
  interval and intersects the namespaces; local configuration always constrains.
- `InventoryDelta`: relay-assigned delta id (the ack echoes it), connection id, policy
  revision, a baseline marker, the observation time (when the cluster was read), and a
  bounded list of object changes. An object change carries namespace, a closed kind enum
  (deployment, statefulset, daemonset, configmap, secret), name, UID, observed revision,
  a change kind (created, modified, deleted), and itemized field changes with before and
  after values.
- `InventoryScopeStatus` on the heartbeat: connection id, policy revision, last completed
  tick time, faulted flag, truncated flag. This is how freshness is confirmed without a
  message per empty tick: the heartbeat already flows, and a tick that found nothing
  changed still moves the stamp.
- No free text anywhere on this path: field values are images, quantities, names,
  versions and hashes. Secret and ConfigMap changes carry identity and resource version
  only; content never appears in any message. Field names avoid the schema gate's banned
  segments and the exact name `policy`.
- The contract stacks on the untagged `gen/go/v0.3.0`; this change is `v0.4.0` when
  tagged. The replace directives in both control-plane modules stay until then.

## Relay (`internal/inventory`, additions to `internal/kube`, `internal/config`, `internal/session`)

- Configuration root is `inventory:` in a YAML file named by
  `RELAY_INVENTORY_CONFIG_FILE`, parsed exactly as the redaction policy file is: known
  fields only, versioned, single document, fail loud. It holds an enabled switch, a local
  namespace allowlist for synchronization, and a minimum interval the control plane cannot
  go below. Absent file means enabled, namespaces constrained only by
  `RELAY_ALLOWED_NAMESPACES`, and a built-in floor.
- `internal/kube` gains an inventory reader: bounded one-page lists of Deployment,
  StatefulSet and DaemonSet specs reduced to declared intent (images, declared replicas,
  resource requests and limits, referenced ConfigMap and Secret names, generation, the
  controller revision annotation where present, and a hash of the pod template so a
  template change outside the itemized fields is still visible as a change), plus
  metadata-only lists of ConfigMaps and Secrets via the metadata client, so Secret values
  never enter Relay memory on this path. Status fields are structurally absent from the
  reduced type, which is what keeps ADR-010's line checkable.
- `internal/inventory` owns the snapshot type, the diff, and the synchronizer: one scope
  per Connection, policies applied from the stream, a local schedule that survives
  disconnects (detection continues while offline; deltas queue in a bounded pending
  buffer and are resent until acked, like unacked job results). The first successful tick
  for a scope in this process is a baseline. A pending-buffer overflow schedules a fresh
  baseline rather than dropping changes silently.
- The session's receive loop routes the policy and the ack to the synchronizer; the
  heartbeat carries the scope statuses; a per-connection flush loop enqueues pending
  deltas through the single sender.

## Control plane

- Migration `0015_change_ledger.sql`: `change_ledger_scope` (one row per Connection:
  policy revision, requested interval, covered-since, baseline-at, last-confirmed-at,
  faulted, truncated; environment persisted from the Connection, never joined) and
  `change_ledger` (append-only entries; organization, connection, environment, namespace,
  object kind, name, UID, observed revision, change kind including baseline, observed-at
  and received-at as separate clocks, itemized field changes as JSONB). Dedup is
  `UNIQUE (connection_id, object_uid, observed_revision)` with `ON CONFLICT DO NOTHING`;
  a deletion row carries an empty observed revision, which is naturally unique because a
  UID is deleted at most once. A deleted-and-recreated workload is a different UID and
  therefore a different object.
- `internal/changeledger` owns the vocabulary (entry, field change, object kind, change
  kind, scope status) per ADR-017; `internal/storage/change_ledger.go` reconstructs it.
  New persisted enums are appended to the frozen-enum gate table.
- The relay session handler gains a third durable case: an inventory delta is recorded
  through a guarded insert that verifies the Connection belongs to the organization, is
  served by this registration, answers evidence reads and is not disabled — then acked,
  record before acknowledge. Heartbeat scope statuses update the scope row's freshness.
- Baseline continuity: when a re-baseline arrives, the covered-since boundary is
  preserved only when every row collapsed against what the ledger already held AND the
  silence since the last confirmation is within two requested intervals. A collapse
  proves no watched field moved — declared-intent revisions only advance — but cannot
  prove an object was not deleted in the gap, so the unprovable window is bounded by the
  scope's own cadence rather than trusted at any length. Any insert, or a longer
  silence, moves covered-since to where watching demonstrably resumed. (The first draft
  promised a UID-set comparison instead; chunked baselines make that check unable to see
  the whole set, and the cadence bound is the honest substitute. A deletion during a
  within-cadence restart is still unrecorded — a known limit, recoverable by the
  cross-baseline inference deferred below.)
- On session greet, the control plane sends one policy per Kubernetes evidence Connection
  bound to the registration, upserting the scope row in the same step. The requested
  interval is deployment configuration with a product default of five minutes.
- Retention: a bounded pruner over `change_ledger` on its own schedule, default ninety
  days, following the audit pruner's shape. Purely by age — no row is exempt. An
  exemption for each object's latest row was considered and dropped: it makes table
  growth proportional to object churn instead of retention, and a pruned baseline is
  recoverable as a fresh one the next time a Relay observes the object, at the cost of a
  truthful discontinuity boundary. Corrected here from the first draft, which promised
  the exemption.

## The brief

- `investigation.Store` gains a ledger query: changes in the case's namespace through the
  case's Connection within the window, baseline rows excluded, capped and ordered, plus
  the scope's coverage boundaries and freshness.
- `Brief.RecentChanges` becomes the union of the live event-derived changes (as today)
  and ledger-sourced changes. `Change` gains a source discriminator; a ledger-sourced
  change carries no evidence identity and the views say so. The rendered prompt template
  is unchanged, so no prompt version bump.
- Two new gap causes, appended to the frozen enum: a window opening before the ledger's
  covered-since boundary records a ledger-horizon gap; a window extending past the last
  confirmed stamp (or a faulted scope) records a ledger-staleness gap. Both are recorded
  during briefing through the existing gap machinery.
- A ledger row is structurally refused as an EvidenceItem — it has no capability, no
  connection read and no trust class, and `Validate` already refuses all three. A test
  states that refusal so it cannot regress silently.

## Deviations, stated rather than discovered

1. `docs/architecture/hard-coded-in-the-first-investigation.md` promised the ledger
   "replaces `changeReasons` entirely". This build keeps the event-derived changes beside
   the ledger's, because the opening hypotheses would otherwise be blind wherever the
   ledger is cold — a fresh install, a first tick still pending, an outage — and because
   an event-derived change is the only citable form of one: the ledger is a navigation
   index whose changes must be revalidated live, and the events read is that
   revalidation. The hard-coded document is corrected to say so.
2. The specification's watched field set names "the controller revision, and the pod
   template hash". The controller revision for StatefulSets and DaemonSets lives in
   status, which this path must not read; what is recorded is the workload's generation
   (the declared-intent revision), the Deployment revision annotation where the
   controller maintains one, and a template hash computed Relay-side — which detects
   every template change, including the ones the itemized field set does not name.

## Out of scope here, recorded

- An operator HTTP surface for browsing the ledger or scope freshness. The brief and its
  gaps carry the honesty this slice needs; a browsing surface is its own slice.
- Cross-baseline change inference (diffing a new baseline against the ledger's last known
  state to recover changes made during an outage). The discontinuity gap states the truth
  instead; inference is a later refinement.
- Tenant-configurable retention and watched namespaces as a product surface.
- Everything the specification's own out-of-scope list names.

## Verification

- Relay suite: diff and snapshot behaviour, policy flooring and intersection, baseline
  semantics, pending-buffer overflow re-baseline, config parsing failures loud.
- Control plane suite: guarded insert refusals, dedup collapse, baseline continuity both
  ways, gap emission for horizon and staleness, evidence validation refusing a ledger
  row, brief union and ordering.
- End to end (`test/e2e`): a real image change in a real cluster becomes a durable ledger
  row naming the field that moved with before and after; pod churn and rollout status
  movement in the same window produce no rows beyond the baseline. Written so that
  widening the watched set makes the second half fail.
