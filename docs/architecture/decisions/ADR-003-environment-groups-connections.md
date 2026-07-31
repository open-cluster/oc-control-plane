# Environment groups connections and nothing else

Status: ACCEPTED, AMENDED 2026-07-30 and again 2026-07-31. The first amendment names the
Connection as the sole assignment authority, states plainly that Environment is a relevance
boundary rather than a physical security boundary, and settles the Relay's relationship to it.
The second separates the Integration from the Connection, gives a Connection a role, and
decides how an inbound delivery is routed when its URL names no tenant. See the two
**Amendment** sections at the end.

An Environment is a customer-named scope that groups connections, and it exists because
an investigation needs an enforceable boundary: without one, nothing prevents a staging
failure from becoming supporting evidence for a production incident — a false EvidenceItem
carrying valid provenance, the worst outcome the truth model can produce. It is also the
subject that investigation policy, coverage readiness, and enterprise access control
attach to, none of which can attach to an organization (too coarse) or a connection (too
fine at forty clusters).

Resources, topology, incidents, and investigations inherit their environment from the
connection that discovered them and are never assigned one directly. Allowing direct
assignment would create a second source of truth about grouping — one declared in the UI,
one observed from infrastructure — and the two diverging is exactly how a declared
topology starts generating false relationships.

## Consequences

- Evidence never crosses an environment boundary. This is an invariant, enforced, not a
  convention.
- Environment is not a tenancy or placement boundary. The organization is the tenant
  boundary; placement is infrastructure. An environment implies no dedicated database and
  no dedicated deployment.
- Environment is a human label, not an infrastructure identity. Cluster fingerprinting
  remains the mechanism for recognising the same cluster across Relay reinstalls; the
  label must never be asked to do the identity's job.

## Considered and rejected

Deleting the concept entirely. Every connected source already carries environment as an
observed attribute (`deployment.environment`, Kubernetes namespaces and labels, cloud
account tags), and real taxonomies overlap in ways one flat entity cannot express — cells,
regions, per-customer stacks, blue/green. That argument defeats an Environment that owns
resources. It does not defeat one that only bounds evidence, policy, and access.

## Amendment, 2026-07-30 (the Connection is the sole authority)

**The Connection is the sole authority.** An Environment is assigned at exactly one point: when a
Connection is created. The Connection is the right authority because it represents the actual
external data source and its credential scope. Resources, Signals, Evidence and Investigations
inherit from the originating Connection and are never assigned an Environment directly, exactly as
the original decision states.

**The Relay carries no Environment.** It is an organization-scoped execution identity and may
serve Connections in several Environments. One Relay installation therefore continues to serve
many Connections without a second grouping authority appearing, because the Relay never asserts an
Environment at all.

**A manually triggered investigation names a Connection**, or selects a Resource that resolves to
one, and inherits its Environment from it. A client may present an Environment for navigation and
filtering; it is never the authority. The control plane derives and persists `environment_id` from
the Connection. A request spanning several Connections is refused unless they share one
Environment. A future chat or Slack trigger is the same path behind a different front door.

**Environment is a relevance and correctness boundary, not a physical security boundary.** Its job
is to stop a staging failure being cited as evidence for a production incident. It is not
isolation: one Relay can hold credentials for Connections in several Environments, so a customer
who requires production and staging isolation deploys separate Relays with separate credentials.
The original consequence "evidence never crosses an environment boundary" stands as a control-plane
invariant and must not be described to a customer as an execution-isolation guarantee. Enforcing
isolation at the execution layer is deferred to a future Relay Group or Trust Zone concept and is
deliberately not built now.

**Every dispatched job carries a `connection_id`,** and the control plane verifies that job,
Connection, resource and Investigation share one Environment before dispatch. This makes the
invariant a checked precondition on the execution path rather than a property of query scoping
alone. `relay_job` today carries a `registration_id` and no `connection_id`; adding it is a
requirement, not an improvement.

**Labels are optional metadata and selectors only.** They may group and filter. They are never an
authorization, credential or tenant boundary and never substitute for an Environment.

## Amendment, 2026-07-31 (the Integration, the role, and how a delivery is routed)

The first amendment settled which record assigns an Environment. It left three things
unsettled, and each of them was about to be decided by whoever wrote the first migration.

**An Integration is a kind; a Connection is one configured instance of it.** The Integration —
Alertmanager, PagerDuty, Zabbix, Kubernetes, Prometheus, Nomad, Proxmox — is a closed vocabulary
compiled into the product and names what an adapter exists for. The Connection is the customer's
record: "Production Alertmanager", "EU Zabbix", "Staging Prometheus". A customer running two
Alertmanager installations creates two Connections against one Integration and one adapter, and
adding the second is configuration rather than code. Collapsing the two — which is what an
`alert_source` row with a `kind` column does — makes the second installation look like a second
integration and puts a vocabulary the product owns in a column the customer writes.

**A Connection has a role: `trigger`, `evidence`, or both.** A Trigger Connection delivers
SignalUpdates inbound and owns its verification secret, replay window, rate limit and
deduplication state. An Evidence Connection answers bounded capability reads outbound and owns
its execution locality and Relay binding. The role is what makes one model serve an Alertmanager
webhook and a Kubernetes cluster: they differ in direction, not in kind, and both are one
configured integration inside one Environment. A Relay binding is therefore optional and
meaningful only for the evidence half — a webhook integration reaches the control plane inbound
and needs no installation at all.

**An inbound delivery names its Connection and nothing else.** The intake route is
`POST /intake/v1/connections/{connection}/signals`. Organization and Environment are read from
the authenticated Connection row; an identifier in the path is never tenancy authority, because a
path is chosen by the caller and a caller who could name a tenant could try every tenant.

That decision has a consequence the placement model forces into the open: with no organization
in the URL there is nothing to resolve a placement from, and ADR-002 says placement is resolved
from the organization and never ambient. Three ways to close the gap were considered.

- *Encode the placement in the identifier.* Rejected: it makes a residency migration change every
  customer's webhook URL, which is the one thing residency migration must not require.
- *Build a cross-placement routing directory.* Rejected for now: it is a new piece of shared
  infrastructure, and the deployments that exist have one placement, so it would be built against
  an imagined load rather than an observed one.
- *Ask each placement the deployment serves, in a fixed order, for a Connection with that
  identifier.* **Accepted.** The identifier is a random UUID, the secret is compared in constant
  time whether or not a row was found, and the row that is found is itself the authority for the
  organization and the environment — so the lookup discovers a tenant rather than trusting one.
  The cost is one primary-key lookup per placement per delivery, and the count of placements is
  small by construction. It survives a placement migration, which is why it wins over the first
  option, and it needs nothing that does not exist, which is why it wins over the second.

The exemption this requires from the "every tenant-scoped store function takes an organization"
gate is recorded there with the same reasoning, so a reader of the gate finds the argument rather
than an unexplained name on a list.

