ALTER TABLE relay_registration
    ADD COLUMN protocol_version bigint;

ALTER TABLE relay_registration
    ADD CONSTRAINT relay_registration_protocol_version_check
    CHECK (protocol_version IS NULL OR protocol_version BETWEEN 1 AND 4294967295);
