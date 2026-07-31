-- Environments, Connections, and the boundary evidence may never cross.
--
-- An Environment is a customer-named scope that groups Connections and nothing else. A
-- Connection is one configured instance of an Integration — "Production Alertmanager", "EU
-- Zabbix" — and it is the SOLE authority for the Environment of everything that arrives
-- through it. An Integration is the kind of system the product knows how to speak to; it is a
-- closed vocabulary compiled into the binary and is never a customer record.
--
-- THIS MIGRATION IS DESTRUCTIVE, deliberately and once. It drops alert_source and recreates
-- signal and signal_delivery against connection rather than backfilling them. That is allowed
-- because the founder verified before it was written that no deployment of the intake slice
-- has received real customer deliveries and nothing external points at the current intake URL.
-- Were that untrue this would be a backfill and a URL compatibility obligation instead; the
-- design would be identical and the work would not. Recorded here rather than remembered,
-- because the next person to read this will want to know why it was allowed to be destructive.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- ---------------------------------------------------------------------------------------
-- Environment
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS environment
(
    environment_id UUID        NOT NULL PRIMARY KEY,
    organization   TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    -- Customer-chosen, and an attribute rather than an identity: everything points at the
    -- identifier, so a rename changes what an Environment is called and nothing else.
    name           TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),

    -- The Default environment, created with the organization so that nothing downstream ever
    -- has to handle its absence. It may be renamed and may not be deleted.
    is_default     BOOLEAN     NOT NULL DEFAULT FALSE,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT environment_name_is_unique_per_organization UNIQUE (organization, name),

    -- The composite key every tenant-scoped reference below points at. Without it a foreign
    -- key naming only the identifier would let one organization's Connection sit inside
    -- another's Environment, and the boundary would be a property of whichever query was
    -- written correctly rather than something the database refuses.
    CONSTRAINT environment_identity_is_org_scoped UNIQUE (organization, environment_id)
);

-- One Default per organization. A partial unique index rather than application logic,
-- because "there is always exactly one" is the guarantee everything downstream is built on.
CREATE UNIQUE INDEX IF NOT EXISTS environment_one_default_per_organization
    ON environment (organization) WHERE is_default;

CREATE INDEX IF NOT EXISTS environment_organization_idx
    ON environment (organization, created_at DESC);

-- ---------------------------------------------------------------------------------------
-- Connection
-- ---------------------------------------------------------------------------------------

-- The relay identity is referenced by Connections, so it needs the same org-scoped composite
-- key an Environment has. One Relay may serve Connections in several Environments; what it
-- may never do is serve another organization's.
ALTER TABLE relay_registration
    ADD CONSTRAINT relay_registration_identity_is_org_scoped UNIQUE (organization, registration_id);

CREATE TABLE IF NOT EXISTS connection
(
    connection_id         UUID        NOT NULL PRIMARY KEY,
    organization          TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    -- The scope this Connection belongs to, and the Environment everything arriving through
    -- it inherits. Assigned here and nowhere else.
    environment_id        UUID        NOT NULL,

    -- Which Integration this is an instance of. The vocabulary is closed and lives in code;
    -- storing the name rather than an integer keeps a row readable during an incident, and
    -- the application refuses a value it has no adapter for.
    integration           TEXT        NOT NULL CHECK (length(integration) BETWEEN 1 AND 64),

    -- Operator-chosen, so a customer running two Alertmanager installations can tell them
    -- apart. Unique within the Environment: two Connections that cannot be distinguished are
    -- an operational trap rather than a flexibility.
    name                  TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),

    -- What this Connection is for, as a bit set: 1 trigger, 2 evidence, 3 both. A trigger
    -- Connection delivers Signals inbound; an evidence Connection answers bounded capability
    -- reads outbound. The two differ in direction, not in kind, which is why one table serves
    -- an Alertmanager webhook and a Kubernetes cluster.
    role                  SMALLINT    NOT NULL CHECK (role IN (1, 2, 3)),

    -- Where work against this Connection runs: 1 control_plane, 2 relay.
    locality              SMALLINT    NOT NULL CHECK (locality IN (1, 2)),

    -- The Relay installation that serves this Connection, when one does.
    relay_registration_id UUID,

    -- SHA-256 of the shared secret a trigger Connection's source must present. Only the digest
    -- is held: the secret is shown to the operator once at creation and no path reads it back,
    -- so a disclosure of this table does not yield the ability to forge deliveries.
    secret_digest         BYTEA CHECK (secret_digest IS NULL OR length(secret_digest) = 32),

    -- Optional metadata for grouping, filtering and selection. Never an authorization, a
    -- credential, or a tenant boundary, and never a substitute for an Environment.
    labels                JSONB       NOT NULL DEFAULT '{}'::jsonb,

    disabled_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT connection_name_is_unique_per_environment UNIQUE (environment_id, name),

    -- The composite key relay_job points at, so a job cannot name another organization's
    -- Connection however its arguments were assembled.
    CONSTRAINT connection_identity_is_org_scoped UNIQUE (organization, connection_id),

    -- An Environment in the SAME organization. This is the tenant boundary enforced by the
    -- database rather than by a WHERE clause someone has to remember.
    CONSTRAINT connection_environment_is_in_the_same_organization
        FOREIGN KEY (organization, environment_id)
            REFERENCES environment (organization, environment_id),

    -- Likewise for the Relay: an installation belonging to this organization, or none.
    CONSTRAINT connection_relay_is_in_the_same_organization
        FOREIGN KEY (organization, relay_registration_id)
            REFERENCES relay_registration (organization, registration_id),

    -- A relay-local Connection names the installation that serves it; a central one names
    -- none. Enforced here rather than remembered by the application, in the same shape as the
    -- job table's whole-lease constraint.
    CONSTRAINT connection_relay_binding_matches_its_locality CHECK (
        (locality = 2) = (relay_registration_id IS NOT NULL)
        ),

    -- A trigger Connection carries the secret its source presents; an evidence-only one is
    -- reached outbound and has nothing to present, so it carries none. A trigger without a
    -- secret would accept anything, which is the failure this states rather than documents.
    CONSTRAINT connection_trigger_carries_a_secret CHECK (
        (role IN (1, 3)) = (secret_digest IS NOT NULL)
        )
);

CREATE INDEX IF NOT EXISTS connection_environment_idx
    ON connection (organization, environment_id, created_at DESC);

-- ---------------------------------------------------------------------------------------
-- Signals move from alert sources to Connections
-- ---------------------------------------------------------------------------------------

-- Dropped in dependency order. See the destructiveness note at the top of this file.
DROP TABLE IF EXISTS signal_delivery;
DROP TABLE IF EXISTS signal;
DROP TABLE IF EXISTS alert_source;

CREATE TABLE IF NOT EXISTS signal
(
    signal_id      UUID        NOT NULL PRIMARY KEY,
    organization   TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    connection_id  UUID        NOT NULL,

    -- Inherited from the Connection that delivered this, never declared by the caller, and
    -- persisted rather than joined: a Connection that later moves must not silently rewrite
    -- the scope every Signal it already delivered was gathered under.
    environment_id UUID        NOT NULL,

    -- The source's own notion of which alert this is. Deduplication uses it because every
    -- alerting system already has one, and inventing a different one guarantees disagreement
    -- about what "the same alert" means.
    --
    -- It identifies the ALERT, not this occurrence of it. Alertmanager's fingerprint is a hash
    -- of the label set, so the same disk filling up next week carries the same one.
    source_key     TEXT        NOT NULL CHECK (length(source_key) BETWEEN 1 AND 512),

    -- 1 firing, 2 resolved.
    status         SMALLINT    NOT NULL CHECK (status IN (1, 2)),

    title          TEXT        NOT NULL CHECK (length(title) <= 512),
    -- Free text from the customer's systems. Untrusted for its whole life: it may be
    -- attacker-influenced and must never become an instruction, a destination, or an
    -- authorisation claim downstream.
    summary        TEXT        NOT NULL CHECK (length(summary) <= 4096),
    -- Normalised labels. The keys are the source's; no vocabulary is imposed here.
    labels         JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- When the source says this episode began and ended, and when we first heard of it.
    -- Collapsing the source's clock into ours would make a delayed delivery indistinguishable
    -- from a delayed failure, which matters because an investigator reasons about ordering.
    started_at     TIMESTAMPTZ NOT NULL,
    resolved_at    TIMESTAMPTZ,
    received_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT signal_connection_is_in_the_same_organization
        FOREIGN KEY (organization, connection_id)
            REFERENCES connection (organization, connection_id),
    CONSTRAINT signal_environment_is_in_the_same_organization
        FOREIGN KEY (organization, environment_id)
            REFERENCES environment (organization, environment_id),

    CONSTRAINT signal_resolution_is_stamped CHECK ((status = 2) = (resolved_at IS NOT NULL)),
    CONSTRAINT signal_resolution_follows_its_start CHECK (
        resolved_at IS NULL OR resolved_at >= started_at
        ),

    -- One row per EPISODE, not per alert. The start time is part of the identity because the
    -- source key is not: keyed on the source key alone, a re-fire would overwrite the resolved
    -- record of the previous occurrence and the history would quietly lose it.
    --
    -- A redelivery of the same episode is still an update rather than a second row, because
    -- the source reports the same start time for it. The database decides that, rather than a
    -- read-then-write two concurrent deliveries could both pass.
    CONSTRAINT signal_episode_is_unique UNIQUE (connection_id, source_key, started_at)
);

CREATE INDEX IF NOT EXISTS signal_source_key_idx
    ON signal (connection_id, source_key, started_at DESC);

CREATE INDEX IF NOT EXISTS signal_environment_idx
    ON signal (organization, environment_id, received_at DESC);

-- Every accepted delivery, recorded before the signals in it are applied.
--
-- It exists to make at-least-once webhooks idempotent at the boundary: a source that retries
-- because it did not see a response must not produce a second anything. The same uniqueness is
-- this surface's replay protection — a body replayed by anyone is recognised as one already
-- accepted and applied to nothing.
CREATE TABLE IF NOT EXISTS signal_delivery
(
    delivery_id   UUID        NOT NULL PRIMARY KEY,
    organization  TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    connection_id UUID        NOT NULL,

    -- SHA-256 over the raw body as received. The dedup identity for a whole delivery, chosen
    -- over a source-supplied identifier because not every source sends one and a body that
    -- hashes the same IS the same delivery.
    body_digest   BYTEA       NOT NULL CHECK (length(body_digest) = 32),

    signal_count  INTEGER     NOT NULL DEFAULT 0 CHECK (signal_count >= 0),

    -- How many alerts the source says it left out of this delivery. A non-zero value is
    -- evidence that this platform's record of that moment is incomplete through no fault of
    -- its own; dropping it would make a truncated delivery indistinguishable from a complete one.
    truncated     INTEGER     NOT NULL DEFAULT 0 CHECK (truncated >= 0),

    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT signal_delivery_connection_is_in_the_same_organization
        FOREIGN KEY (organization, connection_id)
            REFERENCES connection (organization, connection_id),
    CONSTRAINT signal_delivery_is_unique_per_connection UNIQUE (connection_id, body_digest)
);

CREATE INDEX IF NOT EXISTS signal_delivery_organization_idx
    ON signal_delivery (organization, received_at DESC);

-- ---------------------------------------------------------------------------------------
-- Every dispatched job names the Connection it reaches
-- ---------------------------------------------------------------------------------------

-- NOT NULL from the start, with no backfill, because no deployment holds a job row. This is
-- what makes the environment boundary a checked precondition on the EXECUTION path rather than
-- a property of whichever query happened to scope itself correctly: without a connection on
-- the job there is nothing for an equality check to compare.
--
-- If this fails because rows exist, the assumption recorded at the top of this file was wrong
-- for that database. Failing loudly is the correct outcome; a nullable column would have
-- carried the gap forward silently.
ALTER TABLE relay_job
    ADD COLUMN connection_id UUID NOT NULL;

ALTER TABLE relay_job
    ADD CONSTRAINT relay_job_connection_is_in_the_same_organization
        FOREIGN KEY (organization, connection_id)
            REFERENCES connection (organization, connection_id);

COMMENT ON TABLE environment IS
    'Customer-named scopes grouping Connections. A relevance boundary, never execution isolation.';
COMMENT ON TABLE connection IS
    'One configured instance of an Integration. The sole authority for the Environment of what arrives through it.';
COMMENT ON COLUMN connection.integration IS
    'The kind of system this is an instance of. Closed vocabulary, compiled; many Connections may share one.';
COMMENT ON COLUMN connection.role IS
    'Bit set: 1 trigger (delivers Signals inbound), 2 evidence (answers capability reads outbound), 3 both.';
COMMENT ON TABLE signal IS
    'Normalised alerts. Nothing downstream can tell which system delivered one; each carries its Connection''s Environment.';
COMMENT ON COLUMN relay_job.connection_id IS
    'What this job reaches. The Relay is where it runs; this is what it runs against.';
