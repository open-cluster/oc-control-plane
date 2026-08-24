-- Hosted audit delivery is asynchronous. The authoritative local Audit Event and this
-- serialized copy are inserted in the same transaction; remote availability is never part
-- of whether the application mutation commits.
CREATE TABLE audit_forwarding_outbox
(
    event_id         UUID        NOT NULL PRIMARY KEY,
    org_id           TEXT        NOT NULL,
    event_payload    JSONB       NOT NULL,
    attempts         INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    terminal         BOOLEAN     NOT NULL DEFAULT FALSE,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner      TEXT,
    lease_until      TIMESTAMPTZ,
    last_error       TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_attempt_at  TIMESTAMPTZ,
    CHECK ((lease_owner IS NULL) = (lease_until IS NULL))
);

CREATE INDEX audit_forwarding_ready_idx
    ON audit_forwarding_outbox (next_attempt_at, event_id)
    WHERE terminal = FALSE;

CREATE INDEX audit_forwarding_org_idx
    ON audit_forwarding_outbox (org_id, terminal, created_at DESC);

COMMENT ON TABLE audit_forwarding_outbox IS
    'Optional hosted delivery queue carrying an Audit Event independently until successful delivery or explicit replay.';

-- Existing envelopes have no key identifier and therefore remain NULL until the rotation
-- worker authenticates and rewraps them. New writes always set this from their envelope.
ALTER TABLE integration
    ADD COLUMN credential_key_id TEXT;

COMMENT ON COLUMN integration.credential_key_id IS
    'Non-secret key identifier from the credential envelope; NULL only for the version-1 compatibility envelope.';
