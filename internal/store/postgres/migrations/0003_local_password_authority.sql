ALTER TABLE audit_event DROP CONSTRAINT audit_event_deployment_action_check;
ALTER TABLE audit_event ADD CONSTRAINT audit_event_deployment_action_check
    CHECK (org_id IS NOT NULL OR action IN (
        'session.signed-out', 'session.revoked', 'local.password-changed', 'local.password-recovered'
    ));
