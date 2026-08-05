package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The lifecycle a Connection has beyond existing: what a check of it found, what changed and
// when, what depends on it, and what happens when somebody tries to remove it.
//
// It is a file of its own rather than more of connection.go because it is a different subject.
// connection.go answers "what is a Connection and how is one made"; this answers "what has
// happened to it since", which is the half an operator reads during an incident.

// Readiness is how ready one capability is, in the COVERAGE vocabulary rather than in a second
// one invented here. CONTEXT.md already names these states for what an investigation can reach,
// and a validation result is the same fact asked earlier — two vocabularies for one fact is one
// of them being wrong.
type Readiness int16

const (
	// ReadyAvailable is the capability answering.
	ReadyAvailable Readiness = iota + 1
	// ReadyUnauthorized is the far end refusing. It is separated from unavailable because the
	// two have different owners: an authorization refusal is the customer's own policy
	// answering, and telling them to check their network instead would send them somewhere
	// there is nothing to find.
	ReadyUnauthorized
	// ReadyUnavailable is the capability not being servable — not advertised, not reachable.
	ReadyUnavailable
	// ReadyNotAttempted is a check that was not run, which is NOT a pass. An operator reading a
	// clean result should be able to tell what was actually exercised from what was skipped.
	ReadyNotAttempted
)

func (r Readiness) String() string {
	switch r {
	case ReadyAvailable:
		return "available"
	case ReadyUnauthorized:
		return "unauthorized"
	case ReadyUnavailable:
		return "unavailable"
	case ReadyNotAttempted:
		return "not_attempted"
	default:
		return "unrecognised"
	}
}

// ValidationOutcome is what a whole run amounted to.
type ValidationOutcome int16

const (
	// ValidationPassed is everything checked and everything ready.
	ValidationPassed ValidationOutcome = iota + 1
	// ValidationPartial is the outcome this whole design exists for. "Authentication worked and
	// two of five capabilities are unavailable" is a result an operator can act on; collapsed
	// into a boolean it becomes a failure, and a failure is what somebody retries rather than
	// reads.
	ValidationPartial
	ValidationFailed
)

func (o ValidationOutcome) String() string {
	switch o {
	case ValidationPassed:
		return "passed"
	case ValidationPartial:
		return "partial"
	case ValidationFailed:
		return "failed"
	default:
		return "unrecognised"
	}
}

// State is the Connection state this outcome puts the record into. The mapping lives here
// rather than at a call site so a fourth outcome cannot be added without deciding it.
func (o ValidationOutcome) State() ConnectionState {
	switch o {
	case ValidationPassed:
		return ConnectionActive
	case ValidationPartial:
		return ConnectionDegraded
	default:
		return ConnectionFailed
	}
}

// CapabilityReadiness is one capability's result inside one run.
type CapabilityReadiness struct {
	Capability string
	Readiness  Readiness
	// Reason says why, in the operator's language, for anything that is not available. A
	// refusal nobody can act on is a defect that gets rediscovered rather than fixed.
	Reason string
}

// ValidationResult is what a check found, before it is recorded.
type ValidationResult struct {
	// Authentication is whether the credential was exercised and what happened. A provider
	// reached inbound only records ReadyNotAttempted with a reason rather than a pass, because
	// a pass would mean nothing and would be read as though it meant something.
	Authentication       Readiness
	AuthenticationReason string
	Capabilities         []CapabilityReadiness
	// Note is what this run does not establish, taken from the Integration definition's
	// validation contract, so a result carries its own caveats rather than needing them looked
	// up somewhere else.
	Note      string
	StartedAt time.Time
}

// Outcome decides what the whole run amounted to, from the parts.
//
// The rule is stated once, here, because it is the rule that decides what an operator is shown:
// a failed authentication is a failed run whatever the capabilities say, since a capability
// result gathered without a working credential describes nothing. Anything less than every
// capability available, with authentication working, is PARTIAL rather than failed — that is
// what makes "some of it works" a result.
//
// A run that attempted NOTHING is partial too, and never passed. That case is real rather than
// theoretical: an inbound-only provider has no credential to exercise and declares no
// capabilities, so a rule that counted an empty capability list as "everything available" would
// hand it a pass — a green tick resting on zero checks, which is precisely the claim this
// design refuses everywhere else.
func (r ValidationResult) Outcome() ValidationOutcome {
	if r.Authentication == ReadyUnauthorized || r.Authentication == ReadyUnavailable {
		return ValidationFailed
	}
	if !r.Attempted() {
		return ValidationPartial
	}
	for _, capability := range r.Capabilities {
		if capability.Readiness != ReadyAvailable {
			return ValidationPartial
		}
	}
	return ValidationPassed
}

// Attempted reports whether this run actually exercised anything.
//
// It is what stops a result that checked nothing from moving the Connection's state. A run that
// established nothing has no business overwriting what the last run that DID establish something
// found — and "configured" already means "it exists and nothing has checked it", which is the
// honest state for a Connection nothing can check.
func (r ValidationResult) Attempted() bool {
	if r.Authentication != ReadyNotAttempted {
		return true
	}
	for _, capability := range r.Capabilities {
		if capability.Readiness != ReadyNotAttempted {
			return true
		}
	}
	return false
}

// Validation is one recorded run.
type Validation struct {
	ID                    uuid.UUID
	ConfigurationRevision int
	Outcome               ValidationOutcome
	Authentication        Readiness
	AuthenticationReason  string
	Note                  string
	Capabilities          []CapabilityReadiness
	StartedAt             time.Time
	CompletedAt           time.Time
}

// ValidationList is a page of a Connection's validation history, newest first.
type ValidationList struct {
	Validations []Validation
	Next        string
}

// ErrConnectionHasDependents reports a Connection something still depends on. It is a refusal
// rather than a cascade: the dependents are a customer's own history, and deleting them to make
// a delete succeed is the silent breakage story 25 asks to be prevented.
var ErrConnectionHasDependents = errors.New("connection has dependents")

// Dependents is what would be affected by removing a Connection.
//
// Each count is a separate number rather than a total, because they mean different things to
// the person deciding. Signals are history that would be destroyed; jobs are executions that
// reference it; being an Environment's last evidence Connection is a coverage gap that opens
// the moment it goes.
type Dependents struct {
	// Signals is how many normalised SignalUpdates arrived through this Connection.
	Signals int
	// Deliveries is how many delivery attempts were recorded against it.
	Deliveries int
	// Jobs is how many capability executions named it.
	Jobs int
	// LastEvidenceInEnvironment reports that removing this would leave its Environment with no
	// evidence Connection at all — which is a coverage gap rather than a lost row, and is the
	// one dependent here that is about the FUTURE rather than about history.
	LastEvidenceInEnvironment bool
}

// Any reports whether anything at all depends on this Connection.
func (d Dependents) Any() bool {
	return d.Signals > 0 || d.Deliveries > 0 || d.Jobs > 0 || d.LastEvidenceInEnvironment
}

// ConnectionDependents reports what depends on a Connection, so an operator is told before they
// act rather than after.
func (p *Placements) ConnectionDependents(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) (Dependents, error) {
	if !principal.MemberOf(organization) {
		return Dependents{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return Dependents{}, err
	}
	return countDependents(ctx, pool, organization, id)
}

// countDependents is the read behind both the report and the refusal, so the two can never
// disagree about what a Connection is holding.
func countDependents(
	ctx context.Context, on reader, organization tenancy.Organization, id uuid.UUID,
) (Dependents, error) {
	var dependents Dependents
	err := on.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM signal
		         WHERE organization = $1 AND connection_id = $2),
		       (SELECT count(*) FROM trigger_delivery
		         WHERE organization = $1 AND connection_id = $2),
		       (SELECT count(*) FROM relay_job
		         WHERE organization = $1 AND connection_id = $2),
		       -- The last evidence Connection in its Environment. Counted against the
		       -- Connection's own Environment rather than the tenant, because coverage is an
		       -- Environment's property and a Connection elsewhere covers nothing here.
		       (SELECT count(*) = 1
		          FROM connection peers
		         WHERE peers.organization = $1
		           AND peers.role IN (2, 3)
		           AND peers.disabled_at IS NULL
		           AND peers.environment_id = (SELECT environment_id FROM connection
		                                        WHERE organization = $1 AND connection_id = $2)
		           AND EXISTS (SELECT 1 FROM connection self
		                        WHERE self.organization = $1 AND self.connection_id = $2
		                          AND self.role IN (2, 3) AND self.disabled_at IS NULL))`,
		organization.String(), id).Scan(&dependents.Signals, &dependents.Deliveries,
		&dependents.Jobs, &dependents.LastEvidenceInEnvironment)
	if err != nil {
		return Dependents{}, fmt.Errorf("counting what depends on a connection: %w", err)
	}
	return dependents, nil
}

// DeleteConnection removes a Connection that nothing depends on, and refuses one that something
// does.
//
// It refuses rather than cascading, and that is the whole shape of this operation. The product's
// position has been that a Connection is DISABLED rather than removed, so the record of what a
// source produced survives — and that position is right for every Connection that has ever been
// used. What it never covered is the Connection created by mistake five minutes ago, which had
// to be carried forever because the only alternative was destroying history.
//
// So: nothing depends on it, it goes; something does, the caller is told what and disables it
// instead. The dependents travel with the refusal so an operator learns the cost in the same
// answer that declines to impose it.
// It returns only an error, deliberately. The refusal is decided INSIDE the transaction, under
// the row lock, so it cannot be raced by a delivery arriving between the count and the delete —
// and an audited mutation that fails commits nothing, including anything it wanted to hand back.
// So the counts a caller shows come from a second read on the refusal path, which is one extra
// query on an uncommon path and is as-of-render, which is what an operator is looking at anyway.
func (p *Placements) DeleteConnection(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionConnectionDeleted,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var name, integration string
			err := transaction.QueryRow(ctx, `
				SELECT name, integration FROM connection
				 WHERE organization = $1 AND connection_id = $2
				   FOR UPDATE`, organization.String(), id).Scan(&name, &integration)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, ErrConnectionUnknown
			}
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("reading a connection to delete: %w", err)
			}

			// Counted inside the transaction that would do the deleting, and under the row lock
			// taken above. Counting outside would leave a window in which a delivery arrives
			// between the count and the delete, and the answer "nothing depended on it" would be
			// true when it was read and false when it was acted on.
			dependents, err := countDependents(ctx, transaction, organization, id)
			if err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			if dependents.Any() {
				return struct{}{}, audit.Target{}, nil, ErrConnectionHasDependents
			}

			if _, err = transaction.Exec(ctx, `
				DELETE FROM connection WHERE organization = $1 AND connection_id = $2`,
				organization.String(), id); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("deleting a connection: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetConnection, ID: id.String()},
				audit.Detail{
					"name":        name,
					"integration": integration,
					"effect": "the connection was removed; nothing had been delivered through " +
						"it and nothing had read through it",
				}, nil
		})
	return err
}

// ReviseConnection applies a change to a Connection's configuration and increments its revision.
//
// The revision is what makes "it changed" answerable. It is incremented in the same statement
// that writes the configuration, so a revision cannot be observed that no configuration
// produced, and every validation result names the revision it was a result about.
//
// The state falls back to `configured`: a Connection whose configuration just changed has not
// been checked in that configuration, and keeping the old `active` would be the record claiming
// a result about something that no longer exists.
func (p *Placements) ReviseConnection(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, revised map[string]any, labels map[string]string,
) (Connection, error) {
	return audited(ctx, p, principal, organization, audit.ActionConnectionRevised,
		func(ctx context.Context, transaction pgx.Tx) (
			Connection, audit.Target, audit.Detail, error,
		) {
			configuration, err := json.Marshal(orEmptyConfiguration(revised))
			if err != nil {
				return Connection{}, audit.Target{}, nil,
					fmt.Errorf("encoding configuration: %w", err)
			}
			encodedLabels, err := json.Marshal(orEmptyLabels(labels))
			if err != nil {
				return Connection{}, audit.Target{}, nil, fmt.Errorf("encoding labels: %w", err)
			}

			row := transaction.QueryRow(ctx, `
				UPDATE connection
				   SET configuration          = $3,
				       labels                 = $4,
				       configuration_revision = configuration_revision + 1,
				       state                  = 1,
				       updated_at             = now()
				 WHERE organization = $1 AND connection_id = $2
				RETURNING `+connectionColumns,
				organization.String(), id, configuration, encodedLabels)

			revisedConnection, err := scanConnection(row, organization.String())
			if errors.Is(err, pgx.ErrNoRows) {
				return Connection{}, audit.Target{}, nil, ErrConnectionUnknown
			}
			if err != nil {
				return Connection{}, audit.Target{}, nil,
					fmt.Errorf("revising a connection: %w", err)
			}
			// The configuration itself is not in the detail. It is a customer's own settings and
			// may name hosts and scopes; what the record needs is that it changed, who changed
			// it, and which revision resulted.
			return revisedConnection,
				audit.Target{Kind: audit.TargetConnection, ID: id.String()},
				audit.Detail{
					"configurationRevision": revisedConnection.ConfigurationRevision,
					"effect": "the connection returned to `configured`; the previous validation " +
						"result described a configuration that no longer exists",
				}, nil
		})
}

// RecordValidation writes what a check found and moves the Connection to the state it implies,
// in one transaction.
//
// Both halves commit together for the reason every audited mutation here does: a state saying
// `active` with no result behind it is a claim nobody can check, and a result with no state
// change is a check whose finding nothing acted on.
func (p *Placements) RecordValidation(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, result ValidationResult,
) (Validation, error) {
	return audited(ctx, p, principal, organization, audit.ActionConnectionValidated,
		func(ctx context.Context, transaction pgx.Tx) (
			Validation, audit.Target, audit.Detail, error,
		) {
			outcome := result.Outcome()
			started := result.StartedAt
			if started.IsZero() {
				started = time.Now().UTC()
			}

			// The revision is read from the row being updated rather than passed in, so a result
			// cannot claim to be about a configuration that changed while the check was running.
			//
			// The state is left ALONE when the run attempted nothing. Two cases reach here that
			// way and both would be misreported by a state change: a provider reached inbound
			// only, which has nothing to authenticate against, and a Connection an operator has
			// disabled, which is orthogonal to whether it works. Writing `failed` for either
			// would say a check found something wrong when no check ran.
			var revision int
			err := transaction.QueryRow(ctx, `
				UPDATE connection
				   SET state      = CASE WHEN $4 THEN $3 ELSE state END,
				       updated_at = now()
				 WHERE organization = $1 AND connection_id = $2
				RETURNING configuration_revision`,
				organization.String(), id, int16(outcome.State()), result.Attempted()).Scan(&revision)
			if errors.Is(err, pgx.ErrNoRows) {
				return Validation{}, audit.Target{}, nil, ErrConnectionUnknown
			}
			if err != nil {
				return Validation{}, audit.Target{}, nil,
					fmt.Errorf("recording a connection's validated state: %w", err)
			}

			validation := Validation{
				ID:                    uuid.New(),
				ConfigurationRevision: revision,
				Outcome:               outcome,
				Authentication:        result.Authentication,
				AuthenticationReason:  result.AuthenticationReason,
				Note:                  result.Note,
				Capabilities:          result.Capabilities,
				StartedAt:             started,
				CompletedAt:           time.Now().UTC(),
			}
			if _, err = transaction.Exec(ctx, `
				INSERT INTO connection_validation
					(validation_id, organization, connection_id, configuration_revision, outcome,
					 authentication, authentication_reason, note, started_at, completed_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				validation.ID, organization.String(), id, revision, int16(outcome),
				int16(validation.Authentication), validation.AuthenticationReason,
				validation.Note, validation.StartedAt, validation.CompletedAt); err != nil {
				return Validation{}, audit.Target{}, nil,
					fmt.Errorf("recording a validation: %w", err)
			}

			for _, readiness := range validation.Capabilities {
				if _, err = transaction.Exec(ctx, `
					INSERT INTO connection_validation_capability
						(validation_id, organization, capability, readiness, reason)
					VALUES ($1, $2, $3, $4, $5)`,
					validation.ID, organization.String(), readiness.Capability,
					int16(readiness.Readiness), readiness.Reason); err != nil {
					return Validation{}, audit.Target{}, nil,
						fmt.Errorf("recording a capability's readiness: %w", err)
				}
			}

			return validation,
				audit.Target{Kind: audit.TargetConnection, ID: id.String()},
				audit.Detail{
					"outcome":               outcome.String(),
					"authentication":        validation.Authentication.String(),
					"configurationRevision": revision,
					"capabilitiesChecked":   len(validation.Capabilities),
				}, nil
		})
}

// ConnectionValidations reads a Connection's validation history, newest first.
//
// It is a history rather than a current state because the question is not only "does it work"
// but "has it ever worked" — which is what tells an outage from a configuration that was never
// right, and which of those two decides whether anybody gets paged.
func (p *Placements) ConnectionValidations(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, page Page, descending bool,
) (ValidationList, error) {
	if !principal.MemberOf(organization) {
		return ValidationList{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return ValidationList{}, err
	}
	limit := pageLimit(page.Limit)
	after, afterID, err := decodeCursor(page.After)
	if err != nil {
		return ValidationList{}, err
	}

	// Both directions are served. A listing that advertises a sortable field and then hardcodes
	// one direction answers 200 to a caller who asked for the other, which is the same silent
	// lie that refusing an unknown sort field exists to prevent.
	direction := "DESC"
	comparison := "<"
	if !descending {
		direction, comparison = "ASC", ">"
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT validation_id, configuration_revision, outcome, authentication,
		       authentication_reason, note, started_at, completed_at
		  FROM connection_validation
		 WHERE organization = $1 AND connection_id = $2
		   AND ($4::timestamptz IS NULL
		        OR (completed_at, validation_id) %s ($4::timestamptz, $5::uuid))
		 ORDER BY completed_at %s, validation_id %s
		 LIMIT $3`, comparison, direction, direction),
		organization.String(), id, limit+1, after, afterID)
	if err != nil {
		return ValidationList{}, fmt.Errorf("reading a connection's validations: %w", err)
	}
	defer rows.Close()

	list := ValidationList{Validations: make([]Validation, 0, limit)}
	for rows.Next() {
		var validation Validation
		var outcome, authentication int16
		if err = rows.Scan(&validation.ID, &validation.ConfigurationRevision, &outcome,
			&authentication, &validation.AuthenticationReason, &validation.Note,
			&validation.StartedAt, &validation.CompletedAt); err != nil {
			return ValidationList{}, fmt.Errorf("reading a validation: %w", err)
		}
		if len(list.Validations) == limit {
			last := list.Validations[limit-1]
			list.Next = encodeCursor(last.CompletedAt, last.ID)
			break
		}
		validation.Outcome = ValidationOutcome(outcome)
		validation.Authentication = Readiness(authentication)
		list.Validations = append(list.Validations, validation)
	}
	if err = rows.Err(); err != nil {
		return ValidationList{}, fmt.Errorf("reading a connection's validations: %w", err)
	}
	if err = p.attachCapabilities(ctx, pool, organization, list.Validations); err != nil {
		return ValidationList{}, err
	}
	return list, nil
}

// attachCapabilities fills each run's per-capability results.
//
// It is a second query over the page rather than a join, because a join would repeat every run's
// columns once per capability and the assembly would have to un-repeat them. One extra round
// trip for a bounded page is the cheaper of the two, and it is the one whose code says what it
// does.
func (p *Placements) attachCapabilities(
	ctx context.Context, pool reader, organization tenancy.Organization,
	validations []Validation,
) error {
	if len(validations) == 0 {
		return nil
	}
	identifiers := make([]uuid.UUID, 0, len(validations))
	for _, validation := range validations {
		identifiers = append(identifiers, validation.ID)
	}

	rows, err := pool.Query(ctx, `
		SELECT validation_id, capability, readiness, reason
		  FROM connection_validation_capability
		 WHERE organization = $1 AND validation_id = ANY($2)
		 ORDER BY capability`,
		organization.String(), identifiers)
	if err != nil {
		return fmt.Errorf("reading capability readiness: %w", err)
	}
	defer rows.Close()

	byValidation := make(map[uuid.UUID][]CapabilityReadiness, len(validations))
	for rows.Next() {
		var (
			id        uuid.UUID
			readiness CapabilityReadiness
			state     int16
		)
		if err = rows.Scan(&id, &readiness.Capability, &state, &readiness.Reason); err != nil {
			return fmt.Errorf("reading a capability's readiness: %w", err)
		}
		readiness.Readiness = Readiness(state)
		byValidation[id] = append(byValidation[id], readiness)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("reading capability readiness: %w", err)
	}
	for index := range validations {
		validations[index].Capabilities = byValidation[validations[index].ID]
	}
	return nil
}

// RelayAdvertisement is what the Relay serving a Connection said it could run, at enrolment.
//
// It is what validation checks a declared capability against, and the reason that is a real
// check rather than a formality: a Relay too old to compile a capability will refuse the job
// when it arrives, and finding that out during an investigation is finding it out during an
// incident.
type RelayAdvertisement struct {
	// Registered reports whether the Connection names a Relay that exists and is not revoked.
	Registered bool
	// Connected reports whether that Relay is holding a session right now, from the durable
	// presence rather than from this process's own session registry.
	Connected bool
	Version   string
	// Capabilities is what it advertised, by capability id.
	Capabilities map[string]bool
}

// RelayServing reads what the Relay bound to a Connection advertised and whether it is connected.
func (p *Placements) RelayServing(
	ctx context.Context, organization tenancy.Organization, registration uuid.UUID,
	liveness time.Duration,
) (RelayAdvertisement, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return RelayAdvertisement{}, err
	}

	var (
		version    string
		advertised []byte
		connected  bool
	)
	err = pool.QueryRow(ctx, `
		SELECT relay_version, capabilities,
		       session_ended_at IS NULL
		           AND last_seen_at IS NOT NULL
		           AND last_seen_at > now() - $3::interval
		  FROM relay_registration
		 WHERE organization = $1 AND registration_id = $2 AND revoked_at IS NULL`,
		organization.String(), registration, liveness).Scan(&version, &advertised, &connected)
	if errors.Is(err, pgx.ErrNoRows) {
		return RelayAdvertisement{}, nil
	}
	if err != nil {
		return RelayAdvertisement{}, fmt.Errorf("reading a relay's advertisement: %w", err)
	}

	roster := []struct {
		ID      string `json:"id"`
		Version uint32 `json:"version"`
	}{}
	if len(advertised) > 0 {
		if err = json.Unmarshal(advertised, &roster); err != nil {
			return RelayAdvertisement{}, fmt.Errorf("decoding a relay's advertisement: %w", err)
		}
	}
	capabilities := make(map[string]bool, len(roster))
	for _, entry := range roster {
		capabilities[entry.ID] = true
	}
	return RelayAdvertisement{
		Registered:   true,
		Connected:    connected,
		Version:      version,
		Capabilities: capabilities,
	}, nil
}
