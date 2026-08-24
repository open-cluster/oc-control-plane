package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/seal"
)

func TestIntegrationCredentialRotationRewrapsWithoutChangingItsFingerprint(t *testing.T) {
	database, organization := migratedDatabase(t)
	oldMaterial := make([]byte, seal.KeyLength)
	newMaterial := make([]byte, seal.KeyLength)
	for index := range oldMaterial {
		oldMaterial[index] = byte(index + 1)
		newMaterial[index] = byte(index + 31)
	}
	old, err := seal.NewKeyring(seal.Key{ID: "old", Material: oldMaterial})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	sealed, err := old.Seal("xoxb-rotated", integrations.CredentialBinding(id))
	if err != nil {
		t.Fatal(err)
	}
	created, err := database.CreateIntegration(context.Background(), ownerOf(t, organization), organization,
		integrations.NewIntegration{
			ID: id, Type: integrations.TypeSlack, Name: "rotation target",
			CredentialSealed: sealed, CredentialFingerprint: "minted-and-stable",
		})
	if err != nil {
		t.Fatalf("creating credential holder: %v", err)
	}

	rotating, err := seal.NewKeyring(
		seal.Key{ID: "current", Material: newMaterial},
		seal.Key{ID: "old", Material: oldMaterial},
	)
	if err != nil {
		t.Fatal(err)
	}
	changed, done, err := database.RewrapIntegrationCredentials(
		context.Background(), rotating, 10)
	if err != nil || changed != 1 || !done {
		t.Fatalf("rotation changed=%d done=%t error=%v", changed, done, err)
	}

	pool, _ := database.Pool(organization)
	var stored []byte
	var fingerprint string
	if err = pool.QueryRow(context.Background(), `
		SELECT credential_sealed, credential_fingerprint
		  FROM integration
		 WHERE org_id = $1 AND integration_id = $2`, organization.String(), created.ID).
		Scan(&stored, &fingerprint); err != nil {
		t.Fatal(err)
	}
	keyID, err := seal.EnvelopeKeyID(stored)
	if err != nil || keyID != "current" {
		t.Fatalf("stored key id = %q, %v", keyID, err)
	}
	if fingerprint != "minted-and-stable" {
		t.Fatalf("fingerprint changed during cryptographic rotation: %q", fingerprint)
	}
	opened, err := rotating.Open(stored, integrations.CredentialBinding(created.ID))
	if err != nil || opened != "xoxb-rotated" {
		t.Fatalf("open rewrapped credential = %q, %v", opened, err)
	}
}

func TestIntegrationCredentialRotationWaitsForLockedOutdatedCredential(t *testing.T) {
	database, organization := migratedDatabase(t)
	oldMaterial := make([]byte, seal.KeyLength)
	newMaterial := make([]byte, seal.KeyLength)
	for index := range oldMaterial {
		oldMaterial[index] = byte(index + 1)
		newMaterial[index] = byte(index + 31)
	}
	old, err := seal.NewKeyring(seal.Key{ID: "old", Material: oldMaterial})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	sealed, err := old.Seal("xoxb-locked", integrations.CredentialBinding(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.CreateIntegration(context.Background(), ownerOf(t, organization), organization,
		integrations.NewIntegration{
			ID: id, Type: integrations.TypeSlack, Name: "locked rotation target",
			CredentialSealed: sealed, CredentialFingerprint: "stable",
		}); err != nil {
		t.Fatal(err)
	}

	rotating, err := seal.NewKeyring(
		seal.Key{ID: "current", Material: newMaterial},
		seal.Key{ID: "old", Material: oldMaterial},
	)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := database.Pool(organization)
	lock, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err = lock.Exec(context.Background(), `
		SELECT integration_id FROM integration
		 WHERE org_id = $1 AND integration_id = $2
		 FOR UPDATE`, organization.String(), id); err != nil {
		t.Fatal(err)
	}

	type result struct {
		changed int
		done    bool
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		changed, done, rotationErr := database.RewrapIntegrationCredentials(
			context.Background(), rotating, 10)
		finished <- result{changed: changed, done: done, err: rotationErr}
	}()
	select {
	case result := <-finished:
		t.Fatalf("rotation skipped a locked outdated credential: %#v", result)
	case <-time.After(150 * time.Millisecond):
	}
	if err = lock.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-finished:
		if result.err != nil || result.changed != 1 || !result.done {
			t.Fatalf("rotation after unlock = %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not resume after the credential lock was released")
	}
}
