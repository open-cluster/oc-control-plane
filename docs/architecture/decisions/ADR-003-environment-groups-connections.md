# Environment groups connections and nothing else

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
