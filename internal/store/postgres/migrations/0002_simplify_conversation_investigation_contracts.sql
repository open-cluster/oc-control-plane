DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM conversation_message message
          JOIN conversation conversation
            ON conversation.org_id = message.org_id
           AND conversation.conversation_id = message.conversation_id
         WHERE message.role = 1
           AND ((conversation.surface = 1 AND message.actor_kind <> 1)
             OR (conversation.surface = 2 AND message.actor_kind <> 2))
    ) THEN
        RAISE EXCEPTION 'cannot remove Conversation Message actor classification: retained user attribution conflicts with its Conversation source'
            USING HINT = 'repair or remove the ambiguous retained Conversation data before retrying the migration';
    END IF;

    IF EXISTS (
        SELECT 1
          FROM investigation
         WHERE btrim(question) <> ''
           AND (conversation_id IS NULL OR NOT EXISTS (
               SELECT 1
                 FROM conversation_message message
                WHERE message.org_id = investigation.org_id
                  AND message.investigation_id = investigation.investigation_id
                  AND message.conversation_id = investigation.conversation_id
                  AND message.role = 1))
    ) THEN
        RAISE EXCEPTION 'cannot remove Investigation question previews: retained nonempty questions have no authoritative assigned user Messages'
            USING HINT = 'preserve the unsupported requests outside OpenCluster or attach their authoritative Messages before retrying the migration';
    END IF;
END $$;

ALTER TABLE conversation DROP CONSTRAINT conversation_surface_check;
ALTER TABLE conversation RENAME COLUMN surface TO source;
ALTER TABLE conversation ALTER COLUMN source DROP DEFAULT;
ALTER TABLE conversation ALTER COLUMN source TYPE text
    USING CASE source
        WHEN 1 THEN 'web'
        WHEN 2 THEN 'slack'
        ELSE NULL
    END;
ALTER TABLE conversation ALTER COLUMN source SET DEFAULT 'web';
ALTER TABLE conversation ADD CONSTRAINT conversation_source_check
    CHECK (source IN ('web', 'slack'));

ALTER TABLE conversation_message DROP CONSTRAINT conversation_message_actor_kind_check;
ALTER TABLE conversation_message DROP COLUMN actor_kind;

ALTER TABLE investigation DROP COLUMN question;

UPDATE investigation
   SET conclusion = jsonb_set(
       conclusion,
       '{hypotheses}',
       COALESCE((
           SELECT jsonb_agg(
               CASE WHEN hypothesis.value ->> 'status' = 'exploring'
                    THEN jsonb_set(hypothesis.value - 'id', '{status}', '"unresolved"'::jsonb)
                    ELSE hypothesis.value - 'id'
               END
               ORDER BY hypothesis.ordinality)
             FROM jsonb_array_elements(
                 CASE WHEN jsonb_typeof(conclusion -> 'hypotheses') = 'array'
                      THEN conclusion -> 'hypotheses'
                      ELSE '[]'::jsonb
                 END
             ) WITH ORDINALITY AS hypothesis(value, ordinality)
       ), '[]'::jsonb)
   )
 WHERE conclusion ? 'hypotheses';

UPDATE investigation_event
   SET payload = CASE type
       WHEN 1 THEN '{}'::jsonb
       WHEN 2 THEN jsonb_build_object(
           'text', left(COALESCE(NULLIF(btrim(payload ->> 'text'), ''), 'Progress unavailable'), 512))
       WHEN 3 THEN jsonb_build_object(
           'ordinal', GREATEST(COALESCE((payload ->> 'ordinal')::integer, 1), 1),
           'tool', left(COALESCE(NULLIF(btrim(payload ->> 'tool'), ''), 'unknown tool'), 512),
           'integrationId', COALESCE(NULLIF(btrim(payload ->> 'integrationId'), ''), '00000000-0000-0000-0000-000000000000'),
           'integration', left(COALESCE(NULLIF(btrim(payload ->> 'integration'), ''), 'Unknown Integration'), 512),
           'purpose', left(COALESCE(NULLIF(btrim(payload ->> 'purpose'), ''), 'Read evidence'), 512))
       WHEN 4 THEN jsonb_build_object(
           'ordinal', GREATEST(COALESCE((payload ->> 'ordinal')::integer, 1), 1),
           'outcome', CASE WHEN payload ->> 'outcome' = 'succeeded' THEN 'succeeded' ELSE 'failed' END,
           'durationMs', GREATEST(COALESCE((payload ->> 'durationMs')::bigint, 0), 0),
           'summary', left(COALESCE(
               NULLIF(btrim(payload ->> 'summary'), ''),
               CASE WHEN payload ->> 'outcome' = 'succeeded'
                    THEN 'Tool completed successfully' ELSE 'Tool failed' END), 512),
           'truncated', COALESCE((payload ->> 'truncated')::boolean, false))
       WHEN 6 THEN jsonb_build_object(
           'status', payload ->> 'status',
           'summary', left(COALESCE(payload ->> 'summary', ''), 4096))
       WHEN 7 THEN jsonb_build_object(
           'reason', left(COALESCE(NULLIF(btrim(payload ->> 'reason'), ''), 'Investigation failed'), 1024))
       WHEN 9 THEN jsonb_build_object(
           'message', left(COALESCE(NULLIF(btrim(payload ->> 'message'), ''), 'Investigation cancelled by an operator'), 512))
       ELSE payload
   END
 WHERE type IN (1, 2, 3, 4, 6, 7, 9);

ALTER TABLE investigation_event DROP CONSTRAINT investigation_event_type_check;
ALTER TABLE investigation_event ADD CONSTRAINT investigation_event_type_check
    CHECK (type IN (1, 2, 3, 4, 6, 7, 9)) NOT VALID;
