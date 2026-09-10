package tenancy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

var ErrInvalidOrganization = errors.New("invalid organization identifier")

type Organization struct {
	id uuid.UUID
}

func NewOrganization(id string) (Organization, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil || parsed == uuid.Nil {
		return Organization{}, fmt.Errorf("%w: must be a non-zero UUID", ErrInvalidOrganization)
	}
	return Organization{id: parsed}, nil
}

// String returns the identifier. The zero Organization stringifies empty.
func (o Organization) String() string {
	if o.id == uuid.Nil {
		return ""
	}
	return o.id.String()
}

func (o Organization) IsEmpty() bool {
	return o.id == uuid.Nil
}
