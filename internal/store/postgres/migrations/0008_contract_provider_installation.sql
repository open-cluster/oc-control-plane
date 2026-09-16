DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'integration_installation'
          AND column_name = 'application'
    ) THEN
        ALTER TABLE integration_installation
            ADD COLUMN installation_key text[],
            ADD COLUMN provider_actor_id text;

        UPDATE integration_installation
        SET installation_key = CASE provider
                WHEN 'github' THEN ARRAY[workspace]
                WHEN 'slack' THEN CASE
                    WHEN enterprise = '' THEN ARRAY[application, workspace]
                    ELSE ARRAY[application, enterprise, workspace]
                END
            END,
            provider_actor_id = CASE provider WHEN 'slack' THEN NULLIF(agent, '') END;

        WITH ranked_github_installation AS (
            SELECT integration_id,
                   row_number() OVER (
                       PARTITION BY installation_key
                       ORDER BY CASE WHEN application = 'github' THEN 0 ELSE 1 END,
                                installed_at,
                                integration_id
                   ) AS owner_number
              FROM integration_installation
             WHERE provider = 'github'
        )
        DELETE FROM integration_installation installed
         USING ranked_github_installation ranked
         WHERE installed.integration_id = ranked.integration_id
           AND ranked.owner_number > 1;

        DROP INDEX IF EXISTS integration_installation_is_one_workspace;
        ALTER TABLE integration_installation
            DROP CONSTRAINT integration_installation_pkey,
            ALTER COLUMN installation_key SET NOT NULL,
            DROP COLUMN application,
            DROP COLUMN enterprise,
            DROP COLUMN workspace,
            DROP COLUMN enterprise_wide,
            DROP COLUMN agent,
            DROP COLUMN authorizer,
            DROP COLUMN installed_at,
            DROP COLUMN updated_at,
            ADD CONSTRAINT integration_installation_key_is_complete CHECK (
                cardinality(installation_key) > 0
                AND array_position(installation_key, ''::text) IS NULL
                AND array_position(installation_key, NULL::text) IS NULL
            ),
            ADD CONSTRAINT integration_installation_pkey PRIMARY KEY (org_id, integration_id);

        CREATE UNIQUE INDEX integration_installation_provider_key_unique
            ON integration_installation (provider, installation_key);
    END IF;
END $$;
