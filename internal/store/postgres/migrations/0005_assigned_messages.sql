CREATE INDEX conversation_message_assigned_idx
    ON conversation_message (org_id, investigation_id, conversation_id, sequence)
    WHERE role = 1 AND investigation_id IS NOT NULL;
