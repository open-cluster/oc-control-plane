-- Normalised alerts, and the operational episode they group into.
--
-- A Signal is one alert as a source reported it, keyed on the source's own identity for
-- the alert. An IncidentEpisode is what the source's own grouping said belongs together:
-- THE GROUPING KEY IS THE SOURCE'S OWN AND NOTHING THIS PLATFORM INFERRED. Deriving an
-- episode from a Signal's labels would mean deciding canonical resource identity, which
-- is deliberately unsolved here.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS signal
(
    signal_id      UUID        NOT NULL PRIMARY KEY,
    org_id         TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    integration_id UUID        NOT NULL,

    -- The source's own notion of which alert this is. Deduplication uses it because every
    -- alerting system already has one. It identifies the ALERT, not this occurrence of
    -- it: Alertmanager's fingerprint is a hash of the label set, so the same disk filling
    -- up next week carries the same one.
    source_key     TEXT        NOT NULL CHECK (length(source_key) BETWEEN 1 AND 512),

    -- 1 firing, 2 resolved.
    status         SMALLINT    NOT NULL CHECK (status IN (1, 2)),

    title          TEXT        NOT NULL CHECK (length(title) <= 512),
    -- Free text from the customer's systems. Untrusted for its whole life: it may be
    -- attacker-influenced and must never become an instruction, a destination, or an
    -- authorisation claim downstream.
    summary        TEXT        NOT NULL CHECK (length(summary) <= 4096),
    -- Normalised labels. The keys are the source's; no vocabulary is imposed here.
    labels         JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- When the source says this began and ended, and when we first heard of it. Collapsing
    -- the source's clock into ours would make a delayed delivery indistinguishable from a
    -- delayed failure.
    started_at     TIMESTAMPTZ NOT NULL,
    resolved_at    TIMESTAMPTZ,
    received_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT signal_integration_is_in_the_same_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id),

    CONSTRAINT signal_resolution_is_stamped CHECK ((status = 2) = (resolved_at IS NOT NULL)),
    CONSTRAINT signal_resolution_follows_its_start CHECK (
        resolved_at IS NULL OR resolved_at >= started_at
        ),

    -- One row per EPISODE of an alert, not per delivery. The start time is part of the
    -- identity because the source key is not: keyed on the source key alone, a re-fire
    -- would overwrite the resolved record of the previous occurrence.
    CONSTRAINT signal_episode_is_unique UNIQUE (integration_id, source_key, started_at)
);

CREATE INDEX IF NOT EXISTS signal_source_key_idx
    ON signal (integration_id, source_key, started_at DESC);
CREATE INDEX IF NOT EXISTS signal_org_idx
    ON signal (org_id, received_at DESC);

CREATE TABLE IF NOT EXISTS incident_episode
(
    episode_id       UUID        NOT NULL PRIMARY KEY,
    org_id           TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    integration_id   UUID        NOT NULL,

    -- The source's own notion of what belongs together. Never anything this platform
    -- inferred.
    grouping_key     TEXT        NOT NULL CHECK (length(grouping_key) BETWEEN 1 AND 512),

    -- 1 the source grouped these, 2 the source provided no grouping identity so this
    -- alert is its own episode. Stored so a surprising grouping can be EXPLAINED.
    grouping_basis   SMALLINT    NOT NULL CHECK (grouping_basis IN (1, 2)),

    -- What to call it. Taken from the first Signal that opened the episode, and untrusted
    -- for its whole life like every other string a customer's systems produced.
    title            TEXT        NOT NULL CHECK (length(title) <= 512),

    -- 1 open, 2 resolved. Resolved is RECOMPUTED from its Signals rather than counted up
    -- and down: a counter that drifts is a record that says a failure recovered when it
    -- did not.
    status           SMALLINT    NOT NULL CHECK (status IN (1, 2)),

    -- The source's clock at both ends, and this platform's for neither. An episode's
    -- window is what an investigation would be scoped to, so a delivery delay must not
    -- widen it.
    first_seen_at    TIMESTAMPTZ NOT NULL,
    last_seen_at     TIMESTAMPTZ NOT NULL,
    resolved_at      TIMESTAMPTZ,

    signal_count     INTEGER     NOT NULL DEFAULT 0 CHECK (signal_count >= 0),

    -- Revising a grouping without rewriting history. A merge points the absorbed episode
    -- at the one that survives it and changes NOTHING else: both keep their identity,
    -- their Signals and their record.
    superseded_by    UUID REFERENCES incident_episode (episode_id),
    superseded_at    TIMESTAMPTZ,
    supersede_reason TEXT        NOT NULL DEFAULT '' CHECK (length(supersede_reason) <= 1024),

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT incident_episode_integration_is_in_the_same_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id),

    CONSTRAINT incident_episode_resolution_is_stamped
        CHECK ((status = 2) = (resolved_at IS NOT NULL)),
    CONSTRAINT incident_episode_ends_after_it_starts
        CHECK (last_seen_at >= first_seen_at),
    CONSTRAINT incident_episode_supersession_is_stamped
        CHECK ((superseded_by IS NULL) = (superseded_at IS NULL)),
    CONSTRAINT incident_episode_supersedes_something_else
        CHECK (superseded_by IS NULL OR superseded_by <> episode_id)
);

-- One OPEN episode per grouping key, and the restriction to open ones is the whole rule.
-- A resolved episode stops occupying its key, so the same failure next month opens a NEW
-- episode. A superseded episode DOES still occupy its key: an operator who merged it said
-- these Signals belong with another incident, and the decision must keep applying.
CREATE UNIQUE INDEX IF NOT EXISTS incident_episode_open_key_idx
    ON incident_episode (integration_id, grouping_key)
    WHERE status = 1;

CREATE INDEX IF NOT EXISTS incident_episode_org_idx
    ON incident_episode (org_id, last_seen_at DESC, episode_id DESC);

-- Which episode a Signal was grouped into.
ALTER TABLE signal
    ADD COLUMN IF NOT EXISTS episode_id UUID REFERENCES incident_episode (episode_id);

CREATE INDEX IF NOT EXISTS signal_episode_idx
    ON signal (episode_id, started_at DESC);

COMMENT ON TABLE signal IS
    'Normalised alerts. Nothing downstream can tell which system delivered one.';
COMMENT ON TABLE incident_episode IS
    'One operational episode, grouped from Signals by the identity their own source supplied. Provisional grouping, not causal truth: revisable by a merge that rewrites nothing.';
COMMENT ON COLUMN incident_episode.superseded_by IS
    'The episode an operator merged this one into. Both records survive; nothing is rewritten.';
