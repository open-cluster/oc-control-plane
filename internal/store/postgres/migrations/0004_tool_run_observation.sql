ALTER TABLE investigation_tool_run
    ADD COLUMN observation_text text,
    ADD COLUMN model_clipped boolean NOT NULL DEFAULT false,
    ADD COLUMN window_applied boolean,
    ADD CONSTRAINT investigation_tool_run_observation_bound
        CHECK (octet_length(observation_text) <= 65536);
