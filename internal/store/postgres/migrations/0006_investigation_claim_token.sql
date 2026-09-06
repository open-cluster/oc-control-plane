ALTER TABLE investigation ADD COLUMN lease_token uuid;

UPDATE investigation SET lease_token = gen_random_uuid() WHERE lease_worker <> '';

ALTER TABLE investigation ADD CONSTRAINT investigation_lease_token_is_whole
    CHECK ((lease_worker = '') = (lease_token IS NULL));
