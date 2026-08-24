package app

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSharedHTTPTimeoutsAreDocumented(t *testing.T) {
	document, err := os.ReadFile("../../docs/self-hosted/configuration.mdx")
	if err != nil {
		t.Fatalf("read self-hosted configuration documentation: %v", err)
	}
	documented := strings.Join(strings.Fields(string(document)), " ")

	bounds := []struct {
		duration time.Duration
		purpose  string
	}{
		{readHeaderTimeout, "for headers"},
		{operatorReadTimeout, "to read a request"},
		{operatorWriteTimeout, "to write a response"},
		{operatorIdleTimeout, "for an idle connection"},
	}
	for _, bound := range bounds {
		want := fmt.Sprintf("%d seconds %s", int(bound.duration/time.Second), bound.purpose)
		if !strings.Contains(documented, want) {
			t.Errorf("self-hosted configuration must document shared HTTP bound %q", want)
		}
	}
}
