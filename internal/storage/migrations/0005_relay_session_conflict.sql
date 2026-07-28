-- Two parties competing for one relay identity.
--
-- A relay has one identity and should have one session, so being replaced repeatedly means
-- something else is holding this relay's credential. The victim cannot see that: its own view
-- is only "connected, then immediately superseded", which is what a bad network looks like too.
-- The pattern is only visible centrally, so it is recorded centrally.
--
-- The number of distinct hosts is kept and the addresses are not. More than one host holding
-- one relay's credential is the whole signal; which addresses they were belongs in connection
-- logs, not in a record that will be read, exported, and forwarded onward.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

ALTER TABLE relay_registration
    ADD COLUMN IF NOT EXISTS session_conflict_at    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS session_conflict_hosts INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN relay_registration.session_conflict_at IS
    'When this identity was last seen contested. Not cleared: an operator decides when it is resolved.';
COMMENT ON COLUMN relay_registration.session_conflict_hosts IS
    'Distinct hosts seen taking the session. More than one is the credential-theft signature.';
