package storage_test

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestBinaryCarriesBaselineAndCompatibilityMigrations(t *testing.T) {
	if got := storage.MigrationCount(); got != 12 {
		t.Fatalf("embedded migrations = %d, want baseline plus eleven compatibility migrations", got)
	}
}
