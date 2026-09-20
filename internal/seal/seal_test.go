package seal

import (
	"bytes"
	"strings"
	"testing"
)

func TestSealRoundTripIsRandomizedAndAuthenticated(t *testing.T) {
	sealer, err := New(bytes.Repeat([]byte{1}, KeyLength))
	if err != nil {
		t.Fatal(err)
	}
	binding := []byte("integration-a")
	first, err := sealer.Seal("provider-secret", binding)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sealer.Seal("provider-secret", binding)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) || strings.Contains(string(first), "provider-secret") {
		t.Fatal("sealed credentials must be randomized and opaque")
	}
	opened, err := sealer.Open(first, binding)
	if err != nil || opened != "provider-secret" {
		t.Fatalf("open = %q, %v", opened, err)
	}

	first[len(first)-1] ^= 0xff
	if _, err = sealer.Open(first, binding); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
}

func TestSealBindsCredentialToIntegration(t *testing.T) {
	sealer, err := New(bytes.Repeat([]byte{2}, KeyLength))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal("provider-secret", []byte("integration-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sealer.Open(sealed, []byte("integration-b")); err == nil {
		t.Fatal("another Integration binding opened the credential")
	}
}

func TestNewRequiresAES256Key(t *testing.T) {
	if _, err := New(make([]byte, KeyLength-1)); err == nil {
		t.Fatal("an invalid key length was accepted")
	}
	var unconfigured Sealer
	if unconfigured.Configured() {
		t.Fatal("the zero sealer reports itself configured")
	}
}
