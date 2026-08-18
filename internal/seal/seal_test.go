package seal

import (
	"strings"
	"testing"
)

// The client secret is the one credential in this schema that is encrypted rather than
// digested, because it has to be presented to a token endpoint rather than compared against.
// That makes the round trip load-bearing, and it makes a wrong key having to FAIL load-bearing.
func TestASealedClientSecretComesBackAndOnlyUnderItsOwnKey(t *testing.T) {
	t.Parallel()

	const secret = "the-client-secret-a-provider-issued"
	sealer := sealerWith(t, 1)

	sealed, err := sealer.Seal(secret, nil)
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}
	if strings.Contains(string(sealed), secret) {
		t.Fatal("the sealed value contains the secret")
	}

	opened, err := sealer.Open(sealed, nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if opened != secret {
		t.Errorf("opened %q, want the secret back", opened)
	}

	// Sealing twice must not produce the same bytes, or the column leaks by comparison: two
	// tenants configuring the same provider would be visible as such to anyone with a dump.
	again, err := sealer.Seal(secret, nil)
	if err != nil {
		t.Fatalf("sealing again: %v", err)
	}
	if string(again) == string(sealed) {
		t.Error("the same secret sealed twice produced the same bytes")
	}

	// A different key must FAIL rather than return something. GCM's tag is what tells a rotated
	// key apart from a tampered column, and neither is a secret to present to a provider.
	if _, err := sealerWith(t, 2).Open(sealed, nil); err == nil {
		t.Error("another key opened the secret")
	}
	// And a tampered value likewise.
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := sealer.Open(tampered, nil); err == nil {
		t.Error("a tampered value opened")
	}
}

// A sealed blob is bound to the row it belongs to: a credential copied onto another
// integration's row — by a mistake or by someone with UPDATE on the table — refuses to
// open there instead of authenticating as that row's secret.
func TestASealedValueOpensOnlyUnderItsOwnBinding(t *testing.T) {
	t.Parallel()

	sealer := sealerWith(t, 1)
	sealed, err := sealer.Seal("xoxb-a-bot-token", []byte("integration-row-a"))
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}

	opened, err := sealer.Open(sealed, []byte("integration-row-a"))
	if err != nil || opened != "xoxb-a-bot-token" {
		t.Fatalf("the right binding must open: %q, %v", opened, err)
	}
	if _, err := sealer.Open(sealed, []byte("integration-row-b")); err == nil {
		t.Error("another row's binding opened the secret")
	}
	if _, err := sealer.Open(sealed, nil); err == nil {
		t.Error("no binding opened a bound secret")
	}
}

// The sealed format names the key version it was sealed under, so rotating the key is a
// re-wrap of rows under a new version — never a mass of blobs nothing can attribute to a
// key. A version this deployment does not hold is refused by name.
func TestTheSealedFormatIsVersioned(t *testing.T) {
	t.Parallel()

	sealer := sealerWith(t, 1)
	sealed, err := sealer.Seal("a secret", nil)
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}
	if sealed[0] != KeyVersion {
		t.Fatalf("the sealed value must lead with key version %d, got %d", KeyVersion, sealed[0])
	}

	foreign := append([]byte(nil), sealed...)
	foreign[0] = 9
	_, err = sealer.Open(foreign, nil)
	if err == nil || !strings.Contains(err.Error(), "9") {
		t.Fatalf("an unheld key version must be refused by name, got %v", err)
	}

	if _, err := sealer.Open([]byte{KeyVersion, 1, 2}, nil); err == nil {
		t.Error("a value too short to hold a nonce opened")
	}
	if _, err := sealer.Open(nil, nil); err == nil {
		t.Error("an empty value opened")
	}
}

// A deployment with no key cannot hold a client secret, and says so rather than storing one in
// the clear.
func TestWithNoKeyNothingIsSealed(t *testing.T) {
	t.Parallel()

	var unconfigured Sealer
	if unconfigured.Configured() {
		t.Fatal("the zero sealer reports itself configured")
	}
	if _, err := unconfigured.Seal("a secret", nil); err == nil {
		t.Error("an unconfigured sealer sealed something")
	}
	if _, err := New(make([]byte, 16)); err == nil {
		t.Error("a 128-bit key was accepted where 256 is required")
	}
}

func sealerWith(t *testing.T, seed byte) Sealer {
	t.Helper()

	key := make([]byte, KeyLength)
	for index := range key {
		key[index] = seed + byte(index)
	}
	sealer, err := New(key)
	if err != nil {
		t.Fatalf("building a sealer: %v", err)
	}
	return sealer
}
