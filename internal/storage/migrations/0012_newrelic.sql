-- New Relic joins the catalog.
--
-- No schema beyond the reference row: the user key is sealed as the outbound credential,
-- and the region/account id live in the integration's configuration JSONB.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

INSERT INTO integration_type (integration_type_id, key, name, description, logo, category)
VALUES (7, 'newrelic', 'New Relic',
        'Read correlated issues from your account: bounded, read-only alert context.',
        'newrelic', 'observability')
ON CONFLICT (integration_type_id) DO NOTHING;
