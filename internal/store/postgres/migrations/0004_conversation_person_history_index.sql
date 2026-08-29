CREATE INDEX conversation_message_person_history_idx
    ON conversation_message (org_id, conversation_id, sequence)
    WHERE role = 1;
