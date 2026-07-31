# Packages follow business capability, and storage stays one package

Status: ACCEPTED (2026-07-31 — founder decision while clarifying the Connection model)

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
