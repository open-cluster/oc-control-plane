-- The change ledger: workload revisions and configuration changes, persisted continuously.
--
-- "What changed?" is the most productive question in incident investigation and at 03:40
-- it is frequently unanswerable: events expire on a default one-hour TTL, revision history
-- is bounded, and a ConfigMap edit leaves no history at all. This is the ONE class of
-- context this product persists continuously, because it decays at the source.
--
-- What is recorded is DECLARED INTENT AND IDENTITY, never observed state. That an image
-- moved at 14:02 is a change; that three replicas are ready is state, and state has no
-- column here.
--
-- Rows arrive as at-least-once deltas from a Relay. The dedup key is the integration, the
-- object identity INCLUDING UID, and the observed revision: a redelivery collapses instead
-- of duplicating history. A deletion carries an empty observed revision, which is
-- naturally unique because a UID is deleted at most once.
--
-- The ledger is a navigation index and never evidence: a conclusion resting on a change
-- revalidates live.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS change_ledger_scope
(
    integration_id             UUID        NOT NULL PRIMARY KEY,
    org_id                     TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    policy_revision            BIGINT      NOT NULL DEFAULT 1,
    requested_interval_seconds INTEGER     NOT NULL CHECK (requested_interval_seconds > 0),

    -- Where the ledger's CONTINUOUS knowledge of this scope begins. A re-baseline that
    -- found nothing changed preserves it; one that found anything changed moves it
    -- forward, because the interval nobody was watching can no longer be vouched for.
    covered_since              TIMESTAMPTZ,
    baseline_at                TIMESTAMPTZ,

    -- The last instant this scope was confirmed current: a recorded delta, a baseline, or
    -- a heartbeat stamp reporting a completed tick.
    last_confirmed_at          TIMESTAMPTZ,
    faulted                    BOOLEAN     NOT NULL DEFAULT FALSE,
    truncated                  BOOLEAN     NOT NULL DEFAULT FALSE,

    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT change_ledger_scope_integration_is_in_the_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id)
);

CREATE TABLE IF NOT EXISTS change_ledger
(
    entry_id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id            TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    integration_id    UUID        NOT NULL,

    namespace         TEXT        NOT NULL CHECK (length(namespace) BETWEEN 1 AND 63),
    -- 1 deployment, 2 statefulset, 3 daemonset, 4 configmap, 5 secret.
    object_kind       SMALLINT    NOT NULL CHECK (object_kind IN (1, 2, 3, 4, 5)),
    object_name       TEXT        NOT NULL CHECK (length(object_name) BETWEEN 1 AND 253),
    object_uid        TEXT        NOT NULL CHECK (length(object_uid) BETWEEN 1 AND 128),

    -- The declared-intent revision this observation saw. Empty exactly for a deletion.
    observed_revision TEXT        NOT NULL CHECK (length(observed_revision) <= 128),

    -- 1 baseline, 2 created, 3 modified, 4 deleted. Baselines record where watching began
    -- and are excluded from every change query: installing a Relay is not everything
    -- changing at once.
    change_kind       SMALLINT    NOT NULL CHECK (change_kind IN (1, 2, 3, 4)),
    CONSTRAINT change_ledger_deletion_has_no_revision
        CHECK ((change_kind = 4) = (observed_revision = '')),

    -- Two clocks, deliberately: when the Relay read the cluster, and when this row was
    -- written.
    observed_at       TIMESTAMPTZ NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The itemized field changes, with before and after values: identifiers, image
    -- references, quantities, name lists, versions, hashes. Never content — a Secret's
    -- rotation is a name and a version here, and nothing else about it exists anywhere in
    -- this schema.
    fields            JSONB       NOT NULL DEFAULT '[]'::jsonb,

    CONSTRAINT change_ledger_entry_is_unique_per_observation
        UNIQUE (integration_id, object_uid, observed_revision),
    CONSTRAINT change_ledger_integration_is_in_the_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id)
);

-- The one question: what changed around this resource, in this window. Namespace-wide
-- within one integration, ordered by when the cluster was read.
CREATE INDEX IF NOT EXISTS change_ledger_window_idx
    ON change_ledger (integration_id, namespace, observed_at);

-- Retention pruning walks by age within a tenant.
CREATE INDEX IF NOT EXISTS change_ledger_retention_idx
    ON change_ledger (org_id, received_at);

COMMENT ON TABLE change_ledger IS
    'Workload revisions and configuration changes, continuously persisted because they decay at the source. Declared intent and identity only; a navigation index, never evidence.';
COMMENT ON TABLE change_ledger_scope IS
    'One integration''s synchronization state: coverage boundaries and freshness, so an empty window is answerable as "nothing changed" or "nobody was watching" — never silently.';
