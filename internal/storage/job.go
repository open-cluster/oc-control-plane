package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// JobStatus is where a job has got to. The three terminal values are what the recording
// guard tests, so a job already recorded cannot be recorded a second time.
//
// The values are persisted and appear as literals in the SQL in this package. They are frozen
// by a gate in internal/gates; changing one rewrites what every existing row means.
type JobStatus int16

const (
	JobPending JobStatus = iota
	JobLeased
	JobSucceeded
	JobFailed
	JobCancelled
)

// Job is a unit of work as the control plane holds it.
type Job struct {
	ID uuid.UUID
	// ConnectionID is what this job reaches: one configured instance of an Integration, inside
	// one Environment. The registration below is where it RUNS. Keeping both is what lets a
	// customer with two clusters behind one Relay have a result attributed to the cluster it
	// was read from rather than to the installation that read it.
	ConnectionID   uuid.UUID
	RegistrationID uuid.UUID
	CapabilityID   string
	// Widened to the protocol's type rather than a plain int, so the version a relay is asked
	// for is the version this holds, with no conversion in between that could narrow it.
	CapabilityVersion uint32
	Arguments         []byte
	LeaseSession      uuid.UUID
	LeaseEpoch        int64
}

// JobFence identifies which execution of a job a message belongs to. Both halves travel on
// the wire — the assignment carries them, the result echoes them — so ownership is validated
// on every job-scoped message rather than inferred from who is currently connected.
type JobFence struct {
	JobID        uuid.UUID
	LeaseSession uuid.UUID
	LeaseEpoch   int64
}

// ErrJobRefused reports work that was not enqueued because its Connection could not carry it.
// Nothing was written.
var ErrJobRefused = errors.New("job refused")

// JobRefusal is why work was not enqueued. Each is a different mistake by whoever planned the
// job, and telling them apart is what makes the boundary diagnosable rather than mysterious.
type JobRefusal int

const (
	// JobConnectionUnknown means no such Connection exists in this organization, or it has
	// been disabled. Both are one answer: an operator who turned a Connection off wants work
	// against it refused, not merely recorded.
	JobConnectionUnknown JobRefusal = iota + 1
	// JobConnectionIsNotEvidence means the Connection exists but is trigger-only. It delivers
	// Signals inbound and answers nothing outbound, so there is nothing for a capability to
	// read through it.
	JobConnectionIsNotEvidence
	// JobRelayIsNotTheConnections means the job names a Relay that is not the one bound to its
	// Connection — including a Connection whose locality is control_plane and which no Relay
	// serves. Dispatching anyway would send a read for one customer's cluster to an
	// installation sitting in another.
	JobRelayIsNotTheConnections
)

func (r JobRefusal) String() string {
	switch r {
	case JobConnectionUnknown:
		return "connection unknown or disabled"
	case JobConnectionIsNotEvidence:
		return "connection does not answer evidence reads"
	case JobRelayIsNotTheConnections:
		return "relay is not the one bound to this connection"
	default:
		return "unrecognised"
	}
}

// EnqueueJob records work to be done. It is pending and unleased: nothing is delivered until
// a session claims it, so a job that is enqueued while every relay is offline waits rather
// than being lost.
//
// The Connection is a PRECONDITION rather than a column copied in. The insert only produces a
// row when a live evidence Connection exists in this organization AND the Relay it is bound to
// is the one the job names, so the environment boundary is checked on the execution path
// rather than left to whichever query happened to scope itself correctly. Deciding it inside
// the insert also means a Connection disabled between a check and a write cannot leave work
// queued against it.
func (p *Placements) EnqueueJob(
	ctx context.Context, organization tenancy.Organization, job Job,
) (JobRefusal, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, `
		INSERT INTO relay_job
			(job_id, organization, connection_id, registration_id,
			 capability_id, capability_version, arguments)
		SELECT $1, $2, connection.connection_id, connection.relay_registration_id, $5, $6, $7
		  FROM connection
		 WHERE connection.connection_id  = $3
		   AND connection.organization   = $2
		   AND connection.disabled_at   IS NULL
		   -- 2 evidence, 3 both. A trigger-only Connection answers nothing outbound.
		   AND connection.role          IN (2, 3)
		   -- The registration is taken FROM the Connection and compared to the one the job
		   -- names, rather than trusted from the job. A caller that got it wrong is refused
		   -- instead of silently redirected.
		   AND connection.relay_registration_id = $4`,
		job.ID, organization.String(), job.ConnectionID, job.RegistrationID,
		job.CapabilityID, job.CapabilityVersion, job.Arguments)
	if err != nil {
		return 0, fmt.Errorf("enqueueing job: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return 0, nil
	}
	return p.explainRefusedJob(ctx, organization, job)
}

// explainRefusedJob reads why the guarded insert matched nothing.
//
// It is a second read rather than part of the insert, so what it describes is the state a
// moment later. Under a concurrent change the diagnosis can name the wrong reason; what it
// cannot do is be wrong about the refusal itself, because the row was already not written.
// The explanation is for whoever planned the job, not a decision anything acts on.
func (p *Placements) explainRefusedJob(
	ctx context.Context, organization tenancy.Organization, job Job,
) (JobRefusal, error) {
	connection, err := p.ConnectionForOrganization(ctx, organization, job.ConnectionID)
	switch {
	case errors.Is(err, ErrConnectionUnknown):
		return JobConnectionUnknown, ErrJobRefused
	case err != nil:
		return 0, fmt.Errorf("auditing a refused job: %w", err)
	case connection.Disabled():
		return JobConnectionUnknown, ErrJobRefused
	case !connection.Role.Includes(RoleEvidence):
		return JobConnectionIsNotEvidence, ErrJobRefused
	default:
		return JobRelayIsNotTheConnections, ErrJobRefused
	}
}
