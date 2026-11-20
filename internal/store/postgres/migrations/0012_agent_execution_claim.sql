CREATE INDEX agent_execution_ready
    ON public.agent_execution (created_at, execution_id) WHERE status IN ('queued', 'running');
