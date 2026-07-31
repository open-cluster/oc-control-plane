# An Investigation is a durable case; rounds are its bounded executions

Status: ACCEPTED (2026-07-31 — founder decision in the frontend architecture grilling session)
Amends: ADR-008 (which deferred any Incident entity), ADR-009 (an evidence plan is pinned per
round, not per case), ADR-011 (abstention is a round outcome that a later round may supersede)

An **Investigation** is the durable case for one operational episode: one stable identity, one
URL, one permalink, for the whole life of the failure. An **InvestigationRound** is one bounded,
immutable execution under pinned planner, model and policy versions. Reinvestigating adds a round;
it never creates a second Investigation.

The correction this records is that an alert firing for three hours must not produce thirty-six
investigations. The earlier model — one investigation per run — came from the manually triggered
first slice, where a human asks once and reads once. Generalised to external intake it fragments a
single operational failure across dozens of records, none of which accumulates what the previous
ones learned, and it rots every link anyone shared.

## The lifecycle

`Investigating → WaitingForChange → Reinvestigating → VerifyingRecovery → Resolved`

**The investigator does not run continuously merely because a threshold is still breached.** After
each bounded round it waits. A further round starts only on a meaningful trigger: a severity or
status change, an expansion of affected scope, a related signal, a deployment or configuration
change, new contradictory evidence, an operator-requested recheck, a sparse scheduled recheck, or
a resolution requiring recovery verification.

When the trigger resolves, a bounded recovery-verification round runs and the episode closes after
a quiet period. A rapid re-fire reopens the same episode; a later recurrence creates a new one,
linked to the previous.

`VerifyingRecovery` is deliberate. Most tools close when the alert clears, which records that
someone stopped being paged rather than that anything recovered.

## Consequences

- **The case pack is per round.** A case spanning three days has no single pinned input set, so a
  per-case pack could not satisfy the replay guarantee the term was defined to provide. Each round
  pins its own brief, evidence plan snapshot, control snapshot, planner and model versions.
- **The case file is a current projection, and supersession is visible.** When round 3 explains
  what round 1 abstained on, the earlier outcome remains in the case timeline with its attribution.
  Nothing is rewritten. This produces a genuinely differentiating artifact: *at 14:32 this was
  abstained for lack of node metrics; at 15:10, once the limit change was recorded, it became
  explained.*
- **Execution limits are per round and cumulatively per Investigation.** Per-round bounds alone
  leave a long episode with sparse rechecks unbounded in aggregate. `WaitingForChange` consumes no
  budget, and repeated notifications with no meaningful change start no round.
- **IncidentEpisode exists in the model before it exists in the data.** ADR-008 deferred Incidents
  and its sequencing stands: the first slice is manually triggered and consumes no Signals, so
  nothing can fragment yet. But the one-to-many between Investigation and InvestigationRound must
  be structurally correct from the first migration, because retrofitting it after rows exist is
  precisely the retrofit ADR-008 argued about in the other direction. In v1 one IncidentEpisode has
  one primary Investigation, and a manually started Investigation may use an implicit episode
  without surfacing the distinction.
- **This is not an incident-management product.** Only the minimal episode and deduplication model
  needed to stop repeated external notifications fragmenting one failure. No routing, no on-call,
  no escalation, no silences.
- **Shared links show current state with a case version and last-updated time; exports pin an exact
  case version and name the rounds included.** A link opened later must never quietly differ from
  what the sender saw without saying that it changed.

## Considered and rejected

**One Investigation per execution, with grouping above it.** The previous model. It puts the stable
identity on the wrong object: the thing an engineer shares, links and returns to is the case, not
the run, and an identifier that changes per run breaks every artifact that left the product.

**Continuous investigation while a trigger is firing.** Rejected on cost and on noise: it consumes
budget to rediscover the same thing, and it produces a stream of updates in which a genuinely new
finding is indistinguishable from a re-run.
