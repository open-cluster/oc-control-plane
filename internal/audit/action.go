package audit

// Action names what was done, as a verb-noun string in the same shape a permission takes.
//
// It is text rather than an integer for the same reason a target kind is: the first person to
// read this table is reading it during a security review, and a number would send them to the
// source to find out what happened.
//
// The vocabulary is closed and lives here. An action a route can produce that is not declared
// here is a change to what the record can say, which is a decision rather than a detail.
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

	// Service accounts and their tokens.
	ActionServiceAccountCreated Action = "service-account.created"
	ActionServiceAccountRemoved Action = "service-account.removed"
	ActionTokenIssued           Action = "api-token.issued"
	ActionTokenRevoked          Action = "api-token.revoked"

	// The estate. Revising and verifying are separate actions because they are separate
	// claims: one says the configuration changed, the other says somebody checked what the
	// configuration reaches. Deleting is narrow — refused the moment anything depends on
	// the Integration — and an operation that removes a record has to be as attributable
	// as the one that made it.
	ActionIntegrationCreated       Action = "integration.created"
	ActionIntegrationRevised       Action = "integration.revised"
	ActionIntegrationEnabled       Action = "integration.enabled-set"
	ActionIntegrationVerified      Action = "integration.verified"
	ActionIntegrationDeleted       Action = "integration.deleted"
	ActionIntegrationSecretRotated Action = "integration.webhook-secret.rotated"
	// Replacing the outbound credential is its own act, apart from revising: it is the
	// answer to "who rotated the token, and did the new one work".
	ActionIntegrationCredentialReplaced Action = "integration.credential.replaced"
	// Every unseal of a stored credential is on the record — the verification probe, the
	// investigation runner — so credential USE is observable, not only credential change.
	ActionIntegrationCredentialUnsealed Action = "integration.credential.unsealed"
	ActionRelayRosterRead               Action = "relay.roster.read"
	ActionConflictTrailRead             Action = "relay.conflict-trail.read"
	ActionConflictCleared               Action = "relay.conflict.cleared"
	ActionRelayBootstrapIssued          Action = "relay.bootstrap-token.issued"

	// Incidents. Grouping itself is not audited — it is done by a delivery rather than by a
	// person, hundreds of times a day, and a record that grew a row per alert would bury the acts
	// somebody actually performed. A MERGE is a person overriding that grouping, and it decides
	// what an investigation opened for the incident would be about.
	ActionIncidentMerge Action = "incident.merged"

	// Opening an investigation is the one operator act on it; everything the runner does
	// afterwards is the investigation's own provenance, a record of its own.
	ActionInvestigationOpened    Action = "investigation.opened"
	ActionInvestigationCancelled Action = "investigation.cancelled"
	ActionWebhookWorkReplayed    Action = "webhook-work.replayed"

	// Conversations. Opening one and sending a message to one are both operator acts, and
	// both are recorded because a conversation several people take part in has to be able
	// to answer who asked what. The turns a message opens are investigations and are
	// audited as those; recording them twice would say the same thing in two vocabularies.
	ActionConversationOpened  Action = "conversation.opened"
	ActionConversationMessage Action = "conversation.message-sent"

	// Authorization itself. A refusal is on the record because credential probing is only
	// visible if the attempts that failed are visible too.
	ActionAuthorizationRefused Action = "authorization.refused"

	// A COLLABORATION WRITE: OpenCluster answering in a customer's own chat surface. It is
	// the only thing this product writes into a system it does not own, so it is recorded
	// under a word of its own — deliberately distinct from an external read, and firmly
	// distinct from a production or remediation write, which remain unsupported.
	ActionCollaborationReplied Action = "collaboration.replied"
)
