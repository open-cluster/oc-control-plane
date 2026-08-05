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
| `internal/identity` | Who an operator is: OIDC sign-in with PKCE, memberships, service accounts, API tokens |
| `internal/session` | The opaque server-side session and its cookie. No JWT, so sign-out can end it |
| `internal/authz` | The principal, the seven roles, the permission table, and the one middleware that reads it |
| `internal/audit` | The append-only record: who did what, to which tenant's what, and whether it was allowed |
| `internal/operator` | The operator listener and the composition of the route table. Owns no route decisions of its own |
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
| `OC_OPERATOR_TOKEN_FILE` | with operators | File holding the bootstrap credential. At least 32 characters |
| `OC_OPERATOR_TOKEN_ORGANIZATION` | with a token | The one Organization that credential reaches. Required, never inferred |
| `OC_OPERATOR_TOKEN_ROLE` | no | The one role it holds there. Default `organization_owner`, because a deployment with no members yet needs a credential that can create the first one |
| `OC_OPERATOR_PUBLIC_URL` | with sign-in | Where this surface is reachable from a browser. The OIDC redirect URI is built from it, never from a request's Host header |
| `OC_OPERATOR_CONSOLE_URL` | with sign-in | Where a browser is sent once signed in. Must share a registrable domain with the above |
| `OC_OPERATOR_ALLOWED_ORIGINS` | with a console | Browser origins a cookie-authenticated unsafe request may come from. Empty permits none |
| `OC_IDENTITY_ENCRYPTION_KEY_FILE` | with sign-in | 32-byte key, raw or base64, sealing an identity provider's client secret at rest |
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

## Identity, roles and the record

An operator signs in through their Organization's configured identity provider, over **OIDC** —
Authorization Code with PKCE, the identity token verified against the issuer's own published
keys — or **SAML 2.0**, service-provider initiated, the assertion's signature verified against
the certificates in the provider's published metadata. Either way the control plane applies the
tenant's provisioning policy and issues its own opaque server-side session as an `HttpOnly`,
`Secure`, `SameSite=Lax` cookie.

Everything after a provider has been believed is one code path for both protocols. What differs
is how a provider is believed; the provisioning policy, the verified-domain check, the group
mapping and the session issue are the tenant's and are the same.

It is deliberately **not a JWT**. A signed token carrying its own claims cannot be ended before
it expires, so "sign out ends my session on the server" and "revoke a departing colleague's
access immediately" would both need a revocation list — which is a session table with extra
steps and a second thing to keep consistent with the first. Only the digest of the session
identifier is stored, so a disclosure of that table yields no usable credential.

Because the cookie is `SameSite=Lax` and there is no separate CSRF token, **the console must be
served from the same registrable domain as this surface**. A cross-site console would never send
the cookie at all. Unsafe cookie-authenticated requests additionally require an `Origin` from
`OC_OPERATOR_ALLOWED_ORIGINS`; bearer tokens are exempt, because a browser never attaches one
by itself.

### The eight roles

Fixed sets, compiled rather than editable. Custom roles are out of scope for this release: an
editable role is a second authorization model to review. The table in `internal/authz/role.go`
IS the specification — reading it is how to answer "what can an Investigator do", and
`go test ./internal/gates/ -run TestTheRoleTableIsLegible -v` prints it.

| Role | What it is for |
| --- | --- |
| `organization_owner` | Everything, including who else may administer the tenant |
| `platform_administrator` | Runs the tenant. Cannot appoint an owner — that one permission is the difference between the two |
| `integration_manager` | Configures Environments and Connections, and deliberately cannot change who may sign in |
| `investigator` | Opens, cancels and reads Investigations. Cannot damage the estate |
| `responder` | An Investigator who may also set a Connection's enabled state during an incident |
| `viewer` | Read-only across the tenant |
| `auditor` | Reads the record and nothing else, so that the access does not itself become a risk |
| `directory_synchroniser` | What a customer's directory holds. Reaches the SCIM endpoints and nothing else |

An owner may not be granted by just-in-time provisioning, by an identity provider's group claim,
by a SCIM group mapping, or by an API token: every one of those would make a directory edit or a
leaked CI variable an administrative takeover.

### How a route is authorized

Every route registers as `(method, pattern, permission)` and `Router()` builds the mux from that
table. Three mechanisms keep it honest, and each covers what the others cannot:

1. **The compiler**, for the privileged case. `authz.Privileged` takes the permission
   positionally, so a route that needs one and does not name one does not compile.
   `authz.Public` and `authz.Authenticated` compile by design — the sign-in routes have to
   exist — which is why the two mechanisms below are what actually close it.
2. **Startup.** The table is validated before it becomes a mux, so an undeclared permission, a
   privileged route naming no Organization, or a duplicate is a process that refuses to start
   rather than a route served open.
3. **Gates.** `internal/gates` refuses a `mux.Handle` anywhere under `internal/` outside the
   four packages recorded as having a listener of their own — it reads every package rather than
   a list of the ones with routes today, so a new surface package cannot be silently unscanned.
   It holds the public and authenticated-only routes to named lists with a reason each, and
   fails if a declared permission is reachable by no route.

A request naming an Organization the principal is not a member of answers **404, not 403** — a
403 would confirm the tenant exists, which would turn a path segment into a list of this
deployment's customers. A member who lacks the permission gets 403 naming what they lack.

### The record

Every state change writes an `audit_event` row **inside the same transaction as the change it
records**, so an operation nobody could attribute never happens. Failed authorizations by a
caller holding a credential are recorded too, because credential probing is only visible if the
attempts that failed are. The table is append-only and the database enforces it: statement-level
triggers refuse `UPDATE`, `DELETE` and `TRUNCATE`, and the one path that may prune has to declare
itself in its transaction.

Nothing in it is ever a credential. `audit.Detail` drops any key named like one before the row is
written, mechanically rather than by convention, because the paths that write these events are
precisely the paths holding a secret at the time.

## Provisioning

A customer's directory synchronises its people into one tenant over **SCIM 2.0**, at
`/scim/v2/organizations/{organization}`, with a bearer token holding the
`directory_synchroniser` role and nothing else.

It is deliberately not under `/operator/v1`: an operator API version is this product's to
change, and a directory's base URL is configured once in somebody else's system and then
untouched for years.

| Resource | Operations |
| --- | --- |
| `ServiceProviderConfig`, `ResourceTypes`, `Schemas` | What a directory reads before anything else, answered honestly — it says what is *not* supported rather than omitting it |
| `Users` | list with `userName eq` / `externalId eq`, create, read, replace, patch `active`, delete |
| `Groups` | list with `displayName eq`, create, read, replace, patch members, delete |

Two properties matter more than the coverage:

**What is not understood is refused, not ignored.** A directory whose deprovisioning silently
did nothing would leave everybody believing access had been removed. Anything outside the list
above answers a SCIM error with a `scimType` the directory can act on.

**A directory decides who is in a group; an administrator decides what a group grants.** The
role mapping lives on the operator surface behind `identity.configure`. If a directory could
decide what its own groups meant here, a change in the customer's identity vendor would be a
privilege grant in this product.

Deprovisioning takes effect on the person's **next request**, not at their next sign-in — set a
person inactive, delete them, or remove them from the last group that granted their role, and
the sessions resting on it are revoked in the same transaction.

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

On the operator listener, unauthenticated — this is the whole public surface of the product, and
each route answers a tenant that has configured no way in exactly as it answers one that does not
exist, so none of them can be used to enumerate customers:

| Path | Purpose |
| --- | --- |
| `GET /operator/v1/organizations/{organization}/sign-in/providers` | The identifier and name of each configured provider, so a console can render a chooser |
| `GET /operator/v1/organizations/{organization}/sign-in/{provider}` | Start a sign-in. Which protocol is the provider's, not the caller's: OIDC redirects with a state, a nonce and an S256 PKCE challenge; SAML redirects with an AuthnRequest and a relay state |
| `GET /operator/v1/sign-in/callback` | OIDC's. Redeems the state exactly once, verifies the identity token, and issues the session cookie |
| `POST /operator/v1/organizations/{organization}/sign-in/saml/{provider}/callback` | SAML's assertion consumer service. Redeems the relay state exactly once, then verifies the signature, the audience, the recipient, the conditions and InResponseTo together |

Needing a credential and no permission, because the subject is the caller themselves:

| Path | Purpose |
| --- | --- |
| `GET /operator/v1/session` | Who is signed in, which Organizations they may read, and the flattened permissions their roles carry |
| `POST /operator/v1/session/sign-out` | Deletes the session row and clears the cookie in the same response, so the credential is dead before the response is written |

Under `/operator/v1/organizations/{organization}`, each behind the permission it declares:

| Path | Permission |
| --- | --- |
| `GET\|POST\|PATCH\|DELETE /identity-providers` | `identity.read`, `identity.configure` |
| `GET /members`, `PUT\|DELETE /members/{user}` | `member.read`, `member.manage`. Appointing an owner additionally needs `member.owner.manage` |
| `GET /sessions`, `POST /members/{user}/revoke-sessions` | `member.read`, `session.revoke` |
| `GET\|PUT /policy` | `identity.read`, `identity.configure`. Session lifetime and the declared audit retention |
| `GET /identity-providers/{provider}/saml-metadata` | `identity.read`. This deployment's own SAML metadata, to hand to the identity provider |
| `GET /directory-groups`, `PUT /directory-groups/{group}/role` | `identity.read`, `identity.configure`. What a synchronised group grants here — an administrator's decision, never the directory's |
| `GET\|POST /service-accounts`, `DELETE /service-accounts/{account}` | `service-account.read`, `service-account.manage` |
| `GET\|POST /api-tokens`, `POST /api-tokens/{token}/revoke` | `api-token.read`, `api-token.manage`. A token is shown once, bound to one Organization, one role and an expiry |
| `GET /audit-events` | `audit.read`. The only route the Auditor role reaches |
| `GET /relays`, `GET /relays/{id}/session-conflicts` | `relay.read` |
| `POST /relays/{id}/clear-conflict` | `relay.conflict.clear`. Destroys a credential-theft finding, so only the two administrative roles hold it |
| `GET /integrations` | `integration.read`. Organization-scoped, so configured Connections can be counted per tenant |
| `GET\|POST /connections` | `connection.read`, `connection.create`. Organization-wide, with `?environmentId=` as a filter |
| `POST /connections/{id}/enabled` | `connection.update`. One idempotent operation with a body, replacing the `enable` and `disable` pair |
| `POST /connections/{id}/trigger/rotate-secret` | `connection.trigger.secret.rotate` |
| `GET\|POST /environments`, `PATCH\|DELETE /environments/{id}` | `environment.read`, `.create`, `.update`, `.delete` |

Investigations are read there too:

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
