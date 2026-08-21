-- Connecting an integration through the provider's own installation flow, and the facts a
-- verification established.
--
-- Copying an installation id out of a browser's address bar is not an installation flow.
-- GitHub's own documentation warns that the id a browser carries back can be spoofed, so a
-- connect that trusts it proves nothing about who is connecting. This table is what makes
-- the return trip checkable: only the state travels through the browser, only its digest is
-- stored, and the organization the Integration is bound to comes from the row rather than
-- from the callback's query.
--
-- The table is SHARED by every Integration Type with a browser enrolment, not GitHub's:
-- integration_type_id says whose flow a row is, and the Go side is internal/integrations
-- (connect.go), which knows no provider. A second provider reuses this and adds no schema.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS integration_connect_flow
(
    flow_id             UUID        NOT NULL PRIMARY KEY,
    org_id              TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    integration_type_id SMALLINT    NOT NULL
        REFERENCES integration_type (integration_type_id),

    -- The authenticated principal that started the flow. The callback is refused unless the
    -- caller returning is the same one, so a link handed to somebody else connects nothing.
    principal           TEXT        NOT NULL CHECK (length(principal) BETWEEN 1 AND 256),

    -- SHA-256 of the state parameter. Digested because the value travels through the
    -- browser, and a disclosure of this table must not let anyone complete a flow somebody
    -- else started.
    state_digest        BYTEA       NOT NULL CHECK (length(state_digest) = 32),

    -- Where the browser goes once the integration is connected. Validated as a same-site
    -- absolute path before it is written.
    return_to           TEXT        NOT NULL DEFAULT '/' CHECK (length(return_to) <= 512),

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at          TIMESTAMPTZ NOT NULL,
    consumed_at         TIMESTAMPTZ,

    CONSTRAINT integration_connect_flow_state_is_unique UNIQUE (state_digest),
    CONSTRAINT integration_connect_flow_expires_after_it_started CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS integration_connect_flow_expiry_idx
    ON integration_connect_flow (expires_at);

COMMENT ON TABLE integration_connect_flow IS
    'One provider installation flow in progress. Digest only; single-use; the organization it names is the authority.';

-- Non-secret, provider-shaped facts the last verification established: GitHub records the
-- account it verified against, the account type, whether the installation selected
-- repositories or granted all of them, and how many it reached. Beside verify_grants rather
-- than inside it because grants decide what a tool may do and these only describe what was
-- found, and a general column rather than a GitHub table because the next provider needs the
-- same thing for the same reason. Nothing here is ever a key or an authorization.
ALTER TABLE integration
    ADD COLUMN IF NOT EXISTS verify_facts JSONB;

COMMENT ON COLUMN integration.verify_facts IS
    'Non-secret facts the probe established (account, selection, reach); for display and support, never authorization.';
