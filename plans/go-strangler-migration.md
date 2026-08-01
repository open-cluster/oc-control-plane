# Go Control-Plane Migration — Plan

Status: ACCEPTED. Revision 3 records the founder decisions. Slices 0, 1 and 2 are implemented
and in CI; slice 3 is half done — intake to a Signal is built, incidents and grouping are not.
The sequence in section 6 ends there, so what follows slice 3 has no specification yet.
Date: 2026-07-26 (revision 3 — founder decisions: trigger model is external intake plus
human-initiated; a new clean Go control-plane repository is created; the .NET repository
becomes a frozen reference implementation; strangler migration by bounded vertical slice)
Decision records: ADR-005 (language), ADR-002 (placement), ADR-003 (environment),
ADR-006 (authentication), ADR-007 (trigger model)

## 1. Founder decisions carried by this revision

- Trigger model: normalized external alert intake is the primary production path;
  human-initiated investigation is supported alongside it. No general-purpose internal
  alerting engine is rebuilt.
- A new clean Go control-plane repository is created in the existing GitHub organization.
- The .NET repository becomes a temporary legacy and reference implementation. It receives
  critical bug and security fixes only. All new backend development happens in Go.
- Migration is strangler-style: foundation first, then one bounded vertical slice at a
  time, with explicit API and behavioural compatibility tests, incremental routing, and
  archival only after every production path is migrated and rollback is no longer needed.
- C# architecture is not mechanically translated. Every design is re-derived against
  idiomatic Go.

Freezing rather than deleting supersedes revision 2's step 2. Dead alerting and
observability code is never ported and never deleted — it dies when the repository is
archived. That removes an entire work item.

## 2. What the strangler costs here, honestly

The expensive half of a strangler is dual-running: routing live traffic, dual writes, and
rollback machinery. No design-partner or persistent deployment exists yet, so there is no
live traffic to route. The migration keeps the valuable half — a complete reference
implementation to test behaviour against — without paying for the rest.

This is a temporary property. It expires the moment a design partner installs, and the
plan's sequencing assumes it holds. If a deployment happens first, sections 7 and 8 of
revision 1 (rollout and rollback) come back into force unchanged.

## 3. Repository structure

**SUPERSEDED in part by ADR-016 (2026-07-31), reconciled 2026-08-01.** This table was written
in revision 3 before any of it was built. ADR-016 governs package layout wherever the two
disagree, and the rows it overtook are corrected below rather than left to be discovered by
whoever copies the table next. The rows it did not touch stand.

Module path follows the naming clearance already pending for the Relay; the control plane
carries the same provisional identity and renames in the same atomic change.

| Path | Contents | Status |
| --- | --- | --- |
| `cmd/controlplane` | The single binary. Thin main, `run(ctx) error` beneath it, explicit construction — no dependency-injection container. ADR-016 keeps composition and the composition-root tests here | Built |
| `internal/config` | One validated `Config` struct loaded once; secrets by file reference, never environment values | Built |
| `internal/observability` | `slog` setup, OpenTelemetry tracing, metrics registry. Present from slice 0, not retrofitted | Built |
| `internal/storage` | Placement resolution, connection pools, transaction helpers, embedded migrations | Built |
| `internal/tenancy` | Organization, environment, placement, and the tenant scope every query carries | Built |
| `internal/health` | Liveness, readiness, metrics, request correlation. **Was `internal/api`**; renamed 2026-08-01 because `api` names a layer, and ADR-016 forbids that. The frontend contract, when it is built, is its own module named for what it serves | Built |
| `internal/relay` | Relay control plane: registration, session, job store, leases, dispatch, recording | Built |
| `internal/intake` | External intake adapters and normalization into Signals. **Was `internal/signals`**; built under the name of the capability rather than the noun it produces, with one sub-package per provider per ADR-016 | Built |
| `internal/connection` | Connections and their secrets: the configured instances of an Integration | Built |
| `internal/environment` | Environments: the scope that groups Connections and bounds evidence | Built |
| `internal/capability` | The frozen, versioned contracts a Relay may be asked to execute | Built |
| `internal/operator` | The cross-tenant read surface, behind its own token and its own listener | Built |
| `internal/gates` | Build-failing architecture checks | Built |
| `internal/identity` | Canonical principal; Clerk and OIDC adapters behind one boundary | Not built. ADR-006 is a decision with no implementation specification |
| `internal/incident` | Incident grouping and lifecycle. Singular, per ADR-016 | Not built. Deferred by ADR-008 |
| `internal/investigation` | The truth chain. Singular, per ADR-016. Owns the case, its bounded rounds, the brief, the truth chain from Observation to EvidenceItem, hypotheses, coverage gaps, outcomes, the runner, the model boundary and the read models — the capability owns its own vocabulary, per ADR-017 | Built 2026-08-01. It was created with its slice and not before, which is what the void clause required |
| ~~`proto`~~ | — | **Rejected by ADR-016.** The protocol contract lives in the Relay repository as its own module and is consumed at a pinned version; a second surface here would have no consumer and two places to change a message |
| ~~`gen`~~ | — | **Rejected by ADR-016**, with `proto` |
| `deploy` | Helm chart and manifests | Not built |
| `docs` | Architecture, contracts, operations | Built |

No `pkg` directory. Everything is `internal` until something outside genuinely needs to
import it, which for a private control plane is never. This matches the Relay repository
and the official module-layout guidance, and deliberately rejects the widely-copied
unofficial layout that invents `pkg` by default.

Packages are named for the domain, not the layer. There is no Application, Domain, and
Infrastructure trichotomy inside each package — that is a C# convention that produces
three files per concept and reads as noise in Go.

## 4. Idiomatic Go rules that replace C# patterns

These are the specific translations to refuse, recorded so review has something concrete
to check against.

- **Repositories.** The .NET side declares roughly forty repository interfaces, each with
  one implementation. Go declares an interface only where the consumer genuinely
  substitutes it. A bounded area gets one concrete store type; interfaces are declared by
  the consumer, small, and often single-method.
- **Transaction ownership is explicit and visible in the signature.** The use case opens
  the transaction and passes it down; store functions never open their own connection.
  This is the direct fix for the .NET pattern where twenty-eight files construct
  connections independently and no caller can see a transaction boundary.
- **Dependency injection is construction.** Everything is wired explicitly in `run`. No
  container, no service-collection extension methods, no options binding by convention.
- **Configuration is one struct, validated once**, not a class per section discovered at
  startup.
- **Errors are values.** Typed sentinel and struct errors with `errors.Is` and `errors.As`,
  wrapped with `%w`. No exception-shaped control flow.
- **Concurrency is owned.** Every goroutine has a named owner, a bounded lifetime, and a
  termination path. Worker pools are bounded; unbounded `go` statements are a review
  failure. The Relay's session package is the reference for this in-house.
- **Context is propagated, never stored in a struct.** Every I/O boundary takes a
  `context.Context` first parameter and honours cancellation.
- **Tenant scope is a parameter, never ambient.** Every store function takes the tenant
  scope explicitly. An architecture test fails the build on any exported store function
  that does not, in the same spirit as the Relay's import-graph gates.

## 5. Shared contracts

Three contracts, three owners, no overlap.

**Relay to control plane.** Protobuf under `proto/opencluster/relay/v1` in the Relay
repository, unchanged and still authoritative. With both sides now in Go, the control
plane consumes it as an ordinary Go module dependency rather than a vendored copy. The
copy-plus-manifest-plus-descriptor synchronization existed only because C# could not
consume a Go module; it is retired with the .NET consumer. `buf breaking` continues to run
in the Relay repository, which remains the single gate.

The generated contract is published as a module of its own, nested inside the Relay
repository at its generated-code directory and versioned with its own tags. The Relay
module proper depends on client-go because it reads clusters, and a control plane that
merely speaks the protocol must not inherit that graph into its own requirements,
vulnerability report and license inventory. Because the module path is the directory that
holds it, the split changed no import path. Two gates hold the boundary: one in the Relay
repository fails if the contract's transitive imports ever reach Kubernetes, and one in the
control plane fails if anything other than the contract module is required from the Relay.

**Control plane to frontend.** A new contract, owned by the control plane's own `proto`
directory. Because the frontend is a greenfield rewrite and Buf is already in the
toolchain, the recommendation is Connect: one IDL generating both the Go server and the
TypeScript client, browser-compatible over plain HTTP without a proxy, and able to serve
Connect, gRPC, and gRPC-Web from one handler. The honest caveat: enterprise buyers and
third-party integrators expect REST, so a REST surface is added later for the public API
rather than being the first-party frontend transport. That decision should be taken with
the frontend rewrite, not before it.

**External intake to control plane.** Not a contract OpenCluster owns — Alertmanager,
Grafana, Datadog, and PagerDuty each define their own payload and signing scheme. Each is
an adapter that verifies its own signature, deduplicates by its own event identity, and
normalizes into one internal Signal. Adapters are the only place a vendor payload shape
exists; nothing downstream of normalization knows which system sent the alert.

**Slack.** A fourth intake surface, later. An engineer mentioning the app to request an
investigation implies resolving a human-supplied name to a canonical resource, which is
the identity-resolution problem already flagged as unsolved. Slack intake should not be
scheduled until canonical resource identity exists, or it will silently investigate the
wrong thing.

## 6. Slice sequence

**Slice 0 — Foundation.** No business logic. Repository, module, CI mirroring the Relay's
gate set, configuration, structured logging, tracing and metrics, Postgres access with
placement resolution from ADR-002, embedded migration runner, tenant scope, health and
readiness, graceful shutdown. Acceptance: the service starts, resolves a placement,
applies migrations, serves health, and shuts down cleanly with no leaked goroutine, under
race detection and the full lint and vulnerability gate set.

**Slice 1 — Relay registration.** `RelayRegistrationService.Register` in Go, owning
`relay_registration`. Chosen first because a complete, reviewed .NET implementation exists
to differential-test against, the Protobuf contract already exists, its peer is already
Go, and it is one table.

Prerequisite, split deliberately: R1 must be proven FUNCTIONALLY on .NET — a Go Relay
registers, sessions, executes a job, and has a result recorded end-to-end — because the Go
Relay has never spoken to a real control plane, and building both halves in new code
simultaneously means a failure could be in either. That validation is the point.

R1's formal exit review and the Kestrel-specific half of the R1-F edge gate are NOT
prerequisites and should be skipped. R1-F tests middleware compatibility, HTTPS-redirect
exemption, ALPN behaviour, trailer preservation, and keepalive against Kestrel's HTTP/2
limits specifically — findings about a server being deleted. Edge feasibility is re-run
against the Go server, where the answers will actually apply. Spending an exit review on
code scheduled for replacement buys nothing.

**Slice 2 — Relay session and jobs.** `RelaySessionService.Connect`, durable job store,
server-clock leases, lease epochs, stale-result fencing, idempotent recording, dispatch,
and the session registry. Owns `relay_job`. This slice exercises every production concern
in one place — bidirectional streaming, transaction ownership, idempotency, tenant
isolation, bounded concurrency, cancellation — against a reference implementation that
already passed three review rounds.

**Slice 3 — Signal intake and incidents.** The first slice with no .NET reference, because
it is the new trigger model. One adapter first (Alertmanager), signature verification,
deduplication, normalization into Signals, incident grouping, and the human-initiated
investigation entry point. Correctness is established by specification and tests rather
than by differential comparison.

After slice 3 the control plane can accept a real trigger and dispatch real work, which is
the point at which the Go service becomes the product rather than a port.

## 7. What remains in .NET, and for how long

| Component | Status | Until |
| --- | --- | --- |
| Alerting, notifications, observability, their controllers and workers | Frozen. Never ported | Repository archive |
| `OpenCluster.Kubernetes` | Frozen. Parity oracle for the Relay's capability semantics | R3b, per the existing ledger |
| `OpenCluster.Relay.ControlPlane` | Frozen after R1 exit. Reference for slices 1 and 2 | Slice 2 deletion criteria met |
| `OpenCluster.Auth` | Frozen. Reference for the identity boundary | Identity slice, after the OIDC and provisioning design |
| `OpenCluster.Investigations` | Frozen. The largest and last port | Investigation slices; may outlive everything else |
| `OpenCluster.Api`, `OpenCluster.Core` | Frozen. Dissolve as their domains migrate | Repository archive |

## 8. Deletion criteria per component

The general gate binds for every component: every production path reaches the domain
through Go; behavioural parity is demonstrated against the reference; no .NET code writes
the domain's tables; the semantic contract tests survive the deletion; and an independent
review closes the slice.

Component-specific additions:

- **Relay control plane.** Deleted when a Relay registers, sessions, receives, executes,
  and has a result recorded end-to-end against the Go implementation, with the distributed-
  systems interleavings from the R1 gate suite passing against Go: enqueue while
  disconnected, lost assignment acknowledgement, superseded-session stale epoch, restart in
  the lease-write-to-send gap, and relay crash with an unacked buffer.
- **Kubernetes readers.** Unchanged from the existing ledger: semantic parity for runtime
  and discovery under the full three-part mechanism, routing cutover complete, self-hosted
  deployment updated, independent migration-safety review.
- **Auth.** Deleted when both Clerk and OIDC produce the identical canonical principal in
  Go, the same authorization policy tests pass against both, and a provisioning path exists
  that does not depend on Clerk webhooks.
- **Investigations.** Deleted when the truth-layer invariants are demonstrated in Go —
  fencing, immutability of accepted evidence, certificate-gated absence, conclusion
  citation guards — each with the failing test that proves the guard, not merely a passing
  happy path.
- **Alerting, notifications, observability.** No criteria. Never migrated; archived.

## 9. Tests and acceptance for slice 0

Slice 0 has no business logic, so its acceptance is entirely about the foundation being
load-bearing before anything is built on it.

Behavioural acceptance:

- The service starts with valid configuration and fails fast with a named variable on any
  invalid value, never echoing secret material.
- A tenant resolves to a placement; two organizations on different placements reach
  different databases; an unknown organization is a typed error, never a fallback to a
  default connection.
- Migrations apply exactly once under concurrent startup of two instances, and the second
  instance observes the first's completion rather than racing it.
- Readiness reports unready when the database is unreachable and recovers without a
  restart.
- SIGTERM drains in-flight requests within a bounded deadline and exits zero; no goroutine
  outlives the process context.
- A request carries a trace through to the database span, and a request identifier appears
  in every log line for that request.

Gate acceptance, mirroring the Relay's standard so the two repositories are reviewed the
same way:

- `go build`, `go vet`, `gofmt`, `staticcheck`, and `golangci-lint` clean — with the
  quality linters enabled from the start, not only the security gates. The Relay's
  narrow security-only profile is the reason two defects survived every green run there;
  the control plane does not repeat it.
- `go test -race` green, with genuinely concurrent scenarios rather than race detection on
  serial tests.
- `govulncheck` clean; dependency licences checked as a hard failure, not advisory.
- Secret scanning over full history before any push.
- An architecture test asserting that no exported store function omits the tenant scope,
  and that `internal/health` does not import `internal/storage` directly.
- Goroutine-leak detection in the test suite, so slice 0 establishes the discipline every
  later concurrent slice depends on.

No slice-1 code begins until slice 0's gates are green and reviewed.

## 10. Risks

| Risk | Mitigation |
| --- | --- |
| Restarting R1 to change its language | Slice 1 is gated on R1 exiting in .NET first |
| The strangler acquires dual-running cost that was avoidable | Section 2 states the no-deployment assumption explicitly and names its expiry |
| C# structure is transliterated into Go packages | Section 4 lists the specific patterns to refuse; review checks against it |
| Two repositories drift on the relay protocol | The Relay repository stays the single Protobuf authority with the `buf breaking` gate; the control plane consumes it as a module |
| The frontend contract is decided before the frontend exists | Section 5 defers the REST-versus-Connect commitment to the frontend rewrite |
| Slack intake investigates the wrong resource | Section 5 blocks Slack until canonical resource identity exists |
| The .NET reference rots and stops being a valid oracle | Its test suites stay in a standing gate for the whole migration window, as the Relay's oracle already does |
| "Critical fixes only" erodes into continued .NET development | Any .NET change beyond a security or data-correctness fix requires an explicit exception recorded in this plan |
