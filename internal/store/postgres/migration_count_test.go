package storage_test

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestBinaryCarriesBaselineAndCompatibilityMigrations(t *testing.T) {
	if got := storage.MigrationCount(); got != 6 {
		t.Fatalf("embedded migrations = %d, want baseline plus five compatibility migrations", got)
	}
}
