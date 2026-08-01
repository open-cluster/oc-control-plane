# Architecture hardening

Status: COMPLETE 2026-08-01. Created the same day from an architecture review of a proposed
directory structure against ADR-016.

Four findings. All four are settled: three were built, and the fourth was a decision that is now
ADR-017. Each section keeps the reasoning that produced its outcome, including the two that were
blocked mid-flight, because what was rejected and why is the part that gets re-proposed.

| # | Finding | Outcome |
| --- | --- | --- |
| 1 | Persisted enum values duplicated as SQL literals | Two gates, verified by mutation |
| 2 | `internal/api` is named after a layer | Renamed `internal/health`; strangler table reconciled |
| 3 | `relay.go` and `job.go` past the navigability threshold | Split by file; sub-packages rejected |
| 4 | Domain vocabulary owned by `internal/storage` | ADR-017; applied incrementally |

No behaviour changed anywhere in this plan. Every item is structural, and the full suite was run
against real Postgres to prove it.

---

## 1. Freeze the persisted enum values and gate the SQL literals

Status: DONE 2026-08-01. Both gates live in `internal/gates/persisted_enum_test.go`. Verified by
mutation: inserting a constant into the job status block fails the first gate naming each
constant that moved, and altering one SQL literal fails the second naming the file and the value.

### What is wrong

`JobStatus` in `internal/storage/job.go` is an `iota` block: pending 0, leased 1, succeeded 2,
failed 3, cancelled 4. `ConnectionRole` in `internal/storage/connection.go` is trigger 1,
evidence 2, both 3.

Those numbers are also written as bare literals inside the SQL in the same package — `status = 1`,
`status = 0 OR status = 1`, `CASE WHEN status = 0 THEN 4`, `role IN (2, 3)`, `role IN (1, 3)`.
There are more than a dozen such sites across `job.go`, `connection.go` and `signal.go`.

Nothing connects the two. Inserting a value into the `JobStatus` block, or reordering it, shifts
every constant after it while every SQL literal keeps its old meaning. The compiler cannot see
it, the linter cannot see it, and no gate covers it. The rows already in the database keep the
old numbering either way, so the failure is silent and retroactive.

This is precisely the class of property `internal/gates` exists for: one a reviewer will not
notice being violated.

### What to build

Two gates in `internal/gates`, in a new file named after what it guards.

**Gate one — the values are frozen.** Assert that every `JobStatus` and `ConnectionRole` constant
equals the exact integer it has today. The test states, in its comment, that these values are
persisted in columns and appear as literals in SQL, so they are a storage contract and not an
implementation detail. A developer who reorders the block gets a failing build naming the
constant that moved.

**Gate two — the SQL uses only declared values.** Scan the non-test Go source of
`internal/storage` for SQL comparisons against the `status` and `role` columns, collect the
integer literals they compare to, and fail when a literal is not one of the declared constant
values for that column. This catches a typo'd or invented literal that gate one cannot see.

### How to build it, in order

1. Write a failing test for the detector used by gate two. Give it a fixture string containing a
   comparison against a value that is not declared, and assert the detector reports that value.
   Confirm it fails before the detector exists.
2. Write the detector until that test passes. It reads Go source, finds string literals
   containing SQL, and extracts integer literals compared against the two column names using
   equality, inequality and `IN` list forms.
3. Add a second detector test with a fixture where every literal is legal, asserting nothing is
   reported. This is what stops the gate passing vacuously.
4. Add gate one. It is a plain table of constant to expected value.
5. Add gate two, running the detector over the real `internal/storage` directory.
6. Both must pass against the tree as it stands. If either fails, the finding is real and is
   fixed before the gate lands.

### Rules this must follow

- The gate must fail rather than pass when it finds nothing to inspect. A gate that reads zero
  files or zero literals is a gate that has stopped working, and `requiredModules` in the same
  package already sets this precedent.
- It parses only production files, matching `loadPackages` in `internal/gates`, because a test
  fixture legitimately contains an illegal literal.
- No SQL is changed by this item. The gate records the coupling; it does not remove it.

### Done when

`go test ./internal/gates/...` passes, both gates run against real input, and the detector's own
tests prove it catches a violation and clears a clean file.

---

## 2. Rename `internal/api` to `internal/health`

Status: DONE 2026-08-01. Founder decision: rename, and reconcile the strangler plan's structure
table wherever ADR-016 has superseded it rather than only this row.

The conflict that prompted the decision is recorded below, because the reasoning is what a future
reader needs and the table it concerns is now annotated rather than rewritten.

`plans/go-strangler-migration.md` is ACCEPTED and its structure table reserved `internal/api` for
"Connect handlers over the frontend contract" — a surface that does not exist yet. Renaming the
module now takes a name that an accepted plan has allocated.

That table is stale in several other rows: it also names `proto/`, `gen/`, `internal/signals`,
`internal/incidents`, `internal/identity` and an `internal/investigations` that "exists empty
until then", and ADR-016 rejects the proto and generated-code directories outright and forbids
creating a package for a slice that is not being built. So the table predates ADR-016 and has
already been overtaken on several rows.

Whether `internal/api` is one of those rows is a decision, not a deduction. Two readings:

**Rename now.** The module holds liveness, readiness, metrics and the correlation middleware
those routes run under. When the frontend contract is built it becomes its own module named for
what it serves, and nothing has to move. This is what ADR-016's rule produces if applied.

**Leave it.** The accepted plan intends the frontend Connect handlers to land in this module, and
the current contents are the first occupants rather than the whole of it.

The second reading is what produces the outcome ADR-016 warns about — the frontend contract and
the health probes sharing a module because both are "an API". The first requires accepting that
the strangler plan's structure table is superseded by ADR-016 wherever they disagree, which is a
statement worth making explicitly since more than this one row is affected.

### If the rename is chosen

Directory to `internal/health`, package clause to `health`, file to `health.go`. Update the two
references in `cmd/controlplane` and the gate in `internal/gates`, renaming the gate itself and
the path it checks. Update the row in `README.md` and reconcile the structure table in
`plans/go-strangler-migration.md` with ADR-016 rather than only this row. No behaviour changes and
nothing moves into or out of the module.

### What is wrong

ADR-016 says a package is named after a business capability and never after a layer, and lists
the layer names that are banned. `api` is a layer name and is not on that list, so it was never
decided either way.

Three other modules already serve HTTP without being inside it — `internal/operator`,
`internal/intake` and, through the composition root, the relay endpoint. The name is the problem
rather than the contents: it invites the next HTTP surface to be filed under it, which is the
collection ADR-016 exists to prevent. ADR-016 already refers to this module as "the health
router" in the passage rejecting a `readiness` package.

### What is in the module

Liveness, readiness, the metrics handler mount, and the request-correlation middleware those
routes run under. Its only symbols used from outside are `Handlers` and `RequestIDHeader`.

### What to build

Rename the directory to `internal/health` and the package clause to `health`. Update the three
files that reference it: `cmd/controlplane/main.go`, `cmd/controlplane/main_test.go`, and the
gate in `internal/gates/gates_test.go`. Rename the gate itself from the API spelling to the
health spelling and update the path it checks. Update the package doc comment so it describes
the health and metrics surface rather than "the control plane's HTTP surface", which is the
phrasing that made the old name sound right.

### Rules this must follow

- Nothing moves into or out of the module. This is a rename, not a reorganisation.
- The gate that forbids this module importing `internal/storage` must still run and still pass.
  It is renamed, never deleted.
- No behaviour changes. Every existing test passes unmodified apart from the import path and the
  symbol qualifier.

### Done when

The module builds, the full suite passes, and no reference to the old import path remains
anywhere in the repository including comments and documentation.

---

## 3. Split the storage tree — DONE, by file rather than by package

Status: DONE 2026-08-01. Founder decision: split by file, not into sub-packages.

`relay.go` became `relay.go` (enrolment and credential, 255), `conflict.go` (contested identity
and its trail, 366) and `roster.go` (the operator's read, 109). `job.go` became `job.go` (the Job,
its refusal taxonomy and enqueue, 157), `lease.go` (claiming, adoption, release, sweep, 228),
`result.go` (recording an outcome, 141) and `cancellation.go` (115).

`page.go` (77) came out of both. It was the find that justified the exercise on its own: `Page`,
`pageLimit`, `ErrBadCursor` and the cursor codec lived in `relay.go` while `connection.go` and
`environment.go` were already using them, so shared paging vocabulary was filed under one
caller. The two bounds it uses were named `defaultRosterPage` and `maxRosterPage` and are now
`defaultPageSize` and `maxPageSize`, since all three listings share them.

Every production file in the package is now under 370 lines. No call site changed, both driver
gates are untouched, and the enum gate from item 1 fired on the new files during the split and
had to be told which enum governs each — which is the behaviour it was built for.

The reasoning that ruled out sub-packages is kept below, because it is the part that will be
re-proposed.

### Why sub-packages were rejected

### Why it was proposed

ADR-016's amendment names roughly six hundred lines as the point where one domain's queries stop
being navigable in a single file, and names `relay.go` and `job.go` as already being at it. They
are 772 and 601 lines. The amendment pre-approves the move: a sub-package under
`internal/storage`, taking an already-resolved handle from the placement entry point, with the
driver gate widening from an exact match on one package to a prefix over the storage tree.

### Why it cannot start

The amendment describes this as a move. It is not one.

Every storage function is a method on `*Placements` — around thirty of them, called from 91 sites
across `internal/relay`, `internal/intake`, `internal/connection`, `internal/environment`,
`internal/operator`, `cmd/controlplane` and the test suites. Go does not permit a package to
declare a method on a type owned by another package, so a sub-package cannot host
`EnqueueJob` while it stays a method on `*Placements`.

The split therefore forces a change to the calling shape at every one of those 91 sites. That is
an interface decision, and it was not made when the amendment was written because the obstacle
was not visible from the file sizes.

### The decision to make

Three shapes are available, and one of them is already ruled out.

**An accessor per domain.** The placement entry point gains a method returning a domain store
built with the already-resolved pool, and callers say what they want through it rather than
calling a method on the entry point directly. Placement resolution still happens in one place.
Cost: all 91 call sites change, and the change is mechanical but wide.

**Thin delegating methods left on the entry point.** The methods stay where they are and forward
into the sub-packages. Cost: zero call sites change. This is ruled out — it is a shallow layer
whose interface is as wide as the implementation behind it, which is the shape both ADR-016 and
the module vocabulary this review used reject by name.

**Leave it as one package and split further by file.** Cost: nothing changes structurally; the
navigability problem is addressed by smaller files rather than smaller packages.

### What must happen before any code

Choose the shape. If the accessor shape is chosen, this plan is replaced by a plan of its own
covering the call-site migration, and the driver gate is widened only once the first sub-package
exists — the amendment is explicit that a check loosened for a caller nobody has written yet is
a check that has been loosened.

---

## 4. Where the domain vocabulary lives — DECIDED, recorded as ADR-017

Status: DECIDED 2026-08-01. Recorded as
`docs/architecture/decisions/ADR-017-capabilities-own-the-domain-vocabulary.md`.

The decision: `internal/storage` is infrastructure and must not own the domain vocabulary. A
domain type belongs to the capability that defines its meaning, and persistence depends on those
types and reconstructs them. Signal to `internal/signal`, Connection to `internal/connection`,
the investigation vocabulary to `internal/investigation`, relay execution jobs to an explicitly
named relay-job capability.

**Applied incrementally and never as a repository-wide refactor.** New investigator vocabulary
follows it immediately; existing storage-owned types move only when an active slice already
touches them; no package is created before its slice. Nothing here licenses a migration pass
before the first valuable investigation exists.

The observation that prompted it is kept below, because the ADR states the rule and this states
what was actually in the tree when the rule was written.

### The observation

Twenty files outside `internal/storage` import it, and not only for persistence. They import it
for the vocabulary: `storage.Signal`, `storage.Job`, `storage.Connection`, `storage.ConnectionRole`,
`storage.RoleTrigger`, `storage.RoleEvidence`, `storage.LocalityRelay`.

The consequences are visible in single files. `internal/connection` is the module named after the
Connection concept, but the `Connection` type belongs to `internal/storage`. The `Adapter` seam in
`internal/intake` — whose stated purpose is that nothing downstream can tell which system
delivered an alert — is typed in terms of `storage.Signal`. `internal/intake/adapter.go` is 48
lines and contains both halves of the inconsistency: it keys its registry on
`connection.Alertmanager`, vocabulary owned by the capability module, and types the value as
`storage.Signal`, vocabulary owned by the persistence module.

ADR-016 defends `internal/storage` as not being the layer package it resembles, and that defence
holds for the driver: writing pgx SQL requires importing pgx, so capability-local queries would
mean capability-local driver imports, which the gate genuinely cannot tell apart from an illegal
one. The defence does not address vocabulary ownership, and the ADR does not mention it.

### Why it matters now rather than later

ADR-008 sequences the investigator next. Its vocabulary — EvidenceItem, Hypothesis, CoverageGap,
Completeness certificate, Abstention — carries behaviour and invariants, not just columns. By
current momentum those types land in `internal/storage` because that is where every other domain
type already is. Deciding after they land is more expensive than deciding before.

### The precedent that already exists

`internal/tenancy` is a vocabulary module carrying no I/O, with a gate asserting it depends on
nothing else in this module. Whatever is decided, the shape for a vocabulary module is already
established here and does not need inventing.

### What must happen before any code

An ADR recording whether domain types stay in `internal/storage` or move to vocabulary modules,
and if they move, which types qualify. It must state the rule a future reviewer applies rather
than only the conclusion, because the question recurs with every slice.

---

## Order

Items 1 and 2 are independent of each other and of everything else, and are built first. Items 3
and 4 do not start until their decisions are recorded. Item 4 should be decided before the
investigator slice begins; item 3 has no deadline attached to it.
