ALTER TABLE integration RENAME COLUMN disabled TO disconnected;

UPDATE integration
   SET credential_sealed = NULL, webhook_secret_digest = NULL,
       verification_status = NULL, verified_at = NULL, verification_grants = '{}'::text[]
 WHERE disconnected;

DELETE FROM integration_installation installed
 USING integration
 WHERE installed.org_id = integration.org_id
   AND installed.integration_id = integration.integration_id AND integration.disconnected;

UPDATE relay_job job
   SET status = CASE WHEN job.status = 0 THEN 4 ELSE job.status END,
       terminal_at = CASE WHEN job.status = 0 THEN now() ELSE job.terminal_at END,
       cancel_requested_at = coalesce(job.cancel_requested_at, now())
  FROM integration
 WHERE job.org_id = integration.org_id AND job.integration_id = integration.integration_id
   AND integration.disconnected AND job.status IN (0, 1);

UPDATE slack_message_work work
   SET status = 4, lease_owner = '', lease_expires_at = NULL,
       failure_class = 'integration-disconnected', failure_message = 'the integration is disconnected',
       updated_at = now()
  FROM integration
 WHERE work.org_id = integration.org_id AND work.integration_id = integration.integration_id
   AND integration.disconnected AND work.status IN (1, 2, 3);

UPDATE slack_reply reply
   SET status = 4, lease_owner = NULL, leased_until = NULL,
       note = 'the integration is disconnected', updated_at = now()
  FROM slack_conversation conversation, integration
 WHERE reply.org_id = conversation.org_id AND reply.conversation_id = conversation.conversation_id
   AND conversation.org_id = integration.org_id AND conversation.integration_id = integration.integration_id
   AND integration.disconnected AND reply.status IN (1, 2);
