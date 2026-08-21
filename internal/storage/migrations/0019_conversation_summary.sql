-- Bounded long-session context, and the direct answer (issue #12).
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- THE RUNNING SUMMARY, AND WHAT IT IS NOT.
--
-- When a conversation's estimated context crosses its budget, older turns are replaced in
-- the ASSEMBLED CONTEXT by the next version of this summary. Nothing in
-- conversation_message is edited or deleted: that table stays the authoritative record of
-- what was said, and this one is the model's working memory of it.
--
-- Superseded versions are kept. A summary that turned out to have dropped something is a
-- thing to be able to read afterwards, and the row is small.
--
-- A summary may only restate findings that already exist with the citations they already
-- carry. It never manufactures evidence, because a claim whose support cannot be followed
-- back to a run is exactly what the citation invariant exists to make unstorable.
CREATE TABLE IF NOT EXISTS conversation_summary
(
    conversation_id                 UUID        NOT NULL,
    org_id                          TEXT        NOT NULL
        CHECK (length(org_id) BETWEEN 1 AND 128),

    -- One-based, rising. The newest version is the one the next turn assembles from.
    version                         INTEGER     NOT NULL CHECK (version >= 1),

    -- The message sequence this summary accounts for. Everything after it is still
    -- carried verbatim as the recent tail.
    covers_through_message_sequence BIGINT      NOT NULL
        CHECK (covers_through_message_sequence >= 0),

    -- The structured sections are the contract: the current goal, the problem statement,
    -- durable instructions and constraints, established findings with their citation
    -- references, hypotheses still open, hypotheses ruled out and why, unresolved
    -- questions, important failed reads, decisions made, and the identifiers in play.
    summary                         JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- What the compaction was worth, so "is the summary layer working" is a number rather
    -- than an opinion.
    tokens_before                   INTEGER     NOT NULL DEFAULT 0
        CHECK (tokens_before >= 0),
    tokens_after                    INTEGER     NOT NULL DEFAULT 0
        CHECK (tokens_after >= 0),

    model                           TEXT        NOT NULL DEFAULT ''
        CHECK (length(model) <= 128),
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (org_id, conversation_id, version),
    CONSTRAINT conversation_summary_belongs_to_its_conversation
        FOREIGN KEY (org_id, conversation_id)
            REFERENCES conversation (org_id, conversation_id) ON DELETE CASCADE
);

-- The direct reply in the operator's own words. A concluding document that only knows how
-- to state causal findings forces a peacetime question — "which version is currently
-- deployed?" — into probable_cause/symptom vocabulary that does not fit it. The findings
-- still carry the claims and their citations; this summarises them for the person who
-- asked. Bounded where the conclusion is decoded and again by the runner, and empty for an
-- investigation concluded before this column existed.
ALTER TABLE investigation
    ADD COLUMN answer TEXT NOT NULL DEFAULT '' CHECK (length(answer) <= 4096);

COMMENT ON TABLE conversation_summary IS
    'The structured running summary older turns compact into. Never authoritative; conversation_message is.';
COMMENT ON COLUMN investigation.answer IS
    'The direct reply in the operator''s words; empty when the conclusion carried none.';
