# OpenCluster domain glossary

Every document and every identifier in this repository uses this vocabulary. Each entry
names the one word for a concept and, where confusion has actually occurred, the synonyms
that are banned. A term's entry is its definition; code comments cite the term rather than
restating it.

## Tenancy

**Organization** — the tenant boundary. Every durable record belongs to exactly one, and
`org_id` is the only durable tenant identity: it appears in ownership columns, composite
keys, authorization checks, Relay metadata and URL paths, and it is an opaque identifier,
never a name. Any display name is `org_name`, presentation metadata only, and participates
in nothing.
_Avoid:_ tenant (as an identifier), customer (as an identifier), organization name as a key.

**Placement** — where an organization's data physically lives. Resolved from the
organization at startup, never ambient. A placement is a database today.

**Label** — optional metadata on an Integration, for grouping and filtering. Never an
authorization, a credential, a tenant boundary, or a scope.

## Integrations

**Integration Type** — a kind of tool OpenCluster supports: Alertmanager, Kubernetes,
Slack, GitHub. Product-owned reference data: a row in `integration_type` seeded by
migration, carrying minimal catalog metadata (stable key, name, description, logo,
category), with everything behavioral — configuration schema, capabilities, client,
verification, tools — in the type's provider package under `internal/integrations/`. The
compiled catalog and the seeded rows are held identical by a test.
_Avoid:_ provider (as the record noun; "provider package" for the code is fine), connector,
integration definition (say Definition only for the exported Go value).

**Integration** — one configured installation belonging to an organization: "Production
Alertmanager", "Org Slack". The organization-owned runtime record: name, non-secret
configuration, optional labels, optional Relay binding, status, verification. Several
Integrations of one type are expressly allowed.
_Avoid:_ Connection (the retired record noun), instance, source, data source.

**Capability** — a named, versioned operation an Integration Type makes available:
`kubernetes.container.logs`, `alertmanager.receive_alerts`. Declared by the type; never
chosen or classified by the operator. The Relay-dispatched Kubernetes capabilities are
additionally typed contracts in `internal/capability`.
_Avoid:_ role, trigger/evidence classification, connection mode.

**Verification** — a check of an Integration against reality, judged by the type's own
definition from observed facts: a delivery that actually arrived, a Relay's live session and
advertised capabilities. "Verified" always means the far end answered; a well-formed
configuration proves nothing and is not called verified. A verification records the
**grants** it observed about the credential, in the provider's own vocabulary — Slack: the
OAuth scopes, plus the token's kind — and tool availability derives from them. It also
records **facts**: non-secret, provider-shaped things it established about what is
connected — GitHub: the account, its type, whether the installation selected repositories
or granted all of them, how far the grant reaches, and where in the provider's own screens
the customer changes it. Facts are for display and for support; no authorization decision
reads them, which is what keeps them apart from grants.
_Avoid:_ validation (the retired form-checking sense); metadata (for facts).

**Connect flow** — one provider installation flow in progress: the customer presses Connect
and the provider — not OpenCluster — handles account selection, permission consent and the
return trip. The record binds the attempt to an organization, the principal that started it
and a return target; only a **state** travels through the browser, and only its digest is
stored. Single-use and short-lived. The organization the resulting Integration belongs to
comes from the flow record and never from the callback's query, and the provider is asked to
PROVE that whoever authorized the flow can reach what the callback named — an identifier the
browser carried back proves nothing on its own. A type whose deployment registered no such
flow is connected through its configuration form instead, which is the self-hosted path.
_Avoid:_ OAuth flow (only some are), onboarding, enrolment (that word is the Relay's),
Connection (the retired record noun).

**Connect binding** — what a proven return says to record: a suggested name and the
non-secret configuration of the thing that was connected. The configuration is also its
identity, so connecting the same installation again re-verifies the Integration that exists
rather than adding a second record of it. It carries no credential.
_Avoid:_ enrolment, registration, account link.

**Webhook secret** — the shared secret an inbound source presents. Minted by the platform,
shown to the operator exactly once, stored only as a SHA-256 digest, compared in constant
time, rotated rather than recovered. Its fingerprint is minted, never derived from the
secret.

**Credential** (of an Integration) — the outbound secret a provider is reached with: a
Slack bot token. Probed live against the vendor before anything is stored, sealed with
AES-256-GCM under the deployment's sealing key, write-only after entry; reads render a
minted fingerprint only. A submitted credential is stored sealed or refused loudly, never
dropped.
_Avoid:_ token (as the record noun), API key (for this concept).

**Tool** — one bounded, read-only operation an Integration Type offers an investigation:
`slack.get_channel_history`, `github.read_commits`. Declared beside its capability with
its purpose, when to use it, when NOT to, arguments, permissions and output — all
enforced at catalog assembly. A tool may require verified grants; one the Integration's
recorded grants cannot support is absent from an investigation's set, never a call that
always fails. Every read is bounded by named limits, flags truncation from the vendor's
own answer, and clamps any window argument inside the investigation's window.

## Signals and incidents

**Signal** — one episode of a normalised alert, keyed on the source's own alert identity
plus its start time. Nothing downstream can tell which system delivered one. Free text in a
Signal is untrusted for its whole life.

**Delivery** — one webhook body received by intake. Accepted deliveries are deduplicated by
body digest, so an at-least-once webhook is idempotent; rejected and duplicate attempts are
recorded so a source being turned away is distinguishable from one that went quiet.

**IncidentEpisode** — the operational episode Signals group into, on the grouping identity
the SOURCE supplied and nothing this platform inferred. Provisional grouping, not causal
truth: revisable by a merge that rewrites nothing. An episode resolves when no Signal in it
still fires.

## Execution

**Control plane** — this service: the durable truth, the operator surface, intake, and the
Relay endpoint.

**Relay** — the agent in a customer's cluster. Enrols with a single-use bootstrap token,
holds one outbound session, executes the closed set of Kubernetes capabilities, and
synchronizes the inventory the change ledger records. A Relay belongs to an organization
and nothing else.

**Job** — one dispatched capability execution: durable, leased, fenced by
`(session, epoch)`, never lost and never recorded twice. A job names the Integration it
reaches and the Relay it runs on.

**Change ledger** — workload revisions and configuration changes, persisted continuously
because they decay at the source. Declared intent and identity only, never observed state
and never content. A navigation index, never evidence.

## Investigations

**Investigation** — one bounded answer to "what happened": opened from an episode or an
operator's question, routed to a few relevant sources, run through their tools, ended
concluded with findings or failed with the reason. The slim record: trigger, subject,
window, lifecycle, findings, spend.
_Avoid:_ case, case file, round (as a persisted record).

**Provenance** — what an investigation persists: the sources it was offered with the
reasons, every tool run with its scope, window, outcome, truncation, summary and source
references, and findings citing runs. Operational fact an operator can audit — never a
model's chain of thought.
_Avoid:_ evidence (as a record noun), evidence chain.

**Source** (of an investigation) — one Integration the investigation was offered, with
its rank and the reason recorded. The offer is every enabled Integration whose verified
grants support at least one tool; the investigator itself decides which offered sources
to actually read.

**Run** — one tool execution inside an investigation, succeeded or failed alike. Its
ordinal is what a finding cites.

**Finding** — one thing an investigation established, citing the ordinals of the runs
that support it. A statement citing no run cannot be stored; enforced at decode and again
before persistence. An autonomous conclusion's findings additionally carry a **kind** —
the causal role: probable_cause, contributing_factor, symptom, triggering_change,
propagation_effect, ruled_out, unresolved_lead — and a categorical **confidence**:
confirmed, likely, possible. Never an invented numeric certainty; multiple probable
causes are legal.
_Avoid:_ conclusion (as the record noun), claim.

**Investigator** — the investigation's model boundary, declared by the investigation
domain and implemented by `internal/reasoning` over vendor adapters; the domain never
learns a vendor exists. It opens a **Conversation** from an **Orientation** and returns
**Moves**. A failed reasoning step fails the investigation — it is never presented as a
conclusion. The Orientation is assembled only from context the platform already holds —
the trigger's own metadata, the offered sources, the change ledger's workload digest —
never by querying a vendor. A Move carries further calls or the **Conclusion**: the
concluding document of findings and recommended next steps, checked on its way into the
Investigation record and never itself a persisted record. A ceiling that ends the reads
— spend, tool runs, reasoner turns, wall clock, stagnation — is recorded as what
stopped the investigation, never dressed as a free diagnosis.

**Spend** — what the reasoning consumed: tokens and integer micro-cents, summed over
every call including refused and truncated ones.

## Retired vocabulary

These terms named the previous architecture and must not reappear: Connection, Connection
role, Environment, Evidence Scope (a change-ledger scope and a tool run's scope are
different, current concepts), Execution locality, EvidenceCandidate, EvidenceItem,
EvidenceValidation, Evidence plan, CoverageGap (as a persisted record), Completeness
certificate, CasePack, InvestigationRound, Hypothesis (as a persisted record), Abstention
(as a lifecycle state). The investigation surface stands on operational provenance — the
Investigations section above is its vocabulary — and none of the retired machinery may
return under a new spelling.
