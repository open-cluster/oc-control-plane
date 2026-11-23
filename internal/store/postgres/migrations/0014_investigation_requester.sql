ALTER TABLE public.investigation ADD COLUMN requested_by uuid;

-- A legacy Investigation has no AgentScope checkpoint to resume. Its unassigned Messages remain queued.
WITH interrupted AS (
    UPDATE public.investigation i
       SET status = 3, error = 'worker interrupted', concluded_at = now(),
           lease_worker = '', lease_token = NULL, lease_expires_at = NULL
     WHERE i.status = 1 AND NOT EXISTS (
         SELECT 1 FROM public.agent_execution e
          WHERE e.org_id = i.org_id AND e.investigation_id = i.investigation_id)
    RETURNING i.org_id, i.investigation_id
), cancelled AS (
    UPDATE public.relay_job job
       SET status = CASE WHEN job.status = 0 THEN 4 ELSE job.status END,
           terminal_at = CASE WHEN job.status = 0 THEN now() ELSE job.terminal_at END,
           cancel_requested_at = coalesce(job.cancel_requested_at, now())
      FROM interrupted i
     WHERE job.org_id = i.org_id AND job.investigation_id = i.investigation_id AND job.status IN (0, 1)
)
INSERT INTO public.investigation_event (org_id, investigation_id, sequence, type, payload)
SELECT i.org_id, i.investigation_id, coalesce((SELECT max(e.sequence) FROM public.investigation_event e
    WHERE e.org_id = i.org_id AND e.investigation_id = i.investigation_id), 0) + 1,
    7, '{"reason":"worker interrupted"}'::jsonb
FROM interrupted i;
