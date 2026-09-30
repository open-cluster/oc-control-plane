DO $$
BEGIN
    IF to_regclass('public.investigation') IS NOT NULL THEN
        ALTER TABLE investigation
            ADD COLUMN automatic_incident boolean NOT NULL DEFAULT false,
            ADD CONSTRAINT investigation_automatic_incident_has_no_conversation
                CHECK (NOT automatic_incident OR
                    (incident_id IS NOT NULL AND conversation_id IS NULL AND turn IS NULL));

        CREATE UNIQUE INDEX investigation_one_automatic_per_incident
            ON investigation (org_id, incident_id) WHERE automatic_incident;
    END IF;
END $$;
