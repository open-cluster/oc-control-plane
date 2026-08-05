package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The invariant these tests protect is the one the whole Environment model exists for:
// evidence never crosses an Environment boundary. Every test below is written so that removing
// the scoping makes it fail — a happy-path suite over the same code would stay green against an
// implementation with no boundary at all.
//
// They assert against durable state, because a job that was refused and a job that was enqueued
// and never delivered look identical from the outside and are completely different facts.

// A job whose Connection is not the one its Relay serves is refused before anything is written.
// This is the boundary made a PRECONDITION on the execution path rather than a property of
// whichever query happened to scope itself correctly.
func TestBoundary_AJobNamingARelayThatDoesNotServeItsConnectionIsRefused(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)

	// Two relays, and a Connection served by the first.
	served := enrolledRelay(t, placements, organization)
	other := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, served)

	refusal, err := placements.EnqueueJob(context.Background(), organization, storage.Job{
		ID:                uuid.New(),
		ConnectionID:      connection,
		RegistrationID:    other,
		CapabilityID:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
		Arguments:         []byte("arguments"),
	})

	if !errors.Is(err, storage.ErrJobRefused) {
		t.Fatalf("a job routed to a relay that does not serve its connection = %v, want refused", err)
	}
	if refusal != storage.JobRelayIsNotTheConnections {
		t.Errorf("the refusal was %q, want the relay mismatch named", refusal)
	}
	if claimable := claimableFor(t, placements, organization, other); claimable != 0 {
		t.Fatalf("a refused job left %d rows claimable; nothing may reach a dispatchable state",
			claimable)
	}
}

// A trigger-only Connection delivers Signals inbound and answers nothing outbound, so there is
// nothing for a capability to read through it.
func TestBoundary_AJobAgainstATriggerConnectionIsRefused(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	relay := enrolledRelay(t, placements, organization)

	environment, err := placements.EnsureDefaultEnvironment(context.Background(), ownerOf(t, organization), organization)
	if err != nil {
		t.Fatalf("ensuring the default environment: %v", err)
	}
	trigger, err := placements.CreateConnection(context.Background(), ownerOf(t, organization), organization,
		storage.NewConnection{
			Environment:  environment.ID,
			Integration:  "alertmanager",
			Name:         "Production Alertmanager",
			Role:         storage.RoleTrigger,
			Locality:     storage.LocalityControlPlane,
			SecretDigest: randomDigest(t),
		})
	if err != nil {
		t.Fatalf("creating a trigger connection: %v", err)
	}

	refusal, err := placements.EnqueueJob(context.Background(), organization, storage.Job{
		ID:                uuid.New(),
		ConnectionID:      trigger.ID,
		RegistrationID:    relay,
		CapabilityID:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
		Arguments:         []byte("arguments"),
	})

	if !errors.Is(err, storage.ErrJobRefused) {
		t.Fatalf("a job against a trigger connection = %v, want refused", err)
	}
	if refusal != storage.JobConnectionIsNotEvidence {
		t.Errorf("the refusal was %q, want the role named", refusal)
	}
}

// A Connection an operator turned off carries no new work. Disabling is not deleting, so the
// row is still there — which is exactly why the refusal has to be decided rather than inferred
// from the row's absence.
func TestBoundary_AJobAgainstADisabledConnectionIsRefused(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	relay := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, relay)

	if err := placements.SetConnectionDisabled(
		context.Background(), ownerOf(t, organization), organization, connection, true); err != nil {
		t.Fatalf("disabling the connection: %v", err)
	}

	refusal, err := placements.EnqueueJob(context.Background(), organization, storage.Job{
		ID:                uuid.New(),
		ConnectionID:      connection,
		RegistrationID:    relay,
		CapabilityID:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
		Arguments:         []byte("arguments"),
	})

	if !errors.Is(err, storage.ErrJobRefused) {
		t.Fatalf("a job against a disabled connection = %v, want refused", err)
	}
	if refusal != storage.JobConnectionUnknown {
		t.Errorf("the refusal was %q, want the connection reported unusable", refusal)
	}
	if claimable := claimableFor(t, placements, organization, relay); claimable != 0 {
		t.Fatalf("a refused job left %d rows claimable", claimable)
	}
}

// The tenant boundary, with both organizations on ONE placement. An organization with no
// placement fails before any query runs, which would leave this passing against an
// implementation with no scoping at all — the exact defect it exists to catch.
func TestBoundary_AJobCannotNameAnotherOrganizationsConnection(t *testing.T) {
	t.Parallel()
	placements := openPlacements(t,
		map[string]string{"shared": postgresDSN(t)},
		map[string]string{"org-one": "shared", "org-two": "shared"})
	if _, err := placements.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	one, two := named(t, "org-one"), named(t, "org-two")

	// A Connection and its Relay both belong to the second organization.
	theirRelay := enrolledRelay(t, placements, two)
	theirConnection := evidenceConnection(t, placements, two, theirRelay)

	// The first organization names both, holding correct identifiers for neither.
	refusal, err := placements.EnqueueJob(context.Background(), one, storage.Job{
		ID:                uuid.New(),
		ConnectionID:      theirConnection,
		RegistrationID:    theirRelay,
		CapabilityID:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
		Arguments:         []byte("arguments"),
	})

	if !errors.Is(err, storage.ErrJobRefused) {
		t.Fatalf("a job naming another tenant's connection = %v, want refused", err)
	}
	if refusal != storage.JobConnectionUnknown {
		t.Errorf("the refusal was %q; another tenant's connection is not one this tenant has",
			refusal)
	}
	// And nothing appeared for the tenant that actually owns them, which is the failure that
	// would matter: one organization creating work in another's queue.
	if claimable := claimableFor(t, placements, two, theirRelay); claimable != 0 {
		t.Fatalf("a cross-tenant job created %d claimable rows in the owning tenant", claimable)
	}
}

// One Relay serving two Environments is explicitly allowed: a single installation in a shared
// cluster must not be artificially forbidden, and the Environment of any work is the
// Environment of its Connection rather than of the installation that runs it.
func TestBoundary_OneRelayServesConnectionsInTwoEnvironments(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	relay := enrolledRelay(t, placements, organization)

	staging, err := placements.CreateEnvironment(context.Background(), ownerOf(t, organization), organization, "Staging")
	if err != nil {
		t.Fatalf("creating an environment: %v", err)
	}
	production, err := placements.EnsureDefaultEnvironment(context.Background(), ownerOf(t, organization), organization)
	if err != nil {
		t.Fatalf("ensuring the default environment: %v", err)
	}

	for _, environment := range []uuid.UUID{production.ID, staging.ID} {
		created, createErr := placements.CreateConnection(context.Background(), ownerOf(t, organization), organization,
			storage.NewConnection{
				Environment:       environment,
				Integration:       "kubernetes",
				Name:              "cluster in " + environment.String(),
				Role:              storage.RoleEvidence,
				Locality:          storage.LocalityRelay,
				RelayRegistration: relay,
			})
		if createErr != nil {
			t.Fatalf("one relay must serve several environments: %v", createErr)
		}
		if _, err = placements.EnqueueJob(context.Background(), organization, storage.Job{
			ID:                uuid.New(),
			ConnectionID:      created.ID,
			RegistrationID:    relay,
			CapabilityID:      "kubernetes.workload.runtime",
			CapabilityVersion: 1,
			Arguments:         []byte("arguments"),
		}); err != nil {
			t.Fatalf("enqueueing against %s: %v", environment, err)
		}
	}

	if claimable := claimableFor(t, placements, organization, relay); claimable != 2 {
		t.Fatalf("%d jobs are claimable, want both", claimable)
	}
}

// The claimed job carries the Connection it reaches, so a result can be attributed to the
// cluster it was read from rather than to the installation that read it.
func TestBoundary_AClaimedJobCarriesTheConnectionItReaches(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	relay := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, relay)
	enqueueThrough(t, placements, organization, relay, connection)

	claimed := claim(t, placements, organization, relay, uuid.New())

	if claimed[0].ConnectionID != connection {
		t.Fatalf("the claimed job names connection %s, want %s",
			claimed[0].ConnectionID, connection)
	}
}

// claimableFor counts what a relay could take, which is what "reached a dispatchable state"
// means from outside. Asserting on it rather than on a row count is the difference between
// proving nothing was dispatched and proving nothing was written.
func claimableFor(
	t *testing.T, placements *storage.Placements,
	organization tenancy.Organization, registration uuid.UUID,
) int {
	t.Helper()

	claimed, err := placements.ClaimJobs(context.Background(), organization, storage.JobClaim{
		RegistrationID: registration,
		SessionID:      uuid.New(),
		LeaseFor:       time.Minute,
		Capacity:       10,
	})
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	return len(claimed)
}

func named(t *testing.T, organization string) tenancy.Organization {
	t.Helper()
	name, err := tenancy.NewOrganization(organization)
	if err != nil {
		t.Fatalf("naming the organization: %v", err)
	}
	return name
}
