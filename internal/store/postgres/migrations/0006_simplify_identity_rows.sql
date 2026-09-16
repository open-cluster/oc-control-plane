ALTER TABLE IF EXISTS app_user
    DROP COLUMN IF EXISTS email_verified,
    DROP COLUMN IF EXISTS last_sign_in,
    DROP COLUMN IF EXISTS updated_at;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'local_password'
          AND column_name = 'password_changed_at'
    ) THEN
        ALTER TABLE local_password RENAME COLUMN password_changed_at TO changed_at;
    END IF;
END $$;

ALTER TABLE IF EXISTS local_password ADD COLUMN IF NOT EXISTS changed_at timestamptz DEFAULT now() NOT NULL;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'local_password' AND column_name = 'updated_at'
    ) THEN
        UPDATE local_password SET changed_at = GREATEST(changed_at, updated_at);
        ALTER TABLE local_password DROP COLUMN updated_at;
    END IF;
END $$;

ALTER TABLE IF EXISTS session ADD COLUMN IF NOT EXISTS client_user_agent text;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'session' AND column_name = 'remote_addr'
    ) THEN
        ALTER TABLE session ALTER COLUMN remote_addr DROP DEFAULT;
        ALTER TABLE session ALTER COLUMN remote_addr DROP NOT NULL;
    END IF;
END $$;
