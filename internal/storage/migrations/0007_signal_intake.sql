-- Alerts arriving from the systems a customer already runs, and what they normalise to.
--
-- The product does not detect. It accepts what a customer's alerting already found and starts
-- investigating before a human arrives, so this is the boundary where someone else's alert
-- becomes something this platform can reason about.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- A configured alerting source. One row is one webhook a customer points at this platform.
CREATE TABLE IF NOT EXISTS alert_source
(
    source_id     UUID        NOT NULL PRIMARY KEY,
    organization  TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    -- Which adapter parses this source's payloads. The vocabulary is closed and lives in code;
    -- storing the name rather than an integer keeps a row readable during an incident.
    kind          TEXT        NOT NULL CHECK (length(kind) BETWEEN 1 AND 64),

    -- Operator-chosen label, so a customer with several sources can tell them apart.
    name          TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),

    -- SHA-256 of the shared secret the source must present. Only the digest is held: the
    -- secret is shown to the operator once at creation and no path reads it back, so a
    -- disclosure of this table does not yield the ability to forge alerts.
    secret_digest BYTEA       NOT NULL CHECK (length(secret_digest) = 32),

    disabled_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Intake resolves a source by its identity on every delivery, so this is the hot path.
CREATE INDEX IF NOT EXISTS alert_source_organization_idx
    ON alert_source (organization, created_at DESC);

-- A normalised alert. Nothing downstream of this table can tell which system sent it.
CREATE TABLE IF NOT EXISTS signal
(
    signal_id      UUID        NOT NULL PRIMARY KEY,
    organization   TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    source_id      UUID        NOT NULL REFERENCES alert_source (source_id),

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

    CONSTRAINT signal_resolution_is_stamped CHECK ((status = 2) = (resolved_at IS NOT NULL)),
    CONSTRAINT signal_resolution_follows_its_start CHECK (
        resolved_at IS NULL OR resolved_at >= started_at
        ),

    -- One row per EPISODE, not per alert. The start time is part of the identity because the
    -- source key is not: Alertmanager's fingerprint is a hash of the label set, so the same
    -- disk filling up next month arrives under the same one. Keyed on the source key alone,
    -- a re-fire would overwrite the resolved record of the previous occurrence and the
    -- history would quietly lose it — which is exactly the rewriting this model exists to
    -- prevent.
    --
    -- A redelivery of the same episode is still an update rather than a second row, because
    -- the source reports the same start time for it. The database decides that, rather than a
    -- read-then-write two concurrent deliveries could both pass.
    CONSTRAINT signal_episode_is_unique UNIQUE (source_id, source_key, started_at)
);

-- Reading one alert's history: every episode of it, newest first.
CREATE INDEX IF NOT EXISTS signal_source_key_idx
    ON signal (source_id, source_key, started_at DESC);

CREATE INDEX IF NOT EXISTS signal_organization_idx
    ON signal (organization, received_at DESC);

-- Every accepted delivery, recorded before the signals in it are applied.
--
-- It exists to make at-least-once webhooks idempotent at the boundary: a source that retries
-- because it did not see a response must not produce a second anything. It also answers the
-- operator question the spec asks for — whether a source is delivering, or whether a quiet
-- night is a broken integration.
CREATE TABLE IF NOT EXISTS signal_delivery
(
    delivery_id   UUID        NOT NULL PRIMARY KEY,
    organization  TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    source_id     UUID        NOT NULL REFERENCES alert_source (source_id),

    -- SHA-256 over the raw body as received. The dedup identity for a whole delivery, chosen
    -- over a source-supplied identifier because not every source sends one and a body that
    -- hashes the same IS the same delivery.
    body_digest   BYTEA       NOT NULL CHECK (length(body_digest) = 32),

    signal_count  INTEGER     NOT NULL DEFAULT 0 CHECK (signal_count >= 0),

    -- How many alerts the source says it left out of this delivery. Alertmanager truncates a
    -- payload at its configured maximum and reports the remainder rather than sending it, so a
    -- non-zero value here is evidence that this platform's record of that moment is incomplete
    -- through no fault of its own. Recording it is what lets an operator see that; dropping it
    -- would make a truncated delivery indistinguishable from a complete one.
    truncated     INTEGER     NOT NULL DEFAULT 0 CHECK (truncated >= 0),

    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT signal_delivery_is_unique_per_source UNIQUE (source_id, body_digest)
);

CREATE INDEX IF NOT EXISTS signal_delivery_organization_idx
    ON signal_delivery (organization, received_at DESC);

COMMENT ON TABLE alert_source IS
    'Configured alerting sources. The shared secret is stored as a digest and never read back.';
COMMENT ON TABLE signal IS
    'Normalised alerts. Nothing downstream can tell which system delivered one.';
COMMENT ON TABLE signal_delivery IS
    'Accepted deliveries, deduplicated by body digest so an at-least-once webhook is idempotent.';
