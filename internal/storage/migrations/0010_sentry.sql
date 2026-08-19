-- Sentry joins the catalog.
--
-- No schema beyond the reference row: the auth token is sealed as the outbound credential,
-- and the organization slug lives in the integration's configuration JSONB.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

INSERT INTO integration_type (integration_type_id, key, name, description, logo, category)
VALUES (5, 'sentry', 'Sentry',
        'Read issues from your projects: bounded, read-only error and event context.',
        'sentry', 'observability')
ON CONFLICT (integration_type_id) DO NOTHING;
