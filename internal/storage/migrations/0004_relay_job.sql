-- Durable job truth. The session stream is a delivery channel; this table is what decides
-- whether a job exists, who may complete it, and whether it has already been completed.
--
-- The guarantee is narrow and absolute: a job is never lost and never silently completed
-- twice. Everything below follows from that and none of it can be relaxed without losing it.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS relay_job
(
    job_id              UUID        NOT NULL PRIMARY KEY,

    org_id              TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    -- The relay identity this job is for. A job is dispatched to one registration, and a
    -- result arriving under any other is refused rather than recorded.
    registration_id     UUID        NOT NULL,

    -- What this job reaches. The Relay is where it runs; this is what it runs against.
    integration_id      UUID        NOT NULL,

    capability_id       TEXT        NOT NULL,
    capability_version  INTEGER     NOT NULL,

    -- The typed capability argument, stored as it will be sent. Opaque here: what it means
    -- is the capability's business, and this table must not need to understand it to
    -- guarantee delivery.
    arguments           BYTEA       NOT NULL,

    -- 0 pending, 1 leased, 2 succeeded, 3 failed, 4 cancelled. The three terminal values
    -- are what the recording guard tests, so a job already recorded cannot be recorded
    -- again.
    status              SMALLINT    NOT NULL DEFAULT 0 CHECK (status BETWEEN 0 AND 4),

    -- The fence. A result is accepted only when it echoes both the session that holds the
    -- lease and the generation of that lease. Session churn cannot corrupt job truth.
    lease_session       UUID,
    lease_epoch         BIGINT      NOT NULL DEFAULT 0,

    -- Server clock, always. A relay with a skewed clock must not be able to extend or
    -- shorten its own lease.
    lease_expires_at    TIMESTAMPTZ,

    -- When a stop was asked for. Advisory: the terminal outcome still arrives from the
    -- relay.
    cancel_requested_at TIMESTAMPTZ,

    result              BYTEA,
    terminal_at         TIMESTAMPTZ,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT relay_job_lease_is_whole CHECK (
        (lease_session IS NULL AND lease_expires_at IS NULL)
            OR (lease_session IS NOT NULL AND lease_expires_at IS NOT NULL)
        ),

    CONSTRAINT relay_job_terminal_is_stamped CHECK (
        (status IN (2, 3, 4)) = (terminal_at IS NOT NULL)
        ),

    -- A relay and an integration belonging to the SAME organization as the job, enforced
    -- by the database rather than by a check that has to be remembered at every call site.
    CONSTRAINT relay_job_relay_is_in_the_same_org
        FOREIGN KEY (org_id, registration_id)
            REFERENCES relay_registration (org_id, registration_id),
    CONSTRAINT relay_job_integration_is_in_the_same_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id)
);

-- The claim path: pending work for one relay, and leases that have expired and must return
-- to pending.
CREATE INDEX IF NOT EXISTS relay_job_claimable_idx
    ON relay_job (org_id, registration_id, status, lease_expires_at)
    WHERE status IN (0, 1);

COMMENT ON TABLE relay_job IS
    'Durable job truth. Leases are server-clock and fenced by (lease_session, lease_epoch).';
COMMENT ON COLUMN relay_job.lease_epoch IS
    'Generation of the current lease. A result echoing an older generation is refused, never recorded.';
