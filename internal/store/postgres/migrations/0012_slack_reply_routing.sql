LOCK TABLE slack_conversation, slack_reply, investigation IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM slack_reply r
        LEFT JOIN slack_conversation s ON s.org_id = r.org_id AND s.conversation_id = r.conversation_id
        LEFT JOIN investigation i ON i.org_id = r.org_id AND i.investigation_id = r.investigation_id
        WHERE s.conversation_id IS NULL OR i.investigation_id IS NULL
           OR i.conversation_id IS DISTINCT FROM r.conversation_id
           OR s.integration_id <> r.integration_id
           OR s.channel_id <> r.channel_id OR s.thread_ts <> r.thread_ts
    ) THEN
        RAISE EXCEPTION 'Slack reply disagrees with its Conversation routing or Investigation; reconcile retained destinations before upgrading';
    END IF;
END $$;

ALTER TABLE slack_conversation
    ADD CONSTRAINT slack_conversation_identity_is_org_scoped UNIQUE (org_id, conversation_id);

ALTER TABLE investigation
    ADD CONSTRAINT investigation_identity_in_conversation UNIQUE (org_id, conversation_id, investigation_id);

ALTER TABLE slack_reply
    ADD CONSTRAINT slack_reply_names_its_mapping FOREIGN KEY (org_id, conversation_id)
        REFERENCES slack_conversation (org_id, conversation_id) ON DELETE RESTRICT,
    ADD CONSTRAINT slack_reply_names_its_conversation_investigation FOREIGN KEY (org_id, conversation_id, investigation_id)
        REFERENCES investigation (org_id, conversation_id, investigation_id) ON DELETE CASCADE,
    DROP CONSTRAINT slack_reply_investigation_is_in_the_same_org,
    DROP COLUMN integration_id,
    DROP COLUMN channel_id,
    DROP COLUMN thread_ts;

CREATE FUNCTION prevent_slack_conversation_retargeting() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.org_id, NEW.conversation_id, NEW.integration_id, NEW.channel_id, NEW.thread_ts)
        IS DISTINCT FROM (OLD.org_id, OLD.conversation_id, OLD.integration_id, OLD.channel_id, OLD.thread_ts) THEN
        RAISE EXCEPTION 'Slack Conversation routing is immutable';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER slack_conversation_routing_is_immutable
    BEFORE UPDATE ON slack_conversation
    FOR EACH ROW EXECUTE FUNCTION prevent_slack_conversation_retargeting();

DROP INDEX slack_conversation_by_thread;
