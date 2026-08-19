-- Datadog joins the catalog.
--
-- No schema beyond the reference row: the api key/application key pair is sealed as one
-- JSON-encoded outbound credential, and the site lives in the integration's configuration
-- JSONB.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

INSERT INTO integration_type (integration_type_id, key, name, description, logo, category)
VALUES (6, 'datadog', 'Datadog',
        'Read monitors from your account: bounded, read-only alert state.',
        'datadog', 'observability')
ON CONFLICT (integration_type_id) DO NOTHING;
