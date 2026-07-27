-- Relay enrolment: single-use bootstrap tokens, and the durable identities they mint.
--
-- Both tables hold only digests of the secrets involved. A disclosure of this database
-- yields no working token and no working relay identity, which is the property that lets
-- the credential be sent to the wire exactly once and never read back.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS relay_bootstrap_token
(
    -- SHA-256 of the token. The token itself is never stored, so the primary key is also
    -- the only lookup path: presenting a token is the sole way to name a row.
    token_digest BYTEA       NOT NULL PRIMARY KEY CHECK (length(token_digest) = 32),

    -- The organization this token may enrol into. A token consumed under any other
    -- claimed organization is refused, so a leaked token cannot cross a tenant boundary.
    organization TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    expires_at   TIMESTAMPTZ NOT NULL,

    -- Set by the transaction that spends the token. The guard is on this column being
    -- null, so two concurrent presentations serialise on the row and exactly one wins.
    consumed_at  TIMESTAMPTZ,

    -- Present so a token can be withdrawn before it is spent. Revocation and expiry are
    -- distinguished here and deliberately not distinguished to the caller.
    revoked_at   TIMESTAMPTZ,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS relay_registration
(
    -- Server-minted. Half of the relay identity; the organization is the other half.
    registration_id     UUID        NOT NULL PRIMARY KEY,

    organization        TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    -- SHA-256 of the durable credential. High-entropy and server-generated, so a digest
    -- rather than a slow key derivation: the reason to make verification expensive is to
    -- resist offline attack on a low-entropy human secret, which this is not, while the
    -- cost would fall on every session establishment.
    credential_digest   BYTEA       NOT NULL CHECK (length(credential_digest) = 32),

    -- The cluster this relay claimed at enrolment. Pinned so a later change refuses or
    -- flags rather than silently re-attributing evidence to the wrong cluster.
    cluster_fingerprint TEXT        NOT NULL,

    -- Attestations, kept for support-window decisions and for diagnosing a relay that
    -- advertises something the control plane will not dispatch.
    relay_version       TEXT        NOT NULL,
    capabilities        JSONB       NOT NULL,

    -- Fail-closed: a revoked registration authenticates nothing.
    revoked_at          TIMESTAMPTZ,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Enrolment counts per organization, and later the roster an operator reads. Without it
-- both are sequential scans over every tenant's registrations.
CREATE INDEX IF NOT EXISTS relay_registration_organization_idx
    ON relay_registration (organization, created_at DESC);

COMMENT ON TABLE relay_bootstrap_token IS
    'Single-use enrolment tokens, stored as digests. Consumption and issuance commit together.';
COMMENT ON TABLE relay_registration IS
    'Durable relay identities. The credential is returned once at enrolment and never read back.';
