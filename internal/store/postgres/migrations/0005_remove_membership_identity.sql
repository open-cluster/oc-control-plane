DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'organization_membership'
          AND column_name = 'membership_id'
    ) THEN
        DROP INDEX IF EXISTS organization_membership_org_idx;

        ALTER TABLE organization_membership
            DROP CONSTRAINT organization_membership_pkey,
            DROP CONSTRAINT organization_membership_is_one_per_tenant,
            DROP COLUMN membership_id,
            ADD CONSTRAINT organization_membership_pkey PRIMARY KEY (org_id, user_id);

        CREATE INDEX organization_membership_org_idx
            ON organization_membership (org_id, created_at, user_id);
    END IF;
END $$;
