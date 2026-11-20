ALTER TABLE public.agent_checkpoint
    DROP CONSTRAINT agent_checkpoint_pkey,
    ALTER COLUMN writer_execution_id DROP NOT NULL,
    ADD COLUMN conversation_id uuid,
    ADD COLUMN committed boolean NOT NULL DEFAULT false,
    ADD FOREIGN KEY (org_id, conversation_id)
        REFERENCES public.conversation (org_id, conversation_id) ON DELETE CASCADE,
    ADD CHECK (writer_execution_id IS NOT NULL OR conversation_id IS NOT NULL);

DELETE FROM public.agent_checkpoint c USING public.agent_execution e
 WHERE (e.org_id, e.execution_id) = (c.org_id, c.writer_execution_id)
   AND e.status IN ('failed', 'cancelled');

UPDATE public.agent_checkpoint c
   SET conversation_id = e.conversation_id,
       committed = (e.status = 'completed' AND e.conversation_id IS NOT NULL),
       writer_execution_id = CASE WHEN e.status = 'completed' AND e.conversation_id IS NOT NULL
                                  THEN NULL ELSE e.execution_id END
  FROM public.agent_execution e
 WHERE (e.org_id, e.execution_id) = (c.org_id, c.writer_execution_id);

ALTER TABLE public.agent_checkpoint
    ADD PRIMARY KEY (org_id, session_id, committed),
    ADD CHECK (committed = (writer_execution_id IS NULL)),
    ADD CHECK (conversation_id IS NULL OR session_id = 'conversation/' || conversation_id::text);
