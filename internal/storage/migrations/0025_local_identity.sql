-- Local authentication is additive so an upgrade can roll back without losing the identity
-- data the previous binary understands. Native SAML and SCIM rows remain retained and inert.

CREATE TABLE IF NOT EXISTS local_password
(
    user_id             UUID        NOT NULL PRIMARY KEY
        REFERENCES app_user (user_id) ON DELETE CASCADE,
    password_hash       TEXT        NOT NULL CHECK (length(password_hash) BETWEEN 32 AND 512),
    password_changed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE local_password IS
    'Argon2id password verifiers for local users. The encoded verifier contains versioned parameters and no recoverable password.';

CREATE TABLE IF NOT EXISTS deployment_sign_in_flow
(
    flow_id       UUID        NOT NULL PRIMARY KEY,
    org_id        TEXT        NOT NULL,
    state_digest  BYTEA       NOT NULL UNIQUE CHECK (octet_length(state_digest) = 32),
    code_verifier TEXT,
    nonce         TEXT,
    return_to     TEXT        NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    consumed_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS deployment_sign_in_flow_expiry
    ON deployment_sign_in_flow (expires_at);

COMMENT ON TABLE deployment_sign_in_flow IS
    'Short-lived PKCE, nonce, and state records for the deployment OIDC provider.';
