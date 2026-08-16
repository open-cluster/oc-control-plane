-- Identity, sessions, roles and the record.
--
-- This is the first migration of the rebuilt schema. The previous migration history was
-- squashed on 2026-08-16, before any production deployment existed; version control holds
-- the old files. Everything here exists to answer four questions a security reviewer asks:
-- who is this, which tenants may they see, what may they do, and what did they do.
--
-- Tenancy. org_id is the only durable tenant identity, on every table in this schema. It is
-- an opaque identifier and never a name; nothing here stores an organization display name.
--
-- Naming. Tables are singular. `app_user` rather than `user` because `user` is reserved in
-- Postgres. `operator_session` rather than `session` because a Relay session already exists
-- in this schema and is a completely different subject.
--
-- Placement. A user row is not org-scoped — a person may hold memberships in several
-- organizations — but it IS placement-scoped, because a placement is a separate database.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- ---------------------------------------------------------------------------------------
-- The person
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS app_user
(
    user_id        UUID        NOT NULL PRIMARY KEY,

    -- Who the identity provider says this is. The pair is the identity: a subject is only
    -- unique within its issuer, and treating one issuer's subject as another's is how two
    -- people become one account.
    issuer         TEXT        NOT NULL CHECK (length(issuer) BETWEEN 1 AND 512),
    subject        TEXT        NOT NULL CHECK (length(subject) BETWEEN 1 AND 256),

    -- The address the provider asserted, and whether the provider said it had verified it.
    email          TEXT        NOT NULL CHECK (length(email) BETWEEN 0 AND 320),
    email_verified BOOLEAN     NOT NULL DEFAULT FALSE,

    display_name   TEXT        NOT NULL DEFAULT '' CHECK (length(display_name) <= 256),

    -- A disabled user keeps their history and can sign in to nothing.
    disabled_at    TIMESTAMPTZ,

    last_sign_in   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT app_user_identity_is_the_issuer_and_subject UNIQUE (issuer, subject)
);

CREATE INDEX IF NOT EXISTS app_user_email_idx ON app_user (lower(email));

-- ---------------------------------------------------------------------------------------
-- The membership: which tenant, and as what
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS organization_membership
(
    membership_id UUID        NOT NULL PRIMARY KEY,
    org_id        TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    user_id       UUID        NOT NULL REFERENCES app_user (user_id) ON DELETE CASCADE,

    -- One of the roles this build compiles: admin, editor, viewer, and the machine-only
    -- directory_synchroniser. Stored as text so a row is readable during an incident; the
    -- application refuses a value it has no role for, and an unrecognised one grants
    -- nothing rather than defaulting either way. NULL is the state a directory-provisioned
    -- person is in before any group they are in has been mapped.
    role          TEXT        CHECK (role IS NULL OR length(role) BETWEEN 1 AND 64),

    -- How this membership came to exist: 1 manual, 2 just-in-time at first sign-in, 3 SCIM.
    source        SMALLINT    NOT NULL CHECK (source IN (1, 2, 3)),

    -- The directory's own identifier for this person in this tenant, when SCIM owns it.
    external_id   TEXT        CHECK (external_id IS NULL OR length(external_id) BETWEEN 1 AND 256),

    -- Whether the DIRECTORY says this person is enabled. SCIM has no "gone", it has
    -- `active: false`, so the row survives and stops counting.
    active        BOOLEAN     NOT NULL DEFAULT TRUE,

    granted_by    TEXT        NOT NULL DEFAULT '' CHECK (length(granted_by) <= 256),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One role per person per tenant. Two rows would make "what may they do" a question
    -- with two answers.
    CONSTRAINT organization_membership_is_one_per_tenant UNIQUE (org_id, user_id)
);

CREATE INDEX IF NOT EXISTS organization_membership_user_idx
    ON organization_membership (user_id);
CREATE INDEX IF NOT EXISTS organization_membership_org_idx
    ON organization_membership (org_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS organization_membership_external_id_is_unique_per_org
    ON organization_membership (org_id, external_id) WHERE external_id IS NOT NULL;

-- ---------------------------------------------------------------------------------------
-- The identity provider a tenant configures
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS identity_provider
(
    provider_id            UUID        NOT NULL PRIMARY KEY,
    org_id                 TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    -- Operator-chosen, so a tenant serving a merged population can tell two providers apart.
    name                   TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),

    -- 1 OIDC, 2 SAML.
    protocol               SMALLINT    NOT NULL CHECK (protocol IN (1, 2)),

    issuer                 TEXT        NOT NULL CHECK (length(issuer) BETWEEN 1 AND 512),
    client_id              TEXT        CHECK (client_id IS NULL OR length(client_id) BETWEEN 1 AND 256),

    -- AES-256-GCM, nonce prepended, under a key this process reads from a file. Held
    -- encrypted rather than digested because it has to be PRESENTED to the token endpoint —
    -- unlike every other credential in this schema, which is only ever compared against.
    client_secret_sealed   BYTEA,

    -- The identity provider's own published SAML metadata document, pasted whole: entity
    -- id, sign-on URL and certificates change together at a key rotation.
    saml_metadata          TEXT,

    -- The email domains just-in-time provisioning may admit. Empty means none, so an
    -- unconfigured provider admits nobody rather than everybody.
    verified_domains       TEXT[]      NOT NULL DEFAULT ARRAY []::TEXT[],

    -- Whether a first-time signer-in is provisioned at all, and the role they land on.
    jit_enabled            BOOLEAN     NOT NULL DEFAULT FALSE,
    jit_role               TEXT        NOT NULL DEFAULT 'viewer' CHECK (length(jit_role) BETWEEN 1 AND 64),

    -- Whether the provider must have SAID it verified the address.
    require_verified_email BOOLEAN     NOT NULL DEFAULT TRUE,

    -- Which claim carries the provider's groups, and what each maps to. The map is
    -- {"group name": "role"}; a group naming no role this build has maps to nothing.
    group_claim            TEXT        NOT NULL DEFAULT 'groups' CHECK (length(group_claim) <= 128),
    group_role_map         JSONB       NOT NULL DEFAULT '{}'::jsonb,

    disabled_at            TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT identity_provider_name_is_unique_per_org UNIQUE (org_id, name),
    -- One tenant may configure several providers and must not configure the same client
    -- twice at the same issuer, which would make "which one answered" undecidable.
    CONSTRAINT identity_provider_client_is_unique_per_issuer
        UNIQUE (org_id, issuer, client_id),

    -- OIDC needs a client and a secret. SAML needs neither: the trust is a signing
    -- certificate the identity provider publishes.
    CONSTRAINT identity_provider_carries_what_its_protocol_needs CHECK (
        (protocol = 1 AND client_id IS NOT NULL AND client_secret_sealed IS NOT NULL
            AND saml_metadata IS NULL)
            OR
        (protocol = 2 AND saml_metadata IS NOT NULL AND client_id IS NULL
            AND client_secret_sealed IS NULL)
        )
);

CREATE INDEX IF NOT EXISTS identity_provider_org_idx
    ON identity_provider (org_id, created_at);

-- A SAML provider has no client identifier, so the OIDC uniqueness constraint does not
-- bind it: SQL treats two NULLs as distinct. One issuer per tenant, for SAML.
CREATE UNIQUE INDEX IF NOT EXISTS identity_provider_saml_issuer_is_unique_per_org
    ON identity_provider (org_id, issuer) WHERE protocol = 2;

-- ---------------------------------------------------------------------------------------
-- The sign-in flow in progress
-- ---------------------------------------------------------------------------------------

-- One row per authorization request the control plane started. It is what makes PKCE, the
-- state parameter and single-use redemption enforceable rather than asserted.
CREATE TABLE IF NOT EXISTS sign_in_flow
(
    flow_id       UUID        NOT NULL PRIMARY KEY,
    org_id        TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    provider_id   UUID        NOT NULL REFERENCES identity_provider (provider_id) ON DELETE CASCADE,

    -- SHA-256 of the state parameter. Digested because the value travels through the
    -- browser, and a disclosure of this table must not let anyone complete a flow somebody
    -- else started.
    state_digest  BYTEA       NOT NULL CHECK (length(state_digest) = 32),

    -- The PKCE verifier, held in the clear. Single-use, deleted at redemption, expires in
    -- minutes, and worthless without the matching authorization code.
    code_verifier TEXT        CHECK (code_verifier IS NULL OR length(code_verifier) BETWEEN 43 AND 128),

    -- Bound into the authorization request and checked against the ID token.
    nonce         TEXT        CHECK (nonce IS NULL OR length(nonce) BETWEEN 16 AND 128),

    -- The identifier of the AuthnRequest a SAML flow sent. The response must name it in
    -- InResponseTo.
    request_id    TEXT        CHECK (request_id IS NULL OR length(request_id) BETWEEN 1 AND 256),

    -- Where the browser goes once signed in. Validated as a same-site absolute path before
    -- it is written.
    return_to     TEXT        NOT NULL DEFAULT '/' CHECK (length(return_to) <= 512),

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    consumed_at   TIMESTAMPTZ,

    CONSTRAINT sign_in_flow_state_is_unique UNIQUE (state_digest),
    CONSTRAINT sign_in_flow_expires_after_it_started CHECK (expires_at > created_at),
    CONSTRAINT sign_in_flow_carries_what_its_protocol_needs CHECK (
        (code_verifier IS NOT NULL AND nonce IS NOT NULL AND request_id IS NULL)
            OR
        (request_id IS NOT NULL AND code_verifier IS NULL AND nonce IS NULL)
        )
);

CREATE INDEX IF NOT EXISTS sign_in_flow_expiry_idx ON sign_in_flow (expires_at);

-- ---------------------------------------------------------------------------------------
-- The session
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS operator_session
(
    session_id   UUID        NOT NULL PRIMARY KEY,

    -- SHA-256 of the opaque identifier in the cookie. Only the digest is held.
    token_digest BYTEA       NOT NULL CHECK (length(token_digest) = 32),

    user_id      UUID        NOT NULL REFERENCES app_user (user_id) ON DELETE CASCADE,

    -- The tenant the console was reading as when this was issued. A convenience for the
    -- interface and never an authorization fact: what a session may reach is decided from
    -- the user's memberships on every request.
    org_id       TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    issued_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Set when an administrator ends somebody else's session. Signing oneself out DELETES
    -- the row instead, so the credential is gone rather than marked.
    revoked_at   TIMESTAMPTZ,
    revoked_by   TEXT        NOT NULL DEFAULT '' CHECK (length(revoked_by) <= 256),

    -- What an administrator reads when deciding whether a live session is one they
    -- recognise.
    user_agent   TEXT        NOT NULL DEFAULT '' CHECK (length(user_agent) <= 256),
    address      TEXT        NOT NULL DEFAULT '' CHECK (length(address) <= 128),

    CONSTRAINT operator_session_digest_is_unique UNIQUE (token_digest),
    CONSTRAINT operator_session_expires_after_it_was_issued CHECK (expires_at > issued_at)
);

CREATE INDEX IF NOT EXISTS operator_session_user_idx
    ON operator_session (user_id, issued_at DESC);
CREATE INDEX IF NOT EXISTS operator_session_expiry_idx ON operator_session (expires_at);

-- ---------------------------------------------------------------------------------------
-- Automation
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS service_account
(
    service_account_id UUID        NOT NULL PRIMARY KEY,
    org_id             TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    name               TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    description        TEXT        NOT NULL DEFAULT '' CHECK (length(description) <= 512),

    disabled_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by         TEXT        NOT NULL DEFAULT '' CHECK (length(created_by) <= 256),

    CONSTRAINT service_account_name_is_unique_per_org UNIQUE (org_id, name),
    CONSTRAINT service_account_identity_is_org_scoped UNIQUE (org_id, service_account_id)
);

-- A token is bound to one organization, one role and an expiry, so a leak has a blast
-- radius and a deadline. The role is on the TOKEN rather than on the account.
CREATE TABLE IF NOT EXISTS api_token
(
    token_id           UUID        NOT NULL PRIMARY KEY,
    org_id             TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    service_account_id UUID        NOT NULL,

    token_digest       BYTEA       NOT NULL CHECK (length(token_digest) = 32),

    -- The first characters of the token, so an operator can tell which of their tokens a
    -- row is without the system holding a readable copy of any of them.
    prefix             TEXT        NOT NULL CHECK (length(prefix) BETWEEN 1 AND 32),

    role               TEXT        NOT NULL CHECK (length(role) BETWEEN 1 AND 64),

    -- Never null. A token with no deadline is an ambient root credential.
    expires_at         TIMESTAMPTZ NOT NULL,

    last_used_at       TIMESTAMPTZ,

    revoked_at         TIMESTAMPTZ,
    revoked_by         TEXT        NOT NULL DEFAULT '' CHECK (length(revoked_by) <= 256),

    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by         TEXT        NOT NULL DEFAULT '' CHECK (length(created_by) <= 256),

    CONSTRAINT api_token_digest_is_unique UNIQUE (token_digest),
    CONSTRAINT api_token_expires_after_it_was_created CHECK (expires_at > created_at),

    -- The account must belong to the same organization the token names, enforced by the
    -- database rather than by a WHERE clause someone has to remember.
    CONSTRAINT api_token_account_is_in_the_same_org
        FOREIGN KEY (org_id, service_account_id)
            REFERENCES service_account (org_id, service_account_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS api_token_account_idx
    ON api_token (org_id, service_account_id, created_at DESC);

-- ---------------------------------------------------------------------------------------
-- The tenant's own policy
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS organization_policy
(
    org_id                   TEXT        NOT NULL PRIMARY KEY
        CHECK (length(org_id) BETWEEN 1 AND 128),

    -- Zero means the product's default; the application holds whatever is set inside the
    -- bounds this build serves, so a policy may be tightened and not widened past them.
    session_lifetime_seconds INTEGER     NOT NULL DEFAULT 0 CHECK (session_lifetime_seconds >= 0),

    audit_retention_days     INTEGER     NOT NULL DEFAULT 0 CHECK (audit_retention_days >= 0),

    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by               TEXT        NOT NULL DEFAULT '' CHECK (length(updated_by) <= 256)
);

-- ---------------------------------------------------------------------------------------
-- The directory's groups
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS scim_group
(
    group_id     UUID        NOT NULL PRIMARY KEY,
    org_id       TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    display_name TEXT        NOT NULL CHECK (length(display_name) BETWEEN 1 AND 256),
    external_id  TEXT        CHECK (external_id IS NULL OR length(external_id) BETWEEN 1 AND 256),

    -- What this group grants. NULL grants nothing: a directory synchronises every group it
    -- is told to, and most of them are not about this product.
    role         TEXT        CHECK (role IS NULL OR length(role) BETWEEN 1 AND 64),

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT scim_group_name_is_unique_per_org UNIQUE (org_id, display_name),
    CONSTRAINT scim_group_identity_is_org_scoped UNIQUE (org_id, group_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS scim_group_external_id_is_unique_per_org
    ON scim_group (org_id, external_id) WHERE external_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS scim_group_member
(
    group_id UUID NOT NULL,
    org_id   TEXT NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    user_id  UUID NOT NULL REFERENCES app_user (user_id) ON DELETE CASCADE,

    PRIMARY KEY (group_id, user_id),

    CONSTRAINT scim_group_member_group_is_in_the_same_org
        FOREIGN KEY (org_id, group_id)
            REFERENCES scim_group (org_id, group_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS scim_group_member_user_idx
    ON scim_group_member (org_id, user_id);

-- ---------------------------------------------------------------------------------------
-- The record
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS audit_event
(
    event_id           UUID        NOT NULL PRIMARY KEY,
    org_id             TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    -- 1 user, 2 service_account, 3 system.
    actor_kind         SMALLINT    NOT NULL CHECK (actor_kind IN (1, 2, 3)),
    actor_id           TEXT        NOT NULL DEFAULT '' CHECK (length(actor_id) <= 256),

    -- What the actor was called AT THE TIME OF WRITING, copied rather than joined. A user
    -- who is renamed or deleted must not silently rewrite what the record says they did.
    actor_display_name TEXT        NOT NULL DEFAULT '' CHECK (length(actor_display_name) <= 256),

    action             TEXT        NOT NULL CHECK (length(action) BETWEEN 1 AND 128),
    target_kind        TEXT        NOT NULL DEFAULT '' CHECK (length(target_kind) <= 64),
    target_id          TEXT        NOT NULL DEFAULT '' CHECK (length(target_id) <= 256),

    -- 1 allowed, 2 denied, 3 failed. A denial is recorded as loudly as a success.
    outcome            SMALLINT    NOT NULL CHECK (outcome IN (1, 2, 3)),

    source_address     TEXT        NOT NULL DEFAULT '' CHECK (length(source_address) <= 128),
    request_id         TEXT        NOT NULL DEFAULT '' CHECK (length(request_id) <= 128),

    -- Structured context. NEVER a credential — the application drops any key whose name
    -- says otherwise before this is written, mechanically rather than by convention.
    detail             JSONB       NOT NULL DEFAULT '{}'::jsonb,

    occurred_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS audit_event_org_idx
    ON audit_event (org_id, occurred_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS audit_event_actor_idx
    ON audit_event (org_id, actor_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS audit_event_target_idx
    ON audit_event (org_id, target_kind, target_id, occurred_at DESC);

-- Append-only, enforced HERE rather than by convention. An UPDATE is refused outright; a
-- DELETE is refused unless the transaction has declared itself the retention pruner via a
-- session variable. Statement-level, so an UPDATE matching zero rows is still refused.
CREATE OR REPLACE FUNCTION audit_event_is_append_only() RETURNS trigger AS
$$
BEGIN
    IF TG_OP = 'DELETE' AND
       coalesce(current_setting('opencluster.audit_retention', TRUE), '') = 'pruning' THEN
        RETURN NULL;
    END IF;
    RAISE EXCEPTION 'audit_event is append-only; % is refused', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_event_refuses_update ON audit_event;
CREATE TRIGGER audit_event_refuses_update
    BEFORE UPDATE
    ON audit_event
    FOR EACH STATEMENT
EXECUTE FUNCTION audit_event_is_append_only();

DROP TRIGGER IF EXISTS audit_event_refuses_delete ON audit_event;
CREATE TRIGGER audit_event_refuses_delete
    BEFORE DELETE
    ON audit_event
    FOR EACH STATEMENT
EXECUTE FUNCTION audit_event_is_append_only();

DROP TRIGGER IF EXISTS audit_event_refuses_truncate ON audit_event;
CREATE TRIGGER audit_event_refuses_truncate
    BEFORE TRUNCATE
    ON audit_event
    FOR EACH STATEMENT
EXECUTE FUNCTION audit_event_is_append_only();

COMMENT ON TABLE app_user IS
    'A person who may sign in. Placement-scoped; identity is the issuer and subject together.';
COMMENT ON TABLE organization_membership IS
    'Which tenant a person may see and as what. The role is on the membership, not the user.';
COMMENT ON TABLE identity_provider IS
    'A tenant''s configured identity provider. The client secret is sealed because it must be presented, not compared.';
COMMENT ON TABLE operator_session IS
    'A signed-in operator. Only the digest of the cookie value is held; sign-out deletes the row.';
COMMENT ON TABLE api_token IS
    'A scoped automation credential: one organization, one role, an expiry, a revocation state. Digest only.';
COMMENT ON TABLE audit_event IS
    'Append-only. The database refuses an UPDATE and a DELETE; retention pruning must declare itself in its transaction.';
