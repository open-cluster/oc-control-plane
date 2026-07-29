-- The record of a relay identity being contested, and of someone saying it has been dealt with.
--
-- Withdrawing a mark destroys the current state, because that is what withdrawing means. So the
-- current state cannot also be the record. On a platform whose subject is evidence, deleting
-- the evidence of a security finding at the moment someone acknowledges it is the wrong shape:
-- what an operator most needs on the second occurrence is that there was a first.
--
-- Append-only. Rows here are never updated and never deleted, which is what makes the trail
-- worth reading: a record that can be edited answers a different question from the one an
-- operator is asking when they open it.
--
-- The columns on relay_registration remain the current answer, written in the same transaction
-- as the event that changes it. They are a cached reading of this table rather than a second
-- opinion, and committing them together is what stops the two from ever disagreeing.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS relay_session_conflict_event
(
    event_id        BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    organization    TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    registration_id UUID        NOT NULL,

    -- 1 detected, 2 withdrawn.
    kind            SMALLINT    NOT NULL CHECK (kind IN (1, 2)),

    -- Distinct hosts seen taking the session, as observed at the moment of a detection. More
    -- than one is the credential-theft signature. A withdrawal observes nothing and carries 0.
    distinct_hosts  INTEGER     NOT NULL DEFAULT 0 CHECK (distinct_hosts >= 0),

    -- Where a withdrawal came from. The operator surface is behind one shared token, so an
    -- address is the whole of the attribution available — worth knowing when reading this back,
    -- and worth recording rather than leaving the act anonymous. A detection has no actor.
    withdrawn_from  TEXT,

    at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT relay_session_conflict_event_actor_belongs_to_a_withdrawal CHECK (
        kind = 2 OR withdrawn_from IS NULL
        )
);

-- The trail for one relay, newest first. Without it, reading a registration's history is a scan
-- of every organization's.
CREATE INDEX IF NOT EXISTS relay_session_conflict_event_registration_idx
    ON relay_session_conflict_event (organization, registration_id, event_id DESC);

COMMENT ON TABLE relay_session_conflict_event IS
    'Append-only trail of contested relay identities and of the withdrawals that acknowledged them.';
