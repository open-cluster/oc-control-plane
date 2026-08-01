-- The Investigation: a durable case, its bounded rounds, and everything a round produces.
--
-- Two properties in this file are enforced by the database rather than by application discipline,
-- because both are invisible in review and both have already been got wrong once in this program.
--
-- ONE — the case version advances on ANY change within the case. A trigger on every child table
-- touches the case, and a trigger on the case advances the version on every update of it. Nothing
-- that writes here can forget to. The frozen .NET frontend contract audit recorded the
-- alternative: a version tracking only lifecycle transitions, which left a polling client blind to
-- the evidence growth it was watching for.
--
-- TWO — a claim cannot cite evidence from another case, an absence cannot be recorded without a
-- completeness certificate, and an adaptive request cannot exist without the hypothesis that
-- justified it. Each is a constraint below rather than a check somewhere in Go, because each is a
-- truth-model rule and a rule enforced in one of two writers is a rule that holds until the second
-- writer is added.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

-- ---------------------------------------------------------------------------------------
-- Every dispatched job can be named by the request that proposed it, within one tenant
-- ---------------------------------------------------------------------------------------

-- The org-scoped composite key investigation_request points at, so a request cannot name another
-- organization's job however it was assembled. Same shape as the keys environment, connection and
-- relay_registration already carry.
ALTER TABLE relay_job
    ADD CONSTRAINT relay_job_identity_is_org_scoped UNIQUE (organization, job_id);

-- ---------------------------------------------------------------------------------------
-- The case
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS investigation
(
    investigation_id  UUID        NOT NULL PRIMARY KEY,
    organization      TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),

    -- DERIVED from the Connection when the case was opened, and persisted rather than joined: a
    -- Connection that later moves must not rewrite the scope a finished case was gathered under.
    -- Nothing accepts an Environment from a caller.
    environment_id    UUID        NOT NULL,
    connection_id     UUID        NOT NULL,

    -- The IncidentEpisode this case belongs to. NULL for a manually started case, which uses an
    -- implicit episode. The column exists from the first migration because the one-to-many between
    -- case and round has to be structurally right before rows exist (ADR-013), not because
    -- anything writes it yet.
    episode_key       TEXT CHECK (episode_key IS NULL OR length(episode_key) BETWEEN 1 AND 512),

    -- Scope resolution is deliberately narrow and hard-coded in this slice: one Kubernetes
    -- namespace, one workload kind, one workload name, through one Connection. See
    -- docs/architecture/hard-coded-in-the-first-investigation.md. This code is expected to be
    -- discarded.
    namespace         TEXT        NOT NULL CHECK (length(namespace) BETWEEN 1 AND 63),
    -- 1 deployment, 2 statefulset, 3 daemonset. Matches the protocol's WorkloadKind so the two
    -- cannot drift apart silently.
    workload_kind     SMALLINT    NOT NULL CHECK (workload_kind IN (1, 2, 3)),
    workload_name     TEXT        NOT NULL CHECK (length(workload_name) BETWEEN 1 AND 253),

    window_start      TIMESTAMPTZ NOT NULL,
    window_end        TIMESTAMPTZ NOT NULL,

    -- 1 manual, 2 signal. Only the first is reachable in this slice; the second is declared so the
    -- column means something when intake starts consuming Signals.
    trigger_kind      SMALLINT    NOT NULL CHECK (trigger_kind IN (1, 2)),
    -- Whoever asked. With one shared operator token this says where a request came from and not
    -- who made it, which is a limit of ADR-006 being undecided rather than of this record.
    requested_by      TEXT        NOT NULL CHECK (length(requested_by) <= 256),
    triggered_at      TIMESTAMPTZ NOT NULL,

    -- Severity as its trigger source stated it, with the attribution intact. NOT a control and
    -- never composed: two conflicting translations have no most-restrictive answer (ADR-015). It
    -- is a secondary sort signal on the list read and nothing else, and it is NULL for a manual
    -- start because nobody stated one.
    severity          TEXT CHECK (severity IS NULL OR length(severity) BETWEEN 1 AND 64),
    severity_source   TEXT CHECK (severity_source IS NULL OR length(severity_source) BETWEEN 1 AND 128),

    -- 1 pending, 2 briefing, 3 reasoning, 4 gathering, 5 concluded, 6 abstained, 7 cancelled,
    -- 8 failed. A projection across the case's rounds rather than a second state machine beside
    -- them.
    lifecycle         SMALLINT    NOT NULL CHECK (lifecycle BETWEEN 1 AND 8),

    -- The whole-case version. Monotonic, and advanced by the trigger below on EVERY update of this
    -- row — including the ones the child triggers make when evidence arrives, a gap is recorded or
    -- a hypothesis moves. It is what a client polls on and what an export pins.
    case_version      BIGINT      NOT NULL DEFAULT 1 CHECK (case_version >= 1),

    current_round     INTEGER     NOT NULL DEFAULT 0 CHECK (current_round >= 0),

    -- Precomputed section counts, maintained by the same triggers that advance the version. They
    -- exist so the summary and the list are each ONE row read: the frozen .NET frontend audit
    -- recorded cursor lists with no totals and the incident list fanning out one request per row,
    -- and this is the column-shaped answer to it.
    round_count       INTEGER     NOT NULL DEFAULT 0 CHECK (round_count >= 0),
    evidence_count    INTEGER     NOT NULL DEFAULT 0 CHECK (evidence_count >= 0),
    -- Evidence carrying a defensible source time. An item without one is listed BESIDE the
    -- timeline rather than placed on it, so the two counts are genuinely different numbers.
    timeline_count    INTEGER     NOT NULL DEFAULT 0 CHECK (timeline_count >= 0),
    gap_count         INTEGER     NOT NULL DEFAULT 0 CHECK (gap_count >= 0),
    hypothesis_count  INTEGER     NOT NULL DEFAULT 0 CHECK (hypothesis_count >= 0),
    request_count     INTEGER     NOT NULL DEFAULT 0 CHECK (request_count >= 0),
    outcome_count     INTEGER     NOT NULL DEFAULT 0 CHECK (outcome_count >= 0),

    -- What the case has consumed, summed across its rounds. An operator cannot enable a feature
    -- whose unit cost is unknown. Money is an integer: a float would make two runs that cost the
    -- same report different totals.
    spend_tokens      BIGINT      NOT NULL DEFAULT 0 CHECK (spend_tokens >= 0),
    spend_micro_cents BIGINT      NOT NULL DEFAULT 0 CHECK (spend_micro_cents >= 0),
    spend_millis      BIGINT      NOT NULL DEFAULT 0 CHECK (spend_millis >= 0),

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminal_at       TIMESTAMPTZ,

    -- The composite key every child below points at. Without it a child naming only the identifier
    -- could belong to another organization's case, and the boundary would be a property of
    -- whichever query was written correctly.
    CONSTRAINT investigation_identity_is_org_scoped UNIQUE (organization, investigation_id),

    CONSTRAINT investigation_connection_is_in_the_same_organization
        FOREIGN KEY (organization, connection_id)
            REFERENCES connection (organization, connection_id),
    CONSTRAINT investigation_environment_is_in_the_same_organization
        FOREIGN KEY (organization, environment_id)
            REFERENCES environment (organization, environment_id),

    CONSTRAINT investigation_window_is_bounded CHECK (window_end > window_start),
    -- Every terminal state is stamped. Lifecycle 5 and above are the four terminal ones.
    CONSTRAINT investigation_terminal_is_stamped CHECK ((lifecycle >= 5) = (terminal_at IS NOT NULL)),
    -- Severity is worth nothing without knowing who said it.
    CONSTRAINT investigation_severity_is_attributed CHECK (
        (severity IS NULL) = (severity_source IS NULL)
        )
);

-- The list read's ordering: lifecycle state, then recency. Both are in the index so the ordering
-- is served rather than sorted.
CREATE INDEX IF NOT EXISTS investigation_list_idx
    ON investigation (organization, lifecycle, updated_at DESC, investigation_id DESC);

CREATE INDEX IF NOT EXISTS investigation_environment_idx
    ON investigation (organization, environment_id, created_at DESC);

-- ---------------------------------------------------------------------------------------
-- The case version, advanced by the database
-- ---------------------------------------------------------------------------------------

-- Every update of a case advances its version. Placed BEFORE UPDATE on the row itself rather than
-- written into each statement, so a lifecycle transition, a counter maintained by a child trigger
-- and a write nobody has thought of yet all advance it identically.
CREATE OR REPLACE FUNCTION investigation_version_advances() RETURNS TRIGGER
    LANGUAGE plpgsql AS
$$
BEGIN
    NEW.case_version := OLD.case_version + 1;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER investigation_version_always_advances
    BEFORE UPDATE
    ON investigation
    FOR EACH ROW
EXECUTE FUNCTION investigation_version_advances();

-- Touches the case a child row belongs to, and maintains that child's section counter.
--
-- The counter column is a trigger argument rather than a function per table, so adding a section
-- is one CREATE TRIGGER. An empty argument means the child has no counter — a stance, a claim, a
-- citation — and the touch alone is what matters.
--
-- The early return is the one exception, and it is narrow on purpose: an update that moved ONLY a
-- round's lease is execution bookkeeping and not a change within the case, so it must not wake
-- every polling client. It is expressed as a difference over the whole row minus the named lease
-- columns, so a column added later advances the version by default rather than silently stopping.
CREATE OR REPLACE FUNCTION investigation_case_changed() RETURNS TRIGGER
    LANGUAGE plpgsql AS
$$
DECLARE
    counter  TEXT := TG_ARGV[0];
    subject  UUID;
    delta    INTEGER := 0;
    timeline INTEGER := 0;
BEGIN
    IF TG_OP = 'UPDATE'
        AND to_jsonb(NEW) - 'lease_session' - 'lease_expires_at' - 'heartbeat_at'
        = to_jsonb(OLD) - 'lease_session' - 'lease_expires_at' - 'heartbeat_at' THEN
        RETURN NULL;
    END IF;

    IF TG_OP = 'DELETE' THEN
        subject := OLD.investigation_id;
        delta := -1;
    ELSE
        subject := NEW.investigation_id;
        IF TG_OP = 'INSERT' THEN
            delta := 1;
        END IF;
    END IF;

    -- Evidence maintains a second counter, because an item with no defensible source time is not
    -- on the timeline and a tab labelled with the wrong number is a tab nobody trusts.
    IF TG_TABLE_NAME = 'evidence_item' THEN
        timeline := CASE
                        WHEN TG_OP = 'INSERT' AND NEW.source_observed_at IS NOT NULL THEN 1
                        WHEN TG_OP = 'DELETE' AND OLD.source_observed_at IS NOT NULL THEN -1
                        ELSE 0
            END;
    END IF;

    IF counter = '' THEN
        UPDATE investigation SET updated_at = now() WHERE investigation_id = subject;
    ELSE
        EXECUTE format(
                'UPDATE investigation
                    SET %I = %I + $1, timeline_count = timeline_count + $2
                  WHERE investigation_id = $3', counter, counter)
            USING delta, timeline, subject;
    END IF;
    RETURN NULL;
END;
$$;

-- ---------------------------------------------------------------------------------------
-- Rounds
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS investigation_round
(
    round_id             UUID        NOT NULL PRIMARY KEY,
    organization         TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id     UUID        NOT NULL,

    -- Which round of this case it is, from one. It is what an export names when it says which
    -- rounds it includes, so a document read next quarter carries its own scope.
    ordinal              INTEGER     NOT NULL CHECK (ordinal >= 1),

    -- The case pack. Each of these is pinned per ROUND rather than per case, because a case
    -- spanning three days has no single input set and a per-case pack could not satisfy the replay
    -- guarantee the term was defined to provide (ADR-013).
    --
    -- brief is NULL until it has been assembled, which is what makes "the brief exists before any
    -- hypothesis does" observable rather than asserted.
    brief                JSONB,
    -- The fully resolved execution-control snapshot. "Why did this round stop after two requests"
    -- stays answerable from the case alone, without access to what the configuration says today.
    controls             JSONB       NOT NULL,
    -- The resolved evidence plan: what this round MEANT to read, recoverable without reading the
    -- source at the commit that produced it (ADR-009).
    plan                 JSONB       NOT NULL,

    planner_version      TEXT        NOT NULL CHECK (length(planner_version) <= 128),
    model_version        TEXT        NOT NULL CHECK (length(model_version) <= 128),
    prompt_version       TEXT        NOT NULL CHECK (length(prompt_version) <= 128),
    schema_version       TEXT        NOT NULL CHECK (length(schema_version) <= 128),
    investigator_version TEXT        NOT NULL CHECK (length(investigator_version) <= 128),

    -- 1 concluded, 2 abstained, 3 cancelled, 4 failed. NULL while the round is running.
    outcome              SMALLINT CHECK (outcome IS NULL OR outcome BETWEEN 1 AND 4),
    -- Which bound ran out, when one did. Exhausting a budget is not a failure: it terminates the
    -- reasoning and the round concludes or abstains on what it has.
    reached_limit     SMALLINT CHECK (reached_limit IS NULL OR reached_limit BETWEEN 1 AND 5),

    spend_requests       INTEGER     NOT NULL DEFAULT 0 CHECK (spend_requests >= 0),
    spend_result_bytes   BIGINT      NOT NULL DEFAULT 0 CHECK (spend_result_bytes >= 0),
    spend_tokens         BIGINT      NOT NULL DEFAULT 0 CHECK (spend_tokens >= 0),
    spend_micro_cents    BIGINT      NOT NULL DEFAULT 0 CHECK (spend_micro_cents >= 0),

    -- Leased exactly as relay_job is: a server-clock lease fenced by session and generation. A
    -- control-plane restart returns a round to claimable, and a worker that lost its lease cannot
    -- write. Inventing a second concurrency model for the same problem is the mistake ADR-004
    -- warns about in another domain.
    lease_session        UUID,
    lease_epoch          BIGINT      NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_expires_at     TIMESTAMPTZ,
    heartbeat_at         TIMESTAMPTZ,

    started_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminal_at          TIMESTAMPTZ,

    CONSTRAINT investigation_round_identity_is_org_scoped UNIQUE (organization, round_id),
    CONSTRAINT investigation_round_ordinal_is_unique_per_case UNIQUE (investigation_id, ordinal),
    CONSTRAINT investigation_round_case_is_in_the_same_organization
        FOREIGN KEY (organization, investigation_id)
            REFERENCES investigation (organization, investigation_id),
    CONSTRAINT investigation_round_terminal_is_stamped CHECK (
        (outcome IS NOT NULL) = (terminal_at IS NOT NULL)
        ),
    -- A lease is whole or absent, in the same shape relay_job states it.
    CONSTRAINT investigation_round_lease_is_whole CHECK (
        (lease_session IS NULL) = (lease_expires_at IS NULL)
        )
);

CREATE INDEX IF NOT EXISTS investigation_round_case_idx
    ON investigation_round (investigation_id, ordinal);

-- What a worker claims: a round with no outcome whose lease is absent or has run out.
CREATE INDEX IF NOT EXISTS investigation_round_claimable_idx
    ON investigation_round (organization, started_at)
    WHERE outcome IS NULL;

CREATE TRIGGER investigation_round_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON investigation_round
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('round_count');

-- ---------------------------------------------------------------------------------------
-- Hypotheses
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS investigation_hypothesis
(
    hypothesis_id    UUID        NOT NULL PRIMARY KEY,
    organization     TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id UUID        NOT NULL,
    round_id         UUID        NOT NULL,

    -- The planner's ranking, exposed as an ordinal and never as a score. An internal ranking may
    -- order these; publishing the number would hand a reader a confidence figure the read model
    -- deliberately does not have.
    ordinal          INTEGER     NOT NULL CHECK (ordinal >= 1),
    statement        TEXT        NOT NULL CHECK (length(statement) BETWEEN 1 AND 2048),
    -- What would disprove it. Required, because an explanation nothing could disprove is a belief
    -- and the whole apparatus around it exists to tell those apart.
    falsifies        TEXT        NOT NULL CHECK (length(falsifies) BETWEEN 1 AND 2048),

    -- 1 live, 2 supported, 3 falsified, 4 set aside.
    state            SMALLINT    NOT NULL CHECK (state BETWEEN 1 AND 4),
    set_aside_reason TEXT        NOT NULL DEFAULT '' CHECK (length(set_aside_reason) <= 2048),

    proposed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT investigation_hypothesis_identity_is_org_scoped UNIQUE (organization, hypothesis_id),
    CONSTRAINT investigation_hypothesis_ordinal_is_unique_per_round UNIQUE (round_id, ordinal),
    CONSTRAINT investigation_hypothesis_case_is_in_the_same_organization
        FOREIGN KEY (organization, investigation_id)
            REFERENCES investigation (organization, investigation_id),
    CONSTRAINT investigation_hypothesis_round_is_in_the_same_organization
        FOREIGN KEY (organization, round_id)
            REFERENCES investigation_round (organization, round_id),
    -- An alternative set aside silently is an alternative a reader cannot check, so state 4
    -- carries its reason and nothing else does.
    CONSTRAINT investigation_hypothesis_set_aside_carries_its_reason CHECK (
        (state = 4) = (length(set_aside_reason) > 0)
        )
);

CREATE INDEX IF NOT EXISTS investigation_hypothesis_case_idx
    ON investigation_hypothesis (investigation_id, ordinal);

CREATE TRIGGER investigation_hypothesis_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON investigation_hypothesis
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('hypothesis_count');

-- ---------------------------------------------------------------------------------------
-- Capability requests, with the hypothesis that justified each
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS investigation_request
(
    request_id         UUID        NOT NULL PRIMARY KEY,
    organization       TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id   UUID        NOT NULL,
    round_id           UUID        NOT NULL,

    ordinal            INTEGER     NOT NULL CHECK (ordinal >= 1),
    -- Which adaptive pass proposed it. Zero is the opening plan, which precedes every hypothesis.
    pass               INTEGER     NOT NULL CHECK (pass >= 0),

    capability_id      TEXT        NOT NULL CHECK (length(capability_id) BETWEEN 1 AND 128),
    capability_version INTEGER     NOT NULL CHECK (capability_version >= 1),
    -- The encoded argument message, exactly as it would be dispatched, so a round replays from its
    -- own record with no access to live sources.
    --
    -- NULL only on a refused request, and only when the refusal is one that makes the arguments
    -- inexpressible — a capability this build does not carry has no message to encode them into.
    -- The constraint below is what keeps that narrow: anything that could be dispatched carries
    -- what it would have dispatched.
    arguments          BYTEA,

    -- The hypothesis this read would support or falsify.
    justification      UUID,
    reason             TEXT        NOT NULL DEFAULT '' CHECK (length(reason) <= 2048),

    -- 1 proposed, 2 refused, 3 dispatched, 4 answered, 5 unproductive, 6 failed.
    state              SMALLINT    NOT NULL CHECK (state BETWEEN 1 AND 6),
    refusal            SMALLINT CHECK (refusal IS NULL OR refusal BETWEEN 1 AND 8),

    job_id             UUID,
    result_bytes       BIGINT      NOT NULL DEFAULT 0 CHECK (result_bytes >= 0),

    proposed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at         TIMESTAMPTZ,

    CONSTRAINT investigation_request_identity_is_org_scoped UNIQUE (organization, request_id),
    CONSTRAINT investigation_request_ordinal_is_unique_per_round UNIQUE (round_id, ordinal),
    CONSTRAINT investigation_request_case_is_in_the_same_organization
        FOREIGN KEY (organization, investigation_id)
            REFERENCES investigation (organization, investigation_id),
    CONSTRAINT investigation_request_round_is_in_the_same_organization
        FOREIGN KEY (organization, round_id)
            REFERENCES investigation_round (organization, round_id),
    CONSTRAINT investigation_request_justification_is_in_the_same_organization
        FOREIGN KEY (organization, justification)
            REFERENCES investigation_hypothesis (organization, hypothesis_id),
    CONSTRAINT investigation_request_job_is_in_the_same_organization
        FOREIGN KEY (organization, job_id)
            REFERENCES relay_job (organization, job_id),

    -- THE CONTAINMENT MECHANISM, as a constraint rather than a convention. The planner may never
    -- derive a read from evidence text: the chain from "a log line said X" to "therefore read Y"
    -- passes through a typed hypothesis a human can read (ADR-012). Pass zero is the opening plan,
    -- which precedes every hypothesis and is the only exemption.
    CONSTRAINT investigation_request_adaptive_reads_are_justified CHECK (
        pass = 0 OR justification IS NOT NULL
        ),
    -- A refused request carries why, and nothing else does.
    CONSTRAINT investigation_request_refusal_carries_its_reason CHECK (
        (state = 2) = (refusal IS NOT NULL)
        ),
    -- A refused request was NEVER dispatched, so it names no job. This is what makes "no relay_job
    -- row became dispatchable" a property of the schema rather than of the code path that happened
    -- to run.
    CONSTRAINT investigation_request_refusal_dispatched_nothing CHECK (
        state <> 2 OR job_id IS NULL
        ),
    -- Only a refused request may lack its arguments. Anything that reached a dispatcher carries
    -- exactly what it would have sent, which is what the replay guarantee rests on.
    CONSTRAINT investigation_request_dispatchable_reads_carry_arguments CHECK (
        state = 2 OR arguments IS NOT NULL
        )
);

CREATE INDEX IF NOT EXISTS investigation_request_case_idx
    ON investigation_request (investigation_id, ordinal);

CREATE INDEX IF NOT EXISTS investigation_request_job_idx
    ON investigation_request (job_id) WHERE job_id IS NOT NULL;

CREATE TRIGGER investigation_request_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON investigation_request
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('request_count');

-- ---------------------------------------------------------------------------------------
-- Evidence
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS evidence_item
(
    evidence_id        UUID        NOT NULL PRIMARY KEY,
    organization       TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id   UUID        NOT NULL,
    round_id           UUID        NOT NULL,

    -- Stable within the case, so a paginated section keeps its order while the case is still
    -- growing. A page ordered by arrival time alone would reshuffle under a client mid-read.
    ordinal            INTEGER     NOT NULL CHECK (ordinal >= 1),

    -- Which read produced it. NULL is not reachable through the runner and is permitted so that a
    -- future non-request source of evidence does not need a migration to say so.
    request_id         UUID,

    capability_id      TEXT        NOT NULL CHECK (length(capability_id) BETWEEN 1 AND 128),
    capability_version INTEGER     NOT NULL CHECK (capability_version >= 1),
    -- The customer system this came through, and the source a reader filters by.
    connection_id      UUID        NOT NULL,

    statement          TEXT        NOT NULL CHECK (length(statement) BETWEEN 1 AND 1024),
    -- Bounded on the write path AND read back bounded. A statement long enough to hold a payload
    -- is a payload, and content large enough to exhaust a proxy on the way out is the same defect
    -- from the other direction.
    content            TEXT        NOT NULL DEFAULT '' CHECK (length(content) <= 65536),

    -- An item whose claim is that nothing was there.
    absence            BOOLEAN     NOT NULL DEFAULT FALSE,
    -- 1 centrally verified, 2 relay-attested. A Relay-attested completeness claim is recorded as
    -- such and is NEVER promoted: a promotion makes an attestation indistinguishable from a
    -- verification.
    trust              SMALLINT    NOT NULL CHECK (trust IN (1, 2)),
    -- The recorded basis on which an absence may be stated as fact.
    certificate        JSONB,

    -- The SOURCE's clock, and ours. Kept apart because collapsing them makes a delayed delivery
    -- indistinguishable from a delayed failure, and an investigator reasons about ordering. NULL
    -- source time means the item is listed beside the timeline rather than placed on it.
    source_observed_at TIMESTAMPTZ,
    received_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT evidence_item_identity_is_org_scoped UNIQUE (organization, evidence_id),
    -- The key a citation points at. It carries the case, so a claim cannot cite evidence from
    -- another one — see claim_citation below.
    CONSTRAINT evidence_item_identity_is_case_scoped
        UNIQUE (organization, investigation_id, evidence_id),
    CONSTRAINT evidence_item_ordinal_is_unique_per_case UNIQUE (investigation_id, ordinal),
    CONSTRAINT evidence_item_case_is_in_the_same_organization
        FOREIGN KEY (organization, investigation_id)
            REFERENCES investigation (organization, investigation_id),
    CONSTRAINT evidence_item_round_is_in_the_same_organization
        FOREIGN KEY (organization, round_id)
            REFERENCES investigation_round (organization, round_id),
    CONSTRAINT evidence_item_request_is_in_the_same_organization
        FOREIGN KEY (organization, request_id)
            REFERENCES investigation_request (organization, request_id),

    -- Absence is only a fact with a completeness certificate. Stated here as well as in the
    -- validator, because this is the rule that turns an RBAC misconfiguration into a certified
    -- negative when it is only enforced in one of two writers.
    CONSTRAINT evidence_item_absence_needs_a_certificate CHECK (
        NOT absence OR certificate IS NOT NULL
        )
);

CREATE INDEX IF NOT EXISTS evidence_item_case_idx
    ON evidence_item (investigation_id, ordinal);

-- The filter a case with hundreds of items is navigated by.
CREATE INDEX IF NOT EXISTS evidence_item_capability_idx
    ON evidence_item (investigation_id, capability_id, ordinal);

CREATE INDEX IF NOT EXISTS evidence_item_source_idx
    ON evidence_item (investigation_id, connection_id, ordinal);

CREATE TRIGGER evidence_item_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON evidence_item
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('evidence_count');

-- How one EvidenceItem stands towards one Hypothesis. Contradiction is retained and shown rather
-- than resolved silently in favour of the leading explanation (ADR-011).
CREATE TABLE IF NOT EXISTS evidence_stance
(
    organization     TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id UUID        NOT NULL,
    hypothesis_id    UUID        NOT NULL,
    evidence_id      UUID        NOT NULL,

    -- 1 supports, 2 contradicts, 3 neutral.
    stance           SMALLINT    NOT NULL CHECK (stance IN (1, 2, 3)),
    -- Why it stands that way. Without it a stance is an assertion, and the point of persisting
    -- reasoning artifacts is that a surprising conclusion can be examined rather than argued about.
    reason           TEXT        NOT NULL CHECK (length(reason) BETWEEN 1 AND 2048),
    recorded_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (hypothesis_id, evidence_id),
    CONSTRAINT evidence_stance_hypothesis_is_in_the_same_organization
        FOREIGN KEY (organization, hypothesis_id)
            REFERENCES investigation_hypothesis (organization, hypothesis_id),
    CONSTRAINT evidence_stance_evidence_is_in_the_same_case
        FOREIGN KEY (organization, investigation_id, evidence_id)
            REFERENCES evidence_item (organization, investigation_id, evidence_id)
);

CREATE INDEX IF NOT EXISTS evidence_stance_evidence_idx
    ON evidence_stance (investigation_id, evidence_id, stance);

CREATE TRIGGER evidence_stance_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON evidence_stance
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('');

-- ---------------------------------------------------------------------------------------
-- Coverage gaps, and coverage per capability
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS coverage_gap
(
    gap_id           UUID        NOT NULL PRIMARY KEY,
    organization     TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id UUID        NOT NULL,
    round_id         UUID        NOT NULL,
    ordinal          INTEGER     NOT NULL CHECK (ordinal >= 1),

    -- 1 capability unavailable, 2 capability not permitted, 3 source unreachable, 4 authorization
    -- denied, 5 budget exhausted, 6 redaction masked, 7 retention horizon, 8 result truncated,
    -- 9 request refused, 10 target not found.
    cause            SMALLINT    NOT NULL CHECK (cause BETWEEN 1 AND 10),
    capability_id    TEXT        NOT NULL DEFAULT '' CHECK (length(capability_id) <= 128),
    -- What could not be checked, in this investigation's own terms. NEVER the content that was
    -- withheld: a gap describing a masked field must not carry the field.
    subject          TEXT        NOT NULL CHECK (length(subject) BETWEEN 1 AND 512),
    -- What could not be concluded because of it. Required — a gap with no consequence tells a
    -- reader something is missing without telling them what it cost, which is an apology rather
    -- than a finding.
    consequence      TEXT        NOT NULL CHECK (length(consequence) BETWEEN 1 AND 2048),
    recorded_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT coverage_gap_identity_is_org_scoped UNIQUE (organization, gap_id),
    CONSTRAINT coverage_gap_ordinal_is_unique_per_case UNIQUE (investigation_id, ordinal),
    CONSTRAINT coverage_gap_case_is_in_the_same_organization
        FOREIGN KEY (organization, investigation_id)
            REFERENCES investigation (organization, investigation_id),
    CONSTRAINT coverage_gap_round_is_in_the_same_organization
        FOREIGN KEY (organization, round_id)
            REFERENCES investigation_round (organization, round_id)
);

CREATE INDEX IF NOT EXISTS coverage_gap_case_idx
    ON coverage_gap (investigation_id, ordinal);

CREATE TRIGGER coverage_gap_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON coverage_gap
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('gap_count');

-- Coverage as capability readiness, per round. Reported per typed capability with a state and a
-- reason rather than as a percentage: a percentage over unlike capabilities is a number nobody can
-- act on and every reader takes for a score.
CREATE TABLE IF NOT EXISTS investigation_coverage
(
    organization       TEXT     NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id   UUID     NOT NULL,
    round_id           UUID     NOT NULL,
    capability_id      TEXT     NOT NULL CHECK (length(capability_id) BETWEEN 1 AND 128),
    capability_version INTEGER  NOT NULL CHECK (capability_version >= 1),

    -- 1 checked, 2 checked and empty with a certificate, 3 checked but incomplete, 4 relevant but
    -- unavailable, 5 not applicable to this stack. The fifth is NOT a gap and must never be
    -- reported as one.
    state              SMALLINT NOT NULL CHECK (state BETWEEN 1 AND 5),
    reason             TEXT     NOT NULL CHECK (length(reason) BETWEEN 1 AND 1024),
    evidence_count     INTEGER  NOT NULL DEFAULT 0 CHECK (evidence_count >= 0),

    PRIMARY KEY (round_id, capability_id, capability_version),
    CONSTRAINT investigation_coverage_case_is_in_the_same_organization
        FOREIGN KEY (organization, investigation_id)
            REFERENCES investigation (organization, investigation_id),
    CONSTRAINT investigation_coverage_round_is_in_the_same_organization
        FOREIGN KEY (organization, round_id)
            REFERENCES investigation_round (organization, round_id)
);

CREATE TRIGGER investigation_coverage_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON investigation_coverage
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('');

-- ---------------------------------------------------------------------------------------
-- Outcomes, and the claims that carry their citations
-- ---------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS investigation_outcome
(
    outcome_id          UUID        NOT NULL PRIMARY KEY,
    organization        TEXT        NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id    UUID        NOT NULL,
    round_id            UUID        NOT NULL,
    -- Carried rather than joined, so a superseded outcome stays attributable to its round without
    -- a lookup.
    round_ordinal       INTEGER     NOT NULL CHECK (round_ordinal >= 1),

    -- 1 supported, 2 caveated, 3 abstained. There is no fourth: a confident conclusion without
    -- sufficient support is not a permitted outcome.
    kind                SMALLINT    NOT NULL CHECK (kind IN (1, 2, 3)),
    statement           TEXT        NOT NULL CHECK (length(statement) BETWEEN 1 AND 8192),

    -- A count of distinct Connections behind the supporting claims. A count of sources, never a
    -- score: one source agreeing with itself twice is not two sources.
    independent_sources INTEGER     NOT NULL DEFAULT 0 CHECK (independent_sources >= 0),

    -- A later round replaced this one. It stays readable, attributed and ordered rather than being
    -- rewritten: at 14:32 this was abstained for lack of node metrics; at 15:10 it became
    -- explained, and both are in the case (ADR-013).
    superseded          BOOLEAN     NOT NULL DEFAULT FALSE,
    reached_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT investigation_outcome_identity_is_org_scoped UNIQUE (organization, outcome_id),
    -- One outcome per round. A round that reached two conclusions reached none.
    CONSTRAINT investigation_outcome_is_unique_per_round UNIQUE (round_id),
    CONSTRAINT investigation_outcome_case_is_in_the_same_organization
        FOREIGN KEY (organization, investigation_id)
            REFERENCES investigation (organization, investigation_id),
    CONSTRAINT investigation_outcome_round_is_in_the_same_organization
        FOREIGN KEY (organization, round_id)
            REFERENCES investigation_round (organization, round_id)
);

CREATE INDEX IF NOT EXISTS investigation_outcome_case_idx
    ON investigation_outcome (investigation_id, round_ordinal DESC);

CREATE TRIGGER investigation_outcome_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON investigation_outcome
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('outcome_count');

CREATE TABLE IF NOT EXISTS outcome_claim
(
    claim_id         UUID     NOT NULL PRIMARY KEY,
    organization     TEXT     NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id UUID     NOT NULL,
    outcome_id       UUID     NOT NULL,
    ordinal          INTEGER  NOT NULL CHECK (ordinal >= 1),

    -- 1 supporting, 2 contradicting, 3 affected scope. Affected scope is a CLAIM rather than a
    -- field of its own, which is what stops a figure reaching the page uncited: there is no shape
    -- here for a request count, a user count or a derived percentage.
    role             SMALLINT NOT NULL CHECK (role IN (1, 2, 3)),
    statement        TEXT     NOT NULL CHECK (length(statement) BETWEEN 1 AND 4096),

    CONSTRAINT outcome_claim_identity_is_org_scoped UNIQUE (organization, claim_id),
    CONSTRAINT outcome_claim_ordinal_is_unique_per_outcome UNIQUE (outcome_id, ordinal),
    CONSTRAINT outcome_claim_outcome_is_in_the_same_organization
        FOREIGN KEY (organization, outcome_id)
            REFERENCES investigation_outcome (organization, outcome_id),
    CONSTRAINT outcome_claim_case_is_in_the_same_organization
        FOREIGN KEY (organization, investigation_id)
            REFERENCES investigation (organization, investigation_id)
);

CREATE TRIGGER outcome_claim_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON outcome_claim
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('');

-- Which EvidenceItems a claim rests on.
--
-- The foreign key carries the CASE, not only the organization, so a claim cannot cite evidence
-- belonging to a different investigation. "Every claim resolves to an evidence item that exists in
-- the same case" is therefore something the database refuses to break rather than something a
-- test happens to check.
CREATE TABLE IF NOT EXISTS claim_citation
(
    organization     TEXT NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id UUID NOT NULL,
    claim_id         UUID NOT NULL,
    evidence_id      UUID NOT NULL,

    PRIMARY KEY (claim_id, evidence_id),
    CONSTRAINT claim_citation_claim_is_in_the_same_organization
        FOREIGN KEY (organization, claim_id)
            REFERENCES outcome_claim (organization, claim_id),
    CONSTRAINT claim_citation_evidence_is_in_the_same_case
        FOREIGN KEY (organization, investigation_id, evidence_id)
            REFERENCES evidence_item (organization, investigation_id, evidence_id)
);

CREATE TRIGGER claim_citation_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON claim_citation
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('');

-- The hypotheses an outcome left unresolved, and the gaps that mattered to it. Both are what makes
-- an abstention carry content rather than being a shrug.
CREATE TABLE IF NOT EXISTS outcome_unresolved_hypothesis
(
    organization     TEXT NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id UUID NOT NULL,
    outcome_id       UUID NOT NULL,
    hypothesis_id    UUID NOT NULL,

    PRIMARY KEY (outcome_id, hypothesis_id),
    CONSTRAINT outcome_unresolved_outcome_is_in_the_same_organization
        FOREIGN KEY (organization, outcome_id)
            REFERENCES investigation_outcome (organization, outcome_id),
    CONSTRAINT outcome_unresolved_hypothesis_is_in_the_same_organization
        FOREIGN KEY (organization, hypothesis_id)
            REFERENCES investigation_hypothesis (organization, hypothesis_id)
);

CREATE TRIGGER outcome_unresolved_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON outcome_unresolved_hypothesis
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('');

CREATE TABLE IF NOT EXISTS outcome_relevant_gap
(
    organization     TEXT NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    investigation_id UUID NOT NULL,
    outcome_id       UUID NOT NULL,
    gap_id           UUID NOT NULL,

    PRIMARY KEY (outcome_id, gap_id),
    CONSTRAINT outcome_relevant_gap_outcome_is_in_the_same_organization
        FOREIGN KEY (organization, outcome_id)
            REFERENCES investigation_outcome (organization, outcome_id),
    CONSTRAINT outcome_relevant_gap_gap_is_in_the_same_organization
        FOREIGN KEY (organization, gap_id)
            REFERENCES coverage_gap (organization, gap_id)
);

CREATE TRIGGER outcome_relevant_gap_changes_the_case
    AFTER INSERT OR UPDATE OR DELETE
    ON outcome_relevant_gap
    FOR EACH ROW
EXECUTE FUNCTION investigation_case_changed('');

COMMENT ON TABLE investigation IS
    'The durable case for one operational episode. One identity, one permalink, one monotonic case version.';
COMMENT ON COLUMN investigation.case_version IS
    'Advances on ANY change within the case, not only on a lifecycle transition. Maintained by trigger.';
COMMENT ON TABLE investigation_round IS
    'One bounded immutable execution under pinned planner, model and policy versions. Owns the case pack.';
COMMENT ON TABLE evidence_item IS
    'An immutable validated factual claim with provenance. Never establishes cause on its own.';
COMMENT ON TABLE coverage_gap IS
    'Something the investigation could not check, and the consequence of not checking it.';
COMMENT ON TABLE claim_citation IS
    'Which evidence a claim rests on. The key carries the case, so a claim cannot cite another case''s evidence.';
