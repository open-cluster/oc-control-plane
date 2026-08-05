-- Signals grouped into the operational episode an investigation attaches to.
--
-- Twenty notifications about one failure are one incident, and until now they were twenty rows
-- with nothing above them. What this adds is the thing that says they are one — provisionally,
-- explainably, and revisably, because a grouping decision is a judgement rather than a fact about
-- the world.
--
-- THE GROUPING KEY IS THE SOURCE'S OWN AND NOTHING THIS PLATFORM INFERRED. That is the decision
-- the rest of this file follows from. Deriving an episode from a Signal's labels would mean
-- deciding that the thing one system calls one name and another calls another are the same
-- object, which is canonical resource identity — the largest unsolved question in this product,
-- with one line of design behind it. So the key is what the customer's own alerting already said
-- belonged together: Alertmanager's groupKey, which is computed from the group_by they wrote.
-- When two alerts land in one episode here it is because their own system decided they should.
--
-- A payload carrying no grouping identity produces one episode per alert. That is the
-- conservative failure and it is the right one: a wrong split leaves one redundant record, and a
-- wrong merge produces an investigation with an incoherent scope, which is the failure the truth
-- model treats most seriously.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS incident_episode
(
    episode_id       UUID        NOT NULL PRIMARY KEY,
    organization     TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    connection_id    UUID        NOT NULL,

    -- Inherited from the Connection the Signals arrived through, never declared, and persisted
    -- rather than joined — for the same reason signal.environment_id is. A Connection that later
    -- moves must not rewrite the scope of an episode already grouped under it.
    environment_id   UUID        NOT NULL,

    -- The source's own notion of what belongs together. Never anything this platform inferred.
    grouping_key     TEXT        NOT NULL CHECK (length(grouping_key) BETWEEN 1 AND 512),

    -- 1 the source grouped these, 2 the source provided no grouping identity so this alert is
    -- its own episode. Stored so that a surprising grouping can be EXPLAINED rather than argued
    -- about: an operator looking at two alerts in one incident can be told who decided that.
    grouping_basis   SMALLINT    NOT NULL CHECK (grouping_basis IN (1, 2)),

    -- What to call it. Taken from the first Signal that opened the episode, and untrusted for its
    -- whole life like every other string a customer's systems produced.
    title            TEXT        NOT NULL CHECK (length(title) <= 512),

    -- 1 open, 2 resolved. An episode is resolved when no Signal in it is still firing, which is
    -- RECOMPUTED from its Signals rather than counted up and down: a counter that drifts is a
    -- record that says a failure recovered when it did not.
    status           SMALLINT    NOT NULL CHECK (status IN (1, 2)),

    -- The source's clock at both ends, and this platform's for neither. An episode's window is
    -- what an investigation would be scoped to, so a delivery delay must not widen it.
    first_seen_at    TIMESTAMPTZ NOT NULL,
    last_seen_at     TIMESTAMPTZ NOT NULL,
    resolved_at      TIMESTAMPTZ,

    signal_count     INTEGER     NOT NULL DEFAULT 0 CHECK (signal_count >= 0),

    -- The one Investigation this episode is the subject of. One, not many: repeated notifications
    -- about one failure must not fragment into many cases, and a second one is refused naming the
    -- first rather than quietly opened.
    investigation_id UUID,

    -- Revising a grouping without rewriting history. A merge points the absorbed episode at the
    -- one that survives it and changes NOTHING else: both keep their identity, their Signals and
    -- their record, so correcting a mistake does not destroy the record of having made it.
    superseded_by    UUID REFERENCES incident_episode (episode_id),
    superseded_at    TIMESTAMPTZ,
    supersede_reason TEXT        NOT NULL DEFAULT '' CHECK (length(supersede_reason) <= 1024),

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT incident_episode_connection_is_in_the_same_organization
        FOREIGN KEY (organization, connection_id)
            REFERENCES connection (organization, connection_id),
    CONSTRAINT incident_episode_environment_is_in_the_same_organization
        FOREIGN KEY (organization, environment_id)
            REFERENCES environment (organization, environment_id),
    CONSTRAINT incident_episode_investigation_is_in_the_same_organization
        FOREIGN KEY (organization, investigation_id)
            REFERENCES investigation (organization, investigation_id),

    CONSTRAINT incident_episode_resolution_is_stamped
        CHECK ((status = 2) = (resolved_at IS NOT NULL)),
    CONSTRAINT incident_episode_ends_after_it_starts
        CHECK (last_seen_at >= first_seen_at),
    CONSTRAINT incident_episode_supersession_is_stamped
        CHECK ((superseded_by IS NULL) = (superseded_at IS NULL)),
    -- An episode superseding itself would be a cycle of one, and every read that follows the
    -- pointer would follow it forever.
    CONSTRAINT incident_episode_supersedes_something_else
        CHECK (superseded_by IS NULL OR superseded_by <> episode_id),

    -- The identity an Investigation attaches to, so a case cannot be opened for an episode twice
    -- by two callers racing. The database decides it rather than a read-then-write both could
    -- pass.
    CONSTRAINT incident_episode_has_one_investigation UNIQUE (investigation_id)
);

-- One OPEN episode per grouping key, and the restriction to open ones is the whole rule.
--
-- A resolved episode stops occupying its key, so the same failure next month opens a NEW episode
-- rather than reopening the resolved record of the last one. That is exactly the rule the signal
-- table already keeps for an alert's own episodes, and it is kept the same way: by the database,
-- so two concurrent deliveries cannot both decide to create one.
--
-- A superseded episode DOES still occupy its key. An operator who merged it said these Signals
-- belong with another incident; letting the key go free would mean the next Signal matching it
-- opened a third episode and the operator's decision quietly stopped applying.
CREATE UNIQUE INDEX IF NOT EXISTS incident_episode_open_key_idx
    ON incident_episode (connection_id, grouping_key)
    WHERE status = 1;

CREATE INDEX IF NOT EXISTS incident_episode_organization_idx
    ON incident_episode (organization, last_seen_at DESC, episode_id DESC);

CREATE INDEX IF NOT EXISTS incident_episode_environment_idx
    ON incident_episode (organization, environment_id, last_seen_at DESC);

-- Which episode a Signal was grouped into.
--
-- Nullable, and it stays nullable rather than being backfilled. Signals recorded before this
-- migration were never grouped and there is no honest way to group them now: the grouping
-- identity is one the SOURCE supplied with the delivery, and those deliveries are gone. Inventing
-- one from their labels would be exactly the inference this whole design refuses.
ALTER TABLE signal
    ADD COLUMN IF NOT EXISTS episode_id UUID REFERENCES incident_episode (episode_id);

CREATE INDEX IF NOT EXISTS signal_episode_idx
    ON signal (episode_id, started_at DESC);

COMMENT ON TABLE incident_episode IS
    'One operational episode, grouped from Signals by the identity their own source supplied. Provisional grouping, not causal truth: revisable by a merge that rewrites nothing.';
COMMENT ON COLUMN incident_episode.grouping_key IS
    'The source''s own grouping identity. Never anything this platform inferred from a Signal''s labels.';
COMMENT ON COLUMN incident_episode.superseded_by IS
    'The episode an operator merged this one into. Both records survive; nothing is rewritten.';
