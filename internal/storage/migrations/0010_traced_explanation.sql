-- The traced explanation.
--
-- An outcome that states a cause nobody proposed and nothing was read to disprove is a conclusion
-- rather than a finding, and the case file looked identical either way. Three columns close that:
-- which hypothesis an explanation IS, which call proposed each hypothesis, and a coverage gap cause
-- for an explanation this round had no opportunity to test.

-- Which call proposed a hypothesis. Zero is the opening, from the brief alone; one and two are the
-- adaptive passes; one past the last pass is the conclusion. It defaults to zero so every row
-- written before this migration keeps the meaning it already had — there was no other way for one
-- to arrive.
ALTER TABLE investigation_hypothesis
    ADD COLUMN IF NOT EXISTS pass INTEGER NOT NULL DEFAULT 0 CHECK (pass >= 0);

-- The hypothesis this outcome is the explanation of. Null on an abstention, which states no
-- explanation, and required by the domain on the other two kinds.
--
-- A foreign key rather than a loose identifier, so following the citation is a lookup that cannot
-- dangle, and organization-scoped like every other reference in this schema so one tenant's outcome
-- can never point at another tenant's hypothesis.
ALTER TABLE investigation_outcome
    ADD COLUMN IF NOT EXISTS explains_hypothesis_id UUID NULL;

ALTER TABLE investigation_outcome
    DROP CONSTRAINT IF EXISTS investigation_outcome_explains_a_hypothesis_of_this_organization;

ALTER TABLE investigation_outcome
    ADD CONSTRAINT investigation_outcome_explains_a_hypothesis_of_this_organization
        FOREIGN KEY (organization, explains_hypothesis_id)
            REFERENCES investigation_hypothesis (organization, hypothesis_id);

-- 11 is the explanation nothing was read to disprove. It is a distinct cause because none of the
-- other ten describes it: no capability was unavailable, no limit ran out, no source declined. What
-- ran out was the opportunity to test — a hypothesis proposed at the conclusion arrives after the
-- last read this round can make.
--
-- The old bound is dropped by DISCOVERING its name rather than by asserting one. A column CHECK
-- written inline is named by PostgreSQL, and a DROP IF EXISTS naming it wrongly succeeds silently —
-- leaving the old bound in place and failing every insert of the new cause at 03:00 instead of here.
-- Every matching constraint is dropped, not the first one an unordered SELECT happened to return.
-- A bare SELECT ... INTO takes one arbitrary row and reports no error when several match, which
-- would leave a second bound on the column and fail the new cause at 03:00 rather than here.
DO
$$
    DECLARE
        existing RECORD;
    BEGIN
        FOR existing IN
            SELECT conname
              FROM pg_constraint
             WHERE conrelid = 'coverage_gap'::regclass
               AND contype = 'c'
               AND pg_get_constraintdef(oid) ILIKE '%cause%'
            LOOP
                EXECUTE format('ALTER TABLE coverage_gap DROP CONSTRAINT %I', existing.conname);
            END LOOP;
    END
$$;

ALTER TABLE coverage_gap
    ADD CONSTRAINT coverage_gap_cause_is_a_declared_value CHECK (cause BETWEEN 1 AND 11);
