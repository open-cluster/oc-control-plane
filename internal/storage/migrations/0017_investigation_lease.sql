-- Investigations stop being process-local goroutines (issue #12).
--
-- A turn is claimed under a lease with a heartbeat, so a worker that dies leaves an
-- investigation that is recovered rather than permanently `running`, and several
-- control-plane replicas can run turns for one tenant.
--
-- The shape mirrors relay_job's, deliberately: it is the one place in this schema where
-- "never lost, never done twice" is already proven under server-clock leases, and the
-- reviewer reading the claimer will recognise it. Every timestamp is the server's clock,
-- never a worker's.
--
-- NO NEW STATUS VALUE. An investigation with no lease is waiting to be claimed; one with
-- a live lease is executing. Both are `running`, because both are true, and the first
-- stream event says which. A fourth status would mean every existing reader learns a
-- state that is not a new fact about the investigation.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

ALTER TABLE investigation
    ADD COLUMN lease_worker TEXT NOT NULL DEFAULT ''
        CHECK (length(lease_worker) <= 128);
ALTER TABLE investigation
    ADD COLUMN lease_expires_at TIMESTAMPTZ;
ALTER TABLE investigation
    ADD COLUMN lease_heartbeat_at TIMESTAMPTZ;

-- A lease is a worker and an expiry together. Half a lease would be a row no sweeper can
-- reason about: unclaimable because a worker holds it, and never expiring because nothing
-- says when.
ALTER TABLE investigation
    ADD CONSTRAINT investigation_lease_is_whole
        CHECK ((lease_worker = '') = (lease_expires_at IS NULL));

-- The claimer's read: the running investigations of one organization, oldest first, so
-- work is taken up in the order it was opened. Partial, because everything that has
-- concluded or failed is never claimable again and there are far more of those.
CREATE INDEX IF NOT EXISTS investigation_claimable_idx
    ON investigation (org_id, created_at, investigation_id)
    WHERE status = 1;

-- The sweeper's read, across organizations within one placement.
CREATE INDEX IF NOT EXISTS investigation_lease_expiry_idx
    ON investigation (lease_expires_at)
    WHERE status = 1 AND lease_worker <> '';

COMMENT ON COLUMN investigation.lease_worker IS
    'The worker executing this turn; empty when the investigation is waiting to be claimed.';
COMMENT ON COLUMN investigation.lease_expires_at IS
    'When the claim lapses if the worker stops heartbeating. Server clock, never a worker''s.';
COMMENT ON COLUMN investigation.lease_heartbeat_at IS
    'When the holder last said it was still working.';
