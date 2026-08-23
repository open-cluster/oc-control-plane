-- A Slack thread IS a Conversation, bound deterministically.
--
-- The binding is a lookup and nothing else: an integration, a channel and a thread resolve to
-- exactly one Conversation, and the triple is unique. There is NO inference here — no
-- similarity matching, no "guess which incident this thread is about". A Conversation is
-- associated with an IncidentEpisode only when it was opened from one, and otherwise it simply
-- has none, which is the honest state rather than a guess that reads like knowledge.
--
-- A channel mention outside a thread is keyed on the mention's OWN timestamp, which is the
-- thread OpenCluster then replies in. An agent DM is keyed on its thread the same way. So
-- "start a thread" and "continue a thread" are one code path with one key, rather than two
-- paths that can disagree about which conversation a reply belongs to.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- Surface 2 is Slack. The column's original CHECK admitted only 1, with a comment saying a
-- later surface adds its own value in its own migration. This is that migration.
ALTER TABLE conversation
    DROP CONSTRAINT IF EXISTS conversation_surface_check;

ALTER TABLE conversation
    ADD CONSTRAINT conversation_surface_check CHECK (surface IN (1, 2));

CREATE TABLE IF NOT EXISTS slack_conversation
(
    conversation_id UUID        NOT NULL,
    org_id          TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    integration_id  UUID        NOT NULL,

    channel_id      TEXT        NOT NULL CHECK (length(channel_id) BETWEEN 1 AND 64),

    -- The thread's own timestamp, which is Slack's identity for a thread. For a mention that
    -- started no thread it is that message's timestamp — the thread OpenCluster's reply
    -- creates.
    thread_ts       TEXT        NOT NULL CHECK (length(thread_ts) BETWEEN 1 AND 64),

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (conversation_id),

    CONSTRAINT slack_conversation_is_in_the_same_org
        FOREIGN KEY (org_id, conversation_id)
            REFERENCES conversation (org_id, conversation_id) ON DELETE CASCADE,

    CONSTRAINT slack_conversation_integration_is_in_the_same_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id) ON DELETE CASCADE,

    -- ONE THREAD IS ONE CONVERSATION. Scoped to the integration, which is already scoped to
    -- one organization by its own key — so this is per-tenant by construction rather than by
    -- a column somebody has to remember to include, and two tenants cannot collide on a
    -- channel identifier because they cannot share an integration.
    CONSTRAINT slack_conversation_is_one_thread
        UNIQUE (integration_id, channel_id, thread_ts)
);

-- The lookup an inbound message makes, every time, before anything else is written.
CREATE INDEX IF NOT EXISTS slack_conversation_by_thread
    ON slack_conversation (integration_id, channel_id, thread_ts);
