package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The case itself: opening one, reading it back, and stopping it. The vocabulary belongs to
// internal/investigation and this file reconstructs it from rows (ADR-017); nothing here declares
// what an Investigation is.

// investigationColumns is the one place the case's shape is written down. A column added to one
// read and forgotten in another would be a field that is silently always zero, so every read of
// this table names this list and one mapping function serves them all.
const investigationColumns = `
	investigation_id, environment_id, connection_id, episode_key,
	namespace, workload_kind, workload_name, window_start, window_end,
	trigger_kind, requested_by, triggered_at, lifecycle, case_version, current_round,
	created_at, updated_at, terminal_at`

// OpenInvestigation opens a case against one Connection.
//
// The Environment is not read first and then written: it is SELECTed out of the Connection inside
// the insert, so nothing a caller sent contributes to it and a Connection disabled between a check
// and a write cannot leave a case open against it. A client may send an Environment for
// navigation; there is nowhere here for it to arrive, which is stronger than ignoring it.
//
// The Connection must be this organization's, live, answer evidence reads, and be served by a
// Relay. One refusal covers all four, for the reason a refused job's does: which half of a crossed
// boundary was wrong is not a fact worth handing back.
func (p *Placements) OpenInvestigation(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	wanted investigation.New,
) (investigation.Investigation, error) {
	if err := wanted.Validate(); err != nil {
		return investigation.Investigation{}, err
	}
	return audited(ctx, p, principal, organization, audit.ActionInvestigationOpened,
		func(ctx context.Context, transaction pgx.Tx) (
			investigation.Investigation, audit.Target, audit.Detail, error,
		) {
			row := transaction.QueryRow(ctx, `
				INSERT INTO investigation
					(investigation_id, organization, environment_id, connection_id, episode_key,
					 namespace, workload_kind, workload_name, window_start, window_end,
					 trigger_kind, requested_by, triggered_at, lifecycle)
				SELECT $1, $2, connection.environment_id, connection.connection_id, $4,
				       $5, $6, $7, $8, $9, $10, $11, $12, $13
				  FROM connection
				 WHERE connection.connection_id = $3
				   AND connection.organization  = $2
				   AND connection.disabled_at  IS NULL
				   -- 2 evidence, 3 both. A trigger-only Connection answers nothing outbound, so
				   -- there is nothing for a capability read to reach through it.
				   AND connection.role         IN (2, 3)
				   -- Every capability this build dispatches runs in the customer's own
				   -- infrastructure, so a case against a Connection no Relay serves could never
				   -- make a read. Stated as the binding rather than as the locality, because the
				   -- binding is the thing that is needed.
				   AND connection.relay_registration_id IS NOT NULL
				RETURNING `+investigationColumns,
				uuid.New(), organization.String(), wanted.Scope.Connection,
				nullableText(wanted.EpisodeKey), wanted.Scope.Namespace,
				int16(wanted.Scope.WorkloadKind), wanted.Scope.WorkloadName,
				wanted.Window.Start, wanted.Window.End,
				int16(wanted.Trigger.Kind), wanted.Trigger.RequestedBy, wanted.Trigger.At,
				int16(investigation.LifecyclePending))

			opened, err := scanInvestigation(row, organization.String())
			if errors.Is(err, pgx.ErrNoRows) {
				return investigation.Investigation{}, audit.Target{}, nil,
					investigation.ErrConnectionUnusable
			}
			if err != nil {
				return investigation.Investigation{}, audit.Target{}, nil,
					fmt.Errorf("opening an investigation: %w", err)
			}
			if err = claimEpisode(ctx, transaction, organization, opened); err != nil {
				return investigation.Investigation{}, audit.Target{}, nil, err
			}
			// The scope, not the evidence. A case names a namespace and a workload; nothing an
			// investigation reads from a customer's systems reaches this table.
			return opened,
				audit.Target{Kind: audit.TargetInvestigation, ID: opened.ID.String()},
				audit.Detail{
					"connectionId":  opened.Scope.Connection.String(),
					"environmentId": opened.Environment.String(),
					"namespace":     opened.Scope.Namespace,
					"workload":      opened.Scope.WorkloadName,
				}, nil
		})
}

// Investigation reads one case, scoped to the tenant.
//
// The organization is part of the WHERE clause together with the identifier, so a request naming
// one organization's identity and another's investigation returns nothing rather than that case's
// contents. It is one answer for "no such case" and "not yours", because telling them apart lets a
// caller compose path parameters until one of them lands.
func (p *Placements) Investigation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (investigation.Investigation, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.Investigation{}, err
	}

	row := pool.QueryRow(ctx, `
		SELECT `+investigationColumns+`
		  FROM investigation
		 WHERE investigation_id = $1 AND organization = $2`,
		id, organization.String())
	found, err := scanInvestigation(row, organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Investigation{}, investigation.ErrUnknown
	}
	if err != nil {
		return investigation.Investigation{}, fmt.Errorf("reading an investigation: %w", err)
	}
	return found, nil
}

// CaseVersion reads only the version, which is what a conditional request needs.
//
// It is deliberately its own read. Answering "nothing has changed" by assembling a summary and
// then discarding it would make the cheap answer the expensive one, and cheap is the entire reason
// a client can afford to poll.
func (p *Placements) CaseVersion(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (int64, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}

	var version int64
	err = pool.QueryRow(ctx, `
		SELECT case_version FROM investigation
		 WHERE investigation_id = $1 AND organization = $2`,
		id, organization.String()).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, investigation.ErrUnknown
	}
	if err != nil {
		return 0, fmt.Errorf("reading a case version: %w", err)
	}
	return version, nil
}

// InvestigationsAwaitingWork reports the organizations that have a round nothing is executing.
//
// It is the one investigation read that does not take an organization, and it is deliberate for the
// same reason ConnectionByID is: its whole job is to discover which tenants there is work for, so
// there is no organization in the question to resolve a placement from. It reads no tenant data —
// only which tenants have a claimable round — and every claim it leads to is tenant-scoped.
//
// A placement that cannot be read is reported rather than skipped. Continuing would turn one
// database's outage into "there is no work", and a case would sit unclaimed with nothing saying why.
func (p *Placements) InvestigationsAwaitingWork(
	ctx context.Context,
) ([]tenancy.Organization, error) {
	seen := make(map[string]struct{})
	var awaiting []tenancy.Organization

	// A fixed order, so two deployments of the same configuration behave alike.
	for _, name := range p.names() {
		rows, err := p.pools[name].Query(ctx, `
			SELECT DISTINCT organization
			  FROM investigation_round
			 WHERE outcome IS NULL
			   AND (lease_session IS NULL OR lease_expires_at <= now())`)
		if err != nil {
			return nil, fmt.Errorf("looking for investigation work in placement %q: %w", name, err)
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("reading an organization with waiting work: %w", err)
			}
			if _, already := seen[id]; already {
				continue
			}
			organization, parseErr := tenancy.NewOrganization(id)
			if parseErr != nil {
				// Unreachable while every write validates the organization first, and not a
				// fallback: a row whose tenant cannot be named is not one to guess at.
				continue
			}
			seen[id] = struct{}{}
			awaiting = append(awaiting, organization)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, fmt.Errorf("looking for investigation work in placement %q: %w", name, err)
		}
	}
	return awaiting, nil
}

// CancelInvestigation stops a case, its running round, and anything it has dispatched.
//
// All three happen in one transaction, and the round's lease generation is RAISED rather than
// released. Raising it is what makes the stop take effect: the worker still executing that round
// discovers on its next fenced write that it no longer owns the run, and stops. Releasing the
// lease without raising the generation would let that worker keep writing into a case an operator
// has been told is cancelled.
func (p *Placements) CancelInvestigation(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	if !principal.MemberOf(organization) {
		return ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning a cancellation: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	tag, err := transaction.Exec(ctx, `
		UPDATE investigation
		   SET lifecycle = $3, terminal_at = now()
		 WHERE investigation_id = $1
		   AND organization     = $2
		   AND terminal_at     IS NULL`,
		id, organization.String(), int16(investigation.LifecycleCancelled))
	if err != nil {
		return fmt.Errorf("cancelling an investigation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return p.explainRefusedCancellation(ctx, organization, id)
	}

	if _, err = transaction.Exec(ctx, `
		UPDATE investigation_round
		   SET outcome          = $3,
		       terminal_at      = now(),
		       lease_session    = NULL,
		       lease_expires_at = NULL,
		       -- Raised, not merely cleared. Until the generation moves, the execution still
		       -- running this round reads as one nothing has superseded, and it would go on
		       -- writing into a case that has been stopped.
		       lease_epoch      = lease_epoch + 1
		 WHERE investigation_id = $1
		   AND organization     = $2
		   AND outcome         IS NULL`,
		id, organization.String(), int16(investigation.RoundCancelled)); err != nil {
		return fmt.Errorf("cancelling an investigation round: %w", err)
	}

	// Anything already at a relay is asked to stop. The request is advisory — there is one write
	// path into job truth and this is not it — but a cancelled case that goes on paying for reads
	// is a cancellation in name only.
	if _, err = transaction.Exec(ctx, `
		UPDATE relay_job
		   SET status              = CASE WHEN status = 0 THEN 4 ELSE status END,
		       terminal_at         = CASE WHEN status = 0 THEN now() ELSE terminal_at END,
		       cancel_requested_at = COALESCE(cancel_requested_at, now())
		 WHERE organization = $2
		   AND status IN (0, 1)
		   AND job_id IN (SELECT job_id FROM investigation_request
		                   WHERE investigation_id = $1
		                     AND organization     = $2
		                     AND job_id          IS NOT NULL)`,
		id, organization.String()); err != nil {
		return fmt.Errorf("cancelling an investigation's dispatched reads: %w", err)
	}

	// Recorded in the same transaction as the cancellation. A case an operator stopped, with
	// nothing saying who stopped it, is precisely the question this slice exists to answer.
	if err = writeEvent(ctx, transaction, audit.Event{
		Organization:  organization.String(),
		Actor:         principal.Actor(),
		Action:        audit.ActionInvestigationCancelled,
		Target:        audit.Target{Kind: audit.TargetInvestigation, ID: id.String()},
		Outcome:       audit.OutcomeAllowed,
		SourceAddress: principal.SourceAddress(),
		RequestID:     principal.RequestID(),
	}); err != nil {
		return err
	}

	if err = transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing a cancellation: %w", err)
	}
	return nil
}

// explainRefusedCancellation reads why the guarded update matched nothing. The two answers call for
// different things from whoever asked — check the identifier, or read the outcome it already
// reached — so collapsing them would leave them guessing.
func (p *Placements) explainRefusedCancellation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) error {
	if _, err := p.Investigation(ctx, organization, id); err != nil {
		return err
	}
	return investigation.ErrAlreadyTerminal
}

// scanned is satisfied by both a single-row query and a row set.
func scanInvestigation(row scanned, organization string) (investigation.Investigation, error) {
	found := investigation.Investigation{Organization: organization}
	var (
		episode    *string
		kind       int16
		trigger    int16
		lifecycle  int16
		terminalAt *time.Time
	)
	if err := row.Scan(&found.ID, &found.Environment, &found.Scope.Connection, &episode,
		&found.Scope.Namespace, &kind, &found.Scope.WorkloadName,
		&found.Window.Start, &found.Window.End,
		&trigger, &found.Trigger.RequestedBy, &found.Trigger.At,
		&lifecycle, &found.CaseVersion, &found.CurrentRound,
		&found.CreatedAt, &found.UpdatedAt, &terminalAt); err != nil {
		return investigation.Investigation{}, err
	}
	if episode != nil {
		found.EpisodeKey = *episode
	}
	if terminalAt != nil {
		found.TerminalAt = *terminalAt
	}
	found.Scope.WorkloadKind = investigation.WorkloadKind(kind)
	found.Trigger.Kind = investigation.TriggerKind(trigger)
	found.Lifecycle = investigation.Lifecycle(lifecycle)
	return found, nil
}

// nullableText renders an empty string as SQL NULL, which is what "this case belongs to no named
// episode" means in a column an implicit episode leaves unset.
func nullableText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
