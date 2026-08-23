-- The vendor-side installation an inbound event resolves through.
--
-- SHARED BY EVERY INTEGRATION TYPE THAT IS REACHED INBOUND, and not Slack's. The columns are
-- the neutral shape every chat and app installation has — an application, an optional
-- enterprise, a workspace, the identity this product answers AS, who authorized it, and what
-- was granted — because the alternative is a table per provider and therefore a dispatch from
-- an integration's type to a table, which this repository does not permit anywhere.
-- `integration_connect_flow` is shared for the same reason and by the same argument; a second
-- provider reuses this and adds no schema.
--
-- This table exists for ROUTING and for nothing else. What an operator READS about a connected
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
-- uniqueness would let two tenants each claim one workspace, which is harmless while a provider
-- is outbound-only — each reads that workspace with its own credential and sees only what its
-- own credential can see — and becomes a cross-tenant answer the instant an event arrives. The
-- constraint therefore lands BEFORE any events endpoint accepts anything, rather than beside
-- it: a constraint added after an endpoint exists can already have rows it must reject, and the
-- row it would have to reject is one a customer created in good faith.
--
-- The key is the type together with the application, the enterprise and the workspace. Not the
-- workspace alone: one deployment may serve more than one application registration over its
-- life, and a workspace identifier alone would collide across them.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS integration_installation
(
    integration_id      UUID        NOT NULL PRIMARY KEY,
    org_id              TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    integration_type_id SMALLINT    NOT NULL
        REFERENCES integration_type (integration_type_id),

    -- The vendor application this installation was made under.
    application         TEXT        NOT NULL CHECK (length(application) BETWEEN 1 AND 64),

    -- EMPTY STRING RATHER THAN NULL, and that is what makes the uniqueness hold: in SQL two
    -- NULLs are not equal, so a nullable enterprise column would let the same workspace be
    -- installed twice under two rows that both look distinct to the index.
    enterprise          TEXT        NOT NULL DEFAULT '' CHECK (length(enterprise) <= 64),
    workspace           TEXT        NOT NULL CHECK (length(workspace) BETWEEN 1 AND 64),

    -- Whether the installation is enterprise-WIDE, as the vendor reported it. Not derived from
    -- the enterprise column being set: a workspace-scoped install inside an enterprise grid
    -- carries an enterprise identity and is not enterprise-wide, and collapsing the two would
    -- mislabel exactly the case the enterprise columns exist to identify correctly.
    enterprise_wide     BOOLEAN     NOT NULL DEFAULT FALSE,

    -- The identity this product answers AS in that workspace. Load-bearing rather than
    -- informational: a message authored by this identity is discarded before anything else
    -- looks at it, which is what stops the agent answering itself and looping until a rate
    -- limit ends it.
    agent               TEXT        NOT NULL DEFAULT '' CHECK (length(agent) <= 64),

    -- Who authorized the installation, in the vendor's own identifiers. Recorded for the
    -- trail; nothing reads it to make a decision.
    authorizer          TEXT        NOT NULL DEFAULT '' CHECK (length(authorizer) <= 64),

    -- What the installation carried when it was made, verbatim. The authoritative copy for
    -- tool availability is integration.verify_grants; this is what an operator needs when the
    -- two disagree.
    grants              TEXT[]      NOT NULL DEFAULT '{}',

    -- Credential rotation is NOT enabled in this release. Slack's is opt-in per app, and
    -- enabling it changes credential lifetime, adds a refresh path and adds a failure mode to
    -- every read; a half-working refresh is worse than none. These columns exist and stay NULL
    -- so that enabling it later is expand-only rather than a schema change made under pressure.
    expires_at          TIMESTAMPTZ,
    refresh_sealed      BYTEA,

    installed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The installation belongs to the integration and to its tenant, checked together, so a row
    -- can never name an integration in another organization. CASCADE because this is part of
    -- the integration rather than a dependent of it: disconnecting must not leave a routing key
    -- pointing at a row that is gone.
    CONSTRAINT integration_installation_is_in_the_same_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id) ON DELETE CASCADE
);

-- ONE WORKSPACE RESOLVES TO ONE INTEGRATION, ACROSS THE WHOLE DEPLOYMENT.
--
-- Deliberately not scoped to org_id. See the note above: this is the constraint that makes
-- inbound resolution single-valued, and scoping it per tenant would remove exactly the property
-- it exists for.
CREATE UNIQUE INDEX IF NOT EXISTS integration_installation_is_one_workspace
    ON integration_installation (integration_type_id, application, enterprise, workspace);
