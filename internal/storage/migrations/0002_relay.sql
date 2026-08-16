-- Relay enrolment, durable relay identity, presence, and the conflict trail.
--
-- Both credential tables hold only digests of the secrets involved. A disclosure of this
-- database yields no working token and no working relay identity, which is the property
-- that lets the credential be sent to the wire exactly once and never read back.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS relay_bootstrap_token
(
    -- SHA-256 of the token. The token itself is never stored, so the primary key is also
    -- the only lookup path: presenting a token is the sole way to name a row.
    token_digest BYTEA       NOT NULL PRIMARY KEY CHECK (length(token_digest) = 32),

    -- The organization this token may enrol into. A token consumed under any other
    -- claimed organization is refused, so a leaked token cannot cross a tenant boundary.
    org_id       TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    expires_at   TIMESTAMPTZ NOT NULL,

    -- Set by the transaction that spends the token. The guard is on this column being
    -- null, so two concurrent presentations serialise on the row and exactly one wins.
    consumed_at  TIMESTAMPTZ,

    -- Present so a token can be withdrawn before it is spent.
    revoked_at   TIMESTAMPTZ,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS relay_registration
(
    -- Server-minted. Half of the relay identity; the organization is the other half.
    registration_id        UUID        NOT NULL PRIMARY KEY,

    org_id                 TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    -- SHA-256 of the durable credential. High-entropy and server-generated, so a digest
    -- rather than a slow key derivation.
    credential_digest      BYTEA       NOT NULL CHECK (length(credential_digest) = 32),

    -- The cluster this relay claimed at enrolment. Pinned so a later change refuses or
    -- flags rather than silently re-attributing evidence to the wrong cluster.
    cluster_fingerprint    TEXT        NOT NULL,

    -- Attestations, kept for support-window decisions.
    relay_version          TEXT        NOT NULL,
    capabilities           JSONB       NOT NULL,

    -- Fail-closed: a revoked registration authenticates nothing.
    revoked_at             TIMESTAMPTZ,

    -- Contested-identity summary: when this identity was last seen contested, and how many
    -- distinct hosts were seen taking the session. More than one is the credential-theft
    -- signature. A cached reading of the event trail below, written in the same
    -- transaction, so the two cannot disagree.
    session_conflict_at    TIMESTAMPTZ,
    session_conflict_hosts INTEGER     NOT NULL DEFAULT 0,

    -- Durable presence, written by the session that holds this relay, so "connected" is
    -- the same answer from every instance. Connected is derived — last_seen_at inside the
    -- liveness allowance AND no ending recorded — rather than stored as a boolean.
    session_id             UUID,
    session_started_at     TIMESTAMPTZ,
    session_ended_at       TIMESTAMPTZ,
    last_seen_at           TIMESTAMPTZ,
    session_peer           TEXT CHECK (session_peer IS NULL OR length(session_peer) <= 256),

    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The composite key every tenant-scoped reference points at, so a row in another table
    -- cannot name another organization's relay however its request was assembled.
    CONSTRAINT relay_registration_identity_is_org_scoped UNIQUE (org_id, registration_id)
);

CREATE INDEX IF NOT EXISTS relay_registration_org_idx
    ON relay_registration (org_id, created_at DESC);
CREATE INDEX IF NOT EXISTS relay_registration_presence_idx
    ON relay_registration (org_id, last_seen_at DESC);

-- The record of a relay identity being contested, and of someone saying it has been dealt
-- with. Append-only: what an operator most needs on the second occurrence is that there
-- was a first.
CREATE TABLE IF NOT EXISTS relay_session_conflict_event
(
    event_id        BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    org_id          TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    registration_id UUID        NOT NULL,

    -- 1 detected, 2 withdrawn.
    kind            SMALLINT    NOT NULL CHECK (kind IN (1, 2)),

    -- Distinct hosts seen taking the session, as observed at the moment of a detection. A
    -- withdrawal observes nothing and carries 0.
    distinct_hosts  INTEGER     NOT NULL DEFAULT 0 CHECK (distinct_hosts >= 0),

    -- Where a withdrawal came from. A detection has no actor.
    withdrawn_from  TEXT,

    at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT relay_session_conflict_event_actor_belongs_to_a_withdrawal CHECK (
        kind = 2 OR withdrawn_from IS NULL
        )
);

CREATE INDEX IF NOT EXISTS relay_session_conflict_event_registration_idx
    ON relay_session_conflict_event (org_id, registration_id, event_id DESC);

COMMENT ON TABLE relay_bootstrap_token IS
    'Single-use enrolment tokens, stored as digests. Consumption and issuance commit together.';
COMMENT ON TABLE relay_registration IS
    'Durable relay identities. The credential is returned once at enrolment and never read back.';
COMMENT ON TABLE relay_session_conflict_event IS
    'Append-only trail of contested relay identities and of the withdrawals that acknowledged them.';
