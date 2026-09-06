ALTER TABLE conversation_message ADD COLUMN window_from timestamptz;
ALTER TABLE conversation_message ADD COLUMN window_until timestamptz;

UPDATE conversation_message message
   SET window_from = turn.window_from, window_until = turn.window_until
  FROM investigation turn
 WHERE message.org_id = turn.org_id AND message.investigation_id = turn.investigation_id;

WITH queued AS (
    SELECT DISTINCT ON (org_id, conversation_id) org_id, conversation_id, created_at AS accepted_at
      FROM conversation_message
     WHERE role = 1 AND investigation_id IS NULL
     ORDER BY org_id, conversation_id, sequence
), defaults AS (
    SELECT message.org_id, message.conversation_id, message.sequence,
           CASE WHEN incident.incident_id IS NULL
                THEN COALESCE(queued.accepted_at, message.created_at) - interval '24 hours'
                ELSE incident.first_seen_at - interval '2 hours' END AS window_from,
           CASE WHEN incident.status = 2
                THEN LEAST(incident.last_seen_at, COALESCE(queued.accepted_at, message.created_at))
                ELSE COALESCE(queued.accepted_at, message.created_at) END AS window_until
      FROM conversation_message message
      JOIN conversation ON conversation.org_id = message.org_id AND conversation.conversation_id = message.conversation_id
      LEFT JOIN queued ON queued.org_id = message.org_id AND queued.conversation_id = message.conversation_id
      LEFT JOIN incident ON incident.org_id = conversation.org_id AND incident.incident_id = conversation.incident_id
     WHERE message.window_from IS NULL
)
UPDATE conversation_message message
   SET window_from = defaults.window_from, window_until = defaults.window_until
  FROM defaults
 WHERE message.org_id = defaults.org_id AND message.conversation_id = defaults.conversation_id
   AND message.sequence = defaults.sequence;

ALTER TABLE conversation_message ALTER COLUMN window_from SET NOT NULL;
ALTER TABLE conversation_message ALTER COLUMN window_until SET NOT NULL;
ALTER TABLE conversation_message ADD CONSTRAINT conversation_message_window_is_whole
    CHECK (window_from < window_until);
