-- The workspace installation an inbound Slack event resolves through.
--
-- This table exists for ROUTING and for nothing else. What an operator READS about a Slack
-- integration — which workspace, which bot — is on integration.verify_facts, recorded by the
-- verification; no display path reads a row here. The two are separate on purpose: facts are
-- what the last verification established and may go stale, and this is the key that decides
-- which tenant an event belongs to, which may not.
--
-- WHY THE UNIQUENESS IS NOT SCOPED TO AN ORGANIZATION. Inbound resolution runs
--
--     installation identity -> integration -> organization
--
-- and the whole value of that chain is that the first hop is unambiguous. A per-organization
-- uniqueness would let two tenants each claim one Slack workspace, which is harmless while
-- Slack is outbound-only — each reads that workspace with its own token and sees only what
-- its own token can see — and becomes a cross-tenant answer the instant an event arrives.
-- The constraint therefore lands BEFORE the events endpoint accepts anything, rather than
-- beside it: a constraint added after an endpoint exists can already have rows it must
-- reject, and the row it would have to reject is one a customer created in good faith.
--
-- The key is the app together with the enterprise and the team. Not the team alone: one
-- deployment may serve more than one app registration over its life, and a team id alone
-- would collide across them.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS slack_installation
(
    integration_id        UUID        NOT NULL PRIMARY KEY,
    org_id                TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    -- The Slack app this workspace installed.
    app_id                TEXT        NOT NULL CHECK (length(app_id) BETWEEN 1 AND 64),

    -- EMPTY STRING RATHER THAN NULL, and that is what makes the uniqueness hold: in SQL two
    -- NULLs are not equal, so a nullable enterprise column would let the same workspace be
    -- installed twice under two rows that both look distinct to the index.
    enterprise_id         TEXT        NOT NULL DEFAULT '' CHECK (length(enterprise_id) <= 64),
    team_id               TEXT        NOT NULL CHECK (length(team_id) BETWEEN 1 AND 64),
    is_enterprise_install BOOLEAN     NOT NULL DEFAULT FALSE,

    -- The identity OpenCluster answers as. Load-bearing rather than informational: a message
    -- authored by this user is discarded before anything else looks at it, which is what stops
    -- the agent answering itself and looping until a rate limit ends it.
    bot_user_id           TEXT        NOT NULL CHECK (length(bot_user_id) BETWEEN 1 AND 64),

    -- Who authorized the installation, in Slack's own identifiers. Recorded for the audit
    -- trail; nothing reads it to make a decision.
    authed_user_id        TEXT        NOT NULL DEFAULT '' CHECK (length(authed_user_id) <= 64),

    -- What the workspace granted, verbatim. The authoritative copy for tool availability is
    -- integration.verify_grants; this is what the INSTALLATION carried at the moment it was
    -- made, which is what an operator needs when the two disagree.
    scopes                TEXT[]      NOT NULL DEFAULT '{}',

    -- Token rotation is NOT enabled in this release. Slack's rotation is opt-in per app, and
    -- enabling it changes token lifetime, adds a refresh path and adds a failure mode to every
    -- read; a half-working refresh is worse than none. These columns exist and stay NULL so
    -- that enabling it later is expand-only rather than a schema change made under pressure.
    token_expires_at      TIMESTAMPTZ,
    refresh_token_sealed  BYTEA,

    installed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The installation belongs to the integration and to its tenant, checked together, so a
    -- row can never name an integration in another organization. CASCADE because this is part
    -- of the integration rather than a dependent of it: disconnecting Slack must not leave a
    -- routing key pointing at a row that is gone.
    CONSTRAINT slack_installation_integration_is_in_the_same_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id) ON DELETE CASCADE
);

-- ONE WORKSPACE RESOLVES TO ONE INTEGRATION, ACROSS THE WHOLE DEPLOYMENT.
--
-- Deliberately not scoped to org_id. See the note above: this is the constraint that makes
-- inbound resolution single-valued, and scoping it per tenant would remove exactly the
-- property it exists for.
CREATE UNIQUE INDEX IF NOT EXISTS slack_installation_is_one_workspace
    ON slack_installation (app_id, enterprise_id, team_id);
