package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// DefaultEnvironmentName is what the Environment created with an Organization is called. It
// may be renamed; what may not change is that one exists, because every downstream concept
// treats an Environment as present from its first migration rather than handling its absence.
const DefaultEnvironmentName = "Default"

// Refusals an Environment mutation can produce. Each is a different thing for a caller to do
// about it, which is why they are distinguished here and a relay enrolment's are not: an
// operator acting on their own tenant is not learning which half of a guess was right.
var (
	// ErrEnvironmentNameTaken reports a name another Environment in this organization holds.
	ErrEnvironmentNameTaken = errors.New("environment name is already used in this organization")
	// ErrEnvironmentUnknown reports an Environment this organization does not have.
	ErrEnvironmentUnknown = errors.New("environment unknown")
	// ErrEnvironmentIsDefault reports an attempt to delete the Default. It is undeletable so
	// that the guarantee nothing downstream has to handle its absence actually holds.
	ErrEnvironmentIsDefault = errors.New("the default environment cannot be deleted")
	// ErrEnvironmentInUse reports an Environment that still groups Connections. A scope cannot
	// be removed from under the things that inherit it.
	ErrEnvironmentInUse = errors.New("environment still has connections")
)

// Environment is a customer-named scope grouping Connections.
type Environment struct {
	ID   uuid.UUID
	Name string
	// IsDefault marks the one created with the Organization.
	IsDefault bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// EnvironmentList is a page of an organization's Environments.
type EnvironmentList struct {
	Environments []Environment
	Next         string
}

// EnsureDefaultEnvironment returns the organization's Default Environment, creating it if it
// is not there yet.
//
// The specification says a Default is created "with the Organization, in the same
// transaction". Organizations are not creatable through this control plane — they arrive from
// an external identity provider and their placement is configuration — so there is no
// organization-creating transaction to join. This is where that promise is kept instead: the
// first call for an organization creates it, and every later one finds it.
//
// Two concurrent first calls do not produce two Defaults. The partial unique index decides,
// and the loser reads the winner's row rather than failing, because both callers asked for the
// same thing and both should get it.
// It writes no audit event, and that is deliberate: creating the Default is the product
// keeping a promise it made to itself, not an act an operator performed. Recording it would
// make the trail read as though somebody created a scope every time a page was opened.
func (p *Placements) EnsureDefaultEnvironment(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
) (Environment, error) {
	if !principal.MemberOf(organization) {
		return Environment{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return Environment{}, err
	}

	var environment Environment
	err = pool.QueryRow(ctx, `
		INSERT INTO environment (environment_id, organization, name, is_default)
		VALUES ($1, $2, $3, TRUE)
		ON CONFLICT DO NOTHING
		RETURNING environment_id, name, is_default, created_at, updated_at`,
		uuid.New(), organization.String(), DefaultEnvironmentName).
		Scan(&environment.ID, &environment.Name, &environment.IsDefault,
			&environment.CreatedAt, &environment.UpdatedAt)
	if err == nil {
		return environment, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Environment{}, fmt.Errorf("creating the default environment: %w", err)
	}

	// Nothing was inserted, so one already exists — either from an earlier call or from the
	// concurrent one that won. Read it.
	err = pool.QueryRow(ctx, `
		SELECT environment_id, name, is_default, created_at, updated_at
		  FROM environment
		 WHERE organization = $1 AND is_default`,
		organization.String()).
		Scan(&environment.ID, &environment.Name, &environment.IsDefault,
			&environment.CreatedAt, &environment.UpdatedAt)
	if err != nil {
		return Environment{}, fmt.Errorf("reading the default environment: %w", err)
	}
	return environment, nil
}

// CreateEnvironment adds a scope with the name the operator chose, and the record of who did.
func (p *Placements) CreateEnvironment(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	name string,
) (Environment, error) {
	return audited(ctx, p, principal, organization, audit.ActionEnvironmentCreated,
		func(ctx context.Context, transaction pgx.Tx) (
			Environment, audit.Target, audit.Detail, error,
		) {
			var environment Environment
			err := transaction.QueryRow(ctx, `
				INSERT INTO environment (environment_id, organization, name)
				VALUES ($1, $2, $3)
				RETURNING environment_id, name, is_default, created_at, updated_at`,
				uuid.New(), organization.String(), name).
				Scan(&environment.ID, &environment.Name, &environment.IsDefault,
					&environment.CreatedAt, &environment.UpdatedAt)
			if isUniqueViolation(err, "environment_name_is_unique_per_organization") {
				return Environment{}, audit.Target{}, nil, ErrEnvironmentNameTaken
			}
			if err != nil {
				return Environment{}, audit.Target{}, nil,
					fmt.Errorf("creating an environment: %w", err)
			}
			return environment,
				audit.Target{Kind: audit.TargetEnvironment, ID: environment.ID.String()},
				audit.Detail{"name": environment.Name}, nil
		})
}

// RenameEnvironment changes what a scope is called and nothing else. The identity is
// untouched, so everything referring to it keeps referring to it — which is the whole reason
// the name is an attribute rather than the key.
func (p *Placements) RenameEnvironment(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, name string,
) (Environment, error) {
	return audited(ctx, p, principal, organization, audit.ActionEnvironmentRenamed,
		func(ctx context.Context, transaction pgx.Tx) (
			Environment, audit.Target, audit.Detail, error,
		) {
			// The previous name is read under FOR UPDATE rather than joined into the UPDATE,
			// so the value the record reports as "before" is the one this statement replaced
			// and not one a concurrent rename wrote in between.
			var before string
			err := transaction.QueryRow(ctx, `
				SELECT name FROM environment
				 WHERE environment_id = $1 AND organization = $2 FOR UPDATE`,
				id, organization.String()).Scan(&before)
			if errors.Is(err, pgx.ErrNoRows) {
				return Environment{}, audit.Target{}, nil, ErrEnvironmentUnknown
			}
			if err != nil {
				return Environment{}, audit.Target{}, nil,
					fmt.Errorf("reading an environment: %w", err)
			}

			var environment Environment
			err = transaction.QueryRow(ctx, `
				UPDATE environment
				   SET name = $3, updated_at = now()
				 WHERE environment_id = $1 AND organization = $2
				RETURNING environment_id, name, is_default, created_at, updated_at`,
				id, organization.String(), name).
				Scan(&environment.ID, &environment.Name, &environment.IsDefault,
					&environment.CreatedAt, &environment.UpdatedAt)
			if isUniqueViolation(err, "environment_name_is_unique_per_organization") {
				return Environment{}, audit.Target{}, nil, ErrEnvironmentNameTaken
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return Environment{}, audit.Target{}, nil, ErrEnvironmentUnknown
			}
			if err != nil {
				return Environment{}, audit.Target{}, nil,
					fmt.Errorf("renaming an environment: %w", err)
			}
			return environment,
				audit.Target{Kind: audit.TargetEnvironment, ID: environment.ID.String()},
				audit.Detail{"before": before, "after": environment.Name}, nil
		})
}

// DeleteEnvironment removes a scope that nothing is inside.
//
// Both refusals are decided in one statement rather than read and then acted on, so a
// Connection created between the check and the delete cannot lose its Environment.
func (p *Placements) DeleteEnvironment(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionEnvironmentDeleted,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			tag, err := transaction.Exec(ctx, `
				DELETE FROM environment
				 WHERE environment_id = $1
				   AND organization   = $2
				   AND NOT is_default
				   AND NOT EXISTS (SELECT 1 FROM connection
				                    WHERE connection.environment_id = environment.environment_id)`,
				id, organization.String())
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("deleting an environment: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return struct{}{}, audit.Target{}, nil,
					p.explainRefusedDelete(ctx, organization, id)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetEnvironment, ID: id.String()}, nil, nil
		})
	return err
}

// explainRefusedDelete reads why the guarded delete matched nothing. The three answers call
// for three different things from an operator — rename your mind, move the Connections, or
// check the identifier — so collapsing them into one would leave them guessing.
func (p *Placements) explainRefusedDelete(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	var (
		isDefault   bool
		connections int
	)
	err = pool.QueryRow(ctx, `
		SELECT environment.is_default,
		       (SELECT count(*) FROM connection
		         WHERE connection.environment_id = environment.environment_id)
		  FROM environment
		 WHERE environment_id = $1 AND organization = $2`,
		id, organization.String()).Scan(&isDefault, &connections)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrEnvironmentUnknown
	case err != nil:
		return fmt.Errorf("auditing a refused environment delete: %w", err)
	case isDefault:
		return ErrEnvironmentIsDefault
	case connections > 0:
		return ErrEnvironmentInUse
	default:
		// The row exists, is not the Default, and holds nothing — so the delete would have
		// matched. Something raced it and won, which means the state asked for now holds.
		return nil
	}
}

// ListEnvironments returns an organization's scopes, newest first.
func (p *Placements) ListEnvironments(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization, page Page,
) (EnvironmentList, error) {
	if !principal.MemberOf(organization) {
		return EnvironmentList{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return EnvironmentList{}, err
	}
	limit := pageLimit(page.Limit)
	after, afterID, err := decodeCursor(page.After)
	if err != nil {
		return EnvironmentList{}, err
	}

	rows, err := pool.Query(ctx, `
		SELECT environment_id, name, is_default, created_at, updated_at
		  FROM environment
		 WHERE organization = $1
		   AND ($3::timestamptz IS NULL
		        OR (created_at, environment_id) < ($3::timestamptz, $4::uuid))
		 ORDER BY created_at DESC, environment_id DESC
		 LIMIT $2`,
		organization.String(), limit+1, after, afterID)
	if err != nil {
		return EnvironmentList{}, fmt.Errorf("listing environments: %w", err)
	}
	defer rows.Close()

	list := EnvironmentList{Environments: make([]Environment, 0, limit)}
	for rows.Next() {
		var environment Environment
		if err = rows.Scan(&environment.ID, &environment.Name, &environment.IsDefault,
			&environment.CreatedAt, &environment.UpdatedAt); err != nil {
			return EnvironmentList{}, fmt.Errorf("reading an environment: %w", err)
		}
		if len(list.Environments) == limit {
			last := list.Environments[limit-1]
			list.Next = encodeCursor(last.CreatedAt, last.ID)
			break
		}
		list.Environments = append(list.Environments, environment)
	}
	if err = rows.Err(); err != nil {
		return EnvironmentList{}, fmt.Errorf("listing environments: %w", err)
	}
	return list, nil
}

// isUniqueViolation reports whether an error is the named unique constraint failing. The
// constraint is named rather than the code alone checked, because a table has several and
// reporting the wrong one to an operator sends them to fix something that is not broken.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// isForeignKeyViolation reports whether an error is a foreign key failing. Not named, unlike
// the unique constraints: every foreign key on these tables says the same thing to a caller —
// the row you named is not in the scope you named it from — and distinguishing them would
// hand back which half of a crossed tenant boundary was wrong.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
