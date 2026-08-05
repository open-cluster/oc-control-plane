# Plan — completing the three partially-implemented specifications

Status: IN PROGRESS. Date: 2026-08-05. Repository: the Go control plane.
Specifications completed by this plan: `spec-live-model-provider.md`,
`spec-operator-api-identity-and-rbac.md`, `spec-signal-intake-and-incidents.md`.

This plan is the source of truth for the work. Where it disagrees with the specifications, the
specifications are the intent and this document records the departure with its reason.

---

## 0. What is already true and must not be rebuilt

Checked against the code on 2026-08-05, because three of the specifications' own status lines
understate what exists and building against them would produce a second implementation of
something already shipped.

- Intake is bounded in RATE as well as size. `internal/intake/ratelimit.go` runs before
  authentication. Story 19 of the intake specification is met; its status line says otherwise.
- Delivery health is an operator surface. Every attempt that reaches a real Connection is
  recorded with its disposition and, for a refusal, why. Story 13 is met.
- The human-initiated path exists. An operator opens a case naming a Connection, a scope and a
  window, over any window inside the seven-day bound, with no episode involved. Stories 3, 4 and
  30 are met.
- `internal/reasoning.Recorder` is complete and tested. What is missing is only its wiring into a
  live run.
- The `audit_event` delete trigger already admits a transaction that declares itself the pruner.
  What is missing is only the pruner.

## 1. Live-run transcripts

Closes the single outstanding requirement of `spec-live-model-provider.md`, stories 30 and 31.

**The defect.** `OC_MODEL_TRANSCRIPT_FILE` is read only when a deployment has no live provider, so
a live round records nothing. Three failures in the 2026-08-02 sweep cannot be read after the
fact, and the replay corpus the specification describes does not exist.

**The shape.** A recording is per ROUND, because a transcript is per round. One recorder shared
between two concurrent rounds would interleave them into a transcript that replays as neither.

Steps, in order:

1. In `internal/investigation`, declare two things next to the existing transcript vocabulary.
   `Transcribed` is a `Reasoner` that can also render what it heard, given the round's pinned
   versions. `Transcripts` has two methods: one that wraps the boundary for one round, and one
   that files a finished round's recording. Both live in this package because the round is this
   capability's, and the implementation lives in `internal/reasoning` because the recorder does.
2. Give `Runner` an optional `Transcripts` field. Nil records nothing, which is what every
   deployment did before this existed and what a deployment that configured no directory keeps
   getting.
3. Move the reasoner the round uses from the `Runner` onto the `round` struct. Every call site
   inside a round reads it from there. There are exactly three: the opening hypotheses call in
   `runner.go`, the adaptive-pass call in `runner.go`, and the conclusion call in `concluding.go`.
4. File the recording when the round ends, whatever it ended as. A round that FAILED is the one
   most worth reading, so filing must not be conditional on a conclusion. A failure to file is
   logged and never fails the round: the case is already durable and losing a recording must not
   lose an investigation.
5. In `internal/reasoning`, add the filing implementation: a directory that wraps with the
   existing `Recording` and writes one JSON file per round, named by the investigation and the
   round ordinal so a sweep's artifacts sort together. Write to a temporary name in the same
   directory and rename over, so a reader never sees half a file.
6. Add `OC_MODEL_TRANSCRIPT_DIR` to `internal/config`, validated as a writable directory at
   startup rather than at the end of the first round.
7. Wire it in `cmd/controlplane` ONLY when a live provider answers. Recording a replay would
   produce a copy of the recording being replayed, which is noise in a corpus that exists to hold
   what a model actually said.
8. Pass it through the scenario harness so a live scenario leaves its transcripts beside its other
   artifacts.
9. Add the variable to the README table.

**What this does NOT do.** It does not record what a FAILED model call said, because the
boundary returns an error rather than a document and there is nothing to record. What a transcript
of a failed round shows is the stage it reached — hypotheses but no passes, or passes but no
conclusion — which is exactly what the sweep could not answer.

## 2. The audit retention pruner

Closes the second half of story 21 of `spec-operator-api-identity-and-rbac.md`.

**The defect.** `organization_policy.audit_retention_days` is a schedule a tenant sets and nothing
applies. The surface reports `auditRetentionEnforced: false`, which is honest and is not the
product.

Steps, in order:

1. Add a storage function that deletes audit events older than a tenant's retention horizon. It
   must declare itself the pruner inside its own transaction, using a setting local to that
   transaction so the declaration cannot leak to another statement on the same connection. Any
   other transaction attempting a delete stays refused by the trigger.
2. Delete in BOUNDED batches and report how many rows went. An unbounded delete over a year of a
   busy tenant's record is a lock held long enough to stall the writes that are still arriving,
   and the writes are audit events.
3. Add a storage function listing the tenants that have declared a retention period, across every
   placement this deployment serves. A tenant with zero declares none and is skipped: zero means
   the product's default, which is to keep everything.
4. Add a pruner in `internal/audit` that walks that list on an interval. It is the control plane
   acting on its own behalf, so it names no principal and writes no audit event of its own —
   an append-only table that grew a row every time it was pruned would defeat the pruning.
5. Wire it into `cmd/controlplane` beside the investigation worker, on the same shutdown path.
6. Flip `AuditRetentionEnforced` to true, and only in the same change that makes it true.

**Bound stated rather than discovered.** Pruning is best-effort against the clock: a tenant that
sets thirty days sees rows older than thirty days removed on the next sweep, not at the instant
they cross the horizon. The surface reports the schedule, and the schedule is what is enforced.

## 3. IncidentEpisode and grouping

Closes stories 5, 6, 24 and 25 of `spec-signal-intake-and-incidents.md`, and gives
`Investigation.EpisodeKey` — declared since the first investigation migration and always empty —
its producer.

**The decision that makes this safe.** Grouping uses ONLY an identity the source itself provided.
Nothing is inferred from labels, so canonical resource identity is not on this path and the
failure mode is a split rather than a merge. For Alertmanager the source's own grouping identity
is its `groupKey`, which is derived from the customer's own `group_by` configuration: when two
alerts land in one episode it is because the customer's Alertmanager already decided they belong
together. A payload carrying no grouping identity produces one episode per alert, which is the
conservative failure: a wrong split leaves one redundant record, a wrong merge produces an
investigation with an incoherent scope.

Steps, in order:

1. New package `internal/incident`, owning the vocabulary: the episode, its status, the basis on
   which it was grouped, and the refusals. It declares its own `Store` interface and does NOT
   import persistence, which is what ADR-017 asks and is the shape the identity package's recorded
   debt says the next new package must follow.
2. New migration `0014_incident_episode.sql`.
   - `incident_episode`: identity, organization, environment, connection, the grouping key, the
     grouping basis, a title, a status, first and last seen, resolved-at, a signal count, the
     primary investigation, and the supersession fields. Organization and environment are checked
     against the Connection by the same composite foreign keys the signal table already uses.
   - The open-episode uniqueness is a PARTIAL unique index on the connection and the grouping key,
     restricted to open episodes. A resolved episode stops occupying its key, so the same failure
     next week opens a NEW episode rather than resurrecting the resolved record of the last one.
     This is the same rule the signal table already keeps and it is kept the same way: by the
     database, not by a read-then-write two concurrent deliveries could both pass.
   - `signal.episode_id`, nullable, referencing the episode.
3. Grouping runs inside `RecordDelivery`, in the transaction that writes the signals. A signal
   recorded without its episode would be grouped on a later delivery or never, and both are the
   history quietly changing.
   - Upsert the signal first and read back whether a row was actually written. The existing guard
     means a late redelivery of a firing after its resolution touches nothing; such a delivery
     must also open no episode, and doing the signal first is what makes that free.
   - A newly inserted signal takes the open episode for its grouping key, or opens one.
   - Recompute the episode's aggregate from its signals: the count, the first and last seen, and
     whether it is resolved. An episode is resolved when no signal in it is still firing.
     Recomputing rather than incrementing is what makes the record self-correcting.
4. The operator read surface, under `/operator/v1/organizations/{organization}/incidents`,
   speaking the one query contract in `internal/table` like every other listing:
   - list, filterable by environment, connection and status, sortable, cursor-paged;
   - one episode, carrying its grouping basis so a surprising grouping is explainable;
   - the signals grouped into it, paged.
5. Merging, as story 25's revisability. A merge points the absorbed episode at the surviving one
   and rewrites NOTHING: both keep their identities, their signals and their records, and the
   reason is recorded. It writes an audit event in the transaction that makes it.
   **Splitting is not built**, and the reason is that this grouping shape does not produce the
   error a split corrects: grouping is source-provided and errs toward splitting, so the
   correction an operator needs is a merge. A split becomes worth building when a customer's own
   alert grouping is observed to be wrong, which has not happened.
6. Two new permissions, `incident.read` and `incident.merge`. Read joins the set every looking
   role holds. Merge goes to the roles that already shape an investigation — Investigator,
   Responder and the two administrative roles — because regrouping decides what a case is about.
7. Opening an investigation may name an episode. When it does, the control plane checks the
   episode is this tenant's and reaches the same Connection the scope names, stamps the case with
   it, and records the case as that episode's primary investigation. A second investigation for an
   episode that already has one is REFUSED naming the first, which is the fragmentation ADR-013
   requires the model to prevent.

**Deliberately not built here, each with its reason.**

- Signal-triggered investigation. It needs a Signal's labels resolved to a namespace and a
  workload, which is canonical resource identity — one line of design, and the largest unsolved
  question in the product. `TriggerSignal` stays declared and unreachable.
- Storm shedding of investigations (stories 14 and 15). Nothing opens an investigation from an
  episode, so a bound on how many it may open would be code that never runs. Grouping is what
  bounds a storm in this build: twenty alerts the customer already grouped become one episode.
- Retaining the received payload (story 22). It is a customer-payload retention surface and needs
  a redaction design that does not exist in this repository. The grouping basis covers the risk
  this slice actually creates — a surprising grouping is explainable without holding the body.

## 4. Intake metrics

Closes story 27 of `spec-signal-intake-and-incidents.md`. The meter provider and the scrape
endpoint already exist; what is missing is the instruments.

Counters for delivery outcomes by disposition, for signals recorded, and for episodes opened,
updated and resolved. No organization label on any of them: that identity belongs on a span, and
at the stated scale a tenant label is a cardinality failure in any Prometheus-shaped backend.
The rule already has a named home in `internal/observability` and this follows it.

## 5. Not built, and not by omission

- **Break-glass access, story 32.** Four questions are unanswered: who may invoke it, over what,
  for how long, and who is told AT THE TIME rather than afterwards. The fourth has no answer
  available — this product has no notification surface — so what could be built is an escalation
  route that is audited and silent, which is worse than none. It needs a product decision and a
  place to send the alarm.
- **Recording a performed remediation, story 17.** It decides what an `OutcomeAssessment` is, and
  that belongs to the investigation-outcome slice. Building it here would fix the shape of a type
  from the wrong side.
- **The frontend's contract-drift test, story 34.** It lives in another repository.

## 6. Verification

- `go build ./... && go vet ./... && go test ./... -count=1` in this module, and `go vet ./...`
  in `test/e2e`. The race detector cannot run on the current development machine and that is
  stated rather than worked around.
- The gates must pass unchanged in intent: every new permission is reachable by a route, every new
  route is in the table, no package registers a route outside it, and every persisted enum this
  work adds is frozen.
- New behaviour is asserted where an operator could observe it: through the intake listener for
  grouping, and through the operator surface for reading and merging.
