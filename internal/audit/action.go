package audit

type Action string

const (
	ActionLocalBootstrapCompleted Action = "local.bootstrap-completed"
	ActionLocalPasswordChanged    Action = "local.password-changed"
	ActionLocalPasswordRecovered  Action = "local.password-recovered"
	ActionSignInCompleted         Action = "session.sign-in.completed"
	ActionSignedOut               Action = "session.signed-out"
	ActionUserProvisioned         Action = "user.provisioned"
	ActionMembershipChanged       Action = "membership.changed"
	ActionMembershipRevoked       Action = "membership.revoked"
	ActionPolicyChanged           Action = "organization.policy.changed"

	ActionIntegrationCreated            Action = "integration.created"
	ActionIntegrationRevised            Action = "integration.revised"
	ActionIntegrationEnabled            Action = "integration.enabled-set"
	ActionIntegrationVerified           Action = "integration.verified"
	ActionIntegrationDeleted            Action = "integration.deleted"
	ActionIntegrationSecretRotated      Action = "integration.webhook-secret.rotated"
	ActionIntegrationCredentialReplaced Action = "integration.credential.replaced"
	ActionIntegrationCredentialUnsealed Action = "integration.credential.unsealed"

	ActionRelaySessionConflictDetected Action = "relay.session_conflict.detected"
	ActionRelaySessionConflictCleared  Action = "relay.session_conflict.cleared"
	ActionRelayBootstrapIssued         Action = "relay.bootstrap-token.issued"

	ActionIncidentMerge         Action = "incident.merged"
	ActionPostmortemCreated     Action = "postmortem.created"
	ActionPostmortemRegenerated Action = "postmortem.regenerated"
	ActionPostmortemCorrected   Action = "postmortem.corrected"
	ActionPostmortemReviewed    Action = "postmortem.reviewed"

	ActionInvestigationOpened    Action = "investigation.opened"
	ActionInvestigationCancelled Action = "investigation.cancelled"

	ActionConversationOpened    Action = "conversation.opened"
	ActionConversationMessage   Action = "conversation.message-sent"
	ActionSlackMessageRecovered Action = "slack-message.recovered"

	ActionAuthorizationRefused Action = "authorization.refused"
	ActionCollaborationReplied Action = "collaboration.replied"
)
