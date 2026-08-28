package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// OrganizationHeader selects the active Organization for an Organization-scoped request.
const OrganizationHeader = "X-OpenCluster-Organization"

// Guard holds the narrow dependencies needed to authorize the declared route table.
type Guard struct {
	Resolve             func(*http.Request) (Principal, error)
	ResolveOrganization func(context.Context, tenancy.Organization) (bool, error)
	Record              func(context.Context, tenancy.Organization, audit.Event)
	Origins             []string
	Logger              *slog.Logger
}

// Router validates and registers the declared routes behind authorization middleware.
func Router(table Table, guard Guard) (http.Handler, error) {
	if err := table.Validate(); err != nil {
		return nil, err
	}
	for _, route := range table {
		if (route.organizationScoped || route.organizationOptional) &&
			guard.ResolveOrganization == nil {
			return nil, fmt.Errorf("authz: privileged route %q has no Organization resolver",
				route.Key())
		}
	}
	mux := http.NewServeMux()
	for _, route := range table {
		mux.Handle(route.Key(), guard.protect(route))
	}
	return mux, nil
}

func (g Guard) protect(route Route) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if route.access == AccessPublic {
			route.handler.ServeHTTP(writer, request)
			return
		}

		principal, err := g.Resolve(request)
		if err != nil || principal.IsZero() {
			g.refuseUnauthenticated(writer, request, err)
			return
		}
		if route.access == AccessAuthenticated && !route.organizationScoped &&
			!route.organizationOptional && len(request.Header.Values(OrganizationHeader)) > 0 {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "active organization is not accepted"})
			return
		}

		var organization tenancy.Organization
		selectedOrganization := route.organizationScoped ||
			(route.organizationOptional && len(request.Header.Values(OrganizationHeader)) > 0)
		if selectedOrganization {
			organization, err = activeOrganization(request)
			if err != nil {
				g.recordAttributableRefusal(request, organization, principal, route,
					"active organization selector conflicts with route")
				writeJSON(writer, http.StatusBadRequest,
					errorView{Error: "active organization is required"})
				return
			}
			conflicts, bodyErr := bodyOrganizationConflicts(request, organization)
			if bodyErr != nil {
				writeJSON(writer, http.StatusRequestEntityTooLarge,
					errorView{Error: "request body is too large"})
				return
			}
			if conflicts {
				g.recordAttributableRefusal(request, organization, principal, route,
					"body organization conflicts with active organization")
				writeJSON(writer, http.StatusBadRequest,
					errorView{Error: "body organization conflicts with active organization"})
				return
			}
			known, resolveErr := g.ResolveOrganization(request.Context(), organization)
			if resolveErr != nil {
				g.Logger.ErrorContext(request.Context(), "active Organization resolution failed")
				writeJSON(writer, http.StatusServiceUnavailable,
					errorView{Error: "request failed"})
				return
			}
			if !known {
				writeJSON(writer, http.StatusNotFound,
					errorView{Error: "organization not found"})
				return
			}
		}

		if route.access == AccessAuthenticated && !selectedOrganization {
			if !g.originIsAllowed(principal, request) {
				g.refuseOrigin(writer, request, principal)
				return
			}
			route.handler.ServeHTTP(writer, request.WithContext(
				WithPrincipal(request.Context(), principal)))
			return
		}

		role, member := principal.RoleIn(organization)
		if !member {
			g.recordRefusal(request, organization, principal, route, "not a member")
			writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
			return
		}
		if !g.originIsAllowed(principal, request) {
			g.recordRefusal(request, organization, principal, route, "origin not allowed")
			g.refuseOrigin(writer, request, principal)
			return
		}
		if route.access == AccessPrivileged && !role.Grants(route.permission) {
			g.recordRefusal(request, organization, principal, route, "role does not grant it")
			writeJSON(writer, http.StatusForbidden, errorView{
				Error: "forbidden", Requires: string(route.permission),
			})
			return
		}

		ctx := WithPrincipal(request.Context(), principal)
		ctx = withActiveOrganization(ctx, organization)
		route.handler.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func organizationSelector(request *http.Request) (tenancy.Organization, error) {
	values := request.Header.Values(OrganizationHeader)
	if len(values) != 1 {
		return tenancy.Organization{}, fmt.Errorf("%s must be provided exactly once", OrganizationHeader)
	}
	organization, err := tenancy.NewOrganization(values[0])
	if err != nil {
		return tenancy.Organization{}, fmt.Errorf("%s is invalid: %w", OrganizationHeader, err)
	}
	return organization, nil
}

func activeOrganization(request *http.Request) (tenancy.Organization, error) {
	return organizationSelector(request)
}

const maxOrganizationCheckBody = 64 << 10

func bodyOrganizationConflicts(
	request *http.Request, selected tenancy.Organization,
) (bool, error) {
	if request.Body == nil || request.Body == http.NoBody {
		return false, nil
	}
	mediaType, _, _ := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if !strings.EqualFold(mediaType, "application/json") {
		return false, nil
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxOrganizationCheckBody+1))
	if err != nil {
		return false, err
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > maxOrganizationCheckBody {
		return false, fmt.Errorf("request body exceeds active Organization check limit")
	}
	if !json.Valid(body) {
		return false, nil
	}
	var decoded any
	if err = json.Unmarshal(body, &decoded); err != nil {
		return false, err
	}
	fields, object := decoded.(map[string]any)
	if !object {
		return false, nil
	}
	for _, key := range []string{"organization", "organizationId"} {
		value, exists := fields[key]
		if !exists {
			continue
		}
		organization, stringValue := value.(string)
		if !stringValue {
			return false, nil
		}
		return strings.TrimSpace(organization) != selected.String(), nil
	}
	return false, nil
}
