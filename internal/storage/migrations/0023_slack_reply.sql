-- Answering back in the thread, and how a retry avoids saying anything twice.
--
-- Delivery state is its OWN record, per investigation, and this is the property that makes it
-- safe: the visible message is one stream identified by its timestamp, and the cursor only
-- moves forward. A retry therefore appends what was missed rather than reposting what was
-- already seen, and a worker restarted mid-stream resumes instead of starting again.
--
-- A DELIVERY FAILURE IS RECORDED AGAINST THE DELIVERY AND NEVER AGAINST THE INVESTIGATION.
-- The two are related concerns, not the same one: the investigation concludes or fails on its
-- own terms, and its record and event stream stay complete and readable in the console
-- whatever Slack did. A Slack outage must not be able to corrupt or cancel the work behind it.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS slack_reply
(
    investigation_id UUID        NOT NULL PRIMARY KEY,
    org_id           TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    integration_id   UUID        NOT NULL,

    -- The thread's conversation. Carried so the worker can name the people in it without a
    -- second lookup, and because a reply belongs to a conversation as much as to a thread.
    conversation_id  UUID        NOT NULL,

    channel_id       TEXT        NOT NULL CHECK (length(channel_id) BETWEEN 1 AND 64),
    thread_ts        TEXT        NOT NULL CHECK (length(thread_ts) BETWEEN 1 AND 64),

    -- The visible message this turn is being written into, once it exists. Empty until the
    -- first successful call, and it is the identity every later append and every edit names —
    -- which is what makes a retry append rather than repost.
    stream_ts        TEXT        NOT NULL DEFAULT '' CHECK (length(stream_ts) <= 64),

    -- Which of the two shapes the visible message is: a native stream that is APPENDED to, or
    -- a placeholder that is REPLACED. A resumed delivery must not guess — appending to a
    -- placeholder would erase the answer, and replacing a stream would repeat it — so it is
    -- recorded at the same moment the message's identity is.
    native        BOOLEAN     NOT NULL DEFAULT FALSE,

    -- 1 pending, 2 delivering, 3 delivered, 4 failed. Failed is TERMINAL for the delivery and
    -- says nothing about the investigation, which has its own status.
    status           SMALLINT    NOT NULL DEFAULT 1 CHECK (status IN (1, 2, 3, 4)),

    -- The cursor. Every event at or below this has been rendered into the thread, so the
    -- delivery resumes from here and cannot repeat itself. It only ever moves forward.
    last_sequence    BIGINT      NOT NULL DEFAULT 0 CHECK (last_sequence >= 0),

    -- Retry state: bounded exponential backoff with jitter, honouring the interval Slack asks
    -- for when it asks for one.
    attempts         INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Why the last attempt failed, in this build's own words. Never the vendor's message
    -- verbatim: an operator reads it, and text a far end chose is text somebody else chose.
    note             TEXT        NOT NULL DEFAULT '' CHECK (length(note) <= 512),

    -- The lease. A second worker cannot take a delivery another is holding, which is what
    -- stops two workers writing into one visible message.
    leased_until     TIMESTAMPTZ,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT slack_reply_investigation_is_in_the_same_org
        FOREIGN KEY (org_id, investigation_id)
            REFERENCES investigation (org_id, investigation_id) ON DELETE CASCADE,

    CONSTRAINT slack_reply_integration_is_in_the_same_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id) ON DELETE CASCADE
);

-- What the worker asks for: deliveries that are due, oldest first. Partial, because a
-- delivered or permanently failed one is never claimed again and has no business in the index
-- a hot loop scans.
CREATE INDEX IF NOT EXISTS slack_reply_due
    ON slack_reply (next_attempt_at, investigation_id)
    WHERE status IN (1, 2);

COMMENT ON TABLE slack_reply IS
    'Outbound delivery of one investigation into one Slack thread. Its cursor only moves forward, so a retry appends what was missed rather than reposting what was seen. A failure here is never a failure of the investigation.';
