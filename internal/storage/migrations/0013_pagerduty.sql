-- PagerDuty joins the catalog.
--
-- No schema beyond the reference row: the API access key is sealed as the outbound
-- credential, and there is no other configuration — PagerDuty has one API origin for
-- every account, unlike the providers before it.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

INSERT INTO integration_type (integration_type_id, key, name, description, logo, category)
VALUES (8, 'pagerduty', 'PagerDuty',
        'Read incidents from your account: bounded, read-only on-call context.',
        'pagerduty', 'incident-response')
ON CONFLICT (integration_type_id) DO NOTHING;
