ALTER TABLE public.conversation RENAME COLUMN subject TO title;
ALTER TABLE public.conversation ADD COLUMN custom_title boolean NOT NULL DEFAULT false;
ALTER TABLE public.conversation ADD COLUMN legacy_history boolean NOT NULL DEFAULT false;
ALTER TABLE public.conversation ADD COLUMN deleted_at timestamptz;

UPDATE public.conversation c SET legacy_history = true
 WHERE c.source = 'web' AND (EXISTS (
     SELECT 1 FROM public.conversation_message m
      WHERE m.org_id = c.org_id AND m.conversation_id = c.conversation_id
        AND m.role = 1 AND m.client_message_id IS NULL
 ) OR EXISTS (
     SELECT 1 FROM public.investigation i
      WHERE i.org_id = c.org_id AND i.conversation_id = c.conversation_id AND i.turn IS NOT NULL
 ));
UPDATE public.conversation SET custom_title = true WHERE source = 'web' AND title <> '';

ALTER TABLE public.investigation
    DROP CONSTRAINT investigation_turn_belongs_to_a_conversation,
    ADD CHECK (turn IS NULL OR conversation_id IS NOT NULL),
    ADD COLUMN request_key text,
    ADD COLUMN request_hash bytea,
    ADD CHECK ((request_key IS NULL) = (request_hash IS NULL)),
    ADD UNIQUE (org_id, requested_by, request_key);
