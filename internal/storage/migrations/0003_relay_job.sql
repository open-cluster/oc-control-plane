-- Durable job truth. The session stream is a delivery channel; this table is what decides
-- whether a job exists, who may complete it, and whether it has already been completed.
--
-- The guarantee is narrow and absolute: a job is never lost and never silently completed
-- twice. Everything below follows from that and none of it can be relaxed without losing it.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS relay_job
(
    job_id             UUID        NOT NULL PRIMARY KEY,

    organization       TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    -- The relay identity this job is for. A job is dispatched to one registration, and a
    -- result arriving under any other is refused rather than recorded.
    registration_id    UUID        NOT NULL,

    capability_id      TEXT        NOT NULL,
    capability_version INTEGER     NOT NULL,

    -- The typed capability argument, stored as it will be sent. Opaque here: what it means
    -- is the capability's business, and this table must not need to understand it to
    -- guarantee delivery.
    arguments          BYTEA       NOT NULL,

    -- 0 pending, 1 leased, 2 succeeded, 3 failed, 4 cancelled. The three terminal values
    -- are what the recording guard tests, so a job already recorded cannot be recorded again.
    status             SMALLINT    NOT NULL DEFAULT 0 CHECK (status BETWEEN 0 AND 4),

    -- The fence. A result is accepted only when it echoes both the session that holds the
    -- lease and the generation of that lease. Session churn cannot corrupt job truth,
    -- because ownership is decided here per job rather than by who is currently connected.
    lease_session      UUID,
    lease_epoch        BIGINT      NOT NULL DEFAULT 0,

    -- Server clock, always. A relay with a skewed clock must not be able to extend or
    -- shorten its own lease.
    lease_expires_at   TIMESTAMPTZ,

    result             BYTEA,
    terminal_at        TIMESTAMPTZ,

    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A lease without an owner, or an owner without an expiry, would leave a job that no
    -- sweep can recover and no result can be recorded against.
    CONSTRAINT relay_job_lease_is_whole CHECK (
        (lease_session IS NULL AND lease_expires_at IS NULL)
            OR (lease_session IS NOT NULL AND lease_expires_at IS NOT NULL)
        ),

    -- A terminal job carries the moment it became terminal; a live one does not.
    CONSTRAINT relay_job_terminal_is_stamped CHECK (
        (status IN (2, 3, 4)) = (terminal_at IS NOT NULL)
        )
);

-- The claim path: pending work for one relay, and leases that have expired and must return
-- to pending. Without this both are sequential scans over every organization's job history.
CREATE INDEX IF NOT EXISTS relay_job_claimable_idx
    ON relay_job (organization, registration_id, status, lease_expires_at)
    WHERE status IN (0, 1);

COMMENT ON TABLE relay_job IS
    'Durable job truth. Leases are server-clock and fenced by (lease_session, lease_epoch).';
COMMENT ON COLUMN relay_job.lease_epoch IS
    'Generation of the current lease. A result echoing an older generation is refused, never recorded.';
