ALTER TABLE operator_session
    ALTER COLUMN org_id DROP NOT NULL;

COMMENT ON COLUMN operator_session.org_id IS
    'Organization selected when the session was issued; NULL until the bootstrapped User creates the first Organization.';
