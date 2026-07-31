# Spec — Environments and Connections (minimal)

Status: READY FOR IMPLEMENTATION. Part of the first-investigation slice, not a slice of its own.
Date: 2026-07-31 (revision 3 — the Integration is separated from the Connection, a Connection
gains a role, and intake stops naming a tenant in its URL; revision 2 was a hard reduction after
the architecture grilling session, revision 1 scoped this as a standalone slice ahead of
Incidents, which ADR-008 reversed)
Repository: the Go control plane
Decision records: ADR-003 as amended 2026-07-30 and 2026-07-31 (Connection is the sole
environment authority; Integration and role; opaque intake routing), ADR-008 (investigator
first), ADR-007 (triggers arrive from existing alerting), ADR-002 (placement), ADR-001 (customer
execution plane), ADR-016 (packages follow business capability)
Glossary: `CONTEXT.md`

> **What changed in revision 3.** Revision 2 modelled a Connection as "one configured integration"
> with a `kind` column, which quietly made two Alertmanager installations look like two
> integrations and put a vocabulary the product owns in a column a customer writes. Three
> corrections follow, and none of them grows the slice: an **Integration** is the kind and is
> compiled, a **Connection** is one configured instance of it, and a Connection carries a **role**
> — `trigger`, `evidence`, or both — which is what lets one model serve an Alertmanager webhook
> and a Kubernetes cluster. Intake's URL loses the organization, because a caller chooses its own
> path and a path is therefore not tenancy authority.

## Problem Statement

The control plane can execute a job against a customer's cluster and can turn a customer's alert
into a Signal. It cannot say which part of a customer's world either belongs to, and it has no
concept of a configured integration at all.

An Investigation is about to be built. Under ADR-003 as amended, its Environment is derived from
the Connection it names — never declared by the caller. Neither Environment nor Connection exists.
What exists instead is a relay registration, which is an execution identity rather than a
configured integration, and an `alert_source`, which is a Connection under another name,
introduced during intake because the model was not there to reference.

There is a second, sharper gap. `relay_job` carries a `registration_id` and no `connection_id`, so
there is nothing on the execution path against which an environment equality check could be made.
The invariant that evidence never crosses an Environment boundary is currently enforceable only by
whichever query happens to be written correctly.

There is a third, which revision 2 introduced and this revision closes. `alert_source.kind` held
`alertmanager`, and a Connection was to hold the same column widened to cover Kubernetes. That
conflates the *kind* of system with a *configured instance* of it, and the conflation is not
cosmetic: a customer with a production and a staging Alertmanager needs two records with two
secrets in two Environments, and a model in which the kind and the instance are one column can
only give them one.

## Solution

Two rows, one compiled vocabulary, and one column.

An **Environment** is a customer-named scope belonging to an Organization. It groups Connections
and nothing else. It owns no resources, implies no database and no deployment, and is a relevance
boundary rather than an isolation one. A **Default** environment is created automatically for every
Organization so that nothing downstream has to handle its absence.

An **Integration** is a kind of external system OpenCluster knows how to speak to — Alertmanager
and Kubernetes today; PagerDuty, Zabbix, Prometheus, Nomad, Proxmox later. It is a closed
vocabulary compiled into the product and is never a customer record. It names what an adapter
exists for.

A **Connection** is one configured instance of an Integration — "Production Alertmanager", "EU
Zabbix". It belongs to exactly one Environment, carries a **role** (`trigger`, `evidence`, or
both), carries its execution locality — `control_plane` or `relay` — and, when that locality is
`relay`, names the relay registration that serves it. Every alert source becomes a Connection; the
`alert_source` concept stops existing.

**Every dispatched job carries a `connection_id`**, and the control plane refuses to dispatch when
the job, its Connection and its Investigation do not share one Environment.

## User Stories

1. As an operator, I want a Default Environment to exist the moment my Organization does, so that
   I can install a Relay and investigate without first learning a concept.
2. As an operator, I want to create an additional Environment with a name I chose, so that the
   platform's grouping matches how my organization already talks about its systems.
3. As an operator, I want to list my Environments, so that I can see what scopes exist.
4. As an operator, I want an Environment name to be unique within my organization, so that two
   scopes cannot be confused for one another.
5. As an operator, I want to register a Connection inside an Environment, so that what it reaches
   is bounded by that scope.
6. As an operator, I want to run two Alertmanager installations as two Connections, so that
   production and staging alerting are separate records with separate secrets and separate
   Environments rather than one source I have to disambiguate afterwards.
7. As an operator, I want the list of Integrations to be something the product tells me, so that I
   cannot mistype the name of the adapter that will parse my payloads.
8. As an operator, I want each Connection to declare what it is for — delivering alerts, answering
   reads, or both — so that a webhook is not asked for an execution locality it has no use for and
   a cluster is not asked for a secret it will never be sent.
9. As an operator, I want each Connection to declare its execution locality, so that the platform
   knows whether a job runs centrally or through a Relay.
10. As an operator, I want a Connection whose locality is `relay` to name the relay registration
    that serves it, so that work is routed to the installation that can reach the source.
11. As an operator, I want a Connection whose locality is `control_plane` to need no Relay, so that
    a public SaaS source and an inbound webhook do not require an installation.
12. As an operator, I want to create a Kubernetes Connection bound to a Relay, so that an
    investigation has something to name.
13. As an operator, I want to list the Connections in an Environment, so that I can see what that
    scope can currently reach.
14. As an operator, I want to disable a Connection without deleting it, so that I can stop using a
    source without losing the record of what it produced.
15. As an operator, I want to configure an alerting source through an API rather than by inserting
    a database row, so that onboarding is something I can actually do.
16. As an operator, I want the shared secret for a trigger Connection shown to me exactly once, so
    that I can configure my alerting with it and the platform never stores it readably.
17. As an operator, I want to rotate a Connection's secret, so that a suspected disclosure does not
    mean recreating the Connection and reconfiguring the source's identity.
18. As an operator, I want the webhook URL I configure to name only the Connection, so that
    pasting it into a vendor's settings page discloses nothing about my organization.
19. As an on-call engineer, I want an alert delivered through a Connection to carry that
    Connection's Environment, so that what it eventually forms is scoped without anyone declaring
    a scope.
20. As an on-call engineer, I want evidence never to cross an Environment boundary, so that a
    staging failure cannot be used to explain a production incident.
21. As a security reviewer, I want a Connection to belong to exactly one Organization and one
    Environment, so that one tenant cannot reach another's sources.
22. As a security reviewer, I want the organization and environment of a delivery to be read from
    the authenticated Connection and never from the request path, so that a caller who can choose
    a URL cannot choose a tenant.
23. As a security reviewer, I want a Connection's secret stored so that no path reads it back, so
    that a disclosure of the database does not yield the ability to forge deliveries.
24. As a security reviewer, I want a Connection's secret to have an enforced minimum strength at
    creation, so that a weak one cannot be configured at all.
25. As a security reviewer, I want a repeated delivery body to be recognised and answered without
    being applied twice, so that an at-least-once webhook cannot be replayed into a second Signal.
26. As a security reviewer, I want a per-Connection rate limit at intake, so that one compromised
    or misbehaving source cannot spend the intake surface's capacity on every other tenant's
    behalf.
27. As a security reviewer, I want an Environment to be a label with no authority over placement,
    so that a customer-chosen string can never redirect where data physically lives.
28. As a security reviewer, I want a request naming an Environment in one organization and a
    Connection in another to be refused, so that path parameters cannot be combined to cross a
    tenant boundary.
29. As a security reviewer, I want a job whose Connection is in a different Environment from its
    Investigation to be refused before dispatch, so that the boundary is a precondition on the
    execution path rather than a property of whichever query was written correctly.
30. As a security reviewer, I want a Relay to hold no Environment of its own, so that there is
    exactly one authority and no precedence rule to get wrong.
31. As a security reviewer, I want to be told plainly that an Environment is not execution
    isolation, so that I deploy separate Relays when I need separate credentials.
32. As an engineer, I want the relay registration and the Connection kept separate, so that one
    Relay installation can serve several Connections and an execution identity is not confused
    with a configuration.
33. As an engineer, I want one Relay to be able to serve Connections in several Environments, so
    that a single installation in a shared cluster is not artificially forbidden.
34. As an engineer, I want an Environment's identity to be stable across a rename, so that
    everything referring to it keeps referring to it.
35. As an engineer, I want deleting an Environment to be refused while it still has Connections,
    so that a scope cannot be removed from under the things that inherit it.
36. As an engineer, I want the Default Environment to be undeletable, so that the guarantee
    nothing downstream has to handle its absence actually holds.
37. As an engineer, I want cluster fingerprinting to remain the mechanism for recognising the same
    cluster across Relay reinstalls, so that a human label is never asked to do an identity's job.
38. As an engineer, I want execution locality to be a property of the Connection rather than the
    Capability, so that the same capability can run centrally for one customer and through a Relay
    for another.
39. As an engineer about to build the investigator, I want to name a Connection and receive its
    Environment, so that scope derivation is a lookup rather than a decision.
40. As an engineer, I want labels available on a Connection as optional metadata, so that grouping
    and filtering are possible without anything mistaking them for a boundary.
41. As an engineer, I want an adapter's payload types to be unnameable from outside its own
    package, so that ADR-007's claim — that nothing downstream of normalization knows which system
    sent an alert — is checkable rather than asserted.
42. As the founder, I want this to be small, so that the boundary exists before there is evidence
    to misplace and without delaying the investigator.
43. As the founder, I want the existing intake tests to keep passing through the change, so that a
    model correction does not become a regression.

## Implementation Decisions

**Environment is a row, not a mechanism.** Identity, organization, customer-chosen name,
timestamps, and a flag marking the Default. It owns no resources and holds no policy fields yet;
investigation policy and coverage attach later, and adding columns before a reader exists invents a
contract for nobody.

**A Default Environment is created with the Organization**, in the same transaction. Its name is
`Default` and it is marked as such. It can be renamed but not deleted. This is what lets every
downstream concept treat `environment_id` as non-null from its first migration without an
onboarding step standing between a new Organization and its first investigation.

**Environment identity is separate from its name.** Everything points at the identity; the name is
an attribute. Uniqueness is enforced within an organization, not globally.

**The Integration is a closed compiled vocabulary; the Connection is an instance.** The stored
column holds an Integration identifier the product defines, validated against the compiled set on
write, so a Connection can never name an adapter this build does not have. It is stored as text
rather than an integer for the same reason the capability identifier is: a row an engineer reads
during an incident should say `alertmanager`, not `1`.

**Role is a closed vocabulary on the Connection** — `trigger`, `evidence`, or both — and the
database enforces what each role requires. A trigger Connection carries a secret digest; an
evidence Connection carries an execution locality, and a relay-local one names a registration. A
Connection that is both carries all of it. The constraints are checks in the schema rather than
rules the application remembers, in the same shape as the job table's whole-lease constraint.

**Not every Integration supports every role**, and which roles an Integration offers is compiled
alongside it: Alertmanager is trigger-only today, Kubernetes is evidence-only. A Connection
declaring a role its Integration does not offer is refused at creation, by the product's own
vocabulary rather than by a column.

**Connection absorbs the alert source.** `alert_source` does not survive as a separate concept. A
Connection carries what it carried — name, secret digest, disabled state — plus an Environment, an
Integration, a role, an execution locality, and an optional relay registration.

**VERIFIED with the founder before starting, and it is what makes this a replacement rather than a
migration:** no deployment of the intake slice has received real customer deliveries and no
external system points at the current intake URL. The migration therefore drops `alert_source` and
recreates `signal` and `signal_delivery` against `connection`, rather than backfilling. Were that
assumption wrong the design would be unchanged and the work would not be: it would become a
backfill plus a compatibility obligation on the old URL.

**Intake's route names the Connection and nothing else** —
`POST /intake/v1/connections/{connection}/signals`. The organization and the environment are read
from the authenticated Connection row. ADR-003's second amendment records why, and records the
consequence: with no organization in the URL there is nothing to resolve a placement from, so
intake asks each placement the deployment serves, in a fixed order, for a Connection with that
identifier. The identifier is a random UUID, the secret comparison is constant-time whether or not
a row was found, and the row that is found is the authority. This is the one storage function that
does not take an organization, and it is recorded in the gate's exemption list with that argument
rather than as a bare name.

**A trigger Connection owns its own verification, replay window, dedup and rate limit.**
Verification stays per-adapter — the shared-secret header is what Alertmanager can do, and an
Integration that can sign gets a signature instead. Replay protection and deduplication are the
same mechanism the intake slice already has: the unique constraint on `(connection_id,
body_digest)` means a replayed body is recognised, answered, and applied to nothing. The rate limit
is new and is per Connection, in process, bounded in memory, and shed with a status that tells a
sender to slow down rather than to stop.

**Execution locality is a closed vocabulary on the Connection** — `control_plane` or `relay`. A
relay-local Connection names a relay registration; a central one names none.

**The Relay carries no Environment.** ADR-003 as amended. One registration may serve Connections in
several Environments, and the environment of any work is the environment of its Connection. There
is no precedence rule because there is no second authority. A Relay is not a Connection: it is
where work runs, not what the work reaches.

**`relay_job` gains a non-null `connection_id`.** Existing rows do not exist in any deployment, so
this is a column addition rather than a backfill. Enqueueing refuses, with enumerated reasons and
never a panic, when the Connection is not this organization's, is disabled, answers no evidence
reads, or is served by a Relay other than the one the job names — and the refusal is decided
inside the insert, so a Connection disabled between a check and a write cannot leave work queued
against it.

**Half of this invariant is built and half waits, and the split is stated rather than left to be
discovered.** Everything above is enforceable today because both sides of the comparison exist.
The remaining clause — "refuses when the Connection's Environment does not equal the
Investigation's" — has nothing to compare against until an Investigation exists, which is
`plans/spec-first-investigation.md`. What this slice delivers is the column that makes that check
a one-line predicate when the Investigation arrives, instead of a retrofit across a table with
rows in it. A reviewer should read the absence of that clause in the code as scheduled, not as
missed; a reviewer should also refuse to let it be forgotten.

**Secrets keep the shape intake established**: platform-generated, shown once, stored only as a
digest, compared in constant time. Added here: a minimum strength enforced at creation, and
rotation that replaces the digest without disturbing identity or Environment. **Rotation has no
overlap window in this slice** — a single-digest rotation is a brief outage the operator schedules.
Carrying two live digests is the same shape as the Relay's SPKI pin rotation and is added when
someone asks.

**Environments and Connections are managed through the operator surface**, which already exists, is
already off the public interface, and already reads across tenants behind its own credential. No
new listener. **This slice does not give that surface a real identity** — it stays one shared
token, already recorded as a gap under ADR-006. What it must not do is make the gap worse: no
endpoint here acts across organizations without naming one in its path.

**Labels are a JSONB column on the Connection**, optional, and consumed by nothing in this slice.
They exist so that later grouping does not require a migration, and `CONTEXT.md` records that they
are never a boundary.

**The packages follow ADR-016.** `internal/environment` and `internal/connection` hold the
vocabulary and the handlers; the Alertmanager payload moves into `internal/intake/alertmanager` so
its types cannot be named from outside it; `internal/storage` stays the sole owner of the driver
and gains two files rather than two packages.

**Deferred, explicitly:** environment-scoped access control, investigation policy, coverage
readiness columns, environment inference from source attributes, and any second alerting adapter.

## Testing Decisions

**What makes a good test here.** It asserts what an operator could observe — a Connection was
created and can receive a delivery, a Signal carries the Environment of the Connection that
delivered it, a job whose Connection is in another Environment is refused before dispatch, a
rotated secret accepts the new value and refuses the old. It does not assert row shapes, because
the rows change when Investigations arrive.

**The seam is unchanged: the composition root**, with real HTTP and a real database. This is the
seam every slice has used since the foundation. No new seam is proposed, and a reviewer should
reject any suggestion to introduce a repository interface or a service-layer double for this work.

**The boundary invariant deserves adversarial tests, not happy-path ones.** Two properties, each
written so that removing the scoping makes it fail:

- A request combining one organization's Environment with another's Connection is refused, **with
  both organizations on the same placement** — otherwise an organization with no placement fails
  before any query runs and the test passes against an implementation with no scoping at all. That
  mistake was made once already in the intake suite and caught in review.
- A job naming a Connection in a different Environment from its Investigation is refused before
  anything is dispatched.

**The tenancy of an opaque delivery deserves the same treatment.** A delivery carries no
organization, so the test that matters is that the Signal it produces lands under the organization
and environment of the Connection row and not under anything the caller could influence — proven
with two organizations on one placement, and with a second Connection in a second Environment that
must not receive it.

**Intake's existing tests are the regression gate for the absorption.** They must keep passing with
only the changes the Connection model and the URL shape force. A test needing substantial rewriting
signals that the absorption changed intake's behaviour, which it must not.

**Scenarios.**

- A new Organization has exactly one Environment, named Default and marked as such.
- The Default Environment can be renamed and cannot be deleted.
- An Environment is created, listed, renamed, and its identity survives the rename.
- A duplicate name in one organization is refused; the same name in another is allowed.
- A Connection is created in an Environment and appears in its listing.
- Two Connections against the same Integration coexist in two Environments, each with its own
  secret, and a delivery signed for one is refused by the other.
- A Connection naming an Integration this build does not have is refused.
- A Connection naming a role its Integration does not offer is refused.
- A relay-local Connection without a relay registration is refused; a central one naming a relay
  registration is refused.
- A trigger Connection with no secret is refused; an evidence-only Connection needs none.
- One relay registration serves two Connections in two different Environments, and both work.
- A Connection created through the API accepts a delivery signed with the secret it returned.
- The secret is returned exactly once and no later read exposes it.
- A weak secret is refused at creation.
- A rotated secret accepts the new value and refuses the old.
- A delivery names only its Connection, and the Signal it produces carries that Connection's
  organization and Environment.
- The same body delivered twice produces one Signal and a success both times.
- A Connection exceeding its rate limit is shed, and a second Connection in the same organization
  is unaffected.
- An Environment with Connections cannot be deleted; it can once they are removed.
- A disabled Connection refuses deliveries and is still listed.
- A request naming one organization's Environment and another's Connection is refused, both on the
  same placement.
- A job carrying a `connection_id` whose Environment differs from its Investigation's is refused
  before dispatch, and no `relay_job` row reaches a dispatchable state.

**Prior art.** The operator surface establishes the tenant-scoped read, the digest-compared
credential and the audited mutation. Intake establishes secret generation, digest-only storage and
the delivery path. Relay storage establishes transactional writes and enumerated refusals.

## Out of Scope

- Incidents and grouping.
- Environment-scoped access control, investigation policy, coverage readiness.
- Tenant-scoped operator identity (ADR-006).
- Inventory synchronization and the change ledger (ADR-004, ADR-010) — a slice of its own.
- Any second alerting adapter. PagerDuty and Zabbix are named in the vocabulary as the shape the
  model must accommodate, and neither is built.
- Canonical resource identity.
- Automatic environment inference from source attributes. ADR-003 rejects it: inferring the label
  from `deployment.environment` or namespace conventions reintroduces the second grouping authority
  the whole model exists to prevent.
- A cross-placement routing directory for intake. ADR-003's second amendment records why the
  per-placement lookup is enough until there is a deployment it is not enough for.
- Relay Group / Trust Zone execution isolation.
- Any frontend work.

## Further Notes

Revision 1 argued at length that this must precede Incidents. That argument was correct about the
inheritance rule and wrong about the sequencing, and ADR-008 records why: it optimised for a
retrofit of rows that do not exist while the product's central claim went untested. The reduction
in revision 2 was the consequence, and it is worth noting that almost everything deleted was
correct — it was simply not needed yet.

Revision 3 is the opposite kind of change: it adds no scope and corrects a model. The cost of not
making it would have been paid later and by a customer, when their second Alertmanager needed a
record the schema could not give it.

The one line worth carrying forward from revision 1: `CONTEXT.md` is a description of what is
intended, not of what is built, and the gap between them is still larger than the built part.
Reading it as a specification of existing code will mislead.
