package conversation

import (
	"context"
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
)

func (h Handlers) turns(writer http.ResponseWriter, request *http.Request) {
	_, organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	query, err := listing.Parse(request.URL.Query(), listing.Spec{})
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	page, err := h.Store.ConversationTurns(ctx, organization, id, query.Limit, query.Cursor)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	view := turnPageView{Turns: make([]turnView, 0, len(page.Turns)), Next: listing.Continuation(page.Next)}
	for _, turn := range page.Turns {
		view.Turns = append(view.Turns, turnViewOf(turn))
	}
	writeJSON(writer, http.StatusOK, view)
}

type turnPageView struct {
	Turns []turnView `json:"turns"`
	Next  *string    `json:"next"`
}
