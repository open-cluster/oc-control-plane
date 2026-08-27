package identity_test

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/identity"
)

func TestOSSIdentityRoutesExcludeNativeEnterpriseProtocols(t *testing.T) {
	for _, route := range (identity.Handlers{}).Routes() {
		key := route.Key()
		for _, retired := range []string{
			"/scim/", "/sign-in/saml/", "/identity-providers", "/directory-groups",
		} {
			if strings.Contains(key, retired) {
				t.Errorf("active OSS identity route %q contains retired surface %q", key, retired)
			}
		}
	}
}
