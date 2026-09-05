package tenancy

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

const maxOrganizationLength = 128

var ErrInvalidOrganization = errors.New("invalid organization identifier")

type Organization struct {
	id string
}

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

func (o Organization) IsEmpty() bool {
	return o.id == ""
}
