DROP TABLE IF EXISTS deployment_initialization;

DO $$
BEGIN
    IF to_regclass('public.deployment_sign_in_flow') IS NOT NULL THEN
        ALTER TABLE deployment_sign_in_flow RENAME TO oidc_sign_in_flow;
        ALTER TABLE oidc_sign_in_flow RENAME CONSTRAINT deployment_sign_in_flow_state_digest_check TO oidc_sign_in_flow_state_digest_check;
        ALTER TABLE oidc_sign_in_flow RENAME CONSTRAINT deployment_sign_in_flow_organization_exists TO oidc_sign_in_flow_organization_exists;
        ALTER INDEX deployment_sign_in_flow_expiry RENAME TO oidc_sign_in_flow_expiry;
        ALTER TABLE oidc_sign_in_flow DROP CONSTRAINT deployment_sign_in_flow_pkey;
        ALTER TABLE oidc_sign_in_flow DROP CONSTRAINT deployment_sign_in_flow_state_digest_key;
        ALTER TABLE oidc_sign_in_flow DROP COLUMN flow_id;
        ALTER TABLE oidc_sign_in_flow DROP COLUMN consumed_at;
        ALTER TABLE oidc_sign_in_flow DROP COLUMN created_at;
        ALTER TABLE oidc_sign_in_flow ADD CONSTRAINT oidc_sign_in_flow_pkey PRIMARY KEY (state_digest);
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'integration' AND column_name = 'status'
    ) THEN
        INSERT INTO integration_installation
            (integration_id, org_id, integration_type_id, application, enterprise,
             workspace, enterprise_wide, agent, authorizer, grants, updated_at)
        SELECT integration_id, org_id, integration_type_id,
               configuration ->> 'appID', '', configuration ->> 'teamId', false,
               coalesce(verify_facts ->> 'botUserId', ''), '', '{}', now()
          FROM integration
         WHERE configuration ->> 'appID' <> ''
           AND configuration ->> 'teamId' <> ''
        ON CONFLICT (integration_id) DO NOTHING;

        WITH github_installation AS (
            SELECT integration_id, org_id, integration_type_id, configuration, verify_facts,
                   row_number() OVER (
                       PARTITION BY configuration ->> 'installationId'
                       ORDER BY created_at, integration_id
                   ) AS duplicate_number
              FROM integration
             WHERE configuration ->> 'installationId' <> ''
        )
        INSERT INTO integration_installation
            (integration_id, org_id, integration_type_id, application, enterprise,
             workspace, enterprise_wide, agent, authorizer, grants, updated_at)
        SELECT integration_id, org_id, integration_type_id,
               CASE WHEN duplicate_number = 1
                   THEN 'github'
                   ELSE 'github:legacy:' || integration_id::text
               END,
               '', configuration ->> 'installationId', false,
               coalesce(verify_facts ->> 'account', ''), '', '{}', now()
          FROM github_installation
        ON CONFLICT (integration_id) DO NOTHING;

        ALTER TABLE integration ADD COLUMN verification_status text;
        ALTER TABLE integration ADD COLUMN verified_at timestamptz;
        ALTER TABLE integration ADD COLUMN verification_grants text[] DEFAULT '{}' NOT NULL;
        ALTER TABLE integration ADD COLUMN disabled boolean DEFAULT false NOT NULL;

        UPDATE integration
           SET verification_status = CASE
                   WHEN status IN (2, 3) THEN 'verified'
                   WHEN status = 4 THEN 'failed'
               END,
               verified_at = CASE WHEN status IN (2, 3) THEN last_verified_at END,
               verification_grants = CASE
                   WHEN status IN (2, 3) AND jsonb_typeof(verify_grants) = 'array'
                   THEN ARRAY(SELECT jsonb_array_elements_text(verify_grants))
                   ELSE '{}'
               END,
               disabled = disabled_at IS NOT NULL;

        UPDATE integration
           SET configuration = configuration - 'teamId' - 'appID'
         WHERE configuration ? 'teamId' AND configuration ? 'appID';

        UPDATE integration
           SET configuration = configuration - 'installationId'
         WHERE configuration ? 'installationId';

        ALTER TABLE integration DROP CONSTRAINT integration_credential_is_whole;
        ALTER TABLE integration DROP CONSTRAINT integration_status_check;
        ALTER TABLE integration DROP CONSTRAINT integration_supported_kind;
        ALTER TABLE integration DROP CONSTRAINT integration_webhook_secret_is_whole;
        ALTER TABLE integration DROP CONSTRAINT integration_name_is_unique_per_org;
        ALTER TABLE integration_installation DROP CONSTRAINT integration_installation_supported_kind;
        ALTER TABLE integration_installation DROP COLUMN grants;

        ALTER TABLE integration
            DROP COLUMN webhook_secret_fingerprint,
            DROP COLUMN webhook_secret_created_at,
            DROP COLUMN webhook_secret_rotated_at,
            DROP COLUMN labels,
            DROP COLUMN status,
            DROP COLUMN last_verified_at,
            DROP COLUMN verify_note,
            DROP COLUMN disabled_at,
            DROP COLUMN created_by,
            DROP COLUMN updated_at,
            DROP COLUMN credential_fingerprint,
            DROP COLUMN credential_created_at,
            DROP COLUMN credential_rotated_at,
            DROP COLUMN verify_grants,
            DROP COLUMN verify_facts;

        ALTER TABLE integration ADD CONSTRAINT integration_verification_status_check
            CHECK (verification_status IS NULL OR verification_status = ANY (ARRAY['verified', 'failed']));
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'integration_connect_flow' AND column_name = 'flow_id'
    ) THEN
        ALTER TABLE integration_connect_flow ADD COLUMN provider text;
        UPDATE integration_connect_flow SET provider = 'slack';
        ALTER TABLE integration_connect_flow ALTER COLUMN provider SET NOT NULL;
        ALTER TABLE integration_connect_flow DROP CONSTRAINT integration_connect_flow_pkey;
        ALTER TABLE integration_connect_flow DROP CONSTRAINT integration_connect_flow_state_is_unique;
        ALTER TABLE integration_connect_flow DROP CONSTRAINT integration_connect_flow_expires_after_it_started;
        ALTER TABLE integration_connect_flow DROP CONSTRAINT integration_connect_flow_supported_kind;
        ALTER TABLE integration_connect_flow
            DROP COLUMN flow_id,
            DROP COLUMN integration_type_id,
            DROP COLUMN created_at,
            DROP COLUMN consumed_at;
        ALTER TABLE integration_connect_flow ADD CONSTRAINT integration_connect_flow_pkey PRIMARY KEY (state_digest);
    END IF;
END $$;
