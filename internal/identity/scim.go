package identity

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// SCIM 2.0, as much of it as a directory actually sends.
//
// The standard is large and the part of it Okta and Microsoft Entra exercise is small: create a
// person, ask whether one exists by userName or externalId, replace them, set them inactive,
// delete them, and keep a group's membership in step. That is what is served.
//
// What is NOT served is refused with a SCIM error naming the operation, rather than accepted
// and ignored. A directory whose deprovisioning silently did nothing would be the worst failure
// this surface could have — everybody would believe access had been removed — so anything this
// build does not understand is a 400 the directory will show its administrator.
//
// The tenant comes from the PATH and the credential is checked against it by the same guard
// every other route uses. A directory is configured with a base URL and a bearer token; the
// token is an ordinary api_token bound to one Organization and to the DirectorySynchroniser
// role, so a leaked directory credential reaches the provisioning endpoints and nothing else.

// The SCIM media type and schema identifiers. They are constants because a directory checks
// them and because a typo in one is the kind of thing that works against one vendor.
const (
	scimContentType   = "application/scim+json"
	scimUserSchema    = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimGroupSchema   = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimListSchema    = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimErrorSchema   = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimPatchSchema   = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	scimConfigSchema  = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	scimResourceType  = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	scimDefaultCount  = 100
	scimMaxResults    = 200
	maxSCIMBodyLength = 256 * 1024
)

// scimError is what a directory is told when something is refused. The shape is the
// standard's; the wording is ours, and it is written for the administrator who will read it in
// their identity vendor's log rather than for a machine.
type scimError struct {
	Schemas []string `json:"schemas"`
	Detail  string   `json:"detail"`
	Status  string   `json:"status"`
	// ScimType is the standard's own machine-readable reason. A directory branches on it —
	// uniqueness in particular, which is how it learns to stop retrying a create.
	ScimType string `json:"scimType,omitempty"`
}

func writeSCIM(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", scimContentType)
	writer.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(writer).Encode(body)
	}
}

func writeSCIMError(writer http.ResponseWriter, status int, detail, scimType string) {
	writeSCIM(writer, status, scimError{
		Schemas:  []string{scimErrorSchema},
		Detail:   detail,
		Status:   strconv.Itoa(status),
		ScimType: scimType,
	})
}

// failSCIM maps a storage refusal onto the answer a directory knows how to act on.
func (h Handlers) failSCIM(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrProvisionedUserUnknown),
		errors.Is(err, storage.ErrDirectoryGroupUnknown):
		writeSCIMError(writer, http.StatusNotFound, "no such resource", "")
	case errors.Is(err, storage.ErrProvisionedUserExists),
		errors.Is(err, storage.ErrDirectoryGroupExists):
		// The standard's own type for it. A directory reads this and stops retrying the create,
		// which is the difference between one conflict and a loop.
		writeSCIMError(writer, http.StatusConflict,
			"another resource in this organization already uses that identifier", "uniqueness")
	case errors.Is(err, authz.ErrNotAMember), errors.Is(err, storage.ErrUnknownOrganization):
		writeSCIMError(writer, http.StatusNotFound, "no such organization", "")
	case errors.Is(err, storage.ErrAuditFailed):
		writeSCIMError(writer, http.StatusServiceUnavailable,
			"the change was refused because it could not be recorded", "")
	default:
		h.Logger.ErrorContext(request.Context(), "a provisioning request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeSCIMError(writer, http.StatusInternalServerError, "the request failed", "")
	}
}

// decodeSCIM reads a bounded SCIM body. Unknown fields are TOLERATED here, unlike everywhere
// else on this surface: a directory sends a great deal this build has no use for, and refusing
// a create because Okta included a locale would be refusing to work with Okta.
func decodeSCIM(writer http.ResponseWriter, request *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxSCIMBodyLength))
	if err := decoder.Decode(into); err != nil {
		writeSCIMError(writer, http.StatusBadRequest, "the body is not valid SCIM JSON", "invalidSyntax")
		return false
	}
	return true
}

// --- Users -------------------------------------------------------------------------------

// scimUser is a person in the shape a directory expects to read back.
type scimUser struct {
	Schemas    []string    `json:"schemas"`
	ID         string      `json:"id"`
	ExternalID string      `json:"externalId,omitempty"`
	UserName   string      `json:"userName"`
	Name       *scimName   `json:"name,omitempty"`
	Emails     []scimEmail `json:"emails,omitempty"`
	Active     bool        `json:"active"`
	// Roles is not a SCIM standard attribute this build accepts input on — it is reported so an
	// administrator looking at the directory's view can see what the group mapping produced,
	// and it is read-only. A directory that sent one would be ignored, which is why it is not
	// in the request shape at all.
	Roles []string `json:"roles,omitempty"`
	Meta  scimMeta `json:"meta"`
}

type scimName struct {
	Formatted  string `json:"formatted,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

type scimEmail struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
	Type    string `json:"type,omitempty"`
}

type scimMeta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Location     string `json:"location,omitempty"`
}

// scimUserRequest is what a directory sends. It is deliberately a subset: what is not here is
// not stored, and a directory reading its people back sees exactly what this product kept.
type scimUserRequest struct {
	Schemas    []string    `json:"schemas"`
	ExternalID string      `json:"externalId"`
	UserName   string      `json:"userName"`
	Name       *scimName   `json:"name"`
	Emails     []scimEmail `json:"emails"`
	Active     *bool       `json:"active"`
	// DisplayName is what some directories send instead of a name object.
	DisplayName string `json:"displayName"`
}

func (r scimUserRequest) toStorage() storage.NewProvisionedUser {
	wanted := storage.NewProvisionedUser{
		UserName:    strings.TrimSpace(r.UserName),
		ExternalID:  strings.TrimSpace(r.ExternalID),
		DisplayName: strings.TrimSpace(r.DisplayName),
		// A directory that says nothing about active means active. The standard says so, and a
		// person created inactive by default would be a person who silently cannot sign in.
		Active: r.Active == nil || *r.Active,
	}
	if r.Name != nil {
		if formatted := strings.TrimSpace(r.Name.Formatted); formatted != "" {
			wanted.DisplayName = formatted
		} else if joined := strings.TrimSpace(
			r.Name.GivenName + " " + r.Name.FamilyName); joined != "" && wanted.DisplayName == "" {
			wanted.DisplayName = joined
		}
	}
	for _, address := range r.Emails {
		if wanted.Email == "" || address.Primary {
			wanted.Email = strings.TrimSpace(address.Value)
		}
	}
	if wanted.Email == "" && strings.Contains(wanted.UserName, "@") {
		// Almost every directory sends the address as the userName, and several send nothing
		// else. Taking it is what makes those work without an administrator mapping a field.
		wanted.Email = wanted.UserName
	}
	if wanted.DisplayName == "" {
		wanted.DisplayName = wanted.UserName
	}
	return wanted
}

func (h Handlers) scimUserView(base string, user storage.ProvisionedUser) scimUser {
	view := scimUser{
		Schemas:    []string{scimUserSchema},
		ID:         user.ID.String(),
		ExternalID: user.ExternalID,
		UserName:   user.UserName,
		Active:     user.Active,
		Meta: scimMeta{
			ResourceType: "User",
			Created:      user.CreatedAt.UTC().Format(scimTimeLayout),
			LastModified: user.UpdatedAt.UTC().Format(scimTimeLayout),
			Location:     base + "/Users/" + user.ID.String(),
		},
	}
	if user.Email != "" {
		view.Emails = []scimEmail{{Value: user.Email, Primary: true, Type: "work"}}
	}
	if user.DisplayName != "" {
		view.Name = &scimName{Formatted: user.DisplayName}
	}
	if user.Role != "" && user.Active {
		view.Roles = []string{string(user.Role)}
	}
	return view
}

const scimTimeLayout = "2006-01-02T15:04:05Z"

func (h Handlers) listSCIMUsers(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	filter, ok := parseUserFilter(writer, request.URL.Query().Get("filter"))
	if !ok {
		return
	}
	start, count := scimPaging(request)

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	list, err := h.Database.ProvisionedUsers(ctx, principal, organization, filter, start, count)
	if err != nil {
		h.failSCIM(writer, request, err)
		return
	}

	base := h.scimBase(organization)
	resources := make([]scimUser, 0, len(list.Users))
	for _, user := range list.Users {
		resources = append(resources, h.scimUserView(base, user))
	}
	writeSCIM(writer, http.StatusOK, map[string]any{
		"schemas":      []string{scimListSchema},
		"totalResults": list.Total,
		"startIndex":   start,
		"itemsPerPage": len(resources),
		"Resources":    resources,
	})
}

func (h Handlers) readSCIMUser(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	id, ok := scimIdentifier(writer, request, "user")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	user, err := h.Database.ProvisionedUser(ctx, principal, organization, id)
	if err != nil {
		h.failSCIM(writer, request, err)
		return
	}
	writeSCIM(writer, http.StatusOK, h.scimUserView(h.scimBase(organization), user))
}

func (h Handlers) createSCIMUser(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	var body scimUserRequest
	if !decodeSCIM(writer, request, &body) {
		return
	}
	if strings.TrimSpace(body.UserName) == "" {
		writeSCIMError(writer, http.StatusBadRequest, "userName is required", "invalidValue")
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	created, err := h.Database.ProvisionUser(ctx, principal, organization, body.toStorage())
	if err != nil {
		h.failSCIM(writer, request, err)
		return
	}
	writeSCIM(writer, http.StatusCreated, h.scimUserView(h.scimBase(organization), created))
}

func (h Handlers) replaceSCIMUser(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	id, ok := scimIdentifier(writer, request, "user")
	if !ok {
		return
	}
	var body scimUserRequest
	if !decodeSCIM(writer, request, &body) {
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	replaced, err := h.Database.ReplaceProvisionedUser(
		ctx, principal, organization, id, body.toStorage())
	if err != nil {
		h.failSCIM(writer, request, err)
		return
	}
	writeSCIM(writer, http.StatusOK, h.scimUserView(h.scimBase(organization), replaced))
}

// scimPatch is the operation document a directory sends far more often than a replacement.
type scimPatch struct {
	Schemas    []string        `json:"schemas"`
	Operations []scimOperation `json:"Operations"`
}

type scimOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// patchSCIMUser applies the operations a directory actually sends.
//
// In practice that is one: set active. Everything else a directory changes about a person it
// sends as a replacement. An operation this build does not understand is REFUSED rather than
// ignored, because a directory whose deprovisioning silently did nothing is the worst failure
// this surface could have.
func (h Handlers) patchSCIMUser(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	id, ok := scimIdentifier(writer, request, "user")
	if !ok {
		return
	}
	var body scimPatch
	if !decodeSCIM(writer, request, &body) {
		return
	}
	if len(body.Operations) == 0 {
		writeSCIMError(writer, http.StatusBadRequest, "no operations", "invalidValue")
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	user, err := h.Database.ProvisionedUser(ctx, principal, organization, id)
	if err != nil {
		h.failSCIM(writer, request, err)
		return
	}

	for _, operation := range body.Operations {
		active, understood := activeFromOperation(operation)
		if !understood {
			writeSCIMError(writer, http.StatusBadRequest,
				"this service understands only the active attribute on a user patch; send "+
					"everything else as a replacement", "invalidPath")
			return
		}
		user, err = h.Database.SetProvisionedUserActive(
			ctx, principal, organization, id, active)
		if err != nil {
			h.failSCIM(writer, request, err)
			return
		}
	}
	writeSCIM(writer, http.StatusOK, h.scimUserView(h.scimBase(organization), user))
}

// activeFromOperation reads the one operation this build applies, in the two shapes directories
// send it: a path naming the attribute, and a bare value object holding it.
func activeFromOperation(operation scimOperation) (bool, bool) {
	if !strings.EqualFold(operation.Op, "replace") && !strings.EqualFold(operation.Op, "add") {
		return false, false
	}

	if strings.EqualFold(strings.TrimSpace(operation.Path), "active") {
		var active anyBool
		if err := json.Unmarshal(operation.Value, &active); err != nil {
			return false, false
		}
		return bool(active), true
	}
	if strings.TrimSpace(operation.Path) != "" {
		return false, false
	}

	var attributes map[string]json.RawMessage
	if err := json.Unmarshal(operation.Value, &attributes); err != nil {
		return false, false
	}
	raw, present := attributes["active"]
	if !present || len(attributes) != 1 {
		// More than one attribute in a pathless patch would mean applying some and refusing
		// others, which is worse than refusing the document.
		return false, false
	}
	var active anyBool
	if err := json.Unmarshal(raw, &active); err != nil {
		return false, false
	}
	return bool(active), true
}

func (h Handlers) deleteSCIMUser(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	id, ok := scimIdentifier(writer, request, "user")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Database.DeprovisionUser(ctx, principal, organization, id); err != nil {
		h.failSCIM(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// --- shared helpers ------------------------------------------------------------------------

func (h Handlers) scimCaller(
	writer http.ResponseWriter, request *http.Request,
) (authz.Principal, tenancy.Organization, bool) {
	principal, ok := authz.Of(request)
	if !ok {
		writeSCIMError(writer, http.StatusInternalServerError, "the request failed", "")
		return authz.Principal{}, tenancy.Organization{}, false
	}
	organization, err := tenancy.NewOrganization(request.PathValue("organization"))
	if err != nil {
		writeSCIMError(writer, http.StatusNotFound, "no such organization", "")
		return authz.Principal{}, tenancy.Organization{}, false
	}
	return principal, organization, true
}

func (h Handlers) scimBase(organization tenancy.Organization) string {
	return strings.TrimSuffix(h.PublicURL, "/") + SCIMBase + "/organizations/" +
		organization.String()
}

func scimIdentifier(
	writer http.ResponseWriter, request *http.Request, segment string,
) (uuid.UUID, bool) {
	id, err := uuid.Parse(request.PathValue(segment))
	if err != nil {
		// A directory addressing an identifier this product never issued is asking about
		// something that does not exist, rather than making a malformed request.
		writeSCIMError(writer, http.StatusNotFound, "no such resource", "")
		return uuid.Nil, false
	}
	return id, true
}

func scimPaging(request *http.Request) (int, int) {
	start, err := strconv.Atoi(request.URL.Query().Get("startIndex"))
	if err != nil || start < 1 {
		start = 1
	}
	count, err := strconv.Atoi(request.URL.Query().Get("count"))
	if err != nil || count < 1 {
		count = scimDefaultCount
	}
	return start, min(count, scimMaxResults)
}

// equalityFilter reads the one filter form a directory sends: `attribute eq "value"`.
//
// SCIM's filter language has boolean operators, precedence and a dozen comparators. This
// implements the fragment every directory uses for its existence probe, and the handler REFUSES
// anything else rather than returning a list it did not actually filter — an unfiltered answer
// to a filtered query is how a directory concludes a person does not exist and creates them
// again.
var equalityFilter = regexp.MustCompile(`^\s*(\w+)\s+eq\s+"([^"]*)"\s*$`)

func parseUserFilter(
	writer http.ResponseWriter, filter string,
) (storage.ProvisionedUserFilter, bool) {
	if strings.TrimSpace(filter) == "" {
		return storage.ProvisionedUserFilter{}, true
	}
	match := equalityFilter.FindStringSubmatch(filter)
	if match == nil {
		writeSCIMError(writer, http.StatusBadRequest,
			`this service understands only filters of the form: attribute eq "value"`,
			"invalidFilter")
		return storage.ProvisionedUserFilter{}, false
	}

	switch strings.ToLower(match[1]) {
	case "username":
		return storage.ProvisionedUserFilter{UserName: match[2]}, true
	case "externalid":
		return storage.ProvisionedUserFilter{ExternalID: match[2]}, true
	default:
		writeSCIMError(writer, http.StatusBadRequest,
			"this service filters users by userName or externalId", "invalidFilter")
		return storage.ProvisionedUserFilter{}, false
	}
}
