ALTER TABLE integration_delivery
    ADD COLUMN provider_identity text,
    ADD COLUMN lifecycle_phase text,
    ADD COLUMN request_id text NOT NULL DEFAULT '';

UPDATE integration_delivery
   SET provider_identity = encode(body_digest, 'hex'),
       lifecycle_phase = ''
 WHERE outcome = 1;

ALTER TABLE integration_delivery
    ADD CONSTRAINT integration_delivery_accepted_carries_provider_identity
        CHECK ((outcome <> 1) OR
               (provider_identity IS NOT NULL AND lifecycle_phase IS NOT NULL)),
    ADD CONSTRAINT integration_delivery_nonaccepted_has_no_provider_identity
        CHECK ((outcome = 1) OR
               (provider_identity IS NULL AND lifecycle_phase IS NULL)),
    ADD CONSTRAINT integration_delivery_provider_identity_check
        CHECK (provider_identity IS NULL OR length(provider_identity) BETWEEN 1 AND 256),
    ADD CONSTRAINT integration_delivery_lifecycle_phase_check
        CHECK (lifecycle_phase IS NULL OR lifecycle_phase IN ('', 'firing', 'resolved')),
    ADD CONSTRAINT integration_delivery_request_id_check
        CHECK (length(request_id) <= 128);

DROP INDEX integration_delivery_accepted_is_unique;

CREATE UNIQUE INDEX integration_delivery_accepted_provider_identity_is_unique
    ON integration_delivery (integration_id, provider_identity, lifecycle_phase)
    WHERE outcome = 1;

INSERT INTO integration_type
    (integration_type_id, key, name, description, logo, category)
VALUES
    (5, 'generic_webhook', 'Generic Webhook',
     'Create incidents from canonical firing and resolved Alert Events delivered through an authenticated webhook.',
     '', 'alerting');
