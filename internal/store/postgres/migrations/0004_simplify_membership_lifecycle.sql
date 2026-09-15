DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'organization_membership'
          AND column_name = 'active'
    ) THEN
        DELETE FROM organization_membership WHERE NOT active;

        DROP INDEX IF EXISTS organization_membership_external_id_is_unique_per_org;

        ALTER TABLE organization_membership
            DROP CONSTRAINT IF EXISTS organization_membership_source_check,
            DROP COLUMN source,
            DROP COLUMN external_id,
            DROP COLUMN active,
            DROP COLUMN granted_by,
            DROP COLUMN updated_at;
    END IF;
END $$;
