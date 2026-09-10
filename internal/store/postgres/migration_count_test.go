package storage_test

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestBinaryCarriesOneSchemaBaseline(t *testing.T) {
	if got := storage.MigrationCount(); got != 1 {
		t.Fatalf("embedded migrations = %d, want one final baseline", got)
	}
}
