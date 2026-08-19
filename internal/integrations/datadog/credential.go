package datadog

import (
	"encoding/json"
	"errors"
)

// credential is the two keys a Datadog read needs, together. Reads require BOTH an API key
// and an application key — writes need only the first, but this provider never writes — and
// the catalog's Definition holds exactly one Secret field, so the pair travels sealed as one
// JSON value rather than as two fields the schema has no room for.
type credential struct {
	APIKey         string `json:"apiKey"`
	ApplicationKey string `json:"appKey"`
}

// ErrMalformedCredential reports a secret value that is not the {"apiKey","appKey"} shape
// this provider seals. It is distinguished from a vendor refusal: the far end was never
// reached, so an operator fixing this pastes the credential again rather than checking
// Datadog's status page.
var ErrMalformedCredential = errors.New("the stored credential is not an apiKey/appKey pair")

func parseCredential(raw string) (credential, error) {
	var parsed credential
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return credential{}, ErrMalformedCredential
	}
	if parsed.APIKey == "" || parsed.ApplicationKey == "" {
		return credential{}, ErrMalformedCredential
	}
	return parsed, nil
}

func encodeCredential(apiKey, applicationKey string) (string, error) {
	encoded, err := json.Marshal(credential{APIKey: apiKey, ApplicationKey: applicationKey})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
