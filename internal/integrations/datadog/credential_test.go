package datadog

import (
	"errors"
	"testing"
)

func TestParseCredentialRoundTrips(t *testing.T) {
	t.Parallel()

	encoded, err := encodeCredential("api-key", "app-key")
	if err != nil {
		t.Fatalf("encodeCredential: %v", err)
	}
	parsed, err := parseCredential(encoded)
	if err != nil {
		t.Fatalf("parseCredential: %v", err)
	}
	if parsed.APIKey != "api-key" || parsed.ApplicationKey != "app-key" {
		t.Errorf("parsed = %+v", parsed)
	}
}

func TestParseCredentialRefusesMalformedJSON(t *testing.T) {
	t.Parallel()

	if _, err := parseCredential("not json"); !errors.Is(err, ErrMalformedCredential) {
		t.Errorf("err = %v, want ErrMalformedCredential", err)
	}
}

func TestParseCredentialRefusesAMissingHalf(t *testing.T) {
	t.Parallel()

	if _, err := parseCredential(`{"apiKey":"only-one"}`); !errors.Is(err, ErrMalformedCredential) {
		t.Errorf("err = %v, want ErrMalformedCredential: appKey is missing", err)
	}
}
