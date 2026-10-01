DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
         WHERE table_schema = 'public' AND table_name = 'webhook_job' AND column_name = 'kind'
    ) THEN
        IF EXISTS (SELECT 1 FROM webhook_job WHERE kind IS DISTINCT FROM 2) THEN
            RAISE EXCEPTION 'webhook work must be Slack-only before schema contraction';
        END IF;

        ALTER TABLE webhook_job
            DROP CONSTRAINT webhook_job_has_one_effect_reference,
            DROP CONSTRAINT webhook_job_kind_check,
            DROP CONSTRAINT webhook_job_incident_is_in_the_same_org;
        DROP INDEX webhook_job_source_effect_is_unique;
        ALTER TABLE webhook_job
            DROP COLUMN incident_id,
            DROP COLUMN kind,
            ALTER COLUMN conversation_id SET NOT NULL,
            ALTER COLUMN message_sequence SET NOT NULL;
        CREATE UNIQUE INDEX webhook_job_source_effect_is_unique
            ON webhook_job (org_id, delivery_id, conversation_id, message_sequence);
    END IF;

    ALTER TABLE IF EXISTS investigation DROP COLUMN IF EXISTS webhook_job_id;
END $$;
