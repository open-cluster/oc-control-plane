CREATE TABLE deployment_initialization (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    initialized_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO deployment_initialization (singleton)
SELECT true WHERE EXISTS (SELECT 1 FROM app_user);

ALTER TABLE audit_event DROP CONSTRAINT audit_event_deployment_action_check;
ALTER TABLE audit_event ADD CONSTRAINT audit_event_deployment_action_check
    CHECK (org_id IS NOT NULL OR action IN (
        'session.signed-out', 'session.revoked', 'local.password-changed',
        'local.password-recovered', 'local.bootstrap-completed'
    ));
