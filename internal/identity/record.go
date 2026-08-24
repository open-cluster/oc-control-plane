package identity

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
)

// auditEvents reads a tenant's record.
//
// It is the only route the Auditor role can reach, which is what makes story 19 true: an
// auditor's access does not itself become a risk, because there is nothing else their role
// opens. The events are returned newest first and paged, because the answer to "who disabled
// this Integration and when" is found by reading backwards from now.
func (h Handlers) auditEvents(writer http.ResponseWriter, request *http.Request) {
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

	list, err := h.Database.AuditEvents(ctx, principal, organization, audit.Page{
		Limit: pageSize(request),
		After: request.URL.Query().Get("after"),
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	views := make([]auditEventView, 0, len(list.Events))
	for _, event := range list.Events {
		views = append(views, auditEventViewOf(event))
	}
	writeJSON(writer, http.StatusOK, auditListView{Events: views, Next: list.Next})
}

// identifierIn reads a UUID out of a request body rather than a path, naming the field in the
// refusal so an operator knows which one they got wrong.
func identifierIn(
	writer http.ResponseWriter, value, field string,
) (uuid.UUID, bool) {
	id, err := uuid.Parse(value)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: field + " is not an identity"})
		return uuid.Nil, false
	}
	return id, true
}
