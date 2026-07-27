package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// ErrEnrolmentRefused reports that a bootstrap token did not entitle its presenter to an
// identity. Every reason produces this one error, because telling an unknown token from a
// spent one is exactly what lets an attacker probe for valid tokens. The distinction is
// carried alongside it for the audit trail and must not reach the presenter.
var ErrEnrolmentRefused = errors.New("relay enrolment refused")

// EnrolmentRefusal is why an enrolment was refused. It exists for the server-side audit
// trail; it is never rendered into a response.
type EnrolmentRefusal int

const (
	// RefusalNone is the zero value, used when no refusal occurred.
	RefusalNone EnrolmentRefusal = iota
	RefusalTokenUnknown
	RefusalTokenExpired
	RefusalTokenAlreadyConsumed
	RefusalTokenRevoked
	RefusalOrganizationMismatch
)

func (r EnrolmentRefusal) String() string {
	switch r {
	case RefusalNone:
		return "none"
	case RefusalTokenUnknown:
		return "token unknown"
	case RefusalTokenExpired:
		return "token expired"
	case RefusalTokenAlreadyConsumed:
		return "token already consumed"
	case RefusalTokenRevoked:
		return "token revoked"
	case RefusalOrganizationMismatch:
		return "token issued for another organization"
	default:
		return "unrecognised"
	}
}

// RelayEnrolment is everything a relay presents at registration, already reduced to what is
// durable. The bootstrap token and the credential appear only as digests: this type crosses
// no boundary carrying a secret in a form that could be stored or logged by accident.
type RelayEnrolment struct {
	TokenDigest        []byte
	CredentialDigest   []byte
	ClusterFingerprint string
	RelayVersion       string
	Capabilities       []byte
}

// EnrolRelay spends the bootstrap token and records the identity it mints, in one
// transaction. Both happen or neither does: a token consumed without an identity issued
// would strand an operator with an installation that can never register, and an identity
// without the token spent would let one token enrol a second relay.
//
// Concurrency is resolved by the database rather than by application locking. Two
// simultaneous presentations of one token serialise on its row, so exactly one observes it
// unspent and the other is refused. An application-level guard would be a second source of
// truth for something the row already decides.
func (p *Placements) EnrolRelay(
	ctx context.Context,
	organization tenancy.Organization,
	enrolment RelayEnrolment,
) (uuid.UUID, EnrolmentRefusal, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return uuid.Nil, RefusalNone, err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, RefusalNone, fmt.Errorf("beginning enrolment: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	spent, err := spendBootstrapToken(ctx, transaction, organization, enrolment.TokenDigest)
	if err != nil {
		return uuid.Nil, RefusalNone, err
	}
	if !spent {
		// The token was not spendable. Read why, for the audit trail only, from inside the
		// same transaction so the explanation describes the state the guard actually saw.
		refusal, reasonErr := explainUnspendableToken(ctx, transaction, organization, enrolment.TokenDigest)
		if reasonErr != nil {
			return uuid.Nil, RefusalNone, reasonErr
		}
		return uuid.Nil, refusal, ErrEnrolmentRefused
	}

	registration := relayRegistration{
		id:           uuid.New(),
		organization: organization,
		enrolment:    enrolment,
	}
	if err = insertRegistration(ctx, transaction, registration); err != nil {
		return uuid.Nil, RefusalNone, err
	}
	registrationID := registration.id
	if err = transaction.Commit(ctx); err != nil {
		return uuid.Nil, RefusalNone, fmt.Errorf("committing enrolment: %w", err)
	}
	return registrationID, RefusalNone, nil
}

// spendBootstrapToken marks the token consumed, reporting whether this call was the one that
// spent it. Every condition is in the WHERE clause, so the decision is the database's and
// cannot be raced between a read and a write.
func spendBootstrapToken(
	ctx context.Context,
	transaction pgx.Tx,
	organization tenancy.Organization,
	tokenDigest []byte,
) (bool, error) {
	tag, err := transaction.Exec(ctx, `
		UPDATE relay_bootstrap_token
		   SET consumed_at = now()
		 WHERE token_digest = $1
		   AND organization = $2
		   AND consumed_at IS NULL
		   AND revoked_at IS NULL
		   AND expires_at > now()`,
		tokenDigest, organization.String())
	if err != nil {
		return false, fmt.Errorf("consuming bootstrap token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// explainUnspendableToken reports why the guarded update matched nothing. Its result reaches
// the audit trail and never the caller of Register.
func explainUnspendableToken(ctx context.Context, transaction pgx.Tx,
	organization tenancy.Organization, tokenDigest []byte) (EnrolmentRefusal, error) {
	var (
		tokenOrganization string
		consumed          bool
		revoked           bool
		expired           bool
	)
	err := transaction.QueryRow(ctx, `
		SELECT organization,
		       consumed_at IS NOT NULL,
		       revoked_at IS NOT NULL,
		       expires_at <= now()
		  FROM relay_bootstrap_token
		 WHERE token_digest = $1`, tokenDigest).
		Scan(&tokenOrganization, &consumed, &revoked, &expired)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return RefusalTokenUnknown, nil
	case err != nil:
		return RefusalNone, fmt.Errorf("auditing refused enrolment: %w", err)
	case tokenOrganization != organization.String():
		return RefusalOrganizationMismatch, nil
	case revoked:
		return RefusalTokenRevoked, nil
	case consumed:
		return RefusalTokenAlreadyConsumed, nil
	case expired:
		return RefusalTokenExpired, nil
	default:
		// The row is spendable now, so it was spent and rolled back between the update and
		// this read. Reporting it as already consumed is the honest description.
		return RefusalTokenAlreadyConsumed, nil
	}
}

// relayRegistration is the row about to be written, bundled so the insert takes what it
// needs as one value rather than as a widening parameter list.
type relayRegistration struct {
	id           uuid.UUID
	organization tenancy.Organization
	enrolment    RelayEnrolment
}

func insertRegistration(ctx context.Context, transaction pgx.Tx, registration relayRegistration) error {
	enrolment := registration.enrolment
	_, err := transaction.Exec(ctx, `
		INSERT INTO relay_registration
			(registration_id, organization, credential_digest,
			 cluster_fingerprint, relay_version, capabilities)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		registration.id, registration.organization.String(), enrolment.CredentialDigest,
		enrolment.ClusterFingerprint, enrolment.RelayVersion, enrolment.Capabilities)
	if err != nil {
		return fmt.Errorf("recording relay registration: %w", err)
	}
	return nil
}

// IssueBootstrapToken records a single-use enrolment token for an organization. Only the
// digest is stored, so this is the one moment the token exists here; the caller shows it to
// the operator once and keeps no copy either.
func (p *Placements) IssueBootstrapToken(
	ctx context.Context,
	organization tenancy.Organization,
	tokenDigest []byte,
	expiresAt time.Time,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO relay_bootstrap_token (token_digest, organization, expires_at)
		VALUES ($1, $2, $3)`,
		tokenDigest, organization.String(), expiresAt)
	if err != nil {
		return fmt.Errorf("issuing bootstrap token: %w", err)
	}
	return nil
}

// VerifyRelayCredential reports whether a credential digest matches a live registration.
// It fails closed: a revoked registration authenticates nothing, and an unknown one is
// indistinguishable from a wrong credential.
func (p *Placements) VerifyRelayCredential(
	ctx context.Context,
	organization tenancy.Organization,
	registrationID uuid.UUID,
	credentialDigest []byte,
) (bool, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return false, err
	}
	var matches bool
	err = pool.QueryRow(ctx, `
		SELECT credential_digest = $3
		  FROM relay_registration
		 WHERE registration_id = $1
		   AND organization    = $2
		   AND revoked_at IS NULL`,
		registrationID, organization.String(), credentialDigest).Scan(&matches)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("verifying relay credential: %w", err)
	}
	return matches, nil
}
