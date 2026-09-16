package storage_test

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestBinaryCarriesBaselineAndCompatibilityMigrations(t *testing.T) {
	if got := storage.MigrationCount(); got != 5 {
		t.Fatalf("embedded migrations = %d, want baseline plus four compatibility migrations", got)
	}
}
