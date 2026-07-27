-- Asking a job to stop.
--
-- A cancellation request is not an outcome. A job that is executing stays leased and stays
-- live until its terminal outcome is recorded or its lease expires, because there is exactly
-- one write path into job truth and this is not it: the relay reports what actually happened,
-- and a job that finished just before the request arrived finished.
--
-- A job that has not started needs no relay to stop it, so it is cancelled outright by the
-- same call. That transition is a real outcome and goes through the status column.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

ALTER TABLE relay_job
    ADD COLUMN IF NOT EXISTS cancel_requested_at TIMESTAMPTZ;

COMMENT ON COLUMN relay_job.cancel_requested_at IS
    'When a stop was asked for. Advisory: the terminal outcome still arrives from the relay.';
