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
	ActionProviderConfigured Action = "identity-provider.configured"
	ActionProviderChanged    Action = "identity-provider.changed"
	ActionProviderRemoved    Action = "identity-provider.removed"
	ActionPolicyChanged      Action = "organization.policy.changed"

	// Service accounts and their tokens.
	ActionServiceAccountCreated Action = "service-account.created"
	ActionServiceAccountRemoved Action = "service-account.removed"
	ActionTokenIssued           Action = "api-token.issued"
	ActionTokenRevoked          Action = "api-token.revoked"

	// The estate.
	ActionEnvironmentCreated Action = "environment.created"
	ActionEnvironmentRenamed Action = "environment.renamed"
	ActionEnvironmentDeleted Action = "environment.deleted"
	ActionConnectionCreated  Action = "connection.created"
	ActionConnectionEnabled  Action = "connection.enabled-set"
	ActionConnectionRotated  Action = "connection.trigger-secret.rotated"
	// A Connection's lifecycle. Revising and validating are separate actions because they are
	// separate claims: one says the configuration changed, the other says somebody checked what
	// the configuration reaches. Deleting is here because the product now has a delete — a
	// narrow one, refused the moment anything depends on the Connection — and an operation that
	// removes a record has to be as attributable as the one that made it.
	ActionConnectionRevised   Action = "connection.revised"
	ActionConnectionValidated Action = "connection.validated"
	ActionConnectionDeleted   Action = "connection.deleted"
	// A test event is recorded because it enters the delivery history, and a delivery in the
	// history that nobody can attribute to an operator would be indistinguishable from one the
	// customer's own system sent.
	ActionTriggerTested        Action = "connection.trigger.test-event"
	ActionRelayRosterRead      Action = "relay.roster.read"
	ActionConflictTrailRead    Action = "relay.conflict-trail.read"
	ActionConflictCleared      Action = "relay.conflict.cleared"
	ActionRelayBootstrapIssued Action = "relay.bootstrap-token.issued"

	// Investigations.
	ActionInvestigationOpened    Action = "investigation.opened"
	ActionInvestigationCancelled Action = "investigation.cancelled"
	ActionReinvestigated         Action = "investigation.reinvestigated"

	// Authorization itself. A refusal is on the record because credential probing is only
	// visible if the attempts that failed are visible too.
	ActionAuthorizationRefused Action = "authorization.refused"
)
