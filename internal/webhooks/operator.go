package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/api/pagination"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type OperatorHandlers struct {
	Database *storage.Database
	Logger   *slog.Logger
	Counters WorkInstruments
}

var terminalWorkSpec = table.Spec{
	Sortable:    []string{"updatedAt"},
	DefaultSort: table.Sort{Field: "updatedAt", Descending: true},
}

func (h OperatorHandlers) Routes() authz.Table {
	const base = "/api/v1/organizations/{organization}/webhook-work/terminal"
	return authz.Table{
		authz.Privileged(http.MethodGet, base, authz.InvestigationRead,
			http.HandlerFunc(h.listTerminal)),
		authz.Privileged(http.MethodGet, base+"/{work}", authz.InvestigationRead,
			http.HandlerFunc(h.getTerminal)),
		authz.Privileged(http.MethodPost, base+"/{work}/replay", authz.WebhookWorkReplay,
			http.HandlerFunc(h.replay)),
	}
}

type workView struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Status          string `json:"status"`
	DeliveryID      string `json:"deliveryId"`
	IntegrationID   string `json:"integrationId"`
	IncidentID      string `json:"incidentId,omitempty"`
	ConversationID  string `json:"conversationId,omitempty"`
	MessageSequence int64  `json:"messageSequence,omitempty"`
	Attempts        int    `json:"attempts"`
	FailureClass    string `json:"failureClass"`
	FailureMessage  string `json:"failureMessage"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
}

func viewOfWork(work storage.WebhookWork) workView {
	view := workView{ID: work.ID.String(), Kind: work.Kind.String(),
		Status: work.Status.String(), DeliveryID: work.DeliveryID.String(),
		IntegrationID: work.IntegrationID.String(), Attempts: work.Attempts,
		MessageSequence: work.MessageSequence,
		FailureClass:    work.FailureClass, FailureMessage: work.FailureMessage,
		CreatedAt: work.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: work.UpdatedAt.UTC().Format(time.RFC3339)}
	if work.IncidentID != uuid.Nil {
		view.IncidentID = work.IncidentID.String()
	}
	if work.ConversationID != uuid.Nil {
		view.ConversationID = work.ConversationID.String()
	}
	return view
}

func (h OperatorHandlers) listTerminal(writer http.ResponseWriter, request *http.Request) {
	organization, ok := workOrganization(writer, request)
	if !ok {
		return
	}
	query, err := table.Parse(request.URL.Query(), terminalWorkSpec)
	if err != nil {
		writeWorkJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !query.Sort.Descending {
		writeWorkJSON(writer, http.StatusBadRequest,
			map[string]string{"error": "terminal webhook work supports descending updatedAt order only"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	page, err := h.Database.TerminalWebhookWork(ctx, organization,
		storage.Page{Limit: query.Limit, After: query.Cursor})
	if err != nil {
		if errors.Is(err, storage.ErrBadCursor) {
			writeWorkJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		h.fail(writer, err)
		return
	}
	views := make([]workView, 0, len(page.Work))
	for _, row := range page.Work {
		views = append(views, viewOfWork(row))
	}
	writeWorkJSON(writer, http.StatusOK, map[string]any{"work": views, "next": page.Next})
}

func (h OperatorHandlers) getTerminal(writer http.ResponseWriter, request *http.Request) {
	organization, workID, ok := addressedWork(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	work, err := h.Database.TerminalWebhookWorkByID(ctx, organization, workID)
	if errors.Is(err, storage.ErrWebhookWorkUnknown) {
		writeWorkJSON(writer, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}
	if err != nil {
		h.fail(writer, err)
		return
	}
	writeWorkJSON(writer, http.StatusOK, viewOfWork(work))
}

func (h OperatorHandlers) replay(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authz.Of(request)
	if !ok {
		writeWorkJSON(writer, http.StatusInternalServerError, map[string]string{"error": "request failed"})
		return
	}
	organization, workID, ok := addressedWork(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	if err := h.Database.ReplayWebhookWork(ctx, principal, organization, workID); err != nil {
		if errors.Is(err, storage.ErrWebhookWorkUnknown) {
			writeWorkJSON(writer, http.StatusConflict, map[string]string{"error": "only terminal work can be replayed"})
			return
		}
		h.fail(writer, err)
		return
	}
	h.Counters.Count(ctx, "replayed")
	writer.WriteHeader(http.StatusNoContent)
}

func addressedWork(writer http.ResponseWriter, request *http.Request) (tenancy.Organization, uuid.UUID, bool) {
	organization, ok := workOrganization(writer, request)
	if !ok {
		return tenancy.Organization{}, uuid.Nil, false
	}
	id, err := uuid.Parse(request.PathValue("work"))
	if err != nil {
		writeWorkJSON(writer, http.StatusBadRequest, map[string]string{"error": "work is not an identity"})
		return tenancy.Organization{}, uuid.Nil, false
	}
	return organization, id, true
}

func workOrganization(writer http.ResponseWriter, request *http.Request) (tenancy.Organization, bool) {
	organization, ok := authz.ActiveOrganizationFrom(request.Context())
	if !ok {
		writeWorkJSON(writer, http.StatusInternalServerError, map[string]string{"error": "request failed"})
		return tenancy.Organization{}, false
	}
	return organization, true
}

func (h OperatorHandlers) fail(writer http.ResponseWriter, err error) {
	if h.Logger != nil {
		h.Logger.Error("webhook work operator request failed", slog.String("error", err.Error()))
	}
	writeWorkJSON(writer, http.StatusInternalServerError, map[string]string{"error": "request failed"})
}

func writeWorkJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
