DO $$
BEGIN
    IF to_regclass('public.webhook_job') IS NOT NULL THEN
        ALTER TABLE webhook_job RENAME TO slack_message_work;
        ALTER TABLE slack_message_work RENAME COLUMN job_id TO work_id;
    END IF;
END $$;

DO $$
DECLARE
    rename_pair text[];
BEGIN
	IF to_regclass('public.slack_message_work') IS NULL THEN
		RETURN;
	END IF;
	FOREACH rename_pair SLICE 1 IN ARRAY ARRAY[
        ['webhook_job_attempts_check', 'slack_message_work_attempts_check'],
        ['webhook_job_failure_matches_retry_or_terminal', 'slack_message_work_failure_matches_retry_or_terminal'],
        ['webhook_job_lease_epoch_check', 'slack_message_work_lease_epoch_check'],
        ['webhook_job_lease_is_complete', 'slack_message_work_lease_is_complete'],
        ['webhook_job_status_check', 'slack_message_work_status_check'],
        ['webhook_job_identity_is_org_scoped', 'slack_message_work_identity_is_org_scoped'],
        ['webhook_job_pkey', 'slack_message_work_pkey'],
        ['webhook_job_delivery_is_in_the_same_org', 'slack_message_work_delivery_is_in_the_same_org'],
        ['webhook_job_integration_is_in_the_same_org', 'slack_message_work_integration_is_in_the_same_org'],
        ['webhook_job_message_is_in_the_same_org', 'slack_message_work_message_is_in_the_same_org']
    ]
    LOOP
        IF EXISTS (
            SELECT 1 FROM pg_constraint
             WHERE conname = rename_pair[1]
               AND conrelid = 'slack_message_work'::regclass
        ) THEN
            EXECUTE format('ALTER TABLE slack_message_work RENAME CONSTRAINT %I TO %I',
                rename_pair[1], rename_pair[2]);
        END IF;
    END LOOP;
END $$;

ALTER INDEX IF EXISTS webhook_job_ready_idx RENAME TO slack_message_work_ready_idx;
ALTER INDEX IF EXISTS webhook_job_source_effect_is_unique RENAME TO slack_message_work_source_effect_is_unique;
ALTER INDEX IF EXISTS webhook_job_terminal_idx RENAME TO slack_message_work_terminal_idx;
