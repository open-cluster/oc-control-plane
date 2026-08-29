package identity

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type organizationView struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Membership  struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	} `json:"membership"`
}

type organizationListView struct {
	Organizations []organizationView `json:"organizations"`
}

type createOrganizationRequest struct {
	DisplayName   string `json:"displayName"`
	RequestedSlug string `json:"requestedSlug,omitempty"`
}

func (h Handlers) organizations(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	memberships := principal.Memberships()
	views := make([]organizationView, 0, len(memberships))
	for _, membership := range memberships {
		view := organizationView{
			ID: membership.Organization.String(), DisplayName: membership.DisplayName,
		}
		view.Membership.ID = membership.ID
		view.Membership.Role = string(membership.Role)
		views = append(views, view)
	}
	writeJSON(writer, http.StatusOK, organizationListView{Organizations: views})
}

func (h Handlers) createOrganization(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	if h.CanCreateOrganization == nil || !h.CanCreateOrganization(principal) {
		writeJSON(writer, http.StatusForbidden,
			errorView{Error: "organization creation is not permitted"})
		return
	}
	var body createOrganizationRequest
	if !decode(writer, request, &body) {
		return
	}
	displayName := strings.TrimSpace(body.DisplayName)
	if displayName == "" || len(displayName) > 256 {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "displayName must be between 1 and 256 characters"})
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()
	requested := strings.TrimSpace(body.RequestedSlug) != ""
	attempts := 1
	if !requested {
		attempts = 3
	}
	var membership authz.Membership
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		slug, slugErr := organizationSlug(displayName, body.RequestedSlug)
		if slugErr != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "requestedSlug is not a canonical Organization slug"})
			return
		}
		organization, organizationErr := tenancy.NewOrganization(slug)
		if organizationErr != nil {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "Organization slug is invalid"})
			return
		}
		membership, err = h.Database.CreateOrganization(ctx, principal, organization, displayName)
		if !errors.Is(err, storage.ErrOrganizationExists) || requested {
			break
		}
	}
	if errors.Is(err, storage.ErrOrganizationExists) {
		writeJSON(writer, http.StatusConflict, errorView{Error: "organization already exists"})
		return
	}
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	view := organizationView{ID: membership.Organization.String(), DisplayName: displayName}
	view.Membership.ID = membership.ID
	view.Membership.Role = string(membership.Role)
	writeJSON(writer, http.StatusCreated, view)
}

var canonicalOrganizationSlug = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func organizationSlug(displayName, requested string) (string, error) {
	if strings.TrimSpace(requested) != "" {
		slug := strings.ToLower(strings.TrimSpace(requested))
		if !canonicalOrganizationSlug.MatchString(slug) {
			return "", tenancy.ErrInvalidOrganization
		}
		return slug, nil
	}
	var builder strings.Builder
	separator := false
	for _, character := range strings.ToLower(displayName) {
		if character <= unicode.MaxASCII &&
			((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9')) {
			if separator && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			builder.WriteRune(character)
			separator = false
		} else {
			separator = true
		}
	}
	base := strings.Trim(builder.String(), "-")
	if base == "" {
		base = "organization"
	}
	if len(base) > 54 {
		base = strings.TrimRight(base[:54], "-")
	}
	return base + "-" + strings.ReplaceAll(uuid.New().String()[:8], "-", ""), nil
}

func (h Handlers) permissions(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	role, member := principal.RoleIn(organization)
	if !member {
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
		return
	}
	permissions := make([]string, 0, len(authz.Permissions()))
	for _, permission := range authz.Permissions() {
		if role.Grants(permission) {
			permissions = append(permissions, string(permission))
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"organizationId": organization.String(),
		"role":           string(role),
		"permissions":    permissions,
	})
}
