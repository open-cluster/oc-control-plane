ALTER TABLE public.investigation_tool_run
    ADD COLUMN tool_run_id uuid NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN execution_id uuid,
    DROP CONSTRAINT investigation_tool_run_pkey,
    ALTER COLUMN investigation_id DROP NOT NULL,
    ALTER COLUMN ordinal DROP NOT NULL,
    ALTER COLUMN outcome DROP NOT NULL,
    ALTER COLUMN finished_at DROP NOT NULL,
    ADD PRIMARY KEY (org_id, tool_run_id),
    ADD UNIQUE (org_id, investigation_id, ordinal),
    ADD FOREIGN KEY (org_id, execution_id)
        REFERENCES public.agent_execution (org_id, execution_id) ON DELETE CASCADE,
    ADD CHECK (
        (execution_id IS NULL AND investigation_id IS NOT NULL AND ordinal IS NOT NULL AND outcome IS NOT NULL)
        OR (execution_id IS NOT NULL AND investigation_id IS NULL AND ordinal IS NULL)
    ),
    ADD CHECK ((outcome IS NULL) = (finished_at IS NULL));

CREATE INDEX investigation_tool_run_execution_idx
    ON public.investigation_tool_run (org_id, execution_id) WHERE execution_id IS NOT NULL;

ALTER TABLE public.relay_job
    ADD COLUMN tool_run_id uuid,
    ADD FOREIGN KEY (org_id, tool_run_id)
        REFERENCES public.investigation_tool_run (org_id, tool_run_id) ON DELETE CASCADE,
    ADD CHECK (tool_run_id IS NULL OR investigation_id IS NULL);

CREATE UNIQUE INDEX relay_job_tool_run_idx
    ON public.relay_job (org_id, tool_run_id) WHERE tool_run_id IS NOT NULL;

ALTER TABLE public.postmortem ADD COLUMN source_restricted boolean NOT NULL DEFAULT false;

-- Older snapshots flattened testimony and conclusions into prose. Preserve the
-- snapshot and its review state, but withhold it when Slack was a possible input.
UPDATE public.postmortem p SET source_restricted = true
WHERE EXISTS (
    SELECT 1 FROM public.conversation c
    LEFT JOIN public.investigation i
        ON i.org_id = c.org_id AND i.conversation_id = c.conversation_id
    WHERE c.org_id = p.org_id AND c.source = 'slack' AND c.created_at <= p.updated_at
      AND (c.incident_id = p.incident_id
           OR (i.incident_id = p.incident_id AND i.created_at <= p.updated_at)
           OR i.investigation_id::text IN (
               SELECT ref.value #>> '{}' FROM jsonb_path_query(p.document, '$.**.investigationId') ref(value)))
);
