package tenancy_test

import (
	"errors"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

func TestNewOrganization_AcceptsUUID(t *testing.T) {
	t.Parallel()

	const id = "7d722f19-1978-4dc1-a176-e9be92b7e154"
	organization, err := tenancy.NewOrganization(id)
	if err != nil {
		t.Fatalf("NewOrganization(%q): %v", id, err)
	}
	if organization.String() != id {
		t.Errorf("String() = %q, want %q", organization.String(), id)
	}
	if organization.IsEmpty() {
		t.Error("a constructed Organization must not report zero")
	}
}

func TestNewOrganization_TrimsSurroundingWhitespace(t *testing.T) {
	t.Parallel()

	organization, err := tenancy.NewOrganization("  7d722f19-1978-4dc1-a176-e9be92b7e154\n")
	if err != nil {
		t.Fatalf("NewOrganization: %v", err)
	}
	if organization.String() != "7d722f19-1978-4dc1-a176-e9be92b7e154" {
		t.Errorf("String() = %q", organization.String())
	}
}

func TestNewOrganization_RejectsNonUUID(t *testing.T) {
	t.Parallel()

	if _, err := tenancy.NewOrganization("acme"); !errors.Is(err, tenancy.ErrInvalidOrganization) {
		t.Fatalf("NewOrganization error = %v, want ErrInvalidOrganization", err)
	}
}

// The zero value must be unusable rather than silently meaning "some organization".
// A store function receiving it must be able to tell.
func TestOrganization_ZeroValueIsRecognisable(t *testing.T) {
	t.Parallel()

	var zero tenancy.Organization
	if !zero.IsEmpty() {
		t.Error("the zero Organization must report IsEmpty")
	}
	if zero.String() != "" {
		t.Errorf("the zero Organization must stringify empty, got %q", zero.String())
	}
}

// Organizations are compared by identity, so they can be map keys and can be compared
// with == without a helper.
func TestOrganization_IsComparable(t *testing.T) {
	t.Parallel()

	first, _ := tenancy.NewOrganization("7d722f19-1978-4dc1-a176-e9be92b7e154")
	second, _ := tenancy.NewOrganization("7d722f19-1978-4dc1-a176-e9be92b7e154")
	other, _ := tenancy.NewOrganization("3dcdaf55-7542-47d5-bdf4-c7004e3a78f8")

	if first != second {
		t.Error("organizations with the same identifier must be equal")
	}
	if first == other {
		t.Error("organizations with different identifiers must not be equal")
	}

	byOrganization := map[tenancy.Organization]int{first: 1}
	if byOrganization[second] != 1 {
		t.Error("an organization must be usable as a map key")
	}
}
