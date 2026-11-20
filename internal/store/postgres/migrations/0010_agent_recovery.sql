ALTER TABLE public.agent_execution ADD COLUMN started_at timestamptz;

ALTER TABLE public.agent_execution
    ADD COLUMN reply_message_sequence bigint,
    ADD CONSTRAINT agent_execution_reply_owner FOREIGN KEY (org_id, conversation_id, reply_message_sequence)
        REFERENCES public.conversation_message (org_id, conversation_id, sequence) ON DELETE CASCADE,
    ADD CONSTRAINT agent_execution_reply_scope CHECK (
        reply_message_sequence IS NULL OR (conversation_id IS NOT NULL AND status = 'completed'
                                           AND reply_message_sequence > message_sequence));
