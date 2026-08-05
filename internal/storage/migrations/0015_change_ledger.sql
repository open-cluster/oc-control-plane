-- The change ledger: workload revisions and configuration changes, persisted continuously.
--
-- "What changed?" is the most productive question in incident investigation and at 03:40 it is
-- frequently unanswerable: events expire on a default one-hour TTL, revision history is bounded,
-- and a ConfigMap edit leaves no history at all. This is the ONE class of context this product
-- persists continuously, because it decays at the source; containment, placement and current
-- state are all recoverable from a live read at any moment and are deliberately not here.
--
-- What is recorded is DECLARED INTENT AND IDENTITY, never observed state. That an image moved at
-- 14:02 is a change; that three replicas are ready is state, and state has no column here. This
-- is the line that keeps an investigation product from becoming a monitoring platform, and the
-- schema is the first place it is enforced.
--
-- Rows arrive as at-least-once deltas from a Relay. The dedup key is the Connection, the object
-- identity INCLUDING UID, and the observed revision: a redelivery collapses instead of
-- duplicating history, and a workload deleted and recreated under one name is two objects, never
-- one object that mutated. A deletion carries an empty observed revision, which is naturally
-- unique because a UID is deleted at most once.
--
-- The ledger is a navigation index and never evidence: nothing in the truth chain accepts one of
-- these rows, and any conclusion resting on a change revalidates live.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS change_ledger_scope
(
    connection_id       UUID        NOT NULL PRIMARY KEY,
    organization        TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    -- Inherited from the Connection when the scope is opened, persisted rather than joined, so a
    -- Connection that later moves does not rewrite the scope of history already recorded.
    environment_id      UUID        NOT NULL,

    policy_revision     BIGINT      NOT NULL DEFAULT 1,
    requested_interval_seconds INTEGER NOT NULL CHECK (requested_interval_seconds > 0),

    -- Where the ledger's CONTINUOUS knowledge of this scope begins. A re-baseline that found
    -- nothing changed preserves it; one that found anything changed moves it forward, because
    -- the interval nobody was watching can no longer be vouched for. A brief window opening
    -- before this boundary is answered with a coverage gap, never with silence.
    covered_since       TIMESTAMPTZ,
    baseline_at         TIMESTAMPTZ,

    -- The last instant this scope was confirmed current: a recorded delta, a baseline, or a
    -- heartbeat stamp reporting a completed tick. Stored so the brief can state when the ledger
    -- was last confirmed rather than implying it is current.
    last_confirmed_at   TIMESTAMPTZ,
    faulted             BOOLEAN     NOT NULL DEFAULT FALSE,
    truncated           BOOLEAN     NOT NULL DEFAULT FALSE,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT change_ledger_scope_connection_is_in_the_organization
        FOREIGN KEY (organization, connection_id)
            REFERENCES connection (organization, connection_id),
    CONSTRAINT change_ledger_scope_environment_is_in_the_organization
        FOREIGN KEY (organization, environment_id)
            REFERENCES environment (organization, environment_id)
);

CREATE TABLE IF NOT EXISTS change_ledger
(
    entry_id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization        TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    connection_id       UUID        NOT NULL,
    environment_id      UUID        NOT NULL,

    namespace           TEXT        NOT NULL CHECK (length(namespace) BETWEEN 1 AND 63),
    -- 1 deployment, 2 statefulset, 3 daemonset, 4 configmap, 5 secret.
    object_kind         SMALLINT    NOT NULL CHECK (object_kind IN (1, 2, 3, 4, 5)),
    object_name         TEXT        NOT NULL CHECK (length(object_name) BETWEEN 1 AND 253),
    object_uid          TEXT        NOT NULL CHECK (length(object_uid) BETWEEN 1 AND 128),

    -- The declared-intent revision this observation saw. Empty exactly for a deletion.
    observed_revision   TEXT        NOT NULL CHECK (length(observed_revision) <= 128),

    -- 1 baseline, 2 created, 3 modified, 4 deleted. Baselines record where watching began and
    -- are excluded from every change query: installing a Relay is not everything changing at
    -- once, and a restart is not a second creation of the world.
    change_kind         SMALLINT    NOT NULL CHECK (change_kind IN (1, 2, 3, 4)),
    CONSTRAINT change_ledger_deletion_has_no_revision
        CHECK ((change_kind = 4) = (observed_revision = '')),

    -- Two clocks, deliberately: when the Relay read the cluster, and when this row was written.
    -- A delayed delivery is distinguishable from a delayed change only if both survive.
    observed_at         TIMESTAMPTZ NOT NULL,
    received_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The itemized field changes, with before and after values: identifiers, image references,
    -- quantities, name lists, versions, hashes. Never content — a Secret's rotation is a name
    -- and a version here, and nothing else about it exists anywhere in this schema.
    fields              JSONB       NOT NULL DEFAULT '[]'::jsonb,

    CONSTRAINT change_ledger_entry_is_unique_per_observation
        UNIQUE (connection_id, object_uid, observed_revision),
    CONSTRAINT change_ledger_connection_is_in_the_organization
        FOREIGN KEY (organization, connection_id)
            REFERENCES connection (organization, connection_id),
    CONSTRAINT change_ledger_environment_is_in_the_organization
        FOREIGN KEY (organization, environment_id)
            REFERENCES environment (organization, environment_id)
);

-- The brief's one question: what changed around this resource, in this window. Namespace-wide
-- within one Connection, ordered by when the cluster was read.
CREATE INDEX IF NOT EXISTS change_ledger_window_idx
    ON change_ledger (connection_id, namespace, observed_at);

-- Retention pruning walks by age within a tenant.
CREATE INDEX IF NOT EXISTS change_ledger_retention_idx
    ON change_ledger (organization, received_at);

COMMENT ON TABLE change_ledger IS
    'Workload revisions and configuration changes, continuously persisted because they decay at the source. Declared intent and identity only; a navigation index, never evidence.';
COMMENT ON TABLE change_ledger_scope IS
    'One Connection''s synchronization state: coverage boundaries and freshness, so an empty window is answerable as "nothing changed" or "nobody was watching" — never silently.';
COMMENT ON COLUMN change_ledger.observed_revision IS
    'Dedup component. Workloads carry generation plus a watched-field digest; ConfigMaps and Secrets carry resourceVersion; a deletion is empty, unique because a UID dies once.';
COMMENT ON COLUMN change_ledger_scope.covered_since IS
    'Where continuous knowledge begins. Preserved by a re-baseline that collapsed entirely; moved forward by one that did not.';
