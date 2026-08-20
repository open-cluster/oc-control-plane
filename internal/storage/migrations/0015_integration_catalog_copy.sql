-- Integration catalog metadata is product copy. Keep deployed rows aligned with the
-- compiled catalog without rewriting the migrations that originally introduced them.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

UPDATE integration_type AS current
   SET name = revised.name,
       description = revised.description,
       category = revised.category,
       updated_at = now()
  FROM (VALUES
        (1, 'Prometheus Alertmanager',
         'Create incidents from firing and resolved Alertmanager alerts delivered through an authenticated webhook.',
         'alerting'),
        (2, 'Kubernetes',
         'Give investigations a read-only inventory of Kubernetes workloads through an outbound Relay.',
         'infrastructure'),
        (3, 'Slack',
         'Give investigations read-only access to Slack conversations visible to the connected token; OpenCluster never posts to Slack.',
         'collaboration'),
        (4, 'GitHub',
         'Give investigations read-only access to selected repositories for commits, pull requests, CI failures, files, and releases.',
         'source-control')
       ) AS revised(integration_type_id, name, description, category)
 WHERE current.integration_type_id = revised.integration_type_id;
