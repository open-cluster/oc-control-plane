# Packages follow business capability, and storage stays one package

Status: ACCEPTED (2026-07-31 — founder decision while clarifying the Connection model),
AMENDED the same day. The amendment records the conditions under which two of the rejections
below stop being right, so whoever proposes them next finds the trigger rather than the
refusal. See the **Amendment** section at the end.

A Go package in this repository is named after a business capability — `intake`, `connection`,
`environment`, `relay`, `operator` — and never after a layer. There are no `services`, `models`,
`repositories`, `interfaces`, `helpers`, `utils` or `common` packages, because a package named
after a layer collects everything in the system that happens to be that layer, which is the one
grouping guaranteed to have no cohesion.

**A provider adapter lives below the capability it implements**, not beside its siblings from
other capabilities: `internal/intake/alertmanager`, later `internal/intake/pagerduty`. This is
ADR-007's consequence made structural — a vendor payload shape exists only inside its own
adapter, and nothing downstream of normalization knows which system sent an alert. A package for
each adapter is what makes that checkable rather than asserted, because the payload type cannot
be named from outside the package that owns it.

**An interface is declared in the package that consumes it**, is small, and is introduced when a
second implementation or a genuine seam exists — never in advance of one.

**Composition stays in `cmd/controlplane`.** It is where the process is assembled and where the
composition-root tests live, and those tests are the declared seam for every slice this product
has shipped: real HTTP, a real database, no doubles. Moving wiring into an `internal/app` would
relocate the entire test suite to buy a shorter `main`.

## The exception, and it is the load-bearing one

**`internal/storage` owns every database connection and is not split by capability.** It is
divided into files by domain — `relay.go`, `job.go`, `signal.go`, `connection.go`,
`environment.go` — and stays one package, one import, one owner of the driver.

This is deliberate and is not the layer-package it superficially resembles. ADR-002 makes
placement — which database a tenant's data lives in — a value resolved from the organization and
never ambient. A connection pool built anywhere else is a pool that skipped that resolution, so
it is a tenant-isolation defect rather than a style preference, and it is invisible in review. An
import gate in `internal/gates` fails the build when any package other than `internal/storage`
imports the driver, and a second gate fails it when an exported storage function that reaches a
tenant's data does not take the organization explicitly.

A `postgres/repository.go` inside each capability package would put a pool one import away from
every handler and delete both gates. The cohesion it buys is real but small — the queries are
already grouped by domain in files — and the property it costs is the one the tiering model is
sold on.

## Consequences

- Adding a capability adds a package; adding a provider adds a package below it. Neither adds a
  layer.
- Adding a stored concept adds a file to `internal/storage`, not a package. When that package
  stops being navigable, the answer is to move a domain's queries behind a narrower boundary that
  still owns placement resolution — not to scatter the driver.
- The two gates stay green or the build fails. They are tests rather than lint rules because a
  lint rule can be suppressed inline and these are exactly the properties a reviewer will not
  notice being violated.
- No package is created for a slice that is not being built. An empty `investigation` or
  `incident` package would assert a shape before anything has pushed back on it, which is the
  mistake ADR-008 was written to stop repeating.

## Considered and rejected

**A `platform` package holding database, clock, identifier and telemetry.** Rejected: it is a
layer package under a different name, and it would collect four unrelated things whose only
shared property is being infrastructure. `internal/observability` already exists and is named
after what it does.

**Protocol definitions in `api/` in this repository.** Rejected: the protocol contract lives in
the Relay repository as its own Go module so a consumer can speak it without inheriting the
Kubernetes dependency graph, and the control plane consumes it at a pinned version. A second
proto surface here would have no consumer and two places to change a message.

## Amendment, 2026-07-31 (what would reverse two of these rejections)

The rejections above are right about the repository as it stands, and each was argued from a
fact that can change. A rejection whose condition goes unrecorded gets re-proposed every few
months and re-argued from memory, which is how a decision quietly decays into a habit. Two of
them carry conditions worth naming, and one distinction below was missing entirely.

**`internal/app` stays rejected while `main` is the only thing that assembles the process.**
The argument for it was never a shorter `main`, and refusing it on those grounds was right. The
real argument is that `package main` cannot be imported, so nothing outside `cmd/controlplane`
can start the process in-process. Two facts reverse the decision: a second command that needs
the same wiring, or a test package that must assemble the process without being `package main`.
Neither exists today, and until one does the composition-root suite is in the only place the
language allows it to be.

**What gets extracted then is one function, not a wiring package.** `Run`, taking the context,
the configuration, the log destination and the listener callback, with the whole assembled
process behind it, and `main.go` left holding signal handling and an exit code. The suite moves
with it unchanged, because it is already testing exactly that function through a name only a
`package main` test can reach. The shape that stays rejected under either trigger is the one
that exports a constructor per subsystem and leaves `main` to assemble them: that is the same
wiring at a longer import path, and it is shallow whichever fact made the move necessary.

**The driver is owned by one tree; `internal/storage` is currently the one package in it.**
Rejecting a `postgres` package under each capability is not rejecting sub-packages. When a
domain's queries stop being navigable in one file — around six hundred lines, which `relay.go`
and `job.go` are already at — the move is a sub-package under `internal/storage`, taking an
already-resolved handle from the placement entry point, and the driver gate widens from an
exact match on one package to a prefix over the storage tree. That is one line in
`internal/gates`, and the property survives it: placement is still resolved at the tree's
entry, and every pool is still built in one place.

A sub-package under the capability does not survive it, and that is the whole distinction. A
prefix can express "the storage tree". It cannot express "a package named postgres, under any
capability, whose handle came from the right place" — and a property the check cannot express
is a property that stops being enforced. The cohesion argument is identical in both shapes;
only one of them keeps a pool more than one import away from a handler.

**The gate is widened when the first sub-package exists, and not before.** A check loosened for
a caller nobody has written yet is simply a check that has been loosened.

**Files inside a capability package are named after domain nouns.** `round.go`, `evidence.go`,
`registration.go`, `session.go`, `churn.go` — never `service.go`, `repository.go` or
`models.go`. Layer names kept out at the package level re-enter at the file level, where they
are harder to see and do the same damage: `service.go` in eight packages is eight files with
nothing in common and a name that says nothing about what is in any of them.

`handlers.go` and `views.go` are not that, and they stay. They name a surface and the wire
contract that surface speaks, and the split is load-bearing for the reason each file already
states: a field renamed in a view is a consumer broken somewhere else, which is not true of the
handler beside it.

**`readiness` is not available as a package name.** The word is spoken for: the language
defines Coverage as capability readiness in an environment, a domain concept with rounds and
gaps behind it. HTTP readiness is a function the composition root hands to the health router,
and one function needs no package. A package called `readiness` would make the two
indistinguishable at every import site. When the domain concept is built, it is `coverage`.

## Second amendment, 2026-08-01 (the storage trigger fired, and what was done instead)

The amendment above named the condition under which `internal/storage` becomes a tree of
sub-packages, and the condition has now been met — `relay.go` reached 772 lines and `job.go`
601. Founder decision: **the queries were split into more files in the same package, and the
sub-package move was declined for now.** The gate was therefore not widened, which the amendment
already required.

Recording the refusal matters as much as recording the trigger did. The amendment exists so the
next proposer finds the condition rather than re-arguing from memory; without this section they
would find a met trigger, a pre-approved move, and no sign that it was considered and declined.

**What it costs, measured rather than estimated.** Every exported storage operation is a method
on `*Placements` — 32 of them — and Go does not permit a package to declare a method on a type
owned by another package. Moving the exported surface into sub-packages therefore changes the
calling shape at 32 sites outside this package and 71 including its own tests. The amendment
already implied this by saying the sub-package takes "an already-resolved handle from the
placement entry point", which is the accessor shape; what it did not do is quantify it.

The unexported helpers that take a `pgx.Tx` — `spendBootstrapToken`, `insertRegistration`,
`appendConflictEvent`, `upsertSignal`, `claimDelivery` — could move with no call-site churn at
all. If a sub-package is ever wanted, that is where it costs least and is worth trying first.

**What was done instead.** `relay.go` became `relay.go`, `conflict.go` and `roster.go`; `job.go`
became `job.go`, `lease.go`, `result.go` and `cancellation.go`; and `page.go` took the paging
vocabulary that had been sitting in `relay.go` while `connection.go` and `environment.go` were
already using it. All are domain nouns, per the file-naming rule above.

**What would reverse this.** The navigability argument is now answered by files, so re-proposing
sub-packages needs a different argument than file length — a genuine need to enforce something
at a package boundary that a file cannot, or a call-site cost that has fallen. `connection.go` is
430 lines and is the next file to approach the threshold; reaching it is a reason to split that
file, not a reason to revisit this.

**A note on what this decision does not touch.** ADR-017 makes `internal/storage` stop owning
the domain vocabulary. That is about who declares a type, not about how many packages hold the
queries, and neither decision constrains the other.
