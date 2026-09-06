ALTER TABLE audit_event ALTER COLUMN org_id DROP NOT NULL;
ALTER TABLE audit_event ADD CONSTRAINT audit_event_deployment_action_check
    CHECK (org_id IS NOT NULL OR action IN ('session.signed-out', 'session.revoked'));
