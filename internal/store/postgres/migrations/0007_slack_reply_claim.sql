ALTER TABLE slack_reply ADD COLUMN lease_owner uuid;

UPDATE slack_reply SET lease_owner = gen_random_uuid() WHERE leased_until IS NOT NULL;

ALTER TABLE slack_reply ADD CONSTRAINT slack_reply_lease_is_whole
    CHECK ((leased_until IS NULL) = (lease_owner IS NULL));
