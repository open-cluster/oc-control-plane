-- SAML 2.0 as a second way in, and SCIM as a directory that owns who is in a tenant.
--
-- Migration 0011 left room for both deliberately: identity_provider.protocol already accepted
-- SAML, and organization_membership.source already accepted `scim`. This is the migration that
-- fills that room, and almost all of it is columns on tables that already exist.
--
-- The two new subjects are the ones that had nowhere to live. A directory's groups are records
-- the directory creates and this product does not — they arrive before anybody signs in and
-- they decide what role a person gets — so they are a table. And a membership a DIRECTORY owns
-- differs from one an administrator granted: it carries the directory's own identifier for the
-- person, and it can be deactivated without being removed, because a directory that sets
-- someone inactive still expects to read them back.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- ---------------------------------------------------------------------------------------
-- A provider carries what its protocol needs, and nothing it does not
-- ---------------------------------------------------------------------------------------

-- OIDC needs a client and a secret. SAML needs neither: the trust is a signing certificate the
-- identity provider publishes, and there is no back channel for this product to authenticate
-- on. Leaving the OIDC columns NOT NULL would have meant a SAML row carrying an invented client
-- identifier, which is a value somebody would eventually believe.
ALTER TABLE identity_provider
    ALTER COLUMN client_id DROP NOT NULL;
ALTER TABLE identity_provider
    ALTER COLUMN client_secret_sealed DROP NOT NULL;

-- The identity provider's own published metadata document, pasted whole.
--
-- It is stored as the DOCUMENT rather than as the entity identifier, the sign-on URL and the
-- certificates parsed out into columns, and that is deliberate. Those three are one fact the
-- provider already publishes together, they change together at a key rotation, and a rotation
-- is then a re-paste rather than three fields an administrator has to get consistent. It is
-- also what the SAML library is given, so nothing is re-derived from parts.
ALTER TABLE identity_provider
    ADD COLUMN saml_metadata TEXT;

ALTER TABLE identity_provider
    ADD CONSTRAINT identity_provider_carries_what_its_protocol_needs CHECK (
        (protocol = 1 AND client_id IS NOT NULL AND client_secret_sealed IS NOT NULL
            AND saml_metadata IS NULL)
            OR
        (protocol = 2 AND saml_metadata IS NOT NULL AND client_id IS NULL
            AND client_secret_sealed IS NULL)
        );

-- A SAML provider has no client identifier, so the OIDC uniqueness constraint —
-- (organization, issuer, client_id) — does not bind it: SQL treats two NULLs as distinct, and a
-- tenant could configure the same issuer twice with no way to tell which answered. One issuer
-- per tenant, for SAML, as a partial index.
CREATE UNIQUE INDEX IF NOT EXISTS identity_provider_saml_issuer_is_unique_per_organization
    ON identity_provider (organization, issuer) WHERE protocol = 2;

-- ---------------------------------------------------------------------------------------
-- A sign-in in progress, for a protocol with no PKCE
-- ---------------------------------------------------------------------------------------

-- PKCE and the nonce are OIDC's. SAML binds its response to its request through InResponseTo
-- instead, so both columns become optional and the request identifier gains a column of its
-- own. The single-use redemption is unchanged and is what makes a replayed response fail for
-- BOTH protocols — the same mechanism, which is the point of not having two.
ALTER TABLE sign_in_flow
    ALTER COLUMN code_verifier DROP NOT NULL;
ALTER TABLE sign_in_flow
    ALTER COLUMN nonce DROP NOT NULL;

-- The identifier of the AuthnRequest this flow sent. The response must name it in InResponseTo,
-- and the library is given exactly this one value to accept — not "any request we ever sent".
ALTER TABLE sign_in_flow
    ADD COLUMN request_id TEXT CHECK (request_id IS NULL OR length(request_id) BETWEEN 1 AND 256);

ALTER TABLE sign_in_flow
    ADD CONSTRAINT sign_in_flow_carries_what_its_protocol_needs CHECK (
        (code_verifier IS NOT NULL AND nonce IS NOT NULL AND request_id IS NULL)
            OR
        (request_id IS NOT NULL AND code_verifier IS NULL AND nonce IS NULL)
        );

-- ---------------------------------------------------------------------------------------
-- A membership a directory owns
-- ---------------------------------------------------------------------------------------

-- The directory's own identifier for this person in this tenant. It is on the MEMBERSHIP rather
-- than on the user because a directory provisions into one Organization: the same person
-- arriving from two customers' directories is two memberships and one user, and one external
-- identifier on the user row could not hold both.
ALTER TABLE organization_membership
    ADD COLUMN external_id TEXT CHECK (external_id IS NULL OR length(external_id) BETWEEN 1 AND 256);

-- Whether the DIRECTORY says this person is enabled.
--
-- A directory that sets somebody inactive expects to read them back — SCIM has no "gone", it
-- has `active: false` — so the row survives and stops counting. Every place that resolves what
-- a principal may reach filters on this, which is what makes deprovisioning take effect on the
-- next request rather than at the next sign-in.
--
-- It says nothing about what they may DO. That is the role below, and the two are deliberately
-- separate columns: a person the directory has just created is active and holds nothing until a
-- group somebody mapped puts them in a role.
ALTER TABLE organization_membership
    ADD COLUMN active BOOLEAN NOT NULL DEFAULT TRUE;

-- A membership may now hold NO role.
--
-- It is the state a directory-provisioned person is in before any group they are in has been
-- mapped, and it is the state they return to when the last mapped group is taken away. Before
-- this the two facts shared the `active` column, and a person the directory had just created
-- read back as inactive — which is the directory being told something untrue about its own
-- data.
--
-- A membership with no role grants nothing. Every read that resolves what a principal may
-- reach requires one.
ALTER TABLE organization_membership
    ALTER COLUMN role DROP NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS organization_membership_external_id_is_unique_per_organization
    ON organization_membership (organization, external_id) WHERE external_id IS NOT NULL;

-- ---------------------------------------------------------------------------------------
-- The directory's groups
-- ---------------------------------------------------------------------------------------

-- A group is a record the DIRECTORY creates and this product does not. It arrives before
-- anybody has signed in, it is addressed by an identifier the directory chose, and what it
-- means here is decided by an administrator: the role it grants.
--
-- That is the difference from identity_provider.group_role_map, which maps a group NAME that
-- arrived in a token to a role. A token's group claim is a string that exists only during a
-- sign-in; a SCIM group is a row with members, which is what lets access change when the
-- directory changes rather than when the person next signs in.
CREATE TABLE IF NOT EXISTS scim_group
(
    group_id     UUID        NOT NULL PRIMARY KEY,
    organization TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    display_name TEXT        NOT NULL CHECK (length(display_name) BETWEEN 1 AND 256),
    external_id  TEXT        CHECK (external_id IS NULL OR length(external_id) BETWEEN 1 AND 256),

    -- What this group grants. NULL grants nothing, which is the correct default: a directory
    -- synchronises every group it is told to, and most of them are not about this product. A
    -- group nobody has mapped must not silently admit its members.
    role         TEXT        CHECK (role IS NULL OR length(role) BETWEEN 1 AND 64),

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT scim_group_name_is_unique_per_organization UNIQUE (organization, display_name),
    -- The composite key the membership table below points at, so a group cannot collect
    -- another organization's people however the request was assembled.
    CONSTRAINT scim_group_identity_is_org_scoped UNIQUE (organization, group_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS scim_group_external_id_is_unique_per_organization
    ON scim_group (organization, external_id) WHERE external_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS scim_group_member
(
    group_id     UUID NOT NULL,
    organization TEXT NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    user_id      UUID NOT NULL REFERENCES app_user (user_id) ON DELETE CASCADE,

    PRIMARY KEY (group_id, user_id),

    CONSTRAINT scim_group_member_group_is_in_the_same_organization
        FOREIGN KEY (organization, group_id)
            REFERENCES scim_group (organization, group_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS scim_group_member_user_idx
    ON scim_group_member (organization, user_id);

COMMENT ON COLUMN identity_provider.saml_metadata IS
    'The identity provider''s published metadata, stored whole: entity id, sign-on URL and certificates change together.';
COMMENT ON COLUMN organization_membership.active IS
    'Whether this membership grants anything. A directory sets it false rather than deleting, and access ends on the next request.';
COMMENT ON TABLE scim_group IS
    'A directory''s group, and the role an administrator mapped it to. NULL grants nothing.';
