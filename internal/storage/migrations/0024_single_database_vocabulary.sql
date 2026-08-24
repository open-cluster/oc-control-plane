-- Persist the contracted storage vocabulary for databases that already applied migration 0001.
-- This changes documentation metadata only; no row or schema shape changes.
COMMENT ON TABLE app_user IS
    'A person who may sign in. Deployment-wide; identity is the issuer and subject together.';
