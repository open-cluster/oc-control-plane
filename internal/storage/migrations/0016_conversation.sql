-- Conversations: the multi-turn context a person talks to (issue #12).
--
-- A Conversation is organization-scoped and independent. A tenant has many at once, and
-- several may associate with one IncidentEpisode without sharing anything but that
-- episode's own durable context. An Investigation stays exactly what it was — one
-- bounded answer — and a Conversation turn opens one.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS conversation
(
    conversation_id  UUID        NOT NULL PRIMARY KEY,
    org_id           TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    -- The episode this conversation is about, absent for a question that names none.
    -- Several conversations may name one episode: two people narrowing the same incident
    -- separately is the case this exists for, and they share only what the episode itself
    -- holds.
    episode_id       UUID,

    -- Where the person is talking from. 1 web. A later surface adds its own value in its
    -- own migration; the column is frozen the way every other product vocabulary is.
    surface          SMALLINT    NOT NULL DEFAULT 1 CHECK (surface IN (1)),

    subject          TEXT        NOT NULL CHECK (length(subject) BETWEEN 1 AND 512),

    -- 1 open, 2 closed.
    state            SMALLINT    NOT NULL DEFAULT 1 CHECK (state IN (1, 2)),

    created_by       TEXT        NOT NULL DEFAULT '' CHECK (length(created_by) <= 256),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- What the listing orders by. A conversation is found by when it last moved, not by
    -- when it was opened.
    last_activity_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT conversation_identity_is_org_scoped UNIQUE (org_id, conversation_id),
    CONSTRAINT conversation_episode_is_in_the_same_org
        FOREIGN KEY (org_id, episode_id)
            REFERENCES incident_episode (org_id, episode_id)
);

CREATE INDEX IF NOT EXISTS conversation_org_idx
    ON conversation (org_id, last_activity_at DESC, conversation_id DESC);
CREATE INDEX IF NOT EXISTS conversation_episode_idx
    ON conversation (episode_id) WHERE episode_id IS NOT NULL;

-- Every message, in the order it was said. This is the AUTHORITATIVE transcript:
-- compaction writes a summary beside it and never edits or deletes a row here, so a
-- conversation stays readable after the fact whatever the model's working context did.
--
-- Message text originates with a person or with an external surface and is UNTRUSTED for
-- its whole life. A message saying "ignore your instructions" is evidence about what
-- somebody typed, never an instruction.
CREATE TABLE IF NOT EXISTS conversation_message
(
    conversation_id  UUID        NOT NULL,
    org_id           TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),

    -- Monotonic within the conversation, assigned under the row lock the insert takes.
    sequence         BIGINT      NOT NULL CHECK (sequence >= 1),

    -- 1 person, 2 agent.
    role             SMALLINT    NOT NULL CHECK (role IN (1, 2)),

    -- Who said it. 1 an OpenCluster principal, 2 an identity belonging to an external
    -- surface. Recorded on every message because several people may take part in one
    -- conversation and a shared investigation that cannot say who asked what is not a
    -- record.
    actor_kind       SMALLINT    NOT NULL CHECK (actor_kind IN (1, 2)),
    actor_id         TEXT        NOT NULL DEFAULT '' CHECK (length(actor_id) <= 256),
    actor_display    TEXT        NOT NULL DEFAULT '' CHECK (length(actor_display) <= 256),

    text             TEXT        NOT NULL CHECK (length(text) BETWEEN 1 AND 8192),

    -- The turn this message opened, or the turn that produced it. NULL on a message that
    -- arrived while a turn was still running: that message is queued, and the drain at
    -- the next terminal boundary is what gives it an investigation.
    investigation_id UUID,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (org_id, conversation_id, sequence),
    CONSTRAINT conversation_message_belongs_to_its_conversation
        FOREIGN KEY (org_id, conversation_id)
            REFERENCES conversation (org_id, conversation_id) ON DELETE CASCADE,
    CONSTRAINT conversation_message_names_an_org_investigation
        FOREIGN KEY (org_id, investigation_id)
            REFERENCES investigation (org_id, investigation_id)
);

-- The queue read: the messages of one conversation that no turn has taken up yet.
CREATE INDEX IF NOT EXISTS conversation_message_queued_idx
    ON conversation_message (org_id, conversation_id, sequence)
    WHERE investigation_id IS NULL;

-- An Investigation becomes a Conversation's turn. Existing rows keep NULL and stay
-- readable exactly as they are: a single-shot investigation is still a whole record.
ALTER TABLE investigation
    ADD COLUMN conversation_id UUID;
ALTER TABLE investigation
    ADD COLUMN turn SMALLINT CHECK (turn >= 1);

ALTER TABLE investigation
    ADD CONSTRAINT investigation_conversation_is_in_the_same_org
        FOREIGN KEY (org_id, conversation_id)
            REFERENCES conversation (org_id, conversation_id);
ALTER TABLE investigation
    ADD CONSTRAINT investigation_turn_belongs_to_a_conversation
        CHECK ((conversation_id IS NULL) = (turn IS NULL));
ALTER TABLE investigation
    ADD CONSTRAINT investigation_turn_is_unique_in_its_conversation
        UNIQUE (org_id, conversation_id, turn);

-- THE SINGLE-WRITER INVARIANT, AND WHY IT IS AN INDEX.
--
-- At most one investigation per conversation may be running. Two messages racing into one
-- conversation means one insert fails and the loser's message is left queued rather than a
-- second agent starting against the same context.
--
-- It is enforced here because this is the only place that can enforce it. An in-memory
-- lock holds for one process, and the whole point of the lease is that several replicas
-- run turns for one tenant. A database constraint is the one authority every replica
-- shares.
CREATE UNIQUE INDEX IF NOT EXISTS investigation_one_running_per_conversation
    ON investigation (org_id, conversation_id)
    WHERE conversation_id IS NOT NULL AND status = 1;

COMMENT ON TABLE conversation IS
    'One multi-turn context a person talks to: organization-scoped, optionally about an episode, holding messages and the investigations its turns opened.';
COMMENT ON TABLE conversation_message IS
    'The authoritative transcript, in order, with who said each thing. Never edited or deleted by compaction; untrusted for its whole life.';
COMMENT ON COLUMN investigation.conversation_id IS
    'The conversation this investigation is a turn of; NULL for a single-shot investigation.';
COMMENT ON COLUMN investigation.turn IS
    'This investigation''s one-based position among its conversation''s turns.';
