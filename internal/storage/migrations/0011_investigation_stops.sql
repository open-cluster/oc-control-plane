-- Honest stops, and a slimmer run table.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- stopped_by names the ceiling that forced the concluding turn — 'spend', 'tool_runs',
-- 'reasoner_turns' — and is empty when the model concluded freely. Only a concluded
-- investigation carries one: failed stays reserved for reasoner and platform failure,
-- and resource exhaustion is never rendered as a completed diagnosis, so the operator
-- view labels a stopped investigation as stopped.
ALTER TABLE investigation
    ADD COLUMN stopped_by TEXT NOT NULL DEFAULT '' CHECK (length(stopped_by) <= 64);
ALTER TABLE investigation
    ADD CONSTRAINT investigation_stop_is_a_conclusion CHECK (stopped_by = '' OR status = 2);

-- run_id was a primary key nothing ever served: a run is addressed by its ordinal,
-- which is what findings cite and the operator view renders. The natural key takes
-- over, and the separate unique constraint and index it made redundant go with it.
ALTER TABLE investigation_tool_run
    DROP COLUMN run_id;
ALTER TABLE investigation_tool_run
    ADD CONSTRAINT investigation_tool_run_pkey PRIMARY KEY (investigation_id, ordinal);
ALTER TABLE investigation_tool_run
    DROP CONSTRAINT investigation_tool_run_ordinal_is_unique;
DROP INDEX IF EXISTS investigation_tool_run_idx;

COMMENT ON COLUMN investigation.stopped_by IS
    'The ceiling that forced the concluding turn, empty when the model concluded freely.';
