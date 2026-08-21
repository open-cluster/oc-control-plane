-- The investigation event stream (issue #12).
--
-- A durable, ordered record of SEMANTIC events about one running investigation, so a
-- reader that reconnects or lands on another replica resumes exactly where it stopped.
-- Persistence is what makes reconnect and replica change the same code path; without it
-- a stream is only correct for a reader that never blinks.
--
-- NO MODEL CHAIN OF THOUGHT EVER ENTERS THIS TABLE. Progress text is composed by the
-- platform from facts it already holds — which tool is about to run against which
-- integration, what that tool's own summary said, which ceiling fired. No prompt asks the
-- model to narrate, so there is nothing private to sanitize and no new prompt surface to
-- review. Payloads carry no credential, no header and no raw tool result.
--
-- The table is cascade-deleted with its investigation, so the investigation record stays
-- the single retention unit and no second reaper is introduced.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS investigation_event
(
    investigation_id UUID        NOT NULL,
    org_id           TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    -- Monotonic within the investigation, assigned by the lease holder. Because the lease
    -- is exclusive there is exactly one writer; this primary key is the backstop against
    -- a double-claim writing the same position twice.
    sequence         BIGINT      NOT NULL CHECK (sequence >= 1),

    at               TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- 1 started, 2 progress, 3 tool_started, 4 tool_completed, 5 answer_delta,
    -- 6 concluded, 7 failed, 8 compacted. Eight and no more in this release; the wire
    -- envelope carries the schema version, because this table's shape IS version 1.
    type             SMALLINT    NOT NULL CHECK (type BETWEEN 1 AND 8),

    payload          JSONB       NOT NULL DEFAULT '{}'::jsonb,

    PRIMARY KEY (investigation_id, sequence),
    CONSTRAINT investigation_event_belongs_to_its_investigation
        FOREIGN KEY (org_id, investigation_id)
            REFERENCES investigation (org_id, investigation_id) ON DELETE CASCADE
);

COMMENT ON TABLE investigation_event IS
    'The ordered semantic events of one investigation, for replay and live following. Platform-composed facts only; never a model''s reasoning, a credential or a raw tool payload.';
