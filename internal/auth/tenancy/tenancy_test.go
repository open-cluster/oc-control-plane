package tenancy_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

func TestNewOrganization_AcceptsRealisticIdentifiers(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"org_2abcDEF123", "acme", "a", strings.Repeat("o", 128)} {
		organization, err := tenancy.NewOrganization(id)
		if err != nil {
			t.Fatalf("NewOrganization(%q): %v", id, err)
		}
		if organization.String() != id {
			t.Errorf("String() = %q, want %q", organization.String(), id)
		}
		if organization.IsEmpty() {
			t.Errorf("a constructed organization must not report zero")
		}
	}
}

func TestNewOrganization_TrimsSurroundingWhitespace(t *testing.T) {
	t.Parallel()

	organization, err := tenancy.NewOrganization("  acme\n")
	if err != nil {
		t.Fatalf("NewOrganization: %v", err)
	}
	if organization.String() != "acme" {
		t.Errorf("String() = %q, want %q", organization.String(), "acme")
	}
}

func TestNewOrganization_RejectsUnusableIdentifiers(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty":            "",
		"blank":            "   ",
		"inner whitespace": "acme corp",
		"tab":              "acme\tcorp",
		"too long":         strings.Repeat("o", 129),
	}

	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := tenancy.NewOrganization(id); !errors.Is(err, tenancy.ErrInvalidOrganization) {
				t.Fatalf("NewOrganization(%q) error = %v, want ErrInvalidOrganization", id, err)
			}
		})
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

	first, _ := tenancy.NewOrganization("acme")
	second, _ := tenancy.NewOrganization("acme")
	other, _ := tenancy.NewOrganization("globex")

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

// An identifier can appear in a log line or an error, so it must not be able to smuggle
// control characters into either. Surrounding whitespace is trimmed rather than refused
// (see TestNewOrganization_TrimsSurroundingWhitespace) because a trailing newline is an
// artefact of how identifiers are pasted and stored, not an attack.
func TestNewOrganization_RejectsControlCharacters(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"acme\x00", "ac\rme", "ac\nme", "acme\x1b[31m", "acme\x7f"} {
		if _, err := tenancy.NewOrganization(id); err == nil {
			t.Errorf("NewOrganization(%q) must be refused", id)
		}
	}
}
