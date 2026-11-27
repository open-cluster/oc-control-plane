ALTER TABLE public.agent_execution ADD COLUMN draft_text text,
    ADD CHECK (draft_text IS NULL OR (
        conversation_id IS NOT NULL AND status = 'running' AND char_length(draft_text) <= 8192
    ));
