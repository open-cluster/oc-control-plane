# OpenCluster Control Plane

The central service of OpenCluster: organizations, Integrations, placements, and truth.
Customer-side execution lives in the OpenCluster Relay, a separate Apache-2.0 repository.

Proprietary. See [LICENSE](./LICENSE).

## Status

The control plane is mid-way through its foundational simplification, specified on the issue
tracker. The domain model is: an **Organization** is the tenant boundary; an **Integration
Type** is a kind of tool this build supports; an **Integration** is one configured
installation belonging to an organization. There are no Environments, no Connection roles and
no execution-locality settings — org_id is the only boundary, and where work runs is derived
from whether a Relay serves the integration.

**Alertmanager is connected end-to-end.** Creating the integration mints a webhook secret
shown exactly once beside its delivery address; firing and resolved alerts arrive
authenticated, deduplicated and bounded; Signals group into IncidentEpisodes on the identity
the source itself supplied — never on anything this platform inferred from a Signal's labels —
and an operator reads them, sees why they were grouped, and corrects a grouping with a merge
that rewrites nothing.

**Kubernetes is connected through the Relay.** Relay registration and sessions, typed bounded
capability jobs under a fenced lease, inventory synchronization into the change ledger, and
relay-side redaction are all in place. Verifying a kubernetes Integration judges the bound
Relay's live session and advertised capabilities, so "verified" means the far end answered.

Not built yet, deliberately: the investigation surface. The previous evidence-chain
architecture was removed with the old domain model; its replacement — operational provenance
and a deterministic context router — is the next phase on the issue tracker, and the
model-provider adapters it will use are kept intact under `internal/reasoning`.

## Documents

| Location | Holds |
| --- | --- |
| `./CONTEXT.md` | The domain glossary. Every document and every identifier uses this vocabulary |

A gate fails the build if a document cites another that does not exist, so a rename cannot
silently leave a dangling reference behind.

Slice plans, specifications, and retired decision records are working state, kept out of the
repository. Version control is the archive; specifications for unbuilt work live on the issue
tracker.

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
| `internal/intake` | Inbound SignalUpdates, verification, rate limiting. Payload adapters live in their provider packages |
| `internal/integrations` | The Integration domain: the type catalog and the configured installations. Providers live below it: `alertmanager/`, `kubernetes/` |
| `internal/incident` | IncidentEpisodes: what Signals group into, on their source's own grouping identity |
| `internal/capability` | The frozen, versioned contracts a Relay may be asked to execute |
| `internal/changeledger` | The change ledger vocabulary: what changed, remembered after the cluster forgets |
| `internal/reasoning` | The model-provider boundary and its adapters (`anthropic/`, `zai/`), kept for the provenance rewrite |
| `internal/identity` | Who an operator is: OIDC sign-in with PKCE, memberships, service accounts, API tokens |
| `internal/session` | The opaque server-side session and its cookie. No JWT, so sign-out can end it |
| `internal/authz` | The principal, the three roles, the permission table, and the one middleware that reads it |
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
- **Persisted enum values are frozen.** Job status, signal status, integration status and the
  integration type ids are stored as integers, and some appear as bare literals in SQL. A
  constant that moves keeps its old meaning in every row already written, so the values are
  pinned and the literals checked.

## The Relay protocol

The contract is consumed as a Go module, pinned by `go.mod` and verified by `go.sum`:

```
go get github.com/open-cluster/oc-relay/gen/go@v0.4.0
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
| `OC_OPERATOR_TOKEN_ROLE` | no | The one role it holds there. Default `admin`, because a deployment with no members yet needs a credential that can create the first one |
| `OC_OPERATOR_PUBLIC_URL` | with sign-in | Where this surface is reachable from a browser. The OIDC redirect URI is built from it, never from a request's Host header |
| `OC_OPERATOR_CONSOLE_URL` | with sign-in | Where a browser is sent once signed in. Must share a registrable domain with the above |
| `OC_OPERATOR_ALLOWED_ORIGINS` | with a console | Browser origins a cookie-authenticated unsafe request may come from. Empty permits none |
| `OC_SEALING_KEY_FILE` | with sign-in or credential-bearing integrations | 32-byte key, raw or base64, sealing presentable credentials at rest: identity client secrets and integration tokens |
| `OC_SLACK_API_URL` | no | Where the Slack provider reaches its vendor. Empty means Slack's own origin; it exists for tests and API-compatible proxies |
| `OC_INTAKE_ADDRESS` | no | Listen address for alert intake. Empty takes no alerts |
| `OC_INTAKE_PUBLIC_URL` | with intake | The origin a customer's own alerting reaches intake at. An Integration's webhook endpoint is built from it, never from a request's Host header: that URL is pasted into somebody else's system, and one that works from wherever the console is served is not one that works from the customer's alerting. Empty serves the endpoint as an absence rather than as a guess |
| `OC_MINIMUM_RELAY_VERSION` | no | The relay version floor the fleet summary counts `outdated` against. Empty compares nothing, and the summary says so rather than reporting zero outdated as though every Relay were current |

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

### The roles

Three human roles and one machine role. Fixed sets, compiled rather than editable: custom roles
are out of scope for this release, because an editable role is a second authorization model to
review. The table in `internal/authz/role.go` IS the specification — reading it is how to answer
"what can an Editor do", and `go test ./internal/gates/ -run TestTheRoleTableIsLegible -v`
prints it.

| Role | What it is for |
| --- | --- |
| `admin` | Runs the tenant: identity, members, sessions, tokens, integrations, relays, the record |
| `editor` | Operates the estate during an incident: verifying integrations, turning one off and back on, correcting an incident grouping. Cannot change what the estate is or who may sign in |
| `viewer` | Read-only across the tenant's operational record |
| `directory_synchroniser` | What a customer's directory holds. Reaches the SCIM endpoints and nothing else |

Admin may not be granted by just-in-time provisioning, by an identity provider's group claim,
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
| `POST /intake/v1/integrations/{integration}/signals` | One webhook delivery on an Integration's opaque intake route. Authenticated by that Integration's webhook secret in `X-OpenCluster-Token`, checked before the body is read. Answers `202` accepted, `200` already accepted, `401` unauthorized, `400` not understood, `413` too large, `503` not recorded — the 4xx answers are permanent so a source stops retrying, and `503` is the one that means try again |

The intake listener serves plain HTTP and must be deployed behind a TLS-terminating edge.
Alerting sources cannot sign, so the shared-secret header is a bearer credential that attests
nothing about the body — anyone who captures one request can replay it. A deployment that
publishes this listener without TLS in front of it is handing that credential out in
cleartext.

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
| `GET /members`, `PUT\|DELETE /members/{user}` | `member.read`, `member.manage` |
| `GET /sessions`, `POST /members/{user}/revoke-sessions` | `member.read`, `session.revoke` |
| `GET\|PUT /policy` | `identity.read`, `identity.configure`. Session lifetime and the declared audit retention |
| `GET /identity-providers/{provider}/saml-metadata` | `identity.read`. This deployment's own SAML metadata, to hand to the identity provider |
| `GET /directory-groups`, `PUT /directory-groups/{group}/role` | `identity.read`, `identity.configure`. What a synchronised group grants here — an administrator's decision, never the directory's |
| `GET\|POST /service-accounts`, `DELETE /service-accounts/{account}` | `service-account.read`, `service-account.manage` |
| `GET\|POST /api-tokens`, `POST /api-tokens/{token}/revoke` | `api-token.read`, `api-token.manage`. A token is shown once, bound to one Organization, one role and an expiry |
| `GET /audit-events` | `audit.read`. The record, newest first |
| `GET /relays`, `GET /relays/{id}/session-conflicts` | `relay.read`. The fleet is searched, filtered, sorted and paged by the database |
| `GET /relays/summary` | `relay.read`. Total, connected, disconnected, revoked, outdated, degraded and active requests, from one query so the numbers cannot disagree with each other |
| `GET /relays/{id}/integrations` | `relay.read`. What a Relay serves, which is what disabling it costs |
| `GET /relays/{id}/failures` | `relay.read`. What a Relay recently failed to complete, so an intermittent one is diagnosed from the record. Why each failed is not held — a job records that it failed, not what the relay said — and the envelope's `partial` says so |
| `POST /relays/{id}/clear-conflict` | `relay.conflict.clear`. Destroys a credential-theft finding, so only the Admin holds it |
| `POST /relays/bootstrap-tokens` | `relay.bootstrap-token.issue`. A single-use enrolment token, shown once, with a stated expiry. Separate from reading the fleet: a role that may look at the estate should not by that fact be able to extend it |
| `GET /integration-types` | `integration.read`. The catalog: every type this build serves, its configuration schema, its capabilities, and how many the tenant has configured |
| `GET\|POST /integrations` | `integration.read`, `integration.create`. Organization-wide, with `type`, `relay` and `disabled` as filters the database applies. Creating a webhook-receiving type mints the secret and returns it exactly once, beside the delivery address |
| `GET\|PATCH\|DELETE /integrations/{id}` | `integration.read`, `.update`, `.delete`. The detail carries status, webhook identity and verification; a DELETE is refused the moment anything depends on it |
| `POST /integrations/{id}/enabled` | `integration.update`. One idempotent operation with a body, replacing the `enable` and `disable` pair |
| `POST /integrations/{id}/verify` | `integration.verify`. The type's own definition judges the observed facts — a delivery that arrived, a relay's advertised capabilities — so "verified" means the far end answered, never that a form validated |
| `POST /integrations/{id}/webhook/rotate-secret` | `integration.webhook-secret.rotate`. The new secret is shown once; the old one stops working with no overlap window |

Every listing above speaks ONE query contract and answers ONE envelope, so a console has one
table to build rather than five:

| Parameter | Meaning |
| --- | --- |
| `search` | Free text, on the listings that offer it. Refused on the ones that do not, rather than ignored |
| `sort` | A signed field name — `name`, `-createdAt`. A field the listing does not offer is **refused**: a sort silently dropped returns rows in an order nobody chose, and the caller cannot tell |
| `cursor` | Opaque and resumed from a previous `next`. A tampered one is refused, because silently starting over shows the first page again and reads as the last |
| `limit` | **Clamped**, not refused. Somebody asking for more than they can have wants as much as they can have |

The envelope is `{ items, next, total, partial }`. Every field is present including the empty
ones: `items` is `[]` rather than null, and `next` and `total` are explicitly `null` — `total` is
nullable because a cursor-paginated query cannot always answer it cheaply, and a fabricated count
is worse than an absent one. `partial` is the backend saying "I served this field with no data,
and here is why", so a console renders one honest notice instead of a column of "Not reported".

Incidents are read there too. An **IncidentEpisode** is the operational episode Signals group into,
and the grouping identity is always the SOURCE's own — Alertmanager's group key, computed from the
grouping a customer's own operator configured. Nothing is inferred from a Signal's labels, because
deciding from those that two alerts concern one thing is canonical resource identity, which this
product does not have. A delivery carrying no grouping identity produces one episode per alert and
says `ungrouped`, which is the conservative failure: a wrong split leaves a redundant record, a
wrong merge produces an investigation with an incoherent scope.

| Path | Purpose |
| --- | --- |
| `GET /incidents` | The list. Filterable by `integrationId` and `status`; searchable by title and by the source's own grouping key, because that is what an operator arrives holding |
| `GET /incidents/{id}` | One episode, with the basis on which it was grouped stated both as a value and in words, so a surprising grouping is explainable rather than arguable |
| `GET /incidents/{id}/signals` | The Signals grouped into it, OLDEST first. Every other listing here is newest first; an incident is read forwards |
| `POST /incidents/{id}/merge` | Say two episodes are one incident. The episode in the path gives way, the body names the survivor and the reason. Nothing is rewritten: both keep their identity, their Signals and their record, and the absorbed one gains a pointer. Splitting is deliberately absent — this grouping errs toward splitting, so a merge is the correction it produces |

An episode resolves when no Signal in it is still firing, and it holds its grouping key only while
open — so the same failure next month is a new episode rather than a reopened closed one.

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
