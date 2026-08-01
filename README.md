# OpenCluster Control Plane

The central service of OpenCluster's autonomous incident investigation platform. It owns
organizations, placements, reasoning, and truth. Customer-side execution lives in the
OpenCluster Relay, a separate Apache-2.0 repository.

Proprietary. See [LICENSE](./LICENSE).

## Status

**It investigates.** An engineer names a Kubernetes Connection, a namespace, a workload and a
time window; the control plane derives the Environment from the Connection, opens a durable case,
assembles a deterministic brief from live reads, forms competing hypotheses, makes up to two
further bounded rounds of typed read-only requests, and ends in a most supported explanation whose
every claim cites the evidence it rests on — or in an abstention that names what was missing.

Everything before it is in place: relay registration and sessions, typed bounded capability jobs
under a fenced lease, Environments and Connections, and alert intake.

Two properties are worth stating plainly because they are what the product is for. A confident
conclusion without sufficient support is **not a permitted outcome** — the output schema rejects an
uncited claim before it reaches storage. And a case's read models carry one monotonic version that
advances on *any* change within the case, so a client polling it is never blind to the evidence it
is waiting for.

**It can be evaluated.** The scenario harness (`test/e2e/cmd/scenario`) is ten Kubernetes clusters
broken in known ways, on purpose, with the cause written down before the system ever sees them. It
provisions each one, discards the run loudly if the cluster did not reach its declared broken
state, investigates it through the real control plane and a real Relay, and files two things
apart: an artifact a scorer reads, and the ground truth they must not. Evidence selection and the
conclusion are scored separately, by engineers who did not build the system, blind. One wrong and
confident answer fails the whole set and is not averaged against successes elsewhere.

**Secrets do not leave a customer's cluster.** Relay-side redaction masks high-confidence secret
shapes from the first install; the control plane records a CoverageGap per masked field, so
masking is visible rather than indistinguishable from absence, and a masked field can never
support a certified absence. The end-to-end proof puts a synthetic credential in a real
container's log and asserts it appears nowhere in this database.

Not built: signal-triggered investigation, incidents and grouping, and a live model provider —
the boundary and its replay exist (see below), so the harness runs against recorded transcripts
and refuses a live-provider run rather than quietly replaying one.

What this slice deliberately hard-codes is written down rather than left to be discovered:
[docs/architecture/hard-coded-in-the-first-investigation.md](docs/architecture/hard-coded-in-the-first-investigation.md).

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
| `internal/health` | Liveness, readiness, metrics. Depends on behaviour, not on storage |
| `internal/relay` | Relay registration, sessions, and job delivery over the protocol |
| `internal/intake` | Inbound SignalUpdates, verification, rate limiting. One sub-package per provider |
| `internal/connection` | Connections and their secrets: the configured instances of an Integration |
| `internal/environment` | Environments: the scope that groups Connections and bounds evidence |
| `internal/capability` | The frozen, versioned contracts a Relay may be asked to execute |
| `internal/investigation` | The case, its rounds, the brief, evidence, hypotheses, coverage gaps, outcomes, the runner and the read models |
| `internal/operator` | The cross-tenant read surface, behind its own token and its own listener |
| `internal/gates` | Build-failing architecture checks, including the dependency boundary |

A package is named after a business capability and never after a layer, and a provider adapter
lives below the capability it implements. Four properties are enforced mechanically rather
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
- **Persisted enum values are frozen.** Job status, signal status, connection role and the whole
  investigation vocabulary are stored as integers, and some appear as bare literals in SQL. A
  constant that moves keeps its old meaning in every row already written, so the values are pinned
  and the literals checked.

Three properties of the investigator are enforced by the database rather than by application
discipline, because each is invisible in review:

- **The case version advances on any change within the case**, not only on a lifecycle transition.
  A trigger on every child table touches the case; a trigger on the case advances its version. The
  frozen .NET frontend audit recorded the alternative and its consequence.
- **A claim cannot cite another case's evidence.** The citation's foreign key carries the
  investigation, so "every claim resolves to an EvidenceItem in the same case" is something the
  database refuses to break.
- **An adaptive request cannot exist without the hypothesis that justified it**, and an absence
  cannot be recorded without a completeness certificate. Both are check constraints. The first is
  what stops evidence text steering execution; the second is what stops an RBAC misconfiguration
  becoming a certified negative.

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
| `OC_MODEL_TRANSCRIPT_FILE` | no | A recorded transcript of the model boundary. With none, the investigator fails every round honestly saying the reasoning step could not run — an instance that cannot reason must say so rather than look healthy. A transcript recorded against different components refuses to start rather than replaying |

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

On the operator listener, under `/operator/v1/organizations/{organization}`, behind the operator
token. Relays, environments and connections are configured there; investigations are read there:

| Path | Purpose |
| --- | --- |
| `POST /investigations` | Open a case. Names a Connection, a namespace, a workload kind and name, and a window. It cannot name an Environment — that is derived from the Connection, and a field for it would imply a value that is honoured |
| `GET /investigations` | The list. Ordered by lifecycle state then recency, with attributed severity as a secondary signal. Carries per-row counts and the case's present tense, so rendering it is one request whatever the row count |
| `GET /investigations/{id}` | The summary, and the only thing a client polls. Carries identity, brief, state, current round, the outcome with its basis, counts, spend and the case version. Send the version back in `If-None-Match` and an unchanged case answers `304` from one primary-key read |
| `GET /investigations/{id}/timeline` | Evidence carrying a defensible source time, in source order. Items without one are listed beside it by the evidence read rather than placed on it |
| `GET /investigations/{id}/evidence` | The evidence, without content. Filterable by `capability`, `source` and `stance` |
| `GET /investigations/{id}/evidence/{evidence}` | One item with its bounded content. Separate from the listing so a listing is not the size of its contents |
| `GET /investigations/{id}/hypotheses` | What was proposed, what falsifies each, and where each got to |
| `GET /investigations/{id}/coverage-gaps` | What could not be checked, and what could not be concluded because of it |
| `GET /investigations/{id}/coverage` | Per typed capability: one of five states, with its reason. Not-applicable is not a gap |
| `GET /investigations/{id}/activity` | Every read the case asked for with the hypothesis that justified it, including the ones that returned nothing useful |
| `GET /investigations/{id}/case-file` | The whole case assembled server-side. `?version=` pins it; a version the case has passed is refused rather than answered from the current state. One code path serves the shared link, the export and the harness artifact |
| `POST /investigations/{id}/cancel` | Stop a case. Terminal, and it dispatches nothing further |
| `POST /investigations/{id}/reinvestigate` | Add a round to the same case. Never a second case: the identity, the URL and the permalink survive, and the earlier outcome stays readable and attributed |

Every section response carries the case version it represents, both in the body and as an `ETag`,
so a client can tell a stale section from a current one without guessing.

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

There is exactly one exception, and it is recorded at the seam itself in
`internal/investigation/reasoner.go`: the model boundary. It is nondeterministic and priced per
call, and what is in question about it — whether its explanations are any good — is answered by the
scenario harness against a live provider rather than by a commit gate. CI replays transcripts keyed
by model, prompt version, output schema version and investigator version, and a recording made for
different components is refused rather than replayed.

### The scenario harness

```
cd test/e2e
go run ./cmd/scenario list
go run ./cmd/scenario run   -results ./harness-run -transcripts ./transcripts
go run ./cmd/scenario run   -results ./harness-run -transcripts ./transcripts -scenario red-herring
go run ./cmd/scenario score -results ./harness-run
```

It is a program rather than a test, and **never a commit gate**: a gate that fails on ordinary
model variance is ignored within two weeks. Run it manually, before a release, and periodically —
model drift is a first-class reason, because the same set against a new provider version is the
only way to learn that an upgrade degraded investigations before a customer reports it.

`run` files artifacts under `<results>/artifacts` and ground truth under `<results>/ground-truth`.
Hand over the artifact directory and nothing beside it: a scorer who knows the answer grades
recognition rather than reasoning. `score` joins them afterwards and applies the kill criterion.

It needs a container runtime and the Relay's working tree beside this repository (or named by
`OC_E2E_RELAY_SOURCE`). A run stands up a database and a single-node Kubernetes per scenario, so
the whole set takes tens of minutes.

**It cannot yet be run to a scored result.** `-transcripts` must name a directory holding one
recording per scenario, and none ship — there is no live provider to record from, and a
hand-written transcript would be the builder's imagination scored as though a model had reasoned
it, which is what blind scoring exists to prevent. A run without `-transcripts` refuses and says
so. Everything else — provisioning, readiness verification, the artifact, scoring, the kill
criterion — is built and tested.
