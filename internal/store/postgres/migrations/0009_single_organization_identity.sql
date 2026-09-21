ALTER TABLE IF EXISTS app_user
    ALTER COLUMN user_id SET DEFAULT gen_random_uuid();

ALTER TABLE IF EXISTS session
    DROP CONSTRAINT IF EXISTS session_organization_exists,
    DROP COLUMN IF EXISTS org_id,
    ALTER COLUMN session_id SET DEFAULT gen_random_uuid();

ALTER TABLE IF EXISTS oidc_sign_in_flow
    DROP CONSTRAINT IF EXISTS oidc_sign_in_flow_organization_exists,
    DROP COLUMN IF EXISTS org_id;

ALTER TABLE IF EXISTS organization_membership
    ADD CONSTRAINT organization_membership_user_is_unique UNIQUE (user_id);
