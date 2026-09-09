LOCK TABLE organization, organization_membership, operator_session, relay_bootstrap_token, incident IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM organization_membership WHERE role NOT IN ('admin', 'editor', 'viewer')) THEN
        RAISE EXCEPTION 'organization membership has missing or unsupported roles; reconcile retained authority before upgrading';
    END IF;

    IF EXISTS (
        SELECT 1 FROM organization_policy
        WHERE session_lifetime_seconds <> 0
    ) THEN
        RAISE EXCEPTION 'organization policy has retained session lifetime; configure deployment session lifetime before upgrading';
    END IF;
END $$;

ALTER TABLE organization
    ADD COLUMN organization_id uuid DEFAULT gen_random_uuid() NOT NULL,
    ADD COLUMN audit_retention_days integer DEFAULT 0 NOT NULL,
    ADD CONSTRAINT organization_id_is_unique UNIQUE (organization_id),
    ADD CONSTRAINT organization_audit_retention_days_check CHECK (audit_retention_days >= 0);

UPDATE organization root
   SET audit_retention_days = policy.audit_retention_days
  FROM organization_policy policy
 WHERE root.org_id = policy.org_id;

UPDATE organization_membership SET role = 'viewer' WHERE role IS NULL;

ALTER TABLE organization_membership
    ALTER COLUMN role SET NOT NULL,
    DROP CONSTRAINT organization_membership_role_check,
    ADD CONSTRAINT organization_membership_role_check CHECK (role IN ('admin', 'editor', 'viewer'));

ALTER TABLE operator_session
    RENAME COLUMN token_digest TO credential_digest;
ALTER TABLE operator_session
    RENAME CONSTRAINT operator_session_token_digest_check TO operator_session_credential_digest_check;
ALTER TABLE operator_session
    RENAME CONSTRAINT operator_session_digest_is_unique TO operator_session_credential_digest_is_unique;

ALTER TABLE relay_bootstrap_token
    RENAME COLUMN token_digest TO bootstrap_digest;
ALTER TABLE relay_bootstrap_token
    RENAME CONSTRAINT relay_bootstrap_token_token_digest_check TO relay_bootstrap_token_bootstrap_digest_check;

ALTER TABLE incident
    DROP CONSTRAINT incident_alert_event_count_check,
    DROP COLUMN alert_event_count;

CREATE FUNCTION resolve_tenant_organization_id() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    resolved uuid;
BEGIN
    IF NEW.org_id IS NULL THEN
        IF NEW.organization_id IS NOT NULL THEN
            RAISE foreign_key_violation USING MESSAGE = 'deployment scoped row must not name an Organization UUID';
        END IF;
        RETURN NEW;
    END IF;
    SELECT root.organization_id INTO resolved FROM organization root WHERE root.org_id = NEW.org_id;
    IF resolved IS NULL THEN
        RAISE foreign_key_violation USING MESSAGE = 'tenant row names no known Organization';
    END IF;
    IF NEW.organization_id IS NOT NULL AND NEW.organization_id <> resolved THEN
        RAISE foreign_key_violation USING MESSAGE = 'tenant row Organization UUID does not match its selector';
    END IF;
    NEW.organization_id = resolved;
    RETURN NEW;
END $$;

ALTER TABLE alert_event ADD COLUMN organization_id uuid;
ALTER TABLE audit_event ADD COLUMN organization_id uuid;
ALTER TABLE change_ledger ADD COLUMN organization_id uuid;
ALTER TABLE change_ledger_scope ADD COLUMN organization_id uuid;
ALTER TABLE conversation ADD COLUMN organization_id uuid;
ALTER TABLE conversation_message ADD COLUMN organization_id uuid;
ALTER TABLE deployment_sign_in_flow ADD COLUMN organization_id uuid;
ALTER TABLE incident ADD COLUMN organization_id uuid;
ALTER TABLE integration ADD COLUMN organization_id uuid;
ALTER TABLE integration_connect_flow ADD COLUMN organization_id uuid;
ALTER TABLE integration_delivery ADD COLUMN organization_id uuid;
ALTER TABLE integration_installation ADD COLUMN organization_id uuid;
ALTER TABLE investigation ADD COLUMN organization_id uuid;
ALTER TABLE investigation_event ADD COLUMN organization_id uuid;
ALTER TABLE investigation_tool_run ADD COLUMN organization_id uuid;
ALTER TABLE operator_session ADD COLUMN organization_id uuid;
ALTER TABLE organization_membership ADD COLUMN organization_id uuid;
ALTER TABLE postmortem ADD COLUMN organization_id uuid;
ALTER TABLE relay_bootstrap_token ADD COLUMN organization_id uuid;
ALTER TABLE relay_job ADD COLUMN organization_id uuid;
ALTER TABLE relay_registration ADD COLUMN organization_id uuid;
ALTER TABLE relay_session_conflict_event ADD COLUMN organization_id uuid;
ALTER TABLE slack_conversation ADD COLUMN organization_id uuid;
ALTER TABLE slack_reply ADD COLUMN organization_id uuid;
ALTER TABLE webhook_work ADD COLUMN organization_id uuid;

UPDATE alert_event child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE audit_event child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE change_ledger child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE change_ledger_scope child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE conversation child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE conversation_message child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE deployment_sign_in_flow child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE incident child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE integration child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE integration_connect_flow child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE integration_delivery child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE integration_installation child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE investigation child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE investigation_event child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE investigation_tool_run child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE operator_session child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE organization_membership child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE postmortem child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE relay_bootstrap_token child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE relay_job child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE relay_registration child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE relay_session_conflict_event child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE slack_conversation child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE slack_reply child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;
UPDATE webhook_work child SET organization_id = root.organization_id FROM organization root WHERE child.org_id = root.org_id;

ALTER TABLE alert_event ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE change_ledger ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE change_ledger_scope ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE conversation ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE conversation_message ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE deployment_sign_in_flow ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE incident ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE integration ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE integration_connect_flow ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE integration_delivery ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE integration_installation ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE investigation ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE investigation_event ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE investigation_tool_run ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE organization_membership ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE postmortem ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE relay_bootstrap_token ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE relay_job ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE relay_registration ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE relay_session_conflict_event ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE slack_conversation ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE slack_reply ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE webhook_work ALTER COLUMN organization_id SET NOT NULL;

ALTER TABLE audit_event
    ADD CONSTRAINT audit_event_scope_is_whole CHECK ((org_id IS NULL) = (organization_id IS NULL));
ALTER TABLE operator_session
    ADD CONSTRAINT operator_session_scope_is_whole CHECK ((org_id IS NULL) = (organization_id IS NULL));

ALTER TABLE alert_event ADD CONSTRAINT alert_event_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE audit_event ADD CONSTRAINT audit_event_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE change_ledger ADD CONSTRAINT change_ledger_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE change_ledger_scope ADD CONSTRAINT change_ledger_scope_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE conversation ADD CONSTRAINT conversation_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE conversation_message ADD CONSTRAINT conversation_message_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE deployment_sign_in_flow ADD CONSTRAINT deployment_sign_in_flow_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE incident ADD CONSTRAINT incident_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE integration ADD CONSTRAINT integration_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE integration_connect_flow ADD CONSTRAINT integration_connect_flow_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE integration_delivery ADD CONSTRAINT integration_delivery_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE integration_installation ADD CONSTRAINT integration_installation_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE investigation ADD CONSTRAINT investigation_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE investigation_event ADD CONSTRAINT investigation_event_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE investigation_tool_run ADD CONSTRAINT investigation_tool_run_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE operator_session ADD CONSTRAINT operator_session_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE organization_membership ADD CONSTRAINT organization_membership_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE postmortem ADD CONSTRAINT postmortem_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE relay_bootstrap_token ADD CONSTRAINT relay_bootstrap_token_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE relay_job ADD CONSTRAINT relay_job_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE relay_registration ADD CONSTRAINT relay_registration_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE relay_session_conflict_event ADD CONSTRAINT relay_session_conflict_event_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE slack_conversation ADD CONSTRAINT slack_conversation_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE slack_reply ADD CONSTRAINT slack_reply_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;
ALTER TABLE webhook_work ADD CONSTRAINT webhook_work_organization_exists FOREIGN KEY (organization_id) REFERENCES organization (organization_id) ON DELETE RESTRICT;

CREATE TRIGGER alert_event_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON alert_event FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER audit_event_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON audit_event FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER change_ledger_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON change_ledger FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER change_ledger_scope_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON change_ledger_scope FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER conversation_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON conversation FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER conversation_message_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON conversation_message FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER deployment_sign_in_flow_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON deployment_sign_in_flow FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER incident_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON incident FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER integration_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON integration FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER integration_connect_flow_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON integration_connect_flow FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER integration_delivery_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON integration_delivery FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER integration_installation_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON integration_installation FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER investigation_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON investigation FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER investigation_event_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON investigation_event FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER investigation_tool_run_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON investigation_tool_run FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER operator_session_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON operator_session FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER organization_membership_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON organization_membership FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER postmortem_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON postmortem FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER relay_bootstrap_token_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON relay_bootstrap_token FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER relay_job_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON relay_job FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER relay_registration_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON relay_registration FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER relay_session_conflict_event_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON relay_session_conflict_event FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER slack_conversation_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON slack_conversation FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER slack_reply_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON slack_reply FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();
CREATE TRIGGER webhook_work_organization_id_is_resolved BEFORE INSERT OR UPDATE OF org_id, organization_id ON webhook_work FOR EACH ROW EXECUTE FUNCTION resolve_tenant_organization_id();


CREATE FUNCTION prevent_organization_identity_retargeting() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.org_id, NEW.organization_id)
        IS DISTINCT FROM (OLD.org_id, OLD.organization_id) THEN
        RAISE EXCEPTION 'Organization identity is immutable';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER organization_identity_is_immutable
    BEFORE UPDATE ON organization
    FOR EACH ROW EXECUTE FUNCTION prevent_organization_identity_retargeting();

DROP TABLE organization_policy;
