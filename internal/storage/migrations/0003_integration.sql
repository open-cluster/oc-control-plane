-- Integration Types, Integrations, and the inbound delivery record.
--
-- An Integration Type is a kind of tool OpenCluster supports — Alertmanager, Kubernetes,
-- later Slack and GitHub. The rows here are PRODUCT-OWNED REFERENCE DATA: seeded by
-- migration, never created or edited through any API. Everything behavioral about a type —
-- its configuration schema, capabilities, client, verification — lives in provider Go code
-- keyed by the same stable key; a test proves the two sets agree, so drift fails the build.
--
-- An Integration is one configured installation belonging to an organization: "Production
-- Alertmanager", "Acme Slack". org_id is the tenant boundary; there is no Environment, no
-- Connection role, and no execution-locality column — where work runs is derivable from
-- whether a relay serves the integration.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS integration_type
(
    -- Deliberately small and deliberately seeded with explicit values: the id is a frozen
    -- persisted enum the application compiles as constants, so no runtime lookup maps a
    -- key to an id.
    integration_type_id SMALLINT    NOT NULL PRIMARY KEY,

    -- The stable key provider code is organized around: 'alertmanager', 'kubernetes'.
    -- Immutable by contract; display names are metadata and never identifiers.
    key                 TEXT        NOT NULL UNIQUE CHECK (length(key) BETWEEN 1 AND 64),

    name                TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    description         TEXT        NOT NULL DEFAULT '' CHECK (length(description) <= 512),

    -- A logo asset key the frontend resolves against its brand registry. Empty means the
    -- neutral category icon is drawn.
    logo                TEXT        NOT NULL DEFAULT '' CHECK (length(logo) <= 64),

    -- A controlled value, owned in code and frozen like the schema's other persisted
    -- enums. A category table is deliberately not built: six static values with no
    -- per-category metadata do not justify a table, a seed, and a join.
    category            TEXT        NOT NULL CHECK (length(category) BETWEEN 1 AND 64),

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The first two types. A later provider adds its row in its own migration.
INSERT INTO integration_type (integration_type_id, key, name, description, logo, category)
VALUES (1, 'alertmanager', 'Prometheus Alertmanager',
        'Receive firing and resolved alerts from your existing Alertmanager over a webhook.',
        'alertmanager', 'alerting'),
       (2, 'kubernetes', 'Kubernetes',
        'Read workloads, events and container logs through a Relay running in your cluster.',
        'kubernetes', 'orchestration')
ON CONFLICT (integration_type_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS integration
(
    integration_id           UUID        NOT NULL PRIMARY KEY,
    org_id                   TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    integration_type_id      SMALLINT    NOT NULL
        REFERENCES integration_type (integration_type_id),

    -- Operator-chosen, so a customer running two Alertmanager installations can tell them
    -- apart. Unique within the organization: several integrations of one type are
    -- expressly allowed; two that cannot be distinguished are not.
    name                     TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),

    -- Provider-specific non-secret settings, shaped by the type's configuration schema.
    -- The schema is closed (additionalProperties false) and the application refuses a
    -- field it does not declare. NOTHING SECRET IS EVER STORED HERE.
    configuration            JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- SHA-256 of the shared secret an inbound source must present. Only the digest is
    -- held: the secret is shown to the operator once and no path reads it back. NULL for
    -- a type that receives no webhooks.
    webhook_secret_digest    BYTEA CHECK (
        webhook_secret_digest IS NULL OR length(webhook_secret_digest) = 32),

    -- A MINTED identity for the live webhook secret, never derived from it: a truncated
    -- hash would let a database dump confirm a guess offline. What an operator needs is to
    -- tell one secret from the next after a rotation, and a minted value does that.
    webhook_secret_fingerprint TEXT CHECK (
        webhook_secret_fingerprint IS NULL OR length(webhook_secret_fingerprint) BETWEEN 1 AND 64),
    webhook_secret_created_at  TIMESTAMPTZ,
    webhook_secret_rotated_at  TIMESTAMPTZ,

    -- Optional metadata for grouping and filtering. Never an authorization, a credential,
    -- or a tenant boundary.
    labels                   JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- The Relay installation that serves this integration, when one does. Where work runs
    -- is derived from this column being set; there is no locality column.
    relay_id                 UUID,

    -- Observed health: 1 configured (nothing has checked it), 2 active, 3 degraded,
    -- 4 failed. `disabled` is deliberately not in this list: disabled_at says an operator
    -- turned it off, and collapsing the two would lose whether it was working when they
    -- did — which is what they need to know when they turn it back on.
    status                   SMALLINT    NOT NULL DEFAULT 1 CHECK (status IN (1, 2, 3, 4)),
    last_verified_at         TIMESTAMPTZ,
    -- What the last verification established or could not, in the operator's language.
    verify_note              TEXT        NOT NULL DEFAULT '' CHECK (length(verify_note) <= 512),

    disabled_at              TIMESTAMPTZ,
    created_by               TEXT        NOT NULL DEFAULT '' CHECK (length(created_by) <= 256),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT integration_name_is_unique_per_org UNIQUE (org_id, name),

    -- The composite key every tenant-scoped reference points at, so a job or a signal
    -- cannot name another organization's integration however its request was assembled.
    CONSTRAINT integration_identity_is_org_scoped UNIQUE (org_id, integration_id),

    -- A Relay belonging to this organization, or none. The tenant boundary enforced by
    -- the database rather than by a WHERE clause someone has to remember.
    CONSTRAINT integration_relay_is_in_the_same_org
        FOREIGN KEY (org_id, relay_id)
            REFERENCES relay_registration (org_id, registration_id),

    -- The webhook secret metadata travels together or not at all.
    CONSTRAINT integration_webhook_secret_is_whole CHECK (
        (webhook_secret_digest IS NULL AND webhook_secret_fingerprint IS NULL
            AND webhook_secret_created_at IS NULL)
            OR (webhook_secret_digest IS NOT NULL AND webhook_secret_fingerprint IS NOT NULL
            AND webhook_secret_created_at IS NOT NULL)
        )
);

CREATE INDEX IF NOT EXISTS integration_org_idx
    ON integration (org_id, created_at DESC);
CREATE INDEX IF NOT EXISTS integration_relay_idx
    ON integration (org_id, relay_id) WHERE relay_id IS NOT NULL;

-- Every webhook delivery attempt that reached a real Integration, accepted or not.
--
-- One table serves both jobs the old schema split in two: the ACCEPTED rows carry the
-- body digest whose uniqueness makes an at-least-once webhook idempotent (a source that
-- retries because it never saw a response produces a duplicate, not a second anything),
-- and every row records what happened, so a source that is delivering and being rejected
-- is distinguishable from a source that has gone quiet. Those two call for opposite
-- actions.
--
-- WHAT BOUNDS THE WRITES: the intake surface's per-integration rate limiter runs BEFORE
-- any of this, and an attempt naming an identifier that resolves to no row is never
-- recorded at all.
CREATE TABLE IF NOT EXISTS integration_delivery
(
    delivery_id    UUID        NOT NULL PRIMARY KEY,
    org_id         TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    integration_id UUID        NOT NULL,

    -- 1 accepted, 2 duplicate, 3 rejected. Duplicate is its own outcome: a retrying
    -- source has done nothing wrong and must be told to stop; counting it as a rejection
    -- would make a healthy retry look like a broken signature.
    outcome        SMALLINT    NOT NULL CHECK (outcome IN (1, 2, 3)),

    -- SHA-256 over the raw body as received; the dedup identity for a delivery. NULL only
    -- for a rejection that never read the body. NOTHING FROM THE PAYLOAD OR THE HEADERS
    -- IS EVER STORED HERE beyond this digest.
    body_digest    BYTEA CHECK (body_digest IS NULL OR length(body_digest) = 32),

    -- Why, for a rejection: 'unauthenticated', 'disabled', 'malformed', 'oversized',
    -- 'rate_limited', 'incomplete'. Empty otherwise.
    reason         TEXT        NOT NULL DEFAULT '' CHECK (length(reason) <= 64),

    signal_count   INTEGER     NOT NULL DEFAULT 0 CHECK (signal_count >= 0),

    -- How many alerts the source says it left out of this delivery. A non-zero value is
    -- evidence that this platform's record of that moment is incomplete through no fault
    -- of its own.
    truncated      INTEGER     NOT NULL DEFAULT 0 CHECK (truncated >= 0),

    received_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT integration_delivery_is_in_the_same_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id) ON DELETE CASCADE,
    CONSTRAINT integration_delivery_states_a_reason_exactly_when_it_refused CHECK (
        (outcome = 3) = (reason <> '')
        ),
    CONSTRAINT integration_delivery_accepted_carries_a_digest CHECK (
        outcome <> 1 OR body_digest IS NOT NULL
        )
);

-- The idempotence key: one ACCEPTED row per body per integration. Partial, so duplicate
-- and rejected rows can record the same digest without fighting the constraint.
CREATE UNIQUE INDEX IF NOT EXISTS integration_delivery_accepted_is_unique
    ON integration_delivery (integration_id, body_digest) WHERE outcome = 1;

CREATE INDEX IF NOT EXISTS integration_delivery_integration_idx
    ON integration_delivery (org_id, integration_id, received_at DESC, delivery_id DESC);

-- The health read behind an integration's summary: last accepted delivery.
CREATE INDEX IF NOT EXISTS integration_delivery_accepted_idx
    ON integration_delivery (integration_id, received_at DESC) WHERE outcome = 1;

COMMENT ON TABLE integration_type IS
    'Product-owned reference data: the kinds of tool OpenCluster supports. Seeded by migration; behavior lives in provider Go code keyed by the stable key.';
COMMENT ON TABLE integration IS
    'One configured installation belonging to an organization. No Environment, no role, no locality: org_id is the boundary and the relay binding says where work runs.';
COMMENT ON COLUMN integration.webhook_secret_fingerprint IS
    'A minted identity for the live webhook secret. Not derived from it: a truncated hash would let a dump confirm a guess offline.';
COMMENT ON TABLE integration_delivery IS
    'Every webhook delivery attempt that reached a real integration. Accepted rows are the idempotence key; the rest are the health record.';
