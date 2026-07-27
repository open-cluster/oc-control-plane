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
| `internal/gates` | Build-failing architecture checks |

Two properties are enforced mechanically rather than by review:

- **Placement is resolved, never ambient.** Where an organization's data lives is looked up
  from the organization. An unresolvable organization is a typed error, never a fallback to
  a shared connection.
- **Only `internal/storage` reaches the database.** A connection built elsewhere would
  bypass placement resolution, which is a tenant-isolation defect rather than a style
  violation.

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
