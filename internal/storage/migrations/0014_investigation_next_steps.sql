-- The conclusion's recommended next steps (issue #5 §7).
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- next_steps is the conclusion's ordered list of recommended operator actions:
-- ["roll back the pool change", ...]. Bounded where the answer is decoded and again by
-- the runner, because JSONB cannot say it. Empty for a failed investigation, for one
-- concluded before this column existed, and for a conclusion that carried none.
ALTER TABLE investigation
    ADD COLUMN next_steps JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN investigation.next_steps IS
    'The conclusion''s recommended operator actions, in order; empty when it carried none.';
