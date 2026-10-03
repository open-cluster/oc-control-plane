package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrEnrolmentRefused = errors.New("relay enrolment refused")

type EnrolmentRefusal int

const (
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

type RelayEnrolment struct {
	TokenDigest        []byte
	CredentialDigest   []byte
	ClusterFingerprint string
	RelayVersion       string
	ProtocolVersion    uint32
	Capabilities       []byte
}

func (p *Database) EnrolRelay(
	ctx context.Context,
	organization uuid.UUID,
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

func spendBootstrapToken(
	ctx context.Context,
	transaction pgx.Tx,
	organization uuid.UUID,
	tokenDigest []byte,
) (bool, error) {
	tag, err := transaction.Exec(ctx, `
		UPDATE relay_bootstrap_token
		   SET consumed_at = now()
		 WHERE bootstrap_digest = $1
		   AND org_id = $2
		   AND consumed_at IS NULL
		   AND revoked_at IS NULL
		   AND expires_at > now()`,
		tokenDigest, organization)
	if err != nil {
		return false, fmt.Errorf("consuming bootstrap token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func explainUnspendableToken(ctx context.Context, transaction pgx.Tx,
	organization uuid.UUID, tokenDigest []byte) (EnrolmentRefusal, error) {
	var (
		tokenOrganization uuid.UUID
		consumed          bool
		revoked           bool
		expired           bool
	)
	err := transaction.QueryRow(ctx, `
		SELECT org_id,
		       consumed_at IS NOT NULL,
		       revoked_at IS NOT NULL,
		       expires_at <= now()
		  FROM relay_bootstrap_token
		 WHERE bootstrap_digest = $1`, tokenDigest).
		Scan(&tokenOrganization, &consumed, &revoked, &expired)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return RefusalTokenUnknown, nil
	case err != nil:
		return RefusalNone, fmt.Errorf("auditing refused enrolment: %w", err)
	case tokenOrganization != organization:
		return RefusalOrganizationMismatch, nil
	case revoked:
		return RefusalTokenRevoked, nil
	case consumed:
		return RefusalTokenAlreadyConsumed, nil
	case expired:
		return RefusalTokenExpired, nil
	default:
		return RefusalTokenAlreadyConsumed, nil
	}
}

type relayRegistration struct {
	id           uuid.UUID
	organization uuid.UUID
	enrolment    RelayEnrolment
}

func insertRegistration(ctx context.Context, transaction pgx.Tx, registration relayRegistration) error {
	enrolment := registration.enrolment
	var protocolVersion any
	if enrolment.ProtocolVersion != 0 {
		protocolVersion = int64(enrolment.ProtocolVersion)
	}
	_, err := transaction.Exec(ctx, `
		INSERT INTO relay_registration
			(registration_id, org_id, credential_digest,
			 cluster_fingerprint, relay_version, protocol_version, capabilities)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		registration.id, registration.organization, enrolment.CredentialDigest,
		enrolment.ClusterFingerprint, enrolment.RelayVersion, protocolVersion,
		enrolment.Capabilities)
	if err != nil {
		return fmt.Errorf("recording relay registration: %w", err)
	}
	return nil
}

func (p *Database) IssueBootstrapToken(
	ctx context.Context,
	organization uuid.UUID,
	tokenDigest []byte,
	expiresAt time.Time,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO relay_bootstrap_token (bootstrap_digest, org_id, expires_at)
		VALUES ($1, $2, $3)`,
		tokenDigest, organization, expiresAt)
	if err != nil {
		return fmt.Errorf("issuing bootstrap token: %w", err)
	}
	return nil
}

func (p *Database) VerifyRelayCredential(
	ctx context.Context,
	organization uuid.UUID,
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
		   AND org_id    = $2
		   AND revoked_at IS NULL`,
		registrationID, organization, credentialDigest).Scan(&matches)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("verifying relay credential: %w", err)
	}
	return matches, nil
}
