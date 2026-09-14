DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'webhook_delivery' AND column_name = 'outcome'
    ) THEN
        DELETE FROM webhook_delivery WHERE outcome <> 1;

        DROP INDEX IF EXISTS webhook_delivery_accepted_idx;
        DROP INDEX IF EXISTS webhook_delivery_accepted_provider_identity_is_unique;

        ALTER TABLE webhook_delivery
            DROP CONSTRAINT IF EXISTS webhook_delivery_accepted_carries_a_digest,
            DROP CONSTRAINT IF EXISTS webhook_delivery_accepted_carries_provider_identity,
            DROP CONSTRAINT IF EXISTS webhook_delivery_alert_event_count_check,
            DROP CONSTRAINT IF EXISTS webhook_delivery_body_digest_check,
            DROP CONSTRAINT IF EXISTS webhook_delivery_lifecycle_phase_check,
            DROP CONSTRAINT IF EXISTS webhook_delivery_nonaccepted_has_no_provider_identity,
            DROP CONSTRAINT IF EXISTS webhook_delivery_outcome_check,
            DROP CONSTRAINT IF EXISTS webhook_delivery_states_a_reason_exactly_when_it_refused;

        ALTER TABLE webhook_delivery RENAME COLUMN body_digest TO content_digest;
        ALTER TABLE webhook_delivery
            ALTER COLUMN content_digest SET NOT NULL,
            ALTER COLUMN provider_identity SET NOT NULL,
            ALTER COLUMN lifecycle_phase SET DEFAULT '',
            ALTER COLUMN lifecycle_phase SET NOT NULL,
            DROP COLUMN outcome,
            DROP COLUMN reason,
            DROP COLUMN alert_event_count;

        ALTER TABLE webhook_delivery
            ADD CONSTRAINT webhook_delivery_content_digest_check CHECK (length(content_digest) = 32),
            ADD CONSTRAINT webhook_delivery_lifecycle_phase_check
                CHECK (lifecycle_phase = ANY (ARRAY['', 'firing', 'resolved']));

        CREATE INDEX webhook_delivery_received_idx
            ON webhook_delivery (org_id, received_at DESC, delivery_id DESC);
        CREATE UNIQUE INDEX webhook_delivery_provider_identity_is_unique
            ON webhook_delivery (integration_id, provider_identity, lifecycle_phase);
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'session' AND column_name = 'address'
    ) THEN
        ALTER TABLE session RENAME COLUMN address TO remote_addr;
    END IF;
    ALTER TABLE IF EXISTS session
        DROP COLUMN IF EXISTS revoked_by,
        DROP COLUMN IF EXISTS user_agent;
END $$;
