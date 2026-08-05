package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
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
	// State is where this Connection has got to, as observed rather than as declared. It is
	// separate from DisabledAt on purpose: collapsing the two would lose whether a Connection
	// that is turned off was working when it was turned off, which is the fact somebody needs
	// when they turn it back on during an incident.
	State ConnectionState
	// ConfigurationRevision increments on every accepted change, so "it changed" is answerable
	// and a validation result can name the revision it was a result about.
	ConfigurationRevision int
	// Configuration is the provider-specific settings, shaped by the Integration definition's
	// JSON Schema. It NEVER holds a credential: a write-only field is stripped before this is
	// written and lives as a digest and the metadata below.
	Configuration map[string]any
	// Credential is what a read returns ABOUT the credential, which is never the credential.
	Credential Credential
	DisabledAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Disabled reports whether this Connection has been turned off.
func (c Connection) Disabled() bool { return !c.DisabledAt.IsZero() }

// ConnectionState is where a Connection has got to. It is persisted as the integer in the
// column and the values are frozen by the gate in internal/gates.
type ConnectionState int16

const (
	// ConnectionConfigured is the state a Connection is created in: it exists and nothing has
	// checked it.
	ConnectionConfigured ConnectionState = iota + 1
	ConnectionValidating
	ConnectionActive
	// ConnectionDegraded is the state that makes partial success visible on the record itself:
	// the credential worked and some of what this provider offers is unavailable.
	ConnectionDegraded
	ConnectionFailed
)

func (s ConnectionState) String() string {
	switch s {
	case ConnectionConfigured:
		return "configured"
	case ConnectionValidating:
		return "validating"
	case ConnectionActive:
		return "active"
	case ConnectionDegraded:
		return "degraded"
	case ConnectionFailed:
		return "failed"
	default:
		return "unrecognised"
	}
}

// Credential is everything a read may say about the secret in use, and it is deliberately
// everything EXCEPT the secret.
//
// It exists because "which credential is this, and is it the one that expired" is a real
// question an operator has, and answering it by showing them the credential would put a secret
// in a browser. Identity is enough to answer it; content is not needed and is not offered.
type Credential struct {
	// Method is how the far end is authenticated — `shared_secret`, `bearer_token`. Empty when
	// this Connection holds no credential at all, which an evidence Connection served by a
	// Relay's own service account legitimately does not.
	Method string
	// Reference is an opaque pointer to where the secret lives. It is not a path a caller can
	// dereference and is not a URL; it exists so a support conversation can name one credential
	// rather than describe it.
	Reference string
	// Fingerprint is a MINTED identity for this credential, not a hash of it. A truncated hash
	// would let anyone holding a database dump confirm a guess offline, which is the exact
	// property digest-only storage exists to deny. What an operator needs is to tell one
	// credential from the next, and a minted value does that and leaks nothing.
	Fingerprint string
	CreatedAt   time.Time
	// RotatedAt is zero for a credential that has never been rotated, which is distinguishable
	// from one rotated at creation time.
	RotatedAt time.Time
	// ExpiresAt is zero when the credential has no stated expiry. That is a fact about the
	// credential rather than a missing value: a shared secret this platform minted does not
	// expire on its own.
	ExpiresAt time.Time
}

// Held reports whether this Connection carries a credential at all.
func (c Credential) Held() bool { return c.Method != "" }

// connectionColumns is every column a Connection is read from, named once.
//
// One list rather than five copies, because a column added to one query and forgotten in
// another is a field that is silently always zero — and a Connection whose state always reads
// `configured` would look like a Connection nobody has validated rather than like a bug.
const connectionColumns = `connection_id, environment_id, integration, name, role, locality,
	       relay_registration_id, secret_digest, labels, disabled_at, created_at, updated_at,
	       state, configuration_revision, configuration, credential_method, credential_reference,
	       credential_fingerprint, credential_created_at, credential_rotated_at,
	       credential_expires_at`

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
	// Configuration is the provider-specific settings, already checked against the Integration
	// definition's schema and already stripped of anything write-only.
	Configuration map[string]any
	// Credential is the metadata describing the secret, never the secret. The digest above is
	// what authenticates; this is what an operator reads to tell one credential from the next.
	Credential Credential
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
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	wanted NewConnection,
) (Connection, error) {
	return audited(ctx, p, principal, organization, audit.ActionConnectionCreated,
		func(ctx context.Context, transaction pgx.Tx) (
			Connection, audit.Target, audit.Detail, error,
		) {
			labels, err := json.Marshal(orEmptyLabels(wanted.Labels))
			if err != nil {
				return Connection{}, audit.Target{}, nil,
					fmt.Errorf("encoding labels: %w", err)
			}
			configuration, err := json.Marshal(orEmptyConfiguration(wanted.Configuration))
			if err != nil {
				return Connection{}, audit.Target{}, nil,
					fmt.Errorf("encoding configuration: %w", err)
			}

			// The identifier is minted here rather than inside the statement, because the
			// credential's reference names the Connection it belongs to and there is nothing to
			// name until it exists. A reference assembled afterwards would be a second write.
			id := uuid.New()
			credential := wanted.Credential
			if credential.Reference != "" {
				credential.Reference += ":" + id.String()
			}

			row := transaction.QueryRow(ctx, `
				INSERT INTO connection (connection_id, organization, environment_id, integration,
				                        name, role, locality, relay_registration_id,
				                        secret_digest, labels, configuration,
				                        credential_method, credential_reference,
				                        credential_fingerprint, credential_created_at,
				                        credential_expires_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
				        CASE WHEN $12::TEXT IS NULL THEN NULL ELSE now() END, $15)
				RETURNING `+connectionColumns,
				id, organization.String(), wanted.Environment, wanted.Integration,
				wanted.Name, int16(wanted.Role), int16(wanted.Locality),
				nullableUUID(wanted.RelayRegistration), wanted.SecretDigest, labels, configuration,
				nullableText(credential.Method), nullableText(credential.Reference),
				nullableText(credential.Fingerprint), nullableTime(credential.ExpiresAt))

			created, err := scanConnection(row, organization.String())
			switch {
			case isUniqueViolation(err, "connection_name_is_unique_per_environment"):
				return Connection{}, audit.Target{}, nil, ErrConnectionNameTaken
			case isForeignKeyViolation(err):
				return Connection{}, audit.Target{}, nil, ErrConnectionScope
			case err != nil:
				return Connection{}, audit.Target{}, nil,
					fmt.Errorf("creating a connection: %w", err)
			}
			// The secret this Connection was created with is nowhere in the detail and could not
			// be: audit.Detail drops anything named like a credential on the way in.
			return created,
				audit.Target{Kind: audit.TargetConnection, ID: created.ID.String()},
				audit.Detail{
					"name":          created.Name,
					"integration":   created.Integration,
					"role":          created.Role.String(),
					"locality":      created.Locality.String(),
					"environmentId": created.Environment.String(),
				}, nil
		})
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
			SELECT organization, `+connectionColumns+`
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
		SELECT `+connectionColumns+`
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
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, digest []byte, fingerprint string,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionConnectionRotated,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			// Guarded on the Connection already carrying one: rotating a secret onto an
			// evidence-only Connection would create a credential with no user, and the CHECK
			// would refuse it as a constraint violation rather than as an answer.
			//
			// The fingerprint is replaced along with the digest, and the rotation is stamped.
			// A rotation that kept the old fingerprint would leave an operator unable to tell
			// whether the rotation they asked for had actually happened, which is the one thing
			// they are checking for afterwards.
			tag, err := transaction.Exec(ctx, `
				UPDATE connection
				   SET secret_digest          = $3,
				       credential_fingerprint = $4,
				       credential_rotated_at  = now(),
				       updated_at             = now()
				 WHERE connection_id = $1
				   AND organization  = $2
				   AND secret_digest IS NOT NULL`,
				id, organization.String(), digest, fingerprint)
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("rotating a connection secret: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return struct{}{}, audit.Target{}, nil, ErrConnectionUnknown
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetConnection, ID: id.String()},
				audit.Detail{
					// The fingerprint is an identity rather than a secret, so it is on the
					// record: "which credential is live now" is exactly what somebody reading
					// this event later needs to match against what they are looking at.
					"credentialFingerprint": fingerprint,
					"effect": "the previous trigger verification secret stopped working; " +
						"there is no overlap window",
				}, nil
		})
	return err
}

// SetConnectionDisabled turns a Connection off or back on without deleting it, so an operator
// can stop using a source without losing the record of what it produced.
func (p *Placements) SetConnectionDisabled(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, disabled bool,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionConnectionEnabled,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var wasDisabled *time.Time
			err := transaction.QueryRow(ctx, `
				UPDATE connection
				   SET disabled_at = CASE WHEN $3 THEN coalesce(disabled_at, now()) END,
				       updated_at  = now()
				 WHERE connection_id = $1 AND organization = $2
				RETURNING (SELECT disabled_at FROM connection previous
				            WHERE previous.connection_id = connection.connection_id)`,
				id, organization.String(), disabled).Scan(&wasDisabled)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, ErrConnectionUnknown
			}
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("changing a connection's disabled state: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetConnection, ID: id.String()},
				audit.Detail{"before": wasDisabled == nil, "after": !disabled}, nil
		})
	return err
}

// ListConnections returns the Connections in one Environment, newest first.
//
// The Environment is part of the WHERE clause together with the organization, so naming one
// tenant's Environment while authenticated for another returns nothing rather than that
// Environment's contents.
// The Environment is a FILTER rather than a scope here: the zero UUID means every Environment
// in the organization, which is what lets an operator count a tenant's Connections without
// walking its scopes. The Environment a Connection belongs to is still assigned only at
// creation and is still the authority for everything arriving through it.
func (p *Placements) ListConnections(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	environment uuid.UUID, page Page,
) (ConnectionList, error) {
	return p.QueryConnections(ctx, principal, organization,
		ConnectionQuery{Page: page, Environment: environment})
}

// ConnectionQuery is what a caller may narrow, order and page an Organization's Connections by.
// Every field is applied by the DATABASE: a listing filtered after the rows were sent is a page
// that returns fewer rows than it was asked for and a `next` that means something else.
type ConnectionQuery struct {
	Page Page
	// Relay narrows to the Connections one Relay serves, which is what disabling it would cost.
	Relay uuid.UUID
	// Environment is a FILTER rather than a scope. The zero UUID means every Environment in the
	// organization, which is what lets an operator see the estate rather than one scope at a
	// time. The Environment a Connection belongs to is still assigned only at creation.
	Environment uuid.UUID
	// Search matches the name, because that is what an operator gave the Connection and
	// therefore the only part of it they will remember during an incident.
	Search string
	// Integration narrows to one provider, State to one lifecycle state, Role to what a
	// Connection is used for. Empty means no narrowing.
	Integration string
	State       ConnectionState
	Role        ConnectionRole
	// Disabled narrows to Connections an operator has turned off, or to the ones they have not.
	// It is a pointer because "I did not ask" is a different question from "I asked for the
	// enabled ones".
	Disabled *bool
	// SortField is one of name, createdAt, integration, state. Anything else is a programming
	// error: the handler refuses an unoffered field before it reaches here.
	SortField  string
	Descending bool
}

// connectionOrderings maps a sort field to the column it orders by and the type its cursor casts
// to. A closed map rather than interpolation of caller input, which is what keeps a sort
// parameter from being a way to write SQL.
var connectionOrderings = map[string]ordering{
	"createdAt":   {column: "created_at", cast: "timestamptz", render: connectionCreatedAt},
	"name":        {column: "name", cast: "text", render: connectionName},
	"integration": {column: "integration", cast: "text", render: connectionIntegration},
	"state":       {column: "state", cast: "smallint", render: connectionState},
}

func connectionCreatedAt(c Connection) string   { return c.CreatedAt.Format(time.RFC3339Nano) }
func connectionName(c Connection) string        { return c.Name }
func connectionIntegration(c Connection) string { return c.Integration }
func connectionState(c Connection) string       { return strconv.Itoa(int(c.State)) }

// QueryConnections returns an Organization's Connections, narrowed, ordered and paged by the
// database.
//
// The Environment is part of the WHERE clause together with the organization, so naming one
// tenant's Environment while authenticated for another returns nothing rather than that
// Environment's contents.
func (p *Placements) QueryConnections(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	query ConnectionQuery,
) (ConnectionList, error) {
	if !principal.MemberOf(organization) {
		return ConnectionList{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return ConnectionList{}, err
	}
	order, known := connectionOrderings[query.SortField]
	if !known {
		order = connectionOrderings["createdAt"]
		query.Descending = true
	}
	limit := pageLimit(query.Page.Limit)
	cursorValue, cursorID, err := decodeSortCursor(query.Page.After)
	if err != nil {
		return ConnectionList{}, err
	}

	arguments := []any{organization.String()}
	where := []string{"organization = $1"}
	add := func(clause string, value any) {
		arguments = append(arguments, value)
		where = append(where, fmt.Sprintf(clause, len(arguments)))
	}

	if query.Environment != uuid.Nil {
		add("environment_id = $%d", query.Environment)
	}
	if query.Relay != uuid.Nil {
		add("relay_registration_id = $%d", query.Relay)
	}
	if query.Search != "" {
		add("name ILIKE '%%' || $%d || '%%'", query.Search)
	}
	if query.Integration != "" {
		add("integration = $%d", query.Integration)
	}
	if query.State != 0 {
		add("state = $%d", int16(query.State))
	}
	if query.Role != 0 {
		// A bitwise containment test, so asking for triggers finds the Connections that are
		// BOTH as well. A role is a set, and equality would answer a different question — it
		// would hide every Connection that does more than what was asked about.
		arguments = append(arguments, int16(query.Role))
		where = append(where,
			fmt.Sprintf("(role & $%[1]d) = $%[1]d", len(arguments)))
	}
	if query.Disabled != nil {
		if *query.Disabled {
			where = append(where, "disabled_at IS NOT NULL")
		} else {
			where = append(where, "disabled_at IS NULL")
		}
	}
	if cursorID != nil {
		arguments = append(arguments, cursorValue, *cursorID)
		comparison := "<"
		if !query.Descending {
			comparison = ">"
		}
		where = append(where, fmt.Sprintf("(%s, connection_id) %s ($%d::%s, $%d::uuid)",
			order.column, comparison, len(arguments)-1, order.cast, len(arguments)))
	}

	direction := "DESC"
	if !query.Descending {
		direction = "ASC"
	}
	arguments = append(arguments, limit+1)

	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT %s
		  FROM connection
		 WHERE %s
		 ORDER BY %s %s, connection_id %s
		 LIMIT $%d`,
		connectionColumns, strings.Join(where, "\n   AND "),
		order.column, direction, direction, len(arguments)), arguments...)
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
			list.Next = encodeSortCursor(order.render(last), last.ID)
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

// nullableConnection holds the columns that may be SQL NULL, so one struct is threaded through
// both scanners rather than eleven pointers being declared twice and getting out of order once.
type nullableConnection struct {
	relay                 *uuid.UUID
	labels                []byte
	configuration         []byte
	disabledAt            *time.Time
	credentialMethod      *string
	credentialReference   *string
	credentialFingerprint *string
	credentialCreatedAt   *time.Time
	credentialRotatedAt   *time.Time
	credentialExpiresAt   *time.Time
}

// destinations is the scan target list, in the order connectionColumns names them. It is a
// method rather than an inline list so the two scanners cannot drift apart.
func (n *nullableConnection) destinations(found *Connection) []any {
	return []any{
		&found.ID, &found.Environment, &found.Integration, &found.Name, &found.Role,
		&found.Locality, &n.relay, &found.SecretDigest, &n.labels, &n.disabledAt,
		&found.CreatedAt, &found.UpdatedAt, &found.State, &found.ConfigurationRevision,
		&n.configuration, &n.credentialMethod, &n.credentialReference, &n.credentialFingerprint,
		&n.credentialCreatedAt, &n.credentialRotatedAt, &n.credentialExpiresAt,
	}
}

// scanConnection maps one row. The nullable columns are read through pointers and flattened to
// zero values, because a caller asking whether a Connection is disabled should not have to
// dereference to find out.
func scanConnection(row scanned, organization string) (Connection, error) {
	found := Connection{Organization: organization}
	var nullable nullableConnection
	if err := row.Scan(nullable.destinations(&found)...); err != nil {
		return Connection{}, err
	}
	return finish(found, nullable)
}

// scanConnectionWithOrganization is scanConnection for the one read that discovers the tenant
// rather than being given it.
func scanConnectionWithOrganization(row scanned, organization *string) (Connection, error) {
	var (
		found    Connection
		nullable nullableConnection
	)
	if err := row.Scan(append([]any{organization}, nullable.destinations(&found)...)...); err != nil {
		return Connection{}, err
	}
	return finish(found, nullable)
}

func finish(found Connection, nullable nullableConnection) (Connection, error) {
	if nullable.relay != nil {
		found.RelayRegistration = *nullable.relay
	}
	if nullable.disabledAt != nil {
		found.DisabledAt = *nullable.disabledAt
	}
	if len(nullable.labels) > 0 {
		if err := json.Unmarshal(nullable.labels, &found.Labels); err != nil {
			return Connection{}, fmt.Errorf("decoding connection labels: %w", err)
		}
	}
	if found.Labels == nil {
		found.Labels = map[string]string{}
	}
	if len(nullable.configuration) > 0 {
		if err := json.Unmarshal(nullable.configuration, &found.Configuration); err != nil {
			return Connection{}, fmt.Errorf("decoding connection configuration: %w", err)
		}
	}
	if found.Configuration == nil {
		found.Configuration = map[string]any{}
	}
	found.Credential = credentialFrom(nullable)
	return found, nil
}

// credentialFrom flattens the credential metadata. The schema guarantees the four columns
// travel together, so a partially-present credential is a database that has been edited by
// hand rather than a case to handle.
func credentialFrom(nullable nullableConnection) Credential {
	if nullable.credentialMethod == nil {
		return Credential{}
	}
	credential := Credential{Method: *nullable.credentialMethod}
	if nullable.credentialReference != nil {
		credential.Reference = *nullable.credentialReference
	}
	if nullable.credentialFingerprint != nil {
		credential.Fingerprint = *nullable.credentialFingerprint
	}
	if nullable.credentialCreatedAt != nil {
		credential.CreatedAt = *nullable.credentialCreatedAt
	}
	if nullable.credentialRotatedAt != nil {
		credential.RotatedAt = *nullable.credentialRotatedAt
	}
	if nullable.credentialExpiresAt != nil {
		credential.ExpiresAt = *nullable.credentialExpiresAt
	}
	return credential
}

func orEmptyLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return labels
}

func orEmptyConfiguration(configuration map[string]any) map[string]any {
	if configuration == nil {
		return map[string]any{}
	}
	return configuration
}

// nullableUUID renders the zero UUID as SQL NULL, which is what "no Relay serves this" means
// in a column the schema constrains against the locality.
func nullableUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
