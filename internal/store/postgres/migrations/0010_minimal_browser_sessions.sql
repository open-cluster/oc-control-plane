DO $$
BEGIN
    IF to_regclass('public.session') IS NOT NULL THEN
        DELETE FROM session
        WHERE revoked_at IS NOT NULL OR expires_at <= now();

        DROP INDEX IF EXISTS session_user_idx;

        ALTER TABLE session
            DROP CONSTRAINT IF EXISTS session_expires_after_it_was_issued,
            DROP COLUMN IF EXISTS issued_at,
            DROP COLUMN IF EXISTS last_seen_at,
            DROP COLUMN IF EXISTS revoked_at,
            DROP COLUMN IF EXISTS client_user_agent,
            DROP COLUMN IF EXISTS remote_addr;

        CREATE INDEX session_user_idx ON session (user_id);
    END IF;
END $$;
