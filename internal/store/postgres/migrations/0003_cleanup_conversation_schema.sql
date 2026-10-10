DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM postmortem
         WHERE btrim(reviewed_by) <> ''
           AND btrim(reviewed_by) !~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    ) THEN
        RAISE EXCEPTION 'cannot convert Postmortem reviewer attribution: a retained reviewer is not a User identity'
            USING HINT = 'repair or clear invalid postmortem.reviewed_by values before retrying the migration';
    END IF;

    IF EXISTS (
        SELECT 1
          FROM postmortem
         WHERE btrim(reviewed_by) <> ''
           AND NOT EXISTS (
               SELECT 1
                 FROM app_user
                WHERE app_user.user_id::text = lower(btrim(postmortem.reviewed_by)))
    ) THEN
        RAISE EXCEPTION 'cannot convert Postmortem reviewer attribution: a retained reviewer does not reference a User'
            USING HINT = 'restore the referenced User or clear orphan postmortem.reviewed_by values before retrying the migration';
    END IF;
END $$;

ALTER TABLE conversation_message RENAME COLUMN actor_id TO author_id;
ALTER TABLE conversation_message RENAME COLUMN actor_display TO author_name;
ALTER TABLE conversation_message DROP COLUMN provider_channel_id;

ALTER TABLE conversation DROP CONSTRAINT conversation_state_check;
ALTER TABLE conversation DROP COLUMN state;
ALTER TABLE conversation ALTER COLUMN source DROP DEFAULT;

DROP INDEX organization_membership_user_idx;
ALTER TABLE organization DROP COLUMN created_by;

ALTER TABLE investigation DROP CONSTRAINT investigation_cancellation_is_attributed;
ALTER TABLE investigation DROP COLUMN created_by;
ALTER TABLE investigation DROP COLUMN cancelled_by;
ALTER TABLE investigation ADD CONSTRAINT investigation_cancellation_is_requested
    CHECK ((status = 4) = (cancel_requested_at IS NOT NULL));

UPDATE postmortem
   SET document = document - 'reviewedBy'
 WHERE document ? 'reviewedBy';
ALTER TABLE postmortem ALTER COLUMN reviewed_by DROP DEFAULT;
ALTER TABLE postmortem ALTER COLUMN reviewed_by DROP NOT NULL;
ALTER TABLE postmortem ALTER COLUMN reviewed_by TYPE uuid
    USING NULLIF(btrim(reviewed_by), '')::uuid;
ALTER TABLE postmortem RENAME COLUMN reviewed_by TO reviewed_by_user_id;
ALTER TABLE postmortem ADD CONSTRAINT postmortem_reviewed_by_user_id_fkey
    FOREIGN KEY (reviewed_by_user_id) REFERENCES app_user(user_id);
