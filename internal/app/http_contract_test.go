package app

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/seal"
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

func TestConfiguredSealerRetainsPreviousKeys(t *testing.T) {
	t.Parallel()

	oldMaterial := make([]byte, seal.KeyLength)
	newMaterial := make([]byte, seal.KeyLength)
	for index := range oldMaterial {
		oldMaterial[index] = byte(index + 1)
		newMaterial[index] = byte(index + 11)
	}
	old, err := seal.NewKeyring(seal.Key{ID: "old", Material: oldMaterial})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := old.Seal("credential", []byte("integration-a"))
	if err != nil {
		t.Fatal(err)
	}

	configured, err := configuredSealer(config.Config{
		SealingKeyID: "current",
		SealingKey:   newMaterial,
		PreviousSealingKeys: []config.SealingKey{
			{ID: "old", Material: oldMaterial},
		},
	})
	if err != nil {
		t.Fatalf("compose keyring: %v", err)
	}
	opened, err := configured.Open(sealed, []byte("integration-a"))
	if err != nil || opened != "credential" {
		t.Fatalf("open previous-key envelope: %q, %v", opened, err)
	}
}
