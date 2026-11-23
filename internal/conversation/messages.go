package conversation

import (
	"context"
	"net/http"
	"net/url"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
)

func (h Handlers) messages(writer http.ResponseWriter, request *http.Request) {
	_, org, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "query parameters are malformed"})
		return
	}
	query, err := listing.Parse(values, listing.Spec{})
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	page, err := h.Store.ConversationMessages(ctx, org, id, query.Limit, query.Cursor)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	items := make([]messageView, 0, len(page.Messages))
	for _, message := range page.Messages {
		items = append(items, messageViewOf(message))
	}
	writeJSON(writer, http.StatusOK, listing.NewPage(items, page.Next, nil))
}
