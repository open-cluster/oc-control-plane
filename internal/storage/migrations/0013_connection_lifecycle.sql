-- A Connection gains a lifecycle, a configuration with a revision number, credential metadata
-- that is not the credential, a validation history, and a record of what has been delivered
-- through it.
--
-- What this migration is FOR is the distance between "configured" and "works". Until now a
-- Connection had two observable states — it exists, and it is disabled — and neither of them
-- answers the question an operator actually has during an incident, which is whether the thing
-- at the far end is reachable and whether anything has arrived through it lately. A row that
-- cannot tell a source delivering and being rejected from a quiet night is a row that makes an
-- outage look like calm.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- ---------------------------------------------------------------------------------------
-- The Connection's own lifecycle
-- ---------------------------------------------------------------------------------------

ALTER TABLE connection
    -- Where this Connection has got to, as observed rather than as declared:
    --   1 configured  — it exists and nothing has checked it
    --   2 validating  — a check is running
    --   3 active      — the last check passed in full
    --   4 degraded    — the last check passed in part; some of what it offers is unavailable
    --   5 failed      — the last check did not pass
    --
    -- `disabled` is NOT in this list and is deliberately orthogonal: disabled_at already says
    -- an operator turned it off, and collapsing the two would lose whether a Connection that
    -- is turned off was working when it was turned off. That is exactly what somebody needs to
    -- know when they turn it back on during an incident.
    ADD COLUMN state                  SMALLINT    NOT NULL DEFAULT 1
        CHECK (state IN (1, 2, 3, 4, 5)),

    -- Increments on every accepted change to the configuration, so "it changed" is answerable
    -- and a validation result can name the revision it was a result ABOUT. A result carrying no
    -- revision is a result that keeps looking current after the thing it described was edited.
    ADD COLUMN configuration_revision INTEGER     NOT NULL DEFAULT 1
        CHECK (configuration_revision >= 1),

    -- The provider-specific configuration, shaped by the Integration definition's JSON Schema.
    -- It is JSONB rather than columns because the schema belongs to the definition and differs
    -- per provider; what stops it becoming a junk drawer is that the schema is closed
    -- (additionalProperties false) and the application refuses a field it does not declare.
    --
    -- NOTHING SECRET IS EVER STORED HERE. A write-only field is stripped before this column is
    -- written and lives as a digest and a reference; see the credential columns below.
    ADD COLUMN configuration          JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- What a read returns ABOUT the credential, which is never the credential.
    --
    -- The rule the trigger secret already follows applies to every credential: it is written
    -- once, no path reads it back, and an operator who loses it rotates rather than recovers.
    -- What an operator genuinely needs is to tell an expired token from a revoked one WITHOUT
    -- seeing either, and that needs identity, not content.
    --
    -- credential_fingerprint is therefore MINTED rather than derived. A truncated hash of the
    -- secret would let anyone holding a dump confirm a guess offline, which is the exact
    -- property digest-only storage exists to deny; a value minted at write time identifies
    -- which credential is in use and says nothing about what it is.
    ADD COLUMN credential_method      TEXT        CHECK (
        credential_method IS NULL OR length(credential_method) BETWEEN 1 AND 64),
    ADD COLUMN credential_reference   TEXT        CHECK (
        credential_reference IS NULL OR length(credential_reference) BETWEEN 1 AND 256),
    ADD COLUMN credential_fingerprint TEXT        CHECK (
        credential_fingerprint IS NULL OR length(credential_fingerprint) BETWEEN 1 AND 64),
    ADD COLUMN credential_created_at  TIMESTAMPTZ,
    ADD COLUMN credential_rotated_at  TIMESTAMPTZ,
    -- Null means no stated expiry, which is a fact about the credential rather than a missing
    -- value: a shared secret this platform minted does not expire on its own.
    ADD COLUMN credential_expires_at  TIMESTAMPTZ;

-- The credential metadata travels together or not at all. A reference with no method is a
-- credential nothing can interpret, and a fingerprint with no reference is an identity for
-- something nobody can find.
ALTER TABLE connection
    ADD CONSTRAINT connection_credential_metadata_is_whole CHECK (
        (credential_method IS NULL AND credential_reference IS NULL
            AND credential_fingerprint IS NULL AND credential_created_at IS NULL)
            OR (credential_method IS NOT NULL AND credential_reference IS NOT NULL
            AND credential_fingerprint IS NOT NULL AND credential_created_at IS NOT NULL)
        );

-- Every trigger Connection already holds a secret; this backfills the metadata describing it so
-- the credential surface is not empty for the rows that predate it. The reference names this
-- database's own column, because that IS the store for a trigger secret — an opaque reference to
-- somewhere else would be a claim about infrastructure this deployment does not have.
--
-- The fingerprint is minted from gen_random_uuid rather than from the digest, for the reason
-- stated above. pgcrypto is not required: gen_random_uuid has been in core since PostgreSQL 13.
UPDATE connection
   SET credential_method      = 'shared_secret',
       credential_reference   = 'connection-trigger-secret:' || connection_id::text,
       credential_fingerprint = replace(gen_random_uuid()::text, '-', ''),
       credential_created_at  = created_at
 WHERE secret_digest IS NOT NULL;

CREATE INDEX IF NOT EXISTS connection_state_idx
    ON connection (organization, state, created_at DESC);

-- ---------------------------------------------------------------------------------------
-- Validation history
-- ---------------------------------------------------------------------------------------

-- One run of a check against one Connection at one configuration revision.
--
-- It is a HISTORY rather than a column on the connection row because the question an operator
-- has is not only "does it work now" but "has it ever worked" — which is what tells an outage
-- from a configuration that was never right. A single current-state column cannot answer the
-- second, and the second is the one that decides whether to page somebody.
CREATE TABLE IF NOT EXISTS connection_validation
(
    validation_id          UUID        NOT NULL PRIMARY KEY,
    organization           TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    connection_id          UUID        NOT NULL,

    -- Which revision of the configuration this result is about. Without it a result keeps
    -- looking current after the configuration it described was edited.
    configuration_revision INTEGER     NOT NULL CHECK (configuration_revision >= 1),

    -- What the whole run amounted to: 1 passed, 2 partial, 3 failed.
    --
    -- PARTIAL IS A FIRST-CLASS OUTCOME and is the reason this table exists in this shape.
    -- "Authentication worked and two of five capabilities are unavailable" is a result an
    -- operator can act on; collapsed to a boolean it becomes a failure, and a failure is what
    -- somebody retries rather than reads.
    outcome                SMALLINT    NOT NULL CHECK (outcome IN (1, 2, 3)),

    -- Whether the credential was exercised, in the same readiness vocabulary the capabilities
    -- below use: 1 available, 2 unauthorized, 3 unavailable, 4 not_attempted. A provider that
    -- is reached inbound only has nothing to authenticate against and records not_attempted
    -- with a reason, rather than a pass that would mean nothing.
    authentication         SMALLINT    NOT NULL CHECK (authentication IN (1, 2, 3, 4)),
    authentication_reason  TEXT        NOT NULL DEFAULT '' CHECK (
        length(authentication_reason) <= 512),

    -- What this run does not establish, in the operator's language, taken from the Integration
    -- definition's validation contract.
    note                   TEXT        NOT NULL DEFAULT '' CHECK (length(note) <= 1024),

    started_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT connection_validation_connection_is_in_the_same_organization
        FOREIGN KEY (organization, connection_id)
            REFERENCES connection (organization, connection_id) ON DELETE CASCADE,
    CONSTRAINT connection_validation_finishes_after_it_starts CHECK (completed_at >= started_at),

    -- The composite key the per-capability rows point at, so one tenant's result cannot carry
    -- another tenant's capability rows.
    CONSTRAINT connection_validation_identity_is_org_scoped UNIQUE (organization, validation_id)
);

CREATE INDEX IF NOT EXISTS connection_validation_connection_idx
    ON connection_validation (organization, connection_id, completed_at DESC, validation_id DESC);

-- One capability's readiness inside one run. It is a row rather than a JSON array because it is
-- queried: "which connections cannot serve container logs" is the question behind a coverage
-- gap, and an array is the shape that answers it with a table scan and a parser.
CREATE TABLE IF NOT EXISTS connection_validation_capability
(
    validation_id UUID     NOT NULL,
    organization  TEXT     NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    -- A capability name internal/capability owns. Stored as text so a row read during an
    -- incident says `kubernetes.container.logs` rather than an integer.
    capability    TEXT     NOT NULL CHECK (length(capability) BETWEEN 1 AND 128),

    -- 1 available, 2 unauthorized, 3 unavailable, 4 not_attempted. It is the COVERAGE
    -- vocabulary rather than a second one invented here, because that is what a coverage gap
    -- will be assembled from and two vocabularies for one fact is one of them being wrong.
    readiness     SMALLINT NOT NULL CHECK (readiness IN (1, 2, 3, 4)),
    reason        TEXT     NOT NULL DEFAULT '' CHECK (length(reason) <= 512),

    PRIMARY KEY (validation_id, capability),
    CONSTRAINT connection_validation_capability_belongs_to_its_run
        FOREIGN KEY (organization, validation_id)
            REFERENCES connection_validation (organization, validation_id) ON DELETE CASCADE
);

-- ---------------------------------------------------------------------------------------
-- Trigger delivery health
-- ---------------------------------------------------------------------------------------

-- Every delivery attempt that reached a real Connection, accepted or not.
--
-- signal_delivery records what was ACCEPTED, and it must keep doing exactly that: it is the
-- idempotence key, and putting rejected attempts in it would break the uniqueness that makes
-- an at-least-once webhook safe. This table is the other half — the attempts, with why each
-- one ended the way it did — and without it a source that is delivering and being rejected is
-- indistinguishable from a source that has gone quiet. Those two call for opposite actions.
--
-- WHAT BOUNDS THE WRITES, since this is reachable by anything that can guess an identifier:
-- the intake surface's per-Connection rate limiter runs BEFORE any of this, and an attempt
-- naming an identifier that resolves to no row is never recorded at all. So the write rate is
-- bounded per existing Connection, and an attacker with a random identifier writes nothing.
CREATE TABLE IF NOT EXISTS trigger_delivery
(
    delivery_attempt_id UUID        NOT NULL PRIMARY KEY,
    organization        TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    connection_id       UUID        NOT NULL,

    -- 1 accepted, 2 duplicate, 3 rejected.
    --
    -- Duplicate is its own outcome rather than a kind of acceptance or a kind of rejection. A
    -- source retrying because it never saw a response has done nothing wrong and must be told
    -- to stop; counting those as rejections would make a healthy retry look like a broken
    -- signature, which is the precise confusion this table exists to end.
    outcome             SMALLINT    NOT NULL CHECK (outcome IN (1, 2, 3)),

    -- Why, for a rejection: 'unauthenticated', 'disabled', 'no_trigger_role', 'malformed',
    -- 'oversized', 'rate_limited', 'incomplete'. Empty for an acceptance.
    --
    -- NOTHING FROM THE PAYLOAD OR THE HEADERS IS EVER STORED HERE. The body is untrusted text
    -- from a customer's systems and the headers of a refused delivery are the one place
    -- guaranteed to hold a guess at the credential.
    reason              TEXT        NOT NULL DEFAULT '' CHECK (length(reason) <= 64),

    -- How many Signals this delivery carried, for an acceptance.
    signal_count        INTEGER     NOT NULL DEFAULT 0 CHECK (signal_count >= 0),

    -- Whether this was an operator's test rather than the customer's own system. A test that
    -- was indistinguishable from a real delivery would make "it is working" a claim resting on
    -- a delivery this platform sent itself.
    is_test             BOOLEAN     NOT NULL DEFAULT FALSE,

    received_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT trigger_delivery_connection_is_in_the_same_organization
        FOREIGN KEY (organization, connection_id)
            REFERENCES connection (organization, connection_id) ON DELETE CASCADE,
    CONSTRAINT trigger_delivery_states_a_reason_exactly_when_it_refused CHECK (
        (outcome = 3) = (reason <> '')
        )
);

CREATE INDEX IF NOT EXISTS trigger_delivery_connection_idx
    ON trigger_delivery (organization, connection_id, received_at DESC, delivery_attempt_id DESC);

-- The health read behind a trigger's summary: last received of anything, last accepted, and how
-- many were refused. Partial so it costs nothing on the rows nobody asks that question about.
CREATE INDEX IF NOT EXISTS trigger_delivery_accepted_idx
    ON trigger_delivery (connection_id, received_at DESC) WHERE outcome = 1;

-- ---------------------------------------------------------------------------------------
-- Relay presence
-- ---------------------------------------------------------------------------------------

-- Whether a Relay is connected RIGHT NOW, durably.
--
-- The live session registry is per process by construction, so a fleet summary built from it
-- would report what one instance can see and call it the fleet. These columns are written by
-- the session that holds the relay and read by anything that needs to answer "connected", which
-- makes the answer the same from every instance.
--
-- Connected is derived — last_seen_at inside the liveness allowance AND no ending recorded for
-- the current session — rather than stored as a boolean, because a boolean is a fact that goes
-- stale the moment a process dies without clearing it.
ALTER TABLE relay_registration
    ADD COLUMN session_id         UUID,
    ADD COLUMN session_started_at TIMESTAMPTZ,
    ADD COLUMN session_ended_at   TIMESTAMPTZ,
    ADD COLUMN last_seen_at       TIMESTAMPTZ,
    -- Which host is holding it. One host is a relay; more than one taking turns is the
    -- credential-theft signature the churn watch already names, and this is where the current
    -- one is recorded so an operator can see it without reading a log.
    ADD COLUMN session_peer       TEXT CHECK (session_peer IS NULL OR length(session_peer) <= 256);

CREATE INDEX IF NOT EXISTS relay_registration_presence_idx
    ON relay_registration (organization, last_seen_at DESC);

COMMENT ON COLUMN connection.state IS
    'Observed lifecycle: 1 configured, 2 validating, 3 active, 4 degraded, 5 failed. Disabled is separate.';
COMMENT ON COLUMN connection.configuration IS
    'Provider-specific configuration, shaped by the Integration definition. Never holds a credential.';
COMMENT ON COLUMN connection.credential_fingerprint IS
    'A minted identity for the credential in use. Not derived from it: a truncated hash would let a dump confirm a guess offline.';
COMMENT ON TABLE connection_validation IS
    'What a check of a Connection found, per configuration revision. Partial success is a first-class outcome.';
COMMENT ON TABLE trigger_delivery IS
    'Every delivery attempt that reached a real Connection. signal_delivery holds what was accepted; this holds what happened.';
COMMENT ON COLUMN relay_registration.last_seen_at IS
    'Durable presence, written by the session that holds this relay, so "connected" is the same answer from every instance.';
