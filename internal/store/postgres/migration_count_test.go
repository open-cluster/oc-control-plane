package storage_test

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestBinaryCarriesBaselineAndCompatibilityMigration(t *testing.T) {
	if got := storage.MigrationCount(); got != 2 {
		t.Fatalf("embedded migrations = %d, want baseline plus compatibility migration", got)
	}
}
