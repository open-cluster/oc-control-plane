ALTER TABLE investigation
    DROP CONSTRAINT investigation_status_check,
    ADD CONSTRAINT investigation_status_check CHECK (status IN (1, 2, 3, 4)),
    ADD COLUMN cancel_requested_at TIMESTAMPTZ,
    ADD COLUMN cancelled_by TEXT NOT NULL DEFAULT '' CHECK (length(cancelled_by) <= 256),
    ADD CONSTRAINT investigation_cancellation_is_attributed CHECK (
        (status = 4) = (cancel_requested_at IS NOT NULL AND cancelled_by <> '')
    );

ALTER TABLE investigation_event
    DROP CONSTRAINT investigation_event_type_check,
    ADD CONSTRAINT investigation_event_type_check CHECK (type BETWEEN 1 AND 9);

ALTER TABLE relay_job
    ADD COLUMN investigation_id UUID,
    ADD CONSTRAINT relay_job_investigation_belongs_to_organization
        FOREIGN KEY (org_id, investigation_id)
        REFERENCES investigation (org_id, investigation_id) ON DELETE CASCADE;

CREATE INDEX relay_job_active_investigation_idx
    ON relay_job (org_id, investigation_id)
    WHERE investigation_id IS NOT NULL AND status IN (0, 1);
