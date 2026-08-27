package reasoning

import (
	"context"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

func TestStaticDeploymentResolverReturnsTheMountedOSSDeploymentForEveryOrganization(t *testing.T) {
	t.Parallel()
	deployment := Deployment{Provider: "anthropic", Model: "model", Credential: "secret"}
	resolver := StaticDeploymentResolver{Deployment: deployment}
	first, _ := tenancy.NewOrganization("first")
	second, _ := tenancy.NewOrganization("second")

	for _, organization := range []tenancy.Organization{first, second} {
		got, err := resolver.Resolve(context.Background(), organization)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", organization, err)
		}
		if got.Provider != deployment.Provider || got.Model != deployment.Model ||
			got.Credential.Reveal() != deployment.Credential.Reveal() {
			t.Fatalf("Resolve(%s) = %#v, want mounted deployment", organization, got)
		}
	}
}

func TestStaticDeploymentResolverRejectsAnAmbientOrganization(t *testing.T) {
	t.Parallel()
	resolver := StaticDeploymentResolver{Deployment: Deployment{}}
	if _, err := resolver.Resolve(context.Background(), tenancy.Organization{}); err == nil {
		t.Fatal("Resolve accepted an empty Organization")
	}
}
