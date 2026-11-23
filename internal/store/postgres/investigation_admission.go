package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func (p *Database) CreateInvestigation(
	ctx context.Context, principal authz.Principal, org uuid.UUID,
	wanted investigation.NewInvestigation, maxPending int,
) (investigation.Investigation, error) {
	if principal.Organization() != org || !principal.HavePermission(authz.InvestigationOpen) {
		return investigation.Investigation{}, ErrNotAMember
	}
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return investigation.Investigation{}, err
	}
	var created investigation.Investigation
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		created, err = createInvestigation(ctx, tx, principal, org, wanted, maxPending)
		return err
	})
	if err != nil {
		return investigation.Investigation{}, err
	}
	return created, nil
}

func createInvestigation(ctx context.Context, tx pgx.Tx, principal authz.Principal, org uuid.UUID,
	wanted investigation.NewInvestigation, maxPending int,
) (investigation.Investigation, error) {
	if wanted.RequestKey != "" || wanted.ConversationID != uuid.Nil || maxPending > 0 {
		if err := lockOrganizationAdmissions(ctx, tx, org); err != nil {
			return investigation.Investigation{}, err
		}
	}
	var hash []byte
	if wanted.RequestKey != "" {
		hash = wanted.RequestHash[:]
		prior, err := replayInvestigation(ctx, tx, org, principal.UserID(), wanted.RequestKey, hash)
		if !errors.Is(err, pgx.ErrNoRows) {
			return prior, err
		}
	}
	if wanted.ConversationID != uuid.Nil {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT conversation_id FROM conversation
			WHERE org_id = $1 AND conversation_id = $2 AND source = 'web' AND deleted_at IS NULL FOR SHARE`,
			org, wanted.ConversationID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return investigation.Investigation{}, conversation.ErrUnknown
		}
		if err != nil {
			return investigation.Investigation{}, err
		}
	}
	if wanted.ReplayOnly {
		return investigation.Investigation{}, investigation.ErrAgentUnavailable
	}
	if err := checkWaitingInvestigationCapacity(ctx, tx, org, maxPending, 1); err != nil {
		if errors.Is(err, ErrInvestigationCapacity) {
			return investigation.Investigation{}, investigation.ErrQueueFull
		}
		return investigation.Investigation{}, err
	}
	created, err := scanInvestigation(tx.QueryRow(ctx, `
		INSERT INTO investigation AS i (investigation_id, org_id, incident_id, conversation_id,
		                                 subject, window_from, window_until, requested_by, request_key, request_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), $10)
		RETURNING `+investigationColumns,
		uuid.New(), org, nullableUUID(wanted.IncidentID), nullableUUID(wanted.ConversationID), wanted.Subject,
		wanted.WindowFrom, wanted.WindowUntil, nullableUUID(principal.UserID()), wanted.RequestKey, hash))
	if isForeignKeyViolation(err) {
		return investigation.Investigation{}, investigation.ErrIncidentUnknown
	}
	if err != nil {
		return investigation.Investigation{}, fmt.Errorf("creating an Investigation: %w", err)
	}
	created.ExecutionID, err = queueInvestigation(ctx, tx, org, created.ID)
	if err != nil {
		return investigation.Investigation{}, err
	}
	err = writeEvent(ctx, tx, audit.Event{
		Organization:  org.String(),
		Actor:         principal.Actor(),
		Action:        audit.ActionInvestigationOpened,
		Target:        audit.Target{Kind: audit.TargetInvestigation, ID: created.ID.String()},
		Outcome:       audit.OutcomeAllowed,
		SourceAddress: principal.SourceAddress(),
		RequestID:     principal.RequestID(),
	})
	return created, err
}

func replayInvestigation(ctx context.Context, tx pgx.Tx, org, requester uuid.UUID, key string,
	hash []byte,
) (investigation.Investigation, error) {
	var executionID, investigationID uuid.UUID
	var identical bool
	err := tx.QueryRow(ctx, `SELECT i.investigation_id, e.execution_id, i.request_hash = $4
		FROM investigation i JOIN agent_execution e USING (org_id, investigation_id)
		WHERE i.org_id = $1 AND i.requested_by = $2 AND i.request_key = $3`,
		org, requester, key, hash).Scan(&investigationID, &executionID, &identical)
	if err != nil {
		return investigation.Investigation{}, err
	}
	if !identical {
		return investigation.Investigation{}, investigation.ErrRequestConflict
	}
	prior, err := scanInvestigation(tx.QueryRow(ctx, `SELECT `+investigationColumns+`
		FROM investigation i WHERE i.org_id = $1 AND i.investigation_id = $2 AND `+webInvestigationSource,
		org, investigationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Investigation{}, investigation.ErrUnknown
	}
	prior.ExecutionID = executionID
	return prior, err
}
