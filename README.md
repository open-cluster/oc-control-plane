# OpenCluster Control Plane

The central service of OpenCluster's autonomous incident investigation platform. It owns
organizations, placements, reasoning, and truth. Customer-side execution lives in the
OpenCluster Relay, a separate Apache-2.0 repository.

Proprietary. See [LICENSE](./LICENSE).

## Status

Foundation only. The process starts, resolves an organization's placement, applies its
embedded migrations, serves liveness, readiness, and metrics, and drains on shutdown.
There is **no domain logic yet**: relay registration, signal intake, incidents, and
investigations all come later.

It exists in this shape so that placement resolution, organization scoping, observability,
migration discipline, and shutdown semantics are in place before any domain depends on
them.

## Documents

| Location | Holds |
| --- | --- |
| `CONTEXT.md` | The domain glossary. Every document and every identifier uses this vocabulary |
| `docs/architecture/decisions/` | Decision records, numbered sequentially |
| `docs/architecture/` | The architecture each decision record summarises |
| `plans/` | Migration plan and per-slice specifications, with their real status |

These moved here from the frozen .NET implementation on 2026-07-27, because they govern work
that happens in Go. A gate fails the build if a document cites another that does not exist,
so a rename cannot silently leave a dangling reference behind.

## Quick start

Requires Go (see `go.mod`) and a reachable Docker daemon for the integration tests.

```
make tools     # pinned linters and scanners
make verify    # lint + build + test, the full CI gate set
```

Run against a local Postgres:

```
echo 'postgres://user:password@localhost:5432/controlplane?sslmode=disable' > /tmp/shared.dsn
OC_HTTP_ADDRESS=127.0.0.1:8080 \
OC_PLACEMENTS=shared=/tmp/shared.dsn \
OC_DEFAULT_PLACEMENT=shared \
go run ./cmd/controlplane
```

## Architecture

| Package | Responsibility |
| --- | --- |
| `cmd/controlplane` | Composition root. Explicit construction, no DI container |
| `internal/config` | Validated configuration. Secrets referenced by file path, never by value |
| `internal/tenancy` | The tenant boundary vocabulary. No I/O |
| `internal/storage` | Placement resolution, pools, embedded migrations. The only package that touches the database |
| `internal/observability` | slog, OpenTelemetry traces, metrics exported for Prometheus |
| `internal/api` | Liveness, readiness, metrics. Depends on behaviour, not on storage |
| `internal/relay` | Relay registration, sessions, and job delivery over the protocol |
| `internal/intake` | Inbound SignalUpdates, verification, rate limiting. One sub-package per provider |
| `internal/connection` | Connections and their secrets: the configured instances of an Integration |
| `internal/environment` | Environments: the scope that groups Connections and bounds evidence |
| `internal/capability` | The frozen, versioned contracts a Relay may be asked to execute |
| `internal/operator` | The cross-tenant read surface, behind its own token and its own listener |
| `internal/gates` | Build-failing architecture checks, including the dependency boundary |

A package is named after a business capability and never after a layer, and a provider adapter
lives below the capability it implements. Three properties are enforced mechanically rather
than by review:

- **Placement is resolved, never ambient.** Where an organization's data lives is looked up
  from the organization. An unresolvable organization is a typed error, never a fallback to
  a shared connection.
- **Only `internal/storage` reaches the database.** A connection built elsewhere would
  bypass placement resolution, which is a tenant-isolation defect rather than a style
  violation.
- **The Relay's implementation is not a dependency.** The control plane speaks the Relay's
  protocol and never touches a customer cluster, so no Kubernetes library may appear in its
  requirements or its imports.

## The Relay protocol

The contract is consumed as a Go module, pinned by `go.mod` and verified by `go.sum`:

```
go get github.com/open-cluster/oc-relay/gen/go@v0.1.0
```

It requires only gRPC and protobuf. The Relay's own module carries client-go because it
reads clusters; depending on that to obtain the generated types would put the whole
Kubernetes graph in this service's vulnerability report and licence inventory.

Fetching from the private repository needs
`go env -w GOPRIVATE='github.com/open-cluster/*'` and a credential with organization access.

## Configuration

| Variable | Required | Meaning |
| --- | --- | --- |
| `OC_HTTP_ADDRESS` | yes | Listen address for health, readiness, and metrics |
| `OC_PLACEMENTS` | yes | `name=path-to-dsn-file` pairs, comma separated |
| `OC_DEFAULT_PLACEMENT` | see below | Placement serving organizations with no explicit assignment |
| `OC_PLACEMENT_ASSIGNMENTS` | see below | `organization=placement` pairs, comma separated |
| `OC_SHUTDOWN_TIMEOUT` | no | Drain budget on SIGTERM. Default `15s` |
| `OC_SERVICE_NAME` | no | Telemetry service name. Default `oc-control-plane` |
| `OC_OTLP_ENDPOINT` | no | Trace collector `host:port`. Empty disables trace export |
| `OC_RELAY_ADDRESS` | no | Listen address for the Relay endpoint. Empty serves no relays |
| `OC_RELAY_SPKI_PINS` | with relays | This endpoint's own public key digests, handed to a Relay at enrolment. Comma separated; more than one so a rotation can overlap |
| `OC_OPERATOR_ADDRESS` | no | Listen address for the operator surface. Empty exposes it nowhere |
| `OC_OPERATOR_TOKEN_FILE` | with operators | File holding the operator token. At least 32 characters |
| `OC_INTAKE_ADDRESS` | no | Listen address for alert intake. Empty takes no alerts |

Each surface has its own listener, and that is what makes a deployment able to publish one
without publishing the rest. Intake is the only one a customer's own infrastructure reaches
inbound; the operator surface reads across tenants and belongs somewhere private; health and
metrics belong wherever the cluster scrapes from.

Intake carries no credential of its own — each configured alerting source authenticates with
its own secret, stored as a digest and never read back — which is why there is no
`OC_INTAKE_TOKEN_FILE` beside the operator one.

At least one of `OC_DEFAULT_PLACEMENT` or `OC_PLACEMENT_ASSIGNMENTS` is required; with
neither, no organization could be resolved. The default is the shared tier — enumerating
every organization in an environment variable is not a deployment. An explicit assignment
overrides it, which is how a Business or Enterprise tenant gets a dedicated database. With
no default configured, an unassigned organization is a hard error rather than a guess.

A placement's DSN carries a password, so it is read from a file. No environment value ever
carries a credential, and no error quotes a DSN file's contents.

## Endpoints

| Path | Purpose |
| --- | --- |
| `GET /healthz` | Liveness. Process health only — never consults a dependency, so a database outage cannot cause a restart loop |
| `GET /readyz` | Readiness. Reports unready when the placement this instance must have is unreachable, and recovers without a restart. It deliberately does **not** require every placement: once a tenant has a dedicated database, an all-must-be-up check would let one customer's outage withdraw the instance from service for everyone |
| `GET /metrics` | Prometheus scrape. Served outside the tracing middleware, and carries no per-organization label |

On the intake listener:

| Path | Purpose |
| --- | --- |
| `POST /intake/v1/organizations/{organization}/sources/{source}` | One webhook delivery from a configured alerting source. Authenticated by that source's shared secret in `X-OpenCluster-Token`, checked before the body is read. Answers `202` accepted, `200` already accepted, `401` unauthorized, `400` not understood, `413` too large, `503` not recorded — the 4xx answers are permanent so a source stops retrying, and `503` is the one that means try again |

## Development

```
make lint          # gofmt, vet, staticcheck, golangci-lint
make build         # CGO_ENABLED=0, trimpath
make test          # race detector, real Postgres via Testcontainers
make test-short    # unit tests only, no Docker required
make cover
```

Tests use one behavioural seam: the composition root, started in-process against a real
Postgres with a real listener and real signals. There is no database mock and no interface
per package. Seams are introduced only for external dependencies that cannot be run
locally.
