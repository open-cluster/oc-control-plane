# Spec — Slice 0: Go control-plane foundation

Status: IMPLEMENTED 2026-07-26 — built, reviewed, and pushed
Date: 2026-07-26
Target project: `D:\Development\oc-control-plane`, module `github.com/open-cluster/oc-control-plane`
Decision records: ADR-002 (placement), ADR-003 (environment), ADR-005 (language), ADR-007 (trigger model)
Glossary: `CONTEXT.md`

## Problem Statement

All new backend work is Go, and there is nowhere to put it. The Go code that exists is
the Relay, which is a customer-installed capability executor with no database, no HTTP
server, and no tenancy — none of the machinery a control plane needs. The .NET control
plane has all of that machinery, but it is frozen as a reference implementation and its
data access cannot express the isolation tiers the product intends to sell: the connection
seam takes no organization, and twenty-eight non-test files bypass it entirely by
constructing connections from an ambient string.

So the first Go control-plane slice has a choice. It can begin with a vertical feature and
grow a foundation underneath it accidentally — inheriting the ambient-configuration
problem in a new language — or it can establish the foundation deliberately, once, before
any domain depends on it.

The specific things that are painful to retrofit and cheap to build now: resolving where
an organization's data lives rather than assuming one database; carrying organization
scope explicitly rather than ambiently; emitting traces and metrics from the first request
rather than instrumenting after an incident demands it; and shutting down without losing
in-flight work. Each of these is a week now and a quarter after twenty slices assume its
absence.

## Solution

A new Go project containing a control-plane process that starts, resolves an
organization's placement, applies its own migrations, serves health and readiness, emits
structured logs and traces and metrics, and shuts down gracefully — and contains no
business logic whatsoever.

The process is deliberately useless. Its entire value is that every later slice inherits
placement resolution, organization scoping, observability, migration discipline, and
shutdown semantics rather than inventing them, and that the gates which enforce those
properties fail the build from the first commit rather than being adopted later.

## User Stories

1. As the OpenCluster engineer building slice 1, I want a project that already starts,
   migrates, and shuts down cleanly, so that the first relay-registration slice is about
   registration rather than about process plumbing.
2. As the OpenCluster engineer building any later slice, I want placement resolution to
   already exist, so that I never write a query against an ambient connection and have to
   unpick it later.
3. As the OpenCluster engineer building any later slice, I want organization scope to be a
   required parameter on every store function, so that forgetting it is a build failure
   rather than a cross-tenant data leak discovered in production.
4. As the OpenCluster engineer, I want the build to fail when a package imports across a
   forbidden boundary, so that the architecture is enforced mechanically rather than by
   review attention.
5. As the OpenCluster engineer, I want quality linters enabled from the first commit, so
   that the class of defect that survived every green run in the Relay repository cannot
   repeat here.
6. As the OpenCluster engineer, I want the composition root to be directly testable, so
   that I can assert what the assembled process does rather than what its parts do in
   isolation.
7. As an operator deploying the control plane, I want it to fail immediately and name the
   offending configuration variable when configuration is invalid, so that a
   misconfiguration is a startup error rather than a runtime surprise.
8. As an operator, I want configuration errors to never echo secret material, so that a
   failed start does not write credentials into a log aggregator.
9. As an operator, I want secrets referenced by file path rather than carried in
   environment values, so that they cannot leak through process listings or diagnostic
   dumps of the environment.
10. As an operator, I want a liveness endpoint that reports only process health, so that a
    dependency outage does not cause the orchestrator to restart a healthy process.
11. As an operator, I want a readiness endpoint that reports unready when the database is
    unreachable, so that traffic is withheld until the process can actually serve it.
12. As an operator, I want readiness to recover without a restart once the database
    returns, so that a transient database outage does not require manual intervention.
13. As an operator, I want the process to drain in-flight requests on SIGTERM within a
    bounded deadline and then exit zero, so that a rolling deployment does not fail
    requests.
14. As an operator, I want migrations applied at startup under a lock, so that several
    instances starting simultaneously cannot apply the same migration twice or race each
    other into a corrupt schema.
15. As an operator, I want to see which migrations were applied at startup and which were
    already present, so that a deployment's schema effect is visible without querying the
    database.
16. As an operator, I want a metrics endpoint in the format my cluster already scrapes, so
    that I do not have to introduce a new collection path to monitor the control plane.
17. As an operator, I want the process to emit its own version and build identity, so that
    I can confirm which artifact is actually running.
18. As an on-call engineer, I want every log line for a request to carry the same request
    identifier, so that I can reconstruct one request from interleaved concurrent logs.
19. As an on-call engineer, I want a request's trace to reach the database span, so that I
    can see whether latency was spent in the control plane or in Postgres without adding
    instrumentation during the incident.
20. As an on-call engineer, I want logs to carry the trace identifier, so that I can move
    between a log line and its trace without correlating by timestamp.
21. As an on-call engineer, I want structured logs rather than formatted strings, so that I
    can filter by field instead of by substring.
22. As an on-call engineer, I want metrics to stay low-cardinality, so that a tenant-heavy
    deployment does not make the metrics backend the outage.
23. As an on-call engineer, I want per-organization detail available on traces rather than
    metrics, so that I can investigate one tenant without paying cardinality for all of
    them.
24. As an enterprise customer, I want my organization's data to live in a database
    dedicated to my organization, so that my isolation requirement is met by placement
    rather than by a query predicate.
25. As an enterprise customer, I want the same product binary to serve me as serves the
    shared tier, so that my deployment is not a fork that lags on fixes.
26. As a customer with a data-residency requirement, I want my organization's placement to
    determine the region its data lives in, so that residency is a configuration fact
    rather than a promise.
27. As a security reviewer, I want organization scope to be structurally required rather
    than conventionally applied, so that isolation does not depend on every author
    remembering a predicate.
28. As a security reviewer, I want an unknown organization to produce a typed error rather
    than falling back to a default connection, so that a lookup failure cannot silently
    serve one tenant another tenant's placement.
29. As a security reviewer, I want the dependency and vulnerability scans to fail the build
    rather than warn, so that a known-vulnerable dependency cannot reach a release.
30. As a security reviewer, I want a secret scan over the full history before the first
    push, so that the repository never becomes the thing that leaks a credential.
31. As a security reviewer, I want dependency licences checked as a hard gate, so that a
    copyleft dependency cannot enter a repository intended to stay closed.
32. As a contributor, I want a single command that reproduces every CI gate locally, so
    that I discover a violation before opening a pull request rather than after.
33. As a contributor, I want the project layout to follow the official Go module guidance,
    so that I can find things without reading a layout document.
34. As a contributor, I want packages named for the domain rather than the layer, so that I
    read one package to understand one concept rather than three.
35. As a contributor, I want dependencies constructed explicitly rather than resolved from
    a container, so that I can follow what the process actually builds by reading it.
36. As a contributor, I want transaction ownership visible in a function signature, so that
    I can see where a transaction begins and ends without tracing runtime behaviour.
37. As a contributor, I want generated code committed and drift-gated, so that a build does
    not silently depend on a generator version I do not have.
38. As the founder, I want the foundation to encode the placement decision from ADR-002
    before any domain assumes its absence, so that adjusting for an enterprise customer is
    configuration rather than a migration.
39. As the founder, I want the Go repository's gates to be at least as strict as the
    Relay's from the first commit, so that the second Go codebase does not repeat the first
    one's lint gap.
40. As the founder, I want slice 0 to contain no business logic, so that its review is
    about the foundation and cannot be diluted by feature discussion.
41. As the founder, I want the process to run against a real Postgres in its own tests, so
    that "it starts and migrates" is demonstrated rather than asserted.
42. As the founder, I want observability present from the first slice, so that the first
    production incident is diagnosed with data rather than with a retrofit.

## Implementation Decisions

**Project and module.** A new Go project at `D:\Development\oc-control-plane`, module path
`github.com/open-cluster/oc-control-plane`, private repository in the `open-cluster`
organization. Clean history, no file copied from the .NET repository — shared concepts are
re-authored. The Relay's module-path lesson applies: the module path must match the
repository serving it from the first commit, not after the first push.

**Layout.** A single binary under a command directory, with all packages internal:
configuration, observability, storage, tenancy, identity, relay, signals, incidents,
investigations, and an API package. A protocol directory holds the control plane's own
contract; the Relay protocol is not duplicated here. Generated code is committed and
drift-gated. There is no `pkg` directory — everything is internal until something outside
genuinely needs to import it, which for a private control plane is never.

Packages are named for the domain, not the layer. The Application, Domain, and
Infrastructure trichotomy that the .NET modules use is not carried over; it is a C#
convention that produces three files per concept and reads as noise in Go.

**Composition.** Dependencies are constructed explicitly in the composition root. No
dependency-injection container, no service-collection extension methods, no
options-binding by convention. The entry point is thin and delegates to a function that
returns an error, so the assembled process is directly testable.

**Configuration.** One validated struct, loaded once through an injectable lookup so tests
supply values without touching process environment. Validation fails on the first problem
and names the offending variable. No environment value may carry a secret; secrets are
referenced by file path. This mirrors the Relay's configuration package, which is the
in-house reference.

**Placement.** Per ADR-002, where an organization's data lives is resolved from the
organization, never ambient. The connection seam takes the organization and returns a
connection for that organization's placement. Placement covers database, and is designed to
extend to object storage, region, and model provider without changing the seam's shape. A
lookup that does not resolve produces a typed error; it never falls back to a default
connection. One implementation returns a shared pool, so the shared tier behaves exactly as
a single-database deployment would.

The three tiers the seam must serve without a code path per tier: shared control plane and
shared database; shared control plane and dedicated database; dedicated control plane
deployment and dedicated database. The tier is a configuration value. The same binary
serves all three.

**Organization scope.** Every store function takes the organization scope as an explicit
parameter. It is never read from ambient context, never resolved inside a store, and never
optional. This is enforced structurally, not by convention.

**Transactions.** The use case opens the transaction and passes it down. Store functions
never open their own connection. Transaction boundaries are visible in signatures. This is
the direct correction of the .NET pattern where twenty-eight non-test files construct
connections independently and no caller can see where a transaction begins.

**Database access.** pgx as the Postgres driver, used natively rather than through a
generic SQL abstraction. Migrations are embedded in the binary and applied at startup under
a lock that serialises independently starting instances. Migrations are forward-only and
append-only; an applied migration is never edited. The schema begins at a fresh baseline
rather than carrying the .NET sequence forward, which is defensible only because no
persistent or design-partner deployment exists yet — this window closes the moment one does.

**Observability.** OpenTelemetry as the instrumentation API for both traces and metrics.
Traces export over OTLP. Metrics are instrumented through the OpenTelemetry API but exported
for Prometheus scrape, because a scrape endpoint is what Kubernetes operators expect and
what every customer already runs; an OTLP metrics exporter can be added later without
touching an instrumented call site. gRPC and pgx are instrumented through their standard
OpenTelemetry integrations so a request trace reaches the database span without bespoke work.

Logging uses the standard library's structured logger, as the Relay already does. Trace and
span identifiers are injected as log attributes for correlation. The OpenTelemetry logs
signal is not adopted in this slice; slog with trace correlation satisfies the requirement
and the Go logs SDK is the least mature of the three signals.

Organization identity appears on spans, never as a metric label. At the stated scale of
five thousand organizations, an organization label would be a cardinality failure in any
Prometheus-shaped backend. Exemplars connect an aggregate metric to a representative trace.

The telemetry backend is deliberately not chosen in this slice. OTLP is the wire format
precisely so the backend is swappable, and slice 0 can emit to a local collector.

**Lifecycle.** SIGTERM and SIGINT cancel the process context. In-flight requests drain
within a bounded deadline and the process exits zero. No goroutine outlives the process
context. Liveness reports process health only; readiness reports dependency reachability
and recovers without a restart.

**Health endpoints.** Liveness and readiness are distinct and never conflated. Readiness
failing must not cause a restart; liveness failing must.

**Gates.** The CI gate set matches the Relay's and adds what the Relay is missing: build,
vet, format, staticcheck, and golangci-lint with the quality linters enabled rather than
only the security profile — the Relay's narrow configuration is the reason two real defects
survived every green run there. Race detection runs with genuinely concurrent scenarios.
Vulnerability scanning, dependency licence checking as a hard failure, and full-history
secret scanning before any push. Actions pinned by commit digest. A local command reproduces
every gate.

**What is not built.** No domain logic, no HTTP or RPC handlers beyond health and metrics,
no authentication, no relay protocol implementation, no signal intake. Slice 0 assembles a
process and proves it is load-bearing.

## Testing Decisions

**What makes a good test here.** A test asserts what an operator or a caller could observe:
the process started, the schema advanced, a request was served, a placement resolved to a
different database, the process exited. It does not assert that a particular function was
called, that a particular interface was satisfied, or that a package's internals have a
particular shape. If a test would still pass after the implementation was replaced by a
different correct one, it is testing behaviour; if it would break, it is testing structure.

**One behavioural seam: the composition root.** The process is started in-process against a
real Postgres provided by Testcontainers, with a real HTTP listener on an ephemeral port and
real signal handling. Everything observable in slice 0 is reachable from there: configuration
validation, migration application, placement resolution, readiness transitions, trace
propagation, and shutdown.

No database mock, no fake clock, no interface per package. Seams are introduced only where a
real external dependency exists that cannot be run locally — cloud provider APIs, an external
OIDC issuer — and none of those are in slice 0. Placement correctness and migration
concurrency are precisely the properties a fake cannot demonstrate, and they are slice 0's
whole point.

**Prior art.** The Relay's composition-root tests exercise startup helpers with an injected
logger and configuration struct — the same shape, one level lower. The .NET API already
exposes its entry point specifically so the host can be started in-process for integration
tests, which is the identical pattern in the other language. The .NET test suite already uses
Testcontainers for Postgres and for k3s, so container-backed integration testing is
established practice in this project rather than a new dependency.

**Scenarios the seam must cover.**

- Valid configuration starts the process; each invalid value fails startup naming its
  variable, and no failure message contains secret material.
- Two organizations configured to different placements reach different databases; an
  organization with no placement produces a typed error and never a default connection.
- Migrations apply exactly once when two instances start concurrently against one database;
  the second observes the first's completion rather than racing it.
- Readiness reports unready while the database is unreachable and returns to ready without a
  restart once it recovers; liveness stays healthy throughout.
- SIGTERM drains in-flight requests within the deadline, exits zero, and leaves no goroutine
  running past the process context.
- A request produces a trace whose database span is a descendant, and every log line emitted
  during that request carries the same request identifier and the trace identifier.
- The metrics endpoint serves in the expected format and carries no organization label.

**Structural gates, which are not a behavioural seam.** Static checks over the syntax tree
and import graph, in the shape the Relay already uses and the .NET architecture tests
already establish: no exported store function omits organization scope; the API package does
not import the storage package directly; no database connection is constructed outside the
storage package. These fail the build and are immune to inline suppression directives.

**Goroutine-leak detection runs in the suite from slice 0**, so the discipline exists before
the first concurrent slice depends on it.

## Out of Scope

- Any business logic. Relay registration and sessions are slices 1 and 2; signal intake and
  incidents are slice 3.
- Authentication, authorization, and the identity boundary. ADR-006 governs those and they
  need the provisioning design first.
- The investigation truth layers, which stay in .NET and migrate last if at all.
- The control-plane-to-frontend contract. ADR and transport choice are deferred to the
  frontend rewrite deliberately, so the contract is designed with its client rather than
  before it.
- Migrating, deleting, or modifying any .NET domain. The .NET repository is frozen as a
  reference and receives critical bug and security fixes only.
- Dedicated control-plane deployment automation for the Enterprise tier. The tier is a
  configuration value in this slice; the deployment machinery is gated on the shared
  deployment being reproducible from CI first.
- Object storage for evidence snapshots. Required before scale, not before slice 1.
- Choosing a telemetry backend.
- The `Environment` entity from ADR-003. It groups connections, and there are no connections
  yet.
- Publishing anything. No registry push, image publish, or release upload, matching the
  Relay's posture.

## Further Notes

This slice is the foundation half of the strangler in `plans/go-strangler-migration.md`. It
is explicitly not a migration: nothing moves from .NET, and nothing in .NET is deleted. The
first genuine strangle is slice 1, and it is gated on proving R1 functionally against the
.NET reference first — the Relay has never spoken to a real control plane, and building both
halves in new code simultaneously would leave a failure attributable to either.

The schema re-baseline is free only while no persistent or design-partner deployment exists.
That is true today and is the reason this slice is worth doing now rather than after slice 1.

Four of the five identity surfaces remain provisional pending name clearance. The module
path is settled by the repository name; the protocol package, generated namespace, image
name, and chart name still rename together and must do so before the first persistent
deployment.

No prototype was built for this slice, so no snippet is inlined. The decisions above are
derived from the accepted architecture documents, the measured state of both repositories on
2026-07-26, and the audit of the Relay's Go code against Effective Go and production
concerns.
