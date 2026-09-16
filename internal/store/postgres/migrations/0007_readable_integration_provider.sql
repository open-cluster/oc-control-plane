DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'integration'
          AND column_name = 'integration_type_id'
    ) THEN
        ALTER TABLE integration ADD COLUMN provider text;
        ALTER TABLE integration_installation ADD COLUMN provider text;

        UPDATE integration
        SET provider = CASE integration_type_id
            WHEN 1 THEN 'alertmanager'
            WHEN 2 THEN 'kubernetes'
            WHEN 3 THEN 'slack'
            WHEN 4 THEN 'github'
            WHEN 5 THEN 'generic_webhook'
        END;

        UPDATE integration_installation installed
        SET provider = integration.provider
        FROM integration
        WHERE integration.org_id = installed.org_id
          AND integration.integration_id = installed.integration_id;

        ALTER TABLE integration_installation
            DROP CONSTRAINT IF EXISTS integration_installation_matches_parent_kind;
        DROP INDEX IF EXISTS integration_installation_is_one_workspace;
        ALTER TABLE integration DROP CONSTRAINT IF EXISTS integration_org_id_kind_unique;

        ALTER TABLE integration
            ALTER COLUMN provider SET NOT NULL,
            DROP COLUMN integration_type_id,
            ADD CONSTRAINT integration_org_id_provider_unique
                UNIQUE (org_id, integration_id, provider);

        ALTER TABLE integration_installation
            ALTER COLUMN provider SET NOT NULL,
            DROP COLUMN integration_type_id,
            ADD CONSTRAINT integration_installation_matches_parent_provider
                FOREIGN KEY (org_id, integration_id, provider)
                REFERENCES integration(org_id, integration_id, provider);

        CREATE UNIQUE INDEX integration_installation_is_one_workspace
            ON integration_installation (provider, application, enterprise, workspace);
    END IF;
END $$;
