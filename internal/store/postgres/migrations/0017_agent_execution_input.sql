ALTER TABLE public.agent_execution ADD COLUMN pending_input jsonb,
    ADD CHECK (pending_input IS NULL OR (
        conversation_id IS NOT NULL AND jsonb_typeof(pending_input) = 'object'
    ));
