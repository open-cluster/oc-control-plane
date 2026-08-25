UPDATE integration_type
   SET description = 'Give investigations read-only access to Kubernetes workload runtime, namespace events, and bounded container logs through an outbound Relay.'
 WHERE key = 'kubernetes';

UPDATE integration_type
   SET description = 'Give investigations read-only access to Slack conversations visible to the connected token and reply to direct app mentions in their original thread.'
 WHERE key = 'slack';
