package api

import (
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestRelayCompatibilityReflectsARecordedProtocolVersion(t *testing.T) {
	if got := viewOf(storage.RelaySummary{}).Compatibility; got != "unknown" {
		t.Fatalf("never-connected relay compatibility = %q, want unknown", got)
	}
	if got := viewOf(storage.RelaySummary{ProtocolVersion: 1, LastSeenAt: time.Now()}).Compatibility; got != "compatible" {
		t.Fatalf("seen relay compatibility = %q, want compatible", got)
	}
	if got := viewOf(storage.RelaySummary{LastSeenAt: time.Now()}).Compatibility; got != "unknown" {
		t.Fatalf("historical relay compatibility = %q, want unknown", got)
	}
}
