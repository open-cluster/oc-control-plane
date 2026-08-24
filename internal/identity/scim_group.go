package identity

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// directoryGroup is storage.DirectoryGroup under the name this package uses for it. It is an
// alias rather than a type of its own because the type genuinely belongs here and cannot yet
// live here — see the ADR-017 note in the specification — and a second differently-shaped
// value would deepen that debt rather than mark it.
type directoryGroup = storage.DirectoryGroup

// A directory's groups, and the one decision about them that is not the directory's.
//
// The directory owns who is in a group. An ADMINISTRATOR owns what a group means here — the
// role it grants — and that mapping is not something SCIM can set. If a directory could decide
// what its groups grant, a change in the customer's own identity vendor would be a privilege
// grant in this product, made by whoever can edit a group there.

type scimGroup struct {
	Schemas     []string          `json:"schemas"`
	ID          string            `json:"id"`
	ExternalID  string            `json:"externalId,omitempty"`
	DisplayName string            `json:"displayName"`
	Members     []scimGroupMember `json:"members"`
	Meta        scimMeta          `json:"meta"`
	// Role is read-only and is reported so an administrator inspecting what the directory sees
	// can tell a mapped group from an unmapped one. A directory sending it is ignored, which is
	// why it is absent from the request shape.
	Role string `json:"role,omitempty"`
}

type scimGroupMember struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
	Ref     string `json:"$ref,omitempty"`
}

type scimGroupRequest struct {
	Schemas     []string          `json:"schemas"`
	ExternalID  string            `json:"externalId"`
	DisplayName string            `json:"displayName"`
	Members     []scimGroupMember `json:"members"`
}

func (h Handlers) scimGroupView(base string, group directoryGroup) scimGroup {
	members := make([]scimGroupMember, 0, len(group.Members))
	for _, member := range group.Members {
		members = append(members, scimGroupMember{
			Value: member.String(),
			Ref:   base + "/Users/" + member.String(),
		})
	}
	return scimGroup{
		Schemas:     []string{scimGroupSchema},
		ID:          group.ID.String(),
		ExternalID:  group.ExternalID,
		DisplayName: group.DisplayName,
		Members:     members,
		Role:        string(group.Role),
		Meta: scimMeta{
			ResourceType: "Group",
			Created:      group.CreatedAt.UTC().Format(scimTimeLayout),
			LastModified: group.UpdatedAt.UTC().Format(scimTimeLayout),
			Location:     base + "/Groups/" + group.ID.String(),
		},
	}
}

func (h Handlers) listSCIMGroups(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	name, ok := parseGroupFilter(writer, request.URL.Query().Get("filter"))
	if !ok {
		return
	}
	start, count := scimPaging(request)

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	list, err := h.Database.DirectoryGroups(ctx, principal, organization, name, start, count)
	if err != nil {
		h.failSCIM(writer, request, err)
		return
	}

	base := h.scimBase(organization)
	resources := make([]scimGroup, 0, len(list.Groups))
	for _, group := range list.Groups {
		resources = append(resources, h.scimGroupView(base, group))
	}
	writeSCIM(writer, http.StatusOK, map[string]any{
		"schemas":      []string{scimListSchema},
		"totalResults": list.Total,
		"startIndex":   start,
		"itemsPerPage": len(resources),
		"Resources":    resources,
	})
}

func (h Handlers) readSCIMGroup(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	id, ok := scimIdentifier(writer, request, "group")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	group, err := h.Database.DirectoryGroup(ctx, principal, organization, id)
	if err != nil {
		h.failSCIM(writer, request, err)
		return
	}
	writeSCIM(writer, http.StatusOK, h.scimGroupView(h.scimBase(organization), group))
}

func (h Handlers) createSCIMGroup(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	var body scimGroupRequest
	if !decodeSCIM(writer, request, &body) {
		return
	}
	if strings.TrimSpace(body.DisplayName) == "" {
		writeSCIMError(writer, http.StatusBadRequest, "displayName is required", "invalidValue")
		return
	}
	members, ok := memberIdentifiers(writer, body.Members)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	created, err := h.Database.CreateDirectoryGroup(ctx, principal, organization,
		strings.TrimSpace(body.DisplayName), strings.TrimSpace(body.ExternalID), members)
	if err != nil {
		h.failSCIM(writer, request, err)
		return
	}
	writeSCIM(writer, http.StatusCreated, h.scimGroupView(h.scimBase(organization), created))
}

func (h Handlers) replaceSCIMGroup(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	id, ok := scimIdentifier(writer, request, "group")
	if !ok {
		return
	}
	var body scimGroupRequest
	if !decodeSCIM(writer, request, &body) {
		return
	}
	members, ok := memberIdentifiers(writer, body.Members)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	replaced, err := h.Database.ReplaceDirectoryGroup(ctx, principal, organization, id,
		strings.TrimSpace(body.DisplayName), strings.TrimSpace(body.ExternalID), members)
	if err != nil {
		h.failSCIM(writer, request, err)
		return
	}
	writeSCIM(writer, http.StatusOK, h.scimGroupView(h.scimBase(organization), replaced))
}

// patchSCIMGroup applies the membership changes a directory sends, which is the operation this
// whole surface exists to serve well: a person added to or removed from a group is a person
// gaining or losing a role here, and it has to take effect without anybody signing in again.
func (h Handlers) patchSCIMGroup(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	id, ok := scimIdentifier(writer, request, "group")
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

	var added, removed []uuid.UUID
	replaceWith, replacing := []uuid.UUID(nil), false
	for _, operation := range body.Operations {
		change, understood := memberChangeOf(operation)
		if !understood {
			writeSCIMError(writer, http.StatusBadRequest,
				"this service understands only add, remove and replace on the members "+
					"attribute of a group", "invalidPath")
			return
		}
		switch change.op {
		case "add":
			added = append(added, change.members...)
		case "remove":
			removed = append(removed, change.members...)
		case "replace":
			replaceWith, replacing = change.members, true
		}
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	var (
		group directoryGroup
		err   error
	)
	if replacing {
		existing, readErr := h.Database.DirectoryGroup(ctx, principal, organization, id)
		if readErr != nil {
			h.failSCIM(writer, request, readErr)
			return
		}
		group, err = h.Database.ReplaceDirectoryGroup(ctx, principal, organization, id,
			existing.DisplayName, existing.ExternalID, replaceWith)
	} else {
		group, err = h.Database.ChangeDirectoryGroupMembers(
			ctx, principal, organization, id, added, removed)
	}
	if err != nil {
		h.failSCIM(writer, request, err)
		return
	}
	writeSCIM(writer, http.StatusOK, h.scimGroupView(h.scimBase(organization), group))
}

func (h Handlers) deleteSCIMGroup(writer http.ResponseWriter, request *http.Request) {
	principal, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	id, ok := scimIdentifier(writer, request, "group")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Database.DeleteDirectoryGroup(ctx, principal, organization, id); err != nil {
		h.failSCIM(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// mapSCIMGroupToRole is the administrator's decision, and it is NOT a SCIM route: it is on the
// operator surface behind identity.configure, because it is the one thing here a directory must
// not be able to do.
// groupRoleRequest is what a directory group is mapped to. Named rather than anonymous so
// the deployment's self-description can publish its shape: an anonymous struct has nothing
// for a document to point at, and a client would be left to discover the field by being
// refused.
type groupRoleRequest struct {
	// Role is what membership of this group grants here. Empty UNMAPS the group, which is
	// how an administrator withdraws what it grants.
	Role string `json:"role"`
}

func (h Handlers) mapSCIMGroupToRole(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	id, ok := identifier(writer, request, "group")
	if !ok {
		return
	}
	var body groupRoleRequest
	if !decode(writer, request, &body) {
		return
	}

	// An empty role UNMAPS the group, which is how an administrator withdraws what it grants.
	role := authz.Role("")
	if trimmed := strings.TrimSpace(body.Role); trimmed != "" {
		parsed, known := authz.ParseRole(trimmed)
		if !known {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "role is not one this build has"})
			return
		}
		// The same refusal the just-in-time role and the token role get, for the same reason: a
		// directory group is edited by whoever administers the customer's identity vendor, and
		// an admin arriving that way is an administrative takeover one directory edit wide.
		if parsed == authz.Admin {
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: "admin may not be granted by a directory group; grant it to a person " +
					"deliberately"})
			return
		}
		role = parsed
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	mapped, err := h.Database.MapDirectoryGroupToRole(ctx, principal, organization, id, role)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, directoryGroupViewOf(mapped))
}

// listDirectoryGroups is the administrator's read of the same groups, on the operator surface,
// so they can see what a directory has synchronised and decide what each grants.
func (h Handlers) listDirectoryGroups(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	list, err := h.Database.DirectoryGroups(ctx, principal, organization, "", 1, scimMaxResults)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]directoryGroupView, 0, len(list.Groups))
	for _, group := range list.Groups {
		views = append(views, directoryGroupViewOf(group))
	}
	writeJSON(writer, http.StatusOK, directoryGroupListView{Groups: views})
}

// memberChange is one understood group operation.
type memberChange struct {
	op      string
	members []uuid.UUID
}

// memberPath matches the two forms a directory sends a removal in: the bare attribute, and the
// attribute with a filter naming exactly which member. Entra sends the second.
var memberPath = regexp.MustCompile(`^\s*members(?:\[\s*value\s+eq\s+"([^"]*)"\s*\])?\s*$`)

func memberChangeOf(operation scimOperation) (memberChange, bool) {
	op := strings.ToLower(strings.TrimSpace(operation.Op))
	switch op {
	case "add", "remove", "replace":
	default:
		return memberChange{}, false
	}

	path := strings.TrimSpace(operation.Path)
	if path == "" {
		// A pathless operation carries {"members": [...]}. Anything else in it would mean
		// applying part of a document and refusing the rest.
		var attributes map[string][]scimGroupMember
		if err := json.Unmarshal(operation.Value, &attributes); err != nil {
			return memberChange{}, false
		}
		members, present := attributes["members"]
		if !present || len(attributes) != 1 {
			return memberChange{}, false
		}
		parsed, ok := parseMembers(members)
		return memberChange{op: op, members: parsed}, ok
	}

	match := memberPath.FindStringSubmatch(path)
	if match == nil {
		return memberChange{}, false
	}
	// A path naming exactly one member — members[value eq "..."] — carries the member in the
	// path rather than in a value, which is how a removal usually arrives.
	if match[1] != "" {
		id, err := uuid.Parse(match[1])
		if err != nil {
			return memberChange{}, false
		}
		return memberChange{op: op, members: []uuid.UUID{id}}, true
	}

	var members []scimGroupMember
	if len(operation.Value) == 0 {
		// `remove` on the whole attribute with no value clears it.
		return memberChange{op: "replace", members: nil}, true
	}
	if err := json.Unmarshal(operation.Value, &members); err != nil {
		return memberChange{}, false
	}
	parsed, ok := parseMembers(members)
	return memberChange{op: op, members: parsed}, ok
}

func parseMembers(members []scimGroupMember) ([]uuid.UUID, bool) {
	parsed := make([]uuid.UUID, 0, len(members))
	for _, member := range members {
		id, err := uuid.Parse(strings.TrimSpace(member.Value))
		if err != nil {
			return nil, false
		}
		parsed = append(parsed, id)
	}
	return parsed, true
}

func memberIdentifiers(
	writer http.ResponseWriter, members []scimGroupMember,
) ([]uuid.UUID, bool) {
	parsed, ok := parseMembers(members)
	if !ok {
		writeSCIMError(writer, http.StatusBadRequest,
			"a member's value must be a user identifier this service issued", "invalidValue")
		return nil, false
	}
	return parsed, true
}

func parseGroupFilter(writer http.ResponseWriter, filter string) (string, bool) {
	if strings.TrimSpace(filter) == "" {
		return "", true
	}
	match := equalityFilter.FindStringSubmatch(filter)
	if match == nil || !strings.EqualFold(match[1], "displayName") {
		writeSCIMError(writer, http.StatusBadRequest,
			`this service filters groups by displayName eq "value"`, "invalidFilter")
		return "", false
	}
	return match[2], true
}
