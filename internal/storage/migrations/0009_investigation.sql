-- Investigations and their provenance.
--
-- These tables persist OPERATIONAL PROVENANCE: what triggered an investigation, which
-- integrations the router selected and why, which tools ran over which window, what each
-- returned or failed with, and the findings with their sources. What is deliberately NOT
-- here is an AI chain of thought — no hypothesis, no evidence stance, no coverage
-- machinery. They were left out of the baseline on purpose and arrive here with the
-- provenance model that uses them.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- The composite key an org-scoped reference needs. The baseline gave episodes a plain
-- primary key because nothing referenced them across tables; the investigation trigger
-- does, and the tenant boundary must hold in the database, not in a WHERE clause.
ALTER TABLE incident_episode
    ADD CONSTRAINT incident_episode_identity_is_org_scoped UNIQUE (org_id, episode_id);

CREATE TABLE IF NOT EXISTS investigation
(
    investigation_id UUID        NOT NULL PRIMARY KEY,
    org_id           TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    -- The trigger. An episode-triggered investigation names its episode and the
    -- integration the alert arrived through; a question records the operator's own words.
    -- The link runs THIS way only: an episode does not know its investigations, a listing
    -- finds them by this column.
    episode_id       UUID,
    integration_id   UUID,
    question         TEXT        NOT NULL DEFAULT '' CHECK (length(question) <= 1024),

    subject          TEXT        NOT NULL CHECK (length(subject) BETWEEN 1 AND 512),
    window_from      TIMESTAMPTZ NOT NULL,
    window_until     TIMESTAMPTZ NOT NULL,

    -- 1 running, 2 concluded, 3 failed.
    status           SMALLINT    NOT NULL DEFAULT 1 CHECK (status IN (1, 2, 3)),

    -- The conclusion: [{statement, sources:[run ordinal]}]. Every finding cites at least
    -- one run — enforced where the answer is decoded, because JSONB cannot say it.
    findings         JSONB       NOT NULL DEFAULT '[]'::jsonb,
    error            TEXT        NOT NULL DEFAULT '' CHECK (length(error) <= 1024),

    -- What the reasoning cost. Integer micro-cents, so two identical runs report the
    -- same number.
    spend_input_tokens  BIGINT   NOT NULL DEFAULT 0 CHECK (spend_input_tokens >= 0),
    spend_output_tokens BIGINT   NOT NULL DEFAULT 0 CHECK (spend_output_tokens >= 0),
    spend_micro_cents   BIGINT   NOT NULL DEFAULT 0 CHECK (spend_micro_cents >= 0),

    created_by       TEXT        NOT NULL DEFAULT '' CHECK (length(created_by) <= 256),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    concluded_at     TIMESTAMPTZ,

    CONSTRAINT investigation_identity_is_org_scoped UNIQUE (org_id, investigation_id),
    CONSTRAINT investigation_episode_is_in_the_same_org
        FOREIGN KEY (org_id, episode_id)
            REFERENCES incident_episode (org_id, episode_id),
    CONSTRAINT investigation_integration_is_in_the_same_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id),
    CONSTRAINT investigation_window_ends_after_it_starts CHECK (window_until >= window_from),
    CONSTRAINT investigation_conclusion_is_stamped CHECK ((status = 1) = (concluded_at IS NULL)),
    CONSTRAINT investigation_failure_states_a_reason CHECK ((status = 3) = (error <> ''))
);

CREATE INDEX IF NOT EXISTS investigation_org_idx
    ON investigation (org_id, created_at DESC, investigation_id DESC);
CREATE INDEX IF NOT EXISTS investigation_episode_idx
    ON investigation (episode_id) WHERE episode_id IS NOT NULL;

-- The router's selection: which integrations this investigation read, in rank order,
-- each with the reason it was chosen — including the reason an expansion pulled it in.
CREATE TABLE IF NOT EXISTS investigation_source
(
    investigation_id UUID        NOT NULL,
    org_id           TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    integration_id   UUID        NOT NULL,
    rank             SMALLINT    NOT NULL CHECK (rank >= 1),
    reason           TEXT        NOT NULL CHECK (length(reason) BETWEEN 1 AND 512),
    selected_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (investigation_id, integration_id),
    CONSTRAINT investigation_source_belongs_to_its_investigation
        FOREIGN KEY (org_id, investigation_id)
            REFERENCES investigation (org_id, investigation_id) ON DELETE CASCADE,
    CONSTRAINT investigation_source_names_an_org_integration
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id)
);

-- Every tool execution, succeeded or failed alike: a read that failed is provenance too,
-- and often the provenance that matters.
CREATE TABLE IF NOT EXISTS investigation_tool_run
(
    run_id           UUID        NOT NULL PRIMARY KEY,
    investigation_id UUID        NOT NULL,
    org_id           TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    -- Absent when the proposed tool resolved to no integration at all.
    integration_id   UUID,

    -- The run's one-based position in its investigation: what a finding cites.
    ordinal          SMALLINT    NOT NULL CHECK (ordinal >= 1),

    capability       TEXT        NOT NULL DEFAULT '' CHECK (length(capability) <= 128),
    tool             TEXT        NOT NULL CHECK (length(tool) BETWEEN 1 AND 128),

    -- The call's scope as it ran. NOTHING SECRET IS EVER STORED HERE: arguments come
    -- from the reasoner and credentials never travel in them.
    arguments        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    window_from      TIMESTAMPTZ NOT NULL,
    window_until     TIMESTAMPTZ NOT NULL,

    -- 1 succeeded, 2 failed.
    outcome          SMALLINT    NOT NULL CHECK (outcome IN (1, 2)),
    truncated        BOOLEAN     NOT NULL DEFAULT false,

    -- The provider's own one-line account of what came back, and the identifiers of what
    -- was read — channel ids, repository ids — so a finding can be followed to its origin.
    summary          TEXT        NOT NULL DEFAULT '' CHECK (length(summary) <= 512),
    sources          JSONB       NOT NULL DEFAULT '[]'::jsonb,
    error            TEXT        NOT NULL DEFAULT '' CHECK (length(error) <= 1024),

    started_at       TIMESTAMPTZ NOT NULL,
    finished_at      TIMESTAMPTZ NOT NULL,

    CONSTRAINT investigation_tool_run_belongs_to_its_investigation
        FOREIGN KEY (org_id, investigation_id)
            REFERENCES investigation (org_id, investigation_id) ON DELETE CASCADE,
    CONSTRAINT investigation_tool_run_names_an_org_integration
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id),
    CONSTRAINT investigation_tool_run_ordinal_is_unique
        UNIQUE (investigation_id, ordinal),
    CONSTRAINT investigation_tool_run_failure_states_a_reason CHECK (
        (outcome = 2) = (error <> '')
        )
);

CREATE INDEX IF NOT EXISTS investigation_tool_run_idx
    ON investigation_tool_run (investigation_id, ordinal);

COMMENT ON TABLE investigation IS
    'One investigation: trigger, subject, window, lifecycle, findings, spend. Provenance lives beside it; no chain of thought is stored.';
COMMENT ON TABLE investigation_source IS
    'The context router''s selection, in rank order, each with the reason it was chosen.';
COMMENT ON TABLE investigation_tool_run IS
    'Every tool execution an investigation performed, succeeded or failed alike, with its scope and what came back.';
