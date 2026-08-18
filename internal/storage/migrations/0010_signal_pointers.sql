-- The triggering alert's own pointers, preserved through intake instead of thrown away:
-- the operator's runbook and dashboard annotations, and the source's generator URL.
-- Free text from the customer's systems — untrusted for its whole life, never an
-- instruction, a destination this platform follows, or an authorisation claim.

ALTER TABLE signal
    ADD COLUMN annotations JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN generator_url TEXT NOT NULL DEFAULT ''
        CHECK (length(generator_url) <= 2048);
