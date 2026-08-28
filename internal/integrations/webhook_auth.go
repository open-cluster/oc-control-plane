package integrations

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

const WebhookTokenHeader = "X-OpenCluster-Token"

// AuthenticateWebhookToken checks the bounded static sender credential without exposing
// whether an Integration holds a secret.
func AuthenticateWebhookToken(headers http.Header, integration Integration) bool {
	presented := headers.Get(WebhookTokenHeader)
	if presented == "" || len(presented) > 256 {
		return false
	}
	digest := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(digest[:], integration.WebhookSecretDigest) == 1
}
