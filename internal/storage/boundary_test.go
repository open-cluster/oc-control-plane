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

// A job whose Integration is not the one its Relay serves is refused before anything is
// written. Dispatching anyway would send a read for one customer's cluster to an
// installation sitting in another.
func TestBoundary_AJobNamingARelayThatDoesNotServeItsIntegrationIsRefused(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)

	// Two relays, and an Integration served by the first.
	served := enrolledRelay(t, placements, organization)
	other := enrolledRelay(t, placements, organization)
	integration := kubernetesIntegration(t, placements, organization, served)

	refusal, err := placements.EnqueueJob(context.Background(), organization, storage.Job{
		ID:                uuid.New(),
		IntegrationID:     integration,
		RegistrationID:    other,
		CapabilityID:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
		Arguments:         []byte("arguments"),
	})
	if !errors.Is(err, storage.ErrJobRefused) {
		t.Fatalf("a job routed to a relay that does not serve its integration = %v, want refused", err)
	}
	if refusal != storage.JobRelayIsNotTheIntegrations {
		t.Errorf("the refusal was %q, want the relay mismatch named", refusal)
	}
}

// An Integration an operator turned off carries no new work. Disabling is not deleting, so
// the record survives — but nothing new is dispatched against it.
func TestBoundary_AJobAgainstADisabledIntegrationIsRefused(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)

	relay := enrolledRelay(t, placements, organization)
	integration := kubernetesIntegration(t, placements, organization, relay)

	if err := placements.SetIntegrationDisabled(
		context.Background(), ownerOf(t, organization), organization, integration, true); err != nil {
		t.Fatalf("disabling the integration: %v", err)
	}

	refusal, err := placements.EnqueueJob(context.Background(), organization, storage.Job{
		ID:                uuid.New(),
		IntegrationID:     integration,
		RegistrationID:    relay,
		CapabilityID:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
		Arguments:         []byte("arguments"),
	})
	if !errors.Is(err, storage.ErrJobRefused) {
		t.Fatalf("a job against a disabled integration = %v, want refused", err)
	}
	if refusal != storage.JobIntegrationUnknown {
		t.Errorf("the refusal was %q, want the integration reported unusable", refusal)
	}
}

// One tenant's job cannot name another tenant's Integration, however the request was
// assembled. The composite foreign keys are what refuse it, and this is the test that
// proves they do.
func TestBoundary_AJobCannotNameAnotherOrganizationsIntegration(t *testing.T) {
	t.Parallel()

	placements := openPlacements(t,
		map[string]string{"shared": postgresDSN(t)},
		map[string]string{"boundary-one": "shared", "boundary-two": "shared"})
	if _, err := placements.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	one, two := named(t, "boundary-one"), named(t, "boundary-two")

	// An Integration and its Relay both belong to the second organization.
	myRelay := enrolledRelay(t, placements, one)
	_ = myRelay
	theirRelay := enrolledRelay(t, placements, two)
	theirIntegration := kubernetesIntegration(t, placements, two, theirRelay)

	refusal, err := placements.EnqueueJob(context.Background(), one, storage.Job{
		ID:                uuid.New(),
		IntegrationID:     theirIntegration,
		RegistrationID:    theirRelay,
		CapabilityID:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
		Arguments:         []byte("arguments"),
	})
	if !errors.Is(err, storage.ErrJobRefused) {
		t.Fatalf("a job naming another tenant's integration = %v, want refused", err)
	}
	if refusal != storage.JobIntegrationUnknown {
		t.Errorf("the refusal was %q; another tenant's integration is not one this tenant has",
			refusal)
	}
}

// A claimed job carries the Integration it reaches, so a result is attributed to the
// installation it was read from rather than to the Relay that read it.
func TestBoundary_AClaimedJobCarriesTheIntegrationItReaches(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)

	relay := enrolledRelay(t, placements, organization)
	integration := kubernetesIntegration(t, placements, organization, relay)
	enqueueThrough(t, placements, organization, relay, integration)

	claimed := claimableFor(t, placements, organization, relay)
	if len(claimed) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(claimed))
	}
	if claimed[0].IntegrationID != integration {
		t.Errorf("the claimed job names integration %s, want %s",
			claimed[0].IntegrationID, integration)
	}
}

func claimableFor(
	t *testing.T, placements *storage.Placements,
	organization tenancy.Organization, registration uuid.UUID,
) []storage.Job {
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
	return claimed
}

func named(t *testing.T, organization string) tenancy.Organization {
	t.Helper()
	name, err := tenancy.NewOrganization(organization)
	if err != nil {
		t.Fatalf("naming the organization: %v", err)
	}
	return name
}
