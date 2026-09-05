package audit

type Action string

const (
	// Sessions and identity.
	ActionSignInStarted      Action = "session.sign-in.started"
	ActionSignInCompleted    Action = "session.sign-in.completed"
	ActionSignInRefused      Action = "session.sign-in.refused"
	ActionSignedOut          Action = "session.signed-out"
	ActionSessionRevoked     Action = "session.revoked"
	ActionUserProvisioned    Action = "user.provisioned"
	ActionMembershipGranted  Action = "membership.granted"
	ActionMembershipChanged  Action = "membership.changed"
	ActionMembershipRevoked  Action = "membership.revoked"
	ActionLocalPasswordReset Action = "local-password.reset"
	ActionProviderConfigured Action = "identity-provider.configured"
	ActionProviderChanged    Action = "identity-provider.changed"
	ActionProviderRemoved    Action = "identity-provider.removed"
	ActionPolicyChanged      Action = "organization.policy.changed"

	// Integration
	ActionIntegrationCreated            Action = "integration.created"
	ActionIntegrationRevised            Action = "integration.revised"
	ActionIntegrationEnabled            Action = "integration.enabled-set"
	ActionIntegrationVerified           Action = "integration.verified"
	ActionIntegrationDeleted            Action = "integration.deleted"
	ActionIntegrationSecretRotated      Action = "integration.webhook-secret.rotated"
	ActionIntegrationCredentialReplaced Action = "integration.credential.replaced"
	ActionIntegrationCredentialUnsealed Action = "integration.credential.unsealed"

	// Relay
	ActionConflictCleared      Action = "relay.conflict.cleared"
	ActionRelayBootstrapIssued Action = "relay.bootstrap-token.issued"

	// Incidents
	ActionIncidentMerge         Action = "incident.merged"
	ActionPostmortemCreated     Action = "postmortem.created"
	ActionPostmortemRegenerated Action = "postmortem.regenerated"
	ActionPostmortemCorrected   Action = "postmortem.corrected"
	ActionPostmortemReviewed    Action = "postmortem.reviewed"

	// Investigations
	ActionInvestigationOpened     Action = "investigation.opened"
	ActionInvestigationCancelled  Action = "investigation.cancelled"
	ActionWebhookDeliveryReplayed Action = "webhook-delivery.replayed"

	// Conversations
	ActionConversationOpened  Action = "conversation.opened"
	ActionConversationMessage Action = "conversation.message-sent"

	ActionAuthorizationRefused Action = "authorization.refused"
	ActionCollaborationReplied Action = "collaboration.replied"
)
