LOCK TABLE integration, integration_installation IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM integration_installation
        WHERE expires_at IS NOT NULL OR refresh_sealed IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'installation has retained refresh data; reconcile credential refresh obligations before upgrading';
    END IF;
END $$;

ALTER TABLE integration DROP COLUMN credential_key_id;
ALTER TABLE integration_installation DROP COLUMN expires_at, DROP COLUMN refresh_sealed;
