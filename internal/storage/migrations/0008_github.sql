-- GitHub joins the catalog.
--
-- No schema beyond the reference row: the App credential is deployment configuration, the
-- installation id lives in the integration's configuration JSONB, and repository identity
-- travels as stable numeric ids inside tool calls rather than as rows here.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

INSERT INTO integration_type (integration_type_id, key, name, description, logo, category)
VALUES (4, 'github', 'GitHub',
        'Read repositories, commits and pull requests from the installations you select: read-only change context.',
        'github', 'source-control')
ON CONFLICT (integration_type_id) DO NOTHING;
