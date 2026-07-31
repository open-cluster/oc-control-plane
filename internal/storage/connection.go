package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// ConnectionRole is what a Connection is used for, as a bit set. A trigger Connection
// delivers Signals inbound; an evidence Connection answers bounded capability reads outbound.
// One Connection may be both, which is why this is a set rather than an enumeration of three.
type ConnectionRole int16

const (
	RoleTrigger  ConnectionRole = 1
	RoleEvidence ConnectionRole = 2
	RoleBoth     ConnectionRole = RoleTrigger | RoleEvidence
)

// Includes reports whether this role covers what a caller needs.
func (r ConnectionRole) Includes(needed ConnectionRole) bool { return r&needed == needed }

func (r ConnectionRole) String() string {
	switch r {
	case RoleTrigger:
		return "trigger"
	case RoleEvidence:
		return "evidence"
	case RoleBoth:
		return "both"
	default:
		return "unrecognised"
	}
}

// ExecutionLocality is where work against a Connection runs. A property of the Connection and
// never of the Capability, so the same capability may run centrally for one customer and
// through a Relay for another.
type ExecutionLocality int16

const (
	LocalityControlPlane ExecutionLocality = 1
	LocalityRelay        ExecutionLocality = 2
)

func (l ExecutionLocality) String() string {
	switch l {
	case LocalityControlPlane:
		return "control_plane"
	case LocalityRelay:
		return "relay"
	default:
		return "unrecognised"
	}
}

// Refusals a Connection mutation can produce.
var (
	// ErrConnectionNameTaken reports a name another Connection in the same Environment holds.
	ErrConnectionNameTaken = errors.New("connection name is already used in this environment")
	// ErrConnectionUnknown reports a Connection this organization does not have, or one that
	// has been disabled where the caller needed a live one.
	ErrConnectionUnknown = errors.New("connection unknown")
	// ErrConnectionScope reports a Connection whose Environment or Relay does not belong to
	// the organization the request named. It is the tenant boundary answering, and it is a
	// single error on purpose: which half of a crossed boundary was wrong is not a fact worth
	// returning to whoever tried it.
	ErrConnectionScope = errors.New("connection does not fit the scope it was given")
)

// Connection is one configured instance of an Integration.
type Connection struct {
	ID           uuid.UUID
	Organization string
	Environment  uuid.UUID
	// Integration names the kind of system this is an instance of. The vocabulary is closed
	// and compiled; this column stores which member of it a row is.
	Integration string
	Name        string
	Role        ConnectionRole
	Locality    ExecutionLocality
	// RelayRegistration is the installation that serves this Connection, and is the zero UUID
	// when the locality is control_plane.
	RelayRegistration uuid.UUID
	// SecretDigest is the SHA-256 of the shared secret a trigger source presents. Empty for an
	// evidence-only Connection. The secret itself exists here only at creation and is never
	// read back.
	SecretDigest []byte
	Labels       map[string]string
	DisabledAt   time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Disabled reports whether this Connection has been turned off.
func (c Connection) Disabled() bool { return !c.DisabledAt.IsZero() }

// NewConnection is what an operator asked for. It travels as one value because the fields
// constrain each other: a relay-local Connection needs a Relay, a trigger Connection needs a
// secret, and validating them apart would let an invalid combination reach the database and
// come back as a constraint violation rather than an answer.
type NewConnection struct {
	Environment       uuid.UUID
	Integration       string
	Name              string
	Role              ConnectionRole
	Locality          ExecutionLocality
	RelayRegistration uuid.UUID
	SecretDigest      []byte
	Labels            map[string]string
}

// ConnectionList is a page of an Environment's Connections.
type ConnectionList struct {
	Connections []Connection
	Next        string
}

// CreateConnection records one configured integration inside an Environment.
//
// The Environment and the Relay are not read first and then written against. The composite
// foreign keys mean the insert itself fails when either belongs to another organization, so
// a request combining one tenant's Environment with another's Relay is refused by the
// database rather than by a check that has to be remembered at every call site.
func (p *Placements) CreateConnection(
	ctx context.Context, organization tenancy.Organization, wanted NewConnection,
) (Connection, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return Connection{}, err
	}

	labels, err := json.Marshal(orEmptyLabels(wanted.Labels))
	if err != nil {
		return Connection{}, fmt.Errorf("encoding connection labels: %w", err)
	}

	row := pool.QueryRow(ctx, `
		INSERT INTO connection
			(connection_id, organization, environment_id, integration, name,
			 role, locality, relay_registration_id, secret_digest, labels)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING connection_id, environment_id, integration, name, role, locality,
		          relay_registration_id, secret_digest, labels, disabled_at,
		          created_at, updated_at`,
		uuid.New(), organization.String(), wanted.Environment, wanted.Integration, wanted.Name,
		int16(wanted.Role), int16(wanted.Locality), nullableUUID(wanted.RelayRegistration),
		wanted.SecretDigest, labels)
	created, err := scanConnection(row, organization.String())
	switch {
	case isUniqueViolation(err, "connection_name_is_unique_per_environment"):
		return Connection{}, ErrConnectionNameTaken
	case isForeignKeyViolation(err):
		// The Environment or the Relay is not this organization's. One answer for both: which
		// half of a crossed tenant boundary was wrong is not something to hand back.
		return Connection{}, ErrConnectionScope
	case err != nil:
		return Connection{}, fmt.Errorf("creating a connection: %w", err)
	}
	return created, nil
}

// ConnectionByID resolves a Connection from an opaque identifier alone, across every placement
// this deployment serves, and returns the organization it belongs to.
//
// This is the ONE storage function that does not take an organization, and it is deliberate.
// An inbound delivery names its Connection and nothing else, because a path is chosen by the
// caller and a caller who could name a tenant could try every tenant. With no organization in
// the request there is nothing to resolve a placement from, so each placement is asked in a
// fixed order and the row that is found is itself the authority for the organization and the
// environment. It discovers a tenant rather than trusting one.
//
// The cost is one primary-key lookup per placement, and the count of placements is small by
// construction. ADR-003's second amendment records the two alternatives — encoding the
// placement in the identifier, and a cross-placement routing directory — and why each loses.
func (p *Placements) ConnectionByID(ctx context.Context, id uuid.UUID) (Connection, error) {
	// A fixed order, so two deployments of the same configuration behave alike and a failure
	// is reproducible.
	for _, name := range p.names() {
		var organization string
		row := p.pools[name].QueryRow(ctx, `
			SELECT organization, connection_id, environment_id, integration, name, role,
			       locality, relay_registration_id, secret_digest, labels, disabled_at,
			       created_at, updated_at
			  FROM connection
			 WHERE connection_id = $1`, id)
		found, err := scanConnectionWithOrganization(row, &organization)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			// A placement that cannot be read is reported rather than skipped. Continuing would
			// turn one database's outage into "this connection does not exist", which a source
			// would answer by giving up on a credential that is in fact valid.
			return Connection{}, fmt.Errorf("resolving a connection in placement %q: %w", name, err)
		}
		found.Organization = organization
		return found, nil
	}
	return Connection{}, ErrConnectionUnknown
}

// ConnectionForOrganization reads a Connection an operator named, scoped to their tenant.
func (p *Placements) ConnectionForOrganization(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (Connection, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return Connection{}, err
	}

	row := pool.QueryRow(ctx, `
		SELECT connection_id, environment_id, integration, name, role, locality,
		       relay_registration_id, secret_digest, labels, disabled_at, created_at, updated_at
		  FROM connection
		 WHERE connection_id = $1 AND organization = $2`,
		id, organization.String())
	found, err := scanConnection(row, organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, ErrConnectionUnknown
	}
	if err != nil {
		return Connection{}, fmt.Errorf("reading a connection: %w", err)
	}
	return found, nil
}

// RotateConnectionSecret replaces the digest without disturbing identity or Environment, so a
// suspected disclosure does not mean recreating the Connection and reconfiguring the source.
//
// There is no overlap window in this slice: one digest is live at a time, and a rotation is a
// brief outage the operator schedules. Carrying two is the same shape as the Relay's SPKI pin
// rotation and is added when someone asks for it.
func (p *Placements) RotateConnectionSecret(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID, digest []byte,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	tag, err := pool.Exec(ctx, `
		UPDATE connection
		   SET secret_digest = $3, updated_at = now()
		 WHERE connection_id = $1
		   AND organization  = $2
		   -- Only a trigger Connection has a secret to rotate. An evidence-only one is reached
		   -- outbound and presents nothing, and giving it a digest would break the constraint
		   -- that says so.
		   AND role IN (1, 3)`,
		id, organization.String(), digest)
	if err != nil {
		return fmt.Errorf("rotating a connection secret: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConnectionUnknown
	}
	return nil
}

// SetConnectionDisabled turns a Connection off or back on without deleting it, so an operator
// can stop using a source without losing the record of what it produced.
func (p *Placements) SetConnectionDisabled(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID, disabled bool,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	var disabledAt *time.Time
	if disabled {
		now := time.Now()
		disabledAt = &now
	}
	tag, err := pool.Exec(ctx, `
		UPDATE connection
		   -- Coalesced when disabling, so asking twice does not move the moment it was first
		   -- turned off.
		   SET disabled_at = CASE WHEN $3::timestamptz IS NULL
		                          THEN NULL ELSE COALESCE(disabled_at, $3::timestamptz) END,
		       updated_at  = now()
		 WHERE connection_id = $1 AND organization = $2`,
		id, organization.String(), disabledAt)
	if err != nil {
		return fmt.Errorf("setting a connection's disabled state: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConnectionUnknown
	}
	return nil
}

// ListConnections returns the Connections in one Environment, newest first.
//
// The Environment is part of the WHERE clause together with the organization, so naming one
// tenant's Environment while authenticated for another returns nothing rather than that
// Environment's contents.
func (p *Placements) ListConnections(
	ctx context.Context, organization tenancy.Organization, environment uuid.UUID, page Page,
) (ConnectionList, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return ConnectionList{}, err
	}
	limit := pageLimit(page.Limit)
	after, afterID, err := decodeCursor(page.After)
	if err != nil {
		return ConnectionList{}, err
	}

	rows, err := pool.Query(ctx, `
		SELECT connection_id, environment_id, integration, name, role, locality,
		       relay_registration_id, secret_digest, labels, disabled_at, created_at, updated_at
		  FROM connection
		 WHERE organization   = $1
		   AND environment_id = $2
		   AND ($4::timestamptz IS NULL
		        OR (created_at, connection_id) < ($4::timestamptz, $5::uuid))
		 ORDER BY created_at DESC, connection_id DESC
		 LIMIT $3`,
		organization.String(), environment, limit+1, after, afterID)
	if err != nil {
		return ConnectionList{}, fmt.Errorf("listing connections: %w", err)
	}
	defer rows.Close()

	list := ConnectionList{Connections: make([]Connection, 0, limit)}
	for rows.Next() {
		found, scanErr := scanConnection(rows, organization.String())
		if scanErr != nil {
			return ConnectionList{}, scanErr
		}
		if len(list.Connections) == limit {
			last := list.Connections[limit-1]
			list.Next = encodeCursor(last.CreatedAt, last.ID)
			break
		}
		list.Connections = append(list.Connections, found)
	}
	if err = rows.Err(); err != nil {
		return ConnectionList{}, fmt.Errorf("listing connections: %w", err)
	}
	return list, nil
}

// scanned is what both a single-row query and a row set offer. Naming it here lets one mapping
// function serve every read of this table, so a column added to one query and forgotten in
// another is a compile error rather than a field that is silently always zero.
type scanned interface {
	Scan(destination ...any) error
}

// scanConnection maps one row. The nullable columns are read through pointers and flattened to
// zero values, because a caller asking whether a Connection is disabled should not have to
// dereference to find out.
func scanConnection(row scanned, organization string) (Connection, error) {
	found := Connection{Organization: organization}
	var (
		relay      *uuid.UUID
		labels     []byte
		disabledAt *time.Time
	)
	if err := row.Scan(&found.ID, &found.Environment, &found.Integration, &found.Name,
		&found.Role, &found.Locality, &relay, &found.SecretDigest, &labels,
		&disabledAt, &found.CreatedAt, &found.UpdatedAt); err != nil {
		return Connection{}, err
	}
	return finish(found, relay, labels, disabledAt)
}

// scanConnectionWithOrganization is scanConnection for the one read that discovers the tenant
// rather than being given it.
func scanConnectionWithOrganization(row scanned, organization *string) (Connection, error) {
	var (
		found      Connection
		relay      *uuid.UUID
		labels     []byte
		disabledAt *time.Time
	)
	if err := row.Scan(organization, &found.ID, &found.Environment, &found.Integration,
		&found.Name, &found.Role, &found.Locality, &relay, &found.SecretDigest, &labels,
		&disabledAt, &found.CreatedAt, &found.UpdatedAt); err != nil {
		return Connection{}, err
	}
	return finish(found, relay, labels, disabledAt)
}

func finish(
	found Connection, relay *uuid.UUID, labels []byte, disabledAt *time.Time,
) (Connection, error) {
	if relay != nil {
		found.RelayRegistration = *relay
	}
	if disabledAt != nil {
		found.DisabledAt = *disabledAt
	}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &found.Labels); err != nil {
			return Connection{}, fmt.Errorf("decoding connection labels: %w", err)
		}
	}
	if found.Labels == nil {
		found.Labels = map[string]string{}
	}
	return found, nil
}

func orEmptyLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return labels
}

// nullableUUID renders the zero UUID as SQL NULL, which is what "no Relay serves this" means
// in a column the schema constrains against the locality.
func nullableUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
