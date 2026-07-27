// Package tenancy holds the vocabulary of the tenant boundary. An Organization is that
// boundary: every durable record belongs to exactly one, and every store function takes
// one explicitly rather than reading it from ambient context. The package deliberately
// performs no I/O — where an organization's data physically lives is a placement, which
// internal/storage resolves.
package tenancy

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// maxOrganizationLength bounds an identifier so it cannot be used to inflate a log line
// or an index entry. External identity providers issue identifiers far shorter than this.
const maxOrganizationLength = 128

// ErrInvalidOrganization reports an identifier that cannot name an organization.
var ErrInvalidOrganization = errors.New("invalid organization identifier")

// Organization is the tenant boundary. The identifier is unexported so an Organization
// cannot be constructed by literal without passing validation, which is what lets a store
// function trust the value it receives.
type Organization struct {
	id string
}

// NewOrganization validates and returns an Organization. Surrounding whitespace is
// trimmed; anything else unusable is refused rather than normalised, because silently
// rewriting a tenant identifier is how two organizations become one.
func NewOrganization(id string) (Organization, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return Organization{}, fmt.Errorf("%w: must not be empty", ErrInvalidOrganization)
	}
	if len(trimmed) > maxOrganizationLength {
		return Organization{}, fmt.Errorf("%w: must be at most %d bytes",
			ErrInvalidOrganization, maxOrganizationLength)
	}
	for _, character := range trimmed {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return Organization{}, fmt.Errorf(
				"%w: must not contain whitespace or control characters", ErrInvalidOrganization)
		}
	}
	return Organization{id: trimmed}, nil
}

// String returns the identifier. The zero Organization stringifies empty.
func (o Organization) String() string {
	return o.id
}

// IsEmpty reports whether this is the zero Organization, which names no tenant and must
// never reach a query.
func (o Organization) IsEmpty() bool {
	return o.id == ""
}
