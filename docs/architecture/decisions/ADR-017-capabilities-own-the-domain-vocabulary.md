# Capabilities own the domain vocabulary; storage is infrastructure

Status: ACCEPTED (2026-08-01 — founder decision, prompted by an architecture review of the
package layout). Extends ADR-016, which governs where packages sit but does not say who owns a
type.

`internal/storage` is infrastructure and must not own OpenCluster's domain vocabulary. A domain
type is owned by the business capability that defines its meaning. Persistence depends on those
types and reconstructs them; it does not declare them.

## What is wrong today

Twenty files outside `internal/storage` import it, and not only to persist. They import it for
the language: `storage.Signal`, `storage.Job`, `storage.Connection`, `storage.ConnectionRole`,
`storage.RoleTrigger`, `storage.RoleEvidence`, `storage.LocalityRelay`.

The inversion is visible in single files. `internal/connection` is the package named after the
Connection concept, but the `Connection` type belongs to `internal/storage`. The `Adapter` seam
in `internal/intake` — whose stated purpose is that nothing downstream can tell which system
delivered an alert — is typed in terms of `storage.Signal`. `internal/intake/adapter.go` is
forty-eight lines and holds both halves of the inconsistency: it keys its registry on
`connection.Alertmanager`, vocabulary owned by the capability, and types the value as
`storage.Signal`, vocabulary owned by the persistence package.

ADR-016 defends `internal/storage` against the charge of being a layer package, and that defence
is sound for the driver: writing pgx SQL requires importing pgx, so capability-local queries
would mean capability-local driver imports, and the import gate cannot tell a legitimate one from
a placement-bypassing one. That argument is about *connections*. It was never an argument about
*types*, and ADR-016 does not mention vocabulary ownership at all.

## The rule

A domain type belongs to the package named after the concept it expresses:

- `Signal` belongs to `internal/signal`.
- `Connection` and its role and locality belong to `internal/connection`.
- `Investigation`, `InvestigationRound`, `EvidenceItem`, `Hypothesis` and `CoverageGap` belong to
  `internal/investigation`.
- Relay execution jobs belong to an explicitly named relay-job capability, not to generic
  storage.

`internal/storage` keeps every connection pool, every query, placement resolution and the
migrations. It reads rows into the capability's type and writes the capability's type into rows.
Both gates in `internal/gates` are unaffected: the driver stays in one tree, and every exported
tenant-scoped function still takes the organization.

`internal/tenancy` already is this — vocabulary carrying no I/O, with a gate asserting it depends
on nothing else in the module. The shape does not need inventing, only applying.

## How it is applied

**Incrementally, and never as a repository-wide refactor.**

- New investigator vocabulary follows this rule immediately. There is no existing placement to
  preserve, and this is the decision's whole purpose: ADR-008 sequences the investigator next,
  and its types would otherwise land in `internal/storage` by momentum.
- Existing storage-owned types move only when an active slice already touches them.
- No package is created before its slice, which is ADR-016's rule and is unchanged by this one.
  `internal/signal` and `internal/investigation` do not exist yet and must not be created empty.

Nothing about this decision justifies a migration pass before the first valuable investigation
exists. A refactor that touches every capability to relocate types nothing is currently changing
would spend the schedule that ADR-008 exists to protect.

## Consequences

- The direction of dependency inverts for types: capability packages stop importing
  `internal/storage` to name their own concepts, and `internal/storage` imports the capability
  packages instead. Any import cycle this produces is a signal that a type is owned by the wrong
  capability, not a reason to move it back.
- A capability package that owns vocabulary and performs no I/O can carry the same
  no-internal-imports gate `internal/tenancy` has. Whether each one should is decided when it is
  built, not here.
- The move is visible in review as it happens, one slice at a time, rather than as one change
  nobody can read.
- The rule states the reasoning and not only the conclusion, because the question recurs with
  every slice: if the package named after the concept does not own the type, the concept is
  defined somewhere that cannot explain why.

## Considered and rejected

**Leave the types in `internal/storage`.** Rejected: it makes the persistence package the
authority on what a Signal or a Connection is, and the investigator's vocabulary — evidence,
hypotheses, coverage gaps, abstention — carries invariants and behaviour rather than columns.
Those belong with the capability that reasons about them.

**Move everything now, in one pass.** Rejected: it touches every capability to relocate types no
active slice is changing, and ADR-008 was written to stop exactly that kind of work being
sequenced ahead of the capability that has never been demonstrated.

**A single `internal/domain` package holding every type.** Rejected on ADR-016's grounds: it is a
layer package under a different name, and it would collect concepts whose only shared property is
being domain types.
