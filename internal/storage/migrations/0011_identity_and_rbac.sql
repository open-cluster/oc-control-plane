-- Identity, sessions, roles and the record.
--
-- Until this migration the operator surface had one shared static token, was cross-tenant by
-- design, and could say where a claim came from and never who made it. Everything here exists
-- to answer four questions a security reviewer asks and the product could not: who is this,
-- which tenants may they see, what may they do, and what did they do.
--
-- Naming. The tables are singular, as every other table in this schema is. `app_user` rather
-- than `user` because `user` is reserved in Postgres and `SELECT user` silently means something
-- else. `operator_session` rather than `session` because a Relay session already exists in this
-- schema and is a completely different subject.
--
-- Placement. A user row is not Organization-scoped — a person may hold memberships in several
-- Organizations — but it IS placement-scoped, because a placement is a separate database. A
-- person whose two Organizations sit on two placements has a row in each, and the memberships
-- each row carries are the ones served from that placement. That follows from the placement
-- model rather than from a choice made here, and it is recorded so nobody reads a single row
-- count as a user count across the deployment.
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
    -- Both are recorded because just-in-time provisioning against a verified domain is only
    -- meaningful if the verification is the provider's claim rather than our assumption.
    email          TEXT        NOT NULL CHECK (length(email) BETWEEN 0 AND 320),
    email_verified BOOLEAN     NOT NULL DEFAULT FALSE,

    display_name   TEXT        NOT NULL DEFAULT '' CHECK (length(display_name) <= 256),

    -- A disabled user keeps their history and can sign in to nothing. Deleting the row would
    -- take the memberships and the sessions with it and leave the audit trail naming an
    -- identifier nothing resolves.
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

-- The role sits on the MEMBERSHIP rather than on the user, which is what leaves room for
-- Environment-scoped role assignment later without moving anything: a second scope column
-- narrows this row rather than requiring a second table. Environment-scoped assignment is
-- deliberately deferred and is recorded as such in the specification.
CREATE TABLE IF NOT EXISTS organization_membership
(
    membership_id UUID        NOT NULL PRIMARY KEY,
    organization  TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    user_id       UUID        NOT NULL REFERENCES app_user (user_id) ON DELETE CASCADE,

    -- One of the seven roles this build compiles. Stored as text so a row is readable during
    -- an incident; the application refuses a value it has no role for, and an unrecognised one
    -- grants nothing rather than defaulting either way.
    role          TEXT        NOT NULL CHECK (length(role) BETWEEN 1 AND 64),

    -- How this membership came to exist: 1 manual, 2 just-in-time at first sign-in, 3 SCIM.
    -- It matters at deprovisioning time — a directory-owned membership must not be editable by
    -- hand and then silently overwritten on the next sync.
    source        SMALLINT    NOT NULL CHECK (source IN (1, 2, 3)),

    granted_by    TEXT        NOT NULL DEFAULT '' CHECK (length(granted_by) <= 256),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One role per person per tenant. Two rows would make "what may they do" a question with
    -- two answers, and whichever query was written first would decide.
    CONSTRAINT organization_membership_is_one_per_tenant UNIQUE (organization, user_id)
);

CREATE INDEX IF NOT EXISTS organization_membership_user_idx
    ON organization_membership (user_id);
CREATE INDEX IF NOT EXISTS organization_membership_organization_idx
    ON organization_membership (organization, created_at DESC);

-- ---------------------------------------------------------------------------------------
-- The identity provider a tenant configures
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS identity_provider
(
    provider_id            UUID        NOT NULL PRIMARY KEY,
    organization           TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    -- Operator-chosen, so a tenant serving a merged population can tell two providers apart.
    name                   TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),

    -- 1 OIDC. SAML 2.0 is specified and not built in this slice; the column exists so adding
    -- it is a value rather than a migration that rewrites every row.
    protocol               SMALLINT    NOT NULL CHECK (protocol IN (1, 2)),

    issuer                 TEXT        NOT NULL CHECK (length(issuer) BETWEEN 1 AND 512),
    client_id              TEXT        NOT NULL CHECK (length(client_id) BETWEEN 1 AND 256),

    -- AES-256-GCM, nonce prepended, under a key this process reads from a file. Held encrypted
    -- rather than digested because it has to be PRESENTED to the token endpoint — unlike every
    -- other credential in this schema, which is only ever compared against.
    client_secret_sealed   BYTEA       NOT NULL,

    -- The email domains just-in-time provisioning may admit. Empty means none, so an
    -- unconfigured provider admits nobody rather than everybody.
    verified_domains       TEXT[]      NOT NULL DEFAULT ARRAY []::TEXT[],

    -- Whether a first-time signer-in is provisioned at all, and the role they land on.
    jit_enabled            BOOLEAN     NOT NULL DEFAULT FALSE,
    jit_role               TEXT        NOT NULL DEFAULT 'viewer' CHECK (length(jit_role) BETWEEN 1 AND 64),

    -- Whether the provider must have SAID it verified the address. Story 8 is only true with
    -- this: a domain check over an unverified claim admits anyone who can type the domain.
    require_verified_email BOOLEAN     NOT NULL DEFAULT TRUE,

    -- Which claim carries the provider's groups, and what each maps to. The map is
    -- {"group name": "role"}; a group naming no role this build has maps to nothing.
    group_claim            TEXT        NOT NULL DEFAULT 'groups' CHECK (length(group_claim) <= 128),
    group_role_map         JSONB       NOT NULL DEFAULT '{}'::jsonb,

    disabled_at            TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT identity_provider_name_is_unique_per_organization UNIQUE (organization, name),
    -- One tenant may configure several providers (story 7) and must not configure the same
    -- client twice at the same issuer, which would make "which one answered" undecidable.
    CONSTRAINT identity_provider_client_is_unique_per_issuer
        UNIQUE (organization, issuer, client_id)
);

CREATE INDEX IF NOT EXISTS identity_provider_organization_idx
    ON identity_provider (organization, created_at);

-- ---------------------------------------------------------------------------------------
-- The sign-in flow in progress
-- ---------------------------------------------------------------------------------------

-- One row per authorization request the control plane started. It is what makes PKCE, the
-- state parameter and single-use redemption enforceable rather than asserted: the verifier and
-- the nonce live here and never reach the browser, and redemption is a conditional UPDATE, so
-- two concurrent presentations of the same code cannot both win.
CREATE TABLE IF NOT EXISTS sign_in_flow
(
    flow_id      UUID        NOT NULL PRIMARY KEY,
    organization TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    provider_id  UUID        NOT NULL REFERENCES identity_provider (provider_id) ON DELETE CASCADE,

    -- SHA-256 of the state parameter. Digested for the same reason a session identifier is:
    -- the value travels through the browser and a disclosure of this table must not let anyone
    -- complete a flow somebody else started.
    state_digest BYTEA       NOT NULL CHECK (length(state_digest) = 32),

    -- The PKCE verifier, held in the clear. It is single-use, deleted at redemption, expires in
    -- minutes, and is worthless without the matching authorization code — which the attacker
    -- who had this table would still have to obtain from the browser. Sealing it would add a
    -- key-management dependency to a value with those properties.
    code_verifier TEXT       NOT NULL CHECK (length(code_verifier) BETWEEN 43 AND 128),

    -- Bound into the authorization request and checked against the ID token, so a token minted
    -- for a different request cannot be replayed into this one.
    nonce        TEXT        NOT NULL CHECK (length(nonce) BETWEEN 16 AND 128),

    -- Where the browser goes once signed in. Validated as a same-site absolute path before it
    -- is written, because a value that reached a Location header unvalidated is an open
    -- redirect with the product's own domain on it.
    return_to    TEXT        NOT NULL DEFAULT '/' CHECK (length(return_to) <= 512),

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,

    CONSTRAINT sign_in_flow_state_is_unique UNIQUE (state_digest),
    CONSTRAINT sign_in_flow_expires_after_it_started CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS sign_in_flow_expiry_idx ON sign_in_flow (expires_at);

-- ---------------------------------------------------------------------------------------
-- The session
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS operator_session
(
    session_id   UUID        NOT NULL PRIMARY KEY,

    -- SHA-256 of the opaque identifier in the cookie. Only the digest is held, so a disclosure
    -- of this table yields no usable credential.
    token_digest BYTEA       NOT NULL CHECK (length(token_digest) = 32),

    user_id      UUID        NOT NULL REFERENCES app_user (user_id) ON DELETE CASCADE,

    -- The tenant the console was reading as when this was issued. A convenience for the
    -- interface and never an authorization fact: what a session may reach is decided from the
    -- user's memberships on every request, so changing this can never widen access.
    organization TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    issued_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Set when an administrator ends somebody else's session. Signing oneself out DELETES the
    -- row instead, so the credential is gone rather than marked — story 4 asks for the session
    -- to be dead before the response is written, and a row that still exists is a row a bug
    -- can read.
    revoked_at   TIMESTAMPTZ,
    revoked_by   TEXT        NOT NULL DEFAULT '' CHECK (length(revoked_by) <= 256),

    -- What an administrator reads when deciding whether a live session is one they recognise.
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
    organization       TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    name               TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    description        TEXT        NOT NULL DEFAULT '' CHECK (length(description) <= 512),

    disabled_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by         TEXT        NOT NULL DEFAULT '' CHECK (length(created_by) <= 256),

    CONSTRAINT service_account_name_is_unique_per_organization UNIQUE (organization, name),
    CONSTRAINT service_account_identity_is_org_scoped UNIQUE (organization, service_account_id)
);

-- A token is bound to one Organization, one role and an expiry, so a leak has a blast radius
-- and a deadline. The role is on the TOKEN rather than on the account: two tokens for one
-- automation with different reach is the ordinary case, and putting the role on the account
-- would force a second account to express it.
CREATE TABLE IF NOT EXISTS api_token
(
    token_id           UUID        NOT NULL PRIMARY KEY,
    organization       TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    service_account_id UUID        NOT NULL,

    token_digest       BYTEA       NOT NULL CHECK (length(token_digest) = 32),

    -- The first characters of the token, so an operator can tell which of their tokens a row
    -- is without the system holding a readable copy of any of them.
    prefix             TEXT        NOT NULL CHECK (length(prefix) BETWEEN 1 AND 32),

    role               TEXT        NOT NULL CHECK (length(role) BETWEEN 1 AND 64),

    -- Never null. A token with no deadline is the ambient root credential this whole migration
    -- exists to replace.
    expires_at         TIMESTAMPTZ NOT NULL,

    -- Story 29: what an operator reads to decide which tokens nobody needs. Written on use,
    -- coarsely — see the storage layer for why it is not written on every request.
    last_used_at       TIMESTAMPTZ,

    revoked_at         TIMESTAMPTZ,
    revoked_by         TEXT        NOT NULL DEFAULT '' CHECK (length(revoked_by) <= 256),

    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by         TEXT        NOT NULL DEFAULT '' CHECK (length(created_by) <= 256),

    CONSTRAINT api_token_digest_is_unique UNIQUE (token_digest),
    CONSTRAINT api_token_expires_after_it_was_created CHECK (expires_at > created_at),

    -- The account must belong to the same organization the token names, enforced by the
    -- database rather than by a WHERE clause someone has to remember.
    CONSTRAINT api_token_account_is_in_the_same_organization
        FOREIGN KEY (organization, service_account_id)
            REFERENCES service_account (organization, service_account_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS api_token_account_idx
    ON api_token (organization, service_account_id, created_at DESC);

-- ---------------------------------------------------------------------------------------
-- The tenant's own policy
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS organization_policy
(
    organization             TEXT        NOT NULL PRIMARY KEY
        CHECK (length(organization) BETWEEN 1 AND 128),

    -- Story 11. Zero means the product's default; the application holds whatever is set inside
    -- the bounds this build serves, so a policy may be tightened and not widened past them.
    session_lifetime_seconds INTEGER     NOT NULL DEFAULT 0 CHECK (session_lifetime_seconds >= 0),

    -- Story 21's stated schedule. The pruner that acts on it is deliberately NOT built in this
    -- slice: stating a retention period the product does not enforce is worse than stating none,
    -- so this column is what a later pruner reads and the surface reports it as declared rather
    -- than applied.
    audit_retention_days     INTEGER     NOT NULL DEFAULT 0 CHECK (audit_retention_days >= 0),

    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by               TEXT        NOT NULL DEFAULT '' CHECK (length(updated_by) <= 256)
);

-- ---------------------------------------------------------------------------------------
-- The record
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS audit_event
(
    event_id           UUID        NOT NULL PRIMARY KEY,
    organization       TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    -- 1 user, 2 service_account, 3 system.
    actor_kind         SMALLINT    NOT NULL CHECK (actor_kind IN (1, 2, 3)),
    actor_id           TEXT        NOT NULL DEFAULT '' CHECK (length(actor_id) <= 256),

    -- What the actor was called AT THE TIME OF WRITING, copied rather than joined. A user who
    -- is renamed or deleted must not silently rewrite what the record says about what they did;
    -- a trail that changes when a row elsewhere changes is not a record.
    actor_display_name TEXT        NOT NULL DEFAULT '' CHECK (length(actor_display_name) <= 256),

    action             TEXT        NOT NULL CHECK (length(action) BETWEEN 1 AND 128),
    target_kind        TEXT        NOT NULL DEFAULT '' CHECK (length(target_kind) <= 64),
    target_id          TEXT        NOT NULL DEFAULT '' CHECK (length(target_id) <= 256),

    -- 1 allowed, 2 denied, 3 failed. A denial is recorded as loudly as a success: credential
    -- probing is only visible if the attempts that failed are on the record too.
    outcome            SMALLINT    NOT NULL CHECK (outcome IN (1, 2, 3)),

    source_address     TEXT        NOT NULL DEFAULT '' CHECK (length(source_address) <= 128),
    request_id         TEXT        NOT NULL DEFAULT '' CHECK (length(request_id) <= 128),

    -- Structured context: the previous and new value of a changed setting, the reason a request
    -- was refused. NEVER a credential and never evidence content — the application drops any key
    -- whose name says otherwise before this is written, mechanically rather than by convention.
    detail             JSONB       NOT NULL DEFAULT '{}'::jsonb,

    occurred_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Newest first, per tenant, which is the only way this is ever read.
CREATE INDEX IF NOT EXISTS audit_event_organization_idx
    ON audit_event (organization, occurred_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS audit_event_actor_idx
    ON audit_event (organization, actor_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS audit_event_target_idx
    ON audit_event (organization, target_kind, target_id, occurred_at DESC);

-- Append-only, enforced HERE rather than by convention.
--
-- An UPDATE is refused outright: there is no correct reason to change what the record says
-- happened. A DELETE is refused unless the transaction has declared itself the retention
-- pruner, which it does by setting a session variable — so the one path that may remove rows
-- has to say so, in the transaction, where it is visible. Ordinary application code cannot
-- reach it, and a pruner is deliberately not built in this slice.
--
-- The triggers are STATEMENT-level, so an UPDATE matching zero rows is still refused. A
-- row-level trigger would let `DELETE FROM audit_event WHERE false` succeed and read, to
-- anyone probing, as though deletion were permitted.
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
    'Which tenant a person may see and as what. The role is here, not on the user, which leaves room for Environment scoping.';
COMMENT ON TABLE identity_provider IS
    'A tenant''s configured identity provider. The client secret is sealed because it must be presented, not compared.';
COMMENT ON TABLE sign_in_flow IS
    'One authorization request in progress. Makes PKCE, state and single-use redemption enforceable rather than asserted.';
COMMENT ON TABLE operator_session IS
    'A signed-in operator. Only the digest of the cookie value is held; sign-out deletes the row.';
COMMENT ON TABLE api_token IS
    'A scoped automation credential: one organization, one role, an expiry, a revocation state. Digest only.';
COMMENT ON TABLE audit_event IS
    'Append-only. The database refuses an UPDATE and a DELETE; retention pruning must declare itself in its transaction.';
