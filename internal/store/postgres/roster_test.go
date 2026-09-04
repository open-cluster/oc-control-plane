package storage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestRelayLastSeenSortPagesRelaysThatHaveNeverConnected(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		_, err = pool.Exec(context.Background(), `
			INSERT INTO relay_registration
				(registration_id, org_id, credential_digest, cluster_fingerprint,
				 relay_version, capabilities, created_at)
			VALUES (gen_random_uuid(), $1, sha256($2::bytea), $3, '1.0.0', '[]',
			        now() + make_interval(secs => $4))`,
			organization.String(), []byte(fmt.Sprintf("credential-%d", index)),
			fmt.Sprintf("relay-%d", index), float64(index))
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, descending := range []bool{false, true} {
		seen := map[string]bool{}
		cursor := ""
		for {
			page, listErr := database.ListRelays(context.Background(), ownerOf(t, organization),
				organization, storage.RelayQuery{
					Page: storage.Page{Limit: 1, After: cursor}, SortField: "lastSeenAt",
					Descending: descending, LivenessWindow: time.Minute,
				})
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, relay := range page.Relays {
				seen[relay.RegistrationID.String()] = true
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
		if len(seen) != 3 {
			t.Errorf("descending=%v visited %d relays, want 3", descending, len(seen))
		}
	}
}
