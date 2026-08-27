package seal

import (
	"crypto/aes"
	"crypto/cipher"
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

func TestKeyringReadsOldKeysAndRewrapsUnderTheActiveKey(t *testing.T) {
	t.Parallel()

	oldKey := testKey(3)
	newKey := testKey(7)
	old, err := NewKeyring(Key{ID: "old", Material: oldKey})
	if err != nil {
		t.Fatalf("building old keyring: %v", err)
	}
	sealed, err := old.Seal("xoxb-rotating", []byte("integration-a"))
	if err != nil {
		t.Fatalf("sealing under old key: %v", err)
	}

	rotating, err := NewKeyring(
		Key{ID: "current", Material: newKey},
		Key{ID: "old", Material: oldKey},
	)
	if err != nil {
		t.Fatalf("building rotating keyring: %v", err)
	}
	opened, err := rotating.Open(sealed, []byte("integration-a"))
	if err != nil || opened != "xoxb-rotating" {
		t.Fatalf("opening old envelope during rotation: %q, %v", opened, err)
	}

	rewrapped, changed, err := rotating.Rewrap(sealed, []byte("integration-a"))
	if err != nil {
		t.Fatalf("rewrapping: %v", err)
	}
	if !changed {
		t.Fatal("old envelope was not reported as rewrapped")
	}
	keyID, err := EnvelopeKeyID(rewrapped)
	if err != nil || keyID != "current" {
		t.Fatalf("rewrapped key id = %q, %v; want current", keyID, err)
	}

	currentOnly, err := NewKeyring(Key{ID: "current", Material: newKey})
	if err != nil {
		t.Fatalf("building current keyring: %v", err)
	}
	opened, err = currentOnly.Open(rewrapped, []byte("integration-a"))
	if err != nil || opened != "xoxb-rotating" {
		t.Fatalf("opening rewrapped envelope without old key: %q, %v", opened, err)
	}
	if _, err := currentOnly.Open(sealed, []byte("integration-a")); err == nil {
		t.Fatal("old envelope opened after the old key was removed")
	}
}

func TestRewrapAuthenticatesAnEnvelopeAlreadyNamingTheActiveKey(t *testing.T) {
	t.Parallel()

	keyring, err := NewKeyring(Key{ID: "current", Material: testKey(5)})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := keyring.Seal("credential", []byte("integration-a"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, _, err = keyring.Rewrap(tampered, []byte("integration-a")); err == nil {
		t.Fatal("rewrap accepted a tampered envelope merely because it named the active key")
	}
}

func TestKeyringReadsTheVersionOneCompatibilityEnvelope(t *testing.T) {
	t.Parallel()

	material := testKey(4)
	legacy := legacyEnvelope(t, material, "legacy-value", []byte("integration-a"))
	keyring, err := NewKeyring(Key{ID: "current", Material: testKey(8)},
		Key{ID: "legacy", Material: material})
	if err != nil {
		t.Fatalf("building keyring: %v", err)
	}
	opened, err := keyring.Open(legacy, []byte("integration-a"))
	if err != nil || opened != "legacy-value" {
		t.Fatalf("opening version-one envelope: %q, %v", opened, err)
	}
}

func TestKeyringRefusesAmbiguousOrUnsafeKeyIdentifiers(t *testing.T) {
	t.Parallel()

	material := testKey(1)
	for _, key := range []Key{
		{ID: "", Material: material},
		{ID: "contains space", Material: material},
		{ID: strings.Repeat("x", 65), Material: material},
	} {
		if _, err := NewKeyring(key); err == nil {
			t.Fatalf("unsafe key identifier %q was accepted", key.ID)
		}
	}
	if _, err := NewKeyring(
		Key{ID: "same", Material: material},
		Key{ID: "same", Material: testKey(2)},
	); err == nil {
		t.Fatal("duplicate key identifiers were accepted")
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

	sealer, err := New(testKey(seed))
	if err != nil {
		t.Fatalf("building a sealer: %v", err)
	}
	return sealer
}

func testKey(seed byte) []byte {
	key := make([]byte, KeyLength)
	for index := range key {
		key[index] = seed + byte(index)
	}
	return key
}

func legacyEnvelope(t *testing.T, material []byte, plaintext string, binding []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(material)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	for index := range nonce {
		nonce[index] = byte(index + 1)
	}
	header := append([]byte{LegacyKeyVersion}, nonce...)
	return aead.Seal(header, nonce, []byte(plaintext), binding)
}
