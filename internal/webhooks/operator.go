package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	table "github.com/open-cluster/oc-control-plane/internal/api/pagination"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type DeliveryHandlers struct {
	Database *storage.Database
	Logger   *slog.Logger
	Counters WorkInstruments
}

var deliverySpec = table.Spec{
	Sortable:    []string{"receivedAt"},
	DefaultSort: table.Sort{Field: "receivedAt", Descending: true},
	Filters:     []string{"status"},
}

func (h DeliveryHandlers) Routes() authz.Table {
	const base = "/api/v1/webhook-deliveries"
	return authz.Table{
		authz.Privileged(http.MethodGet, base, authz.InvestigationRead,
			http.HandlerFunc(h.list)),
		authz.Privileged(http.MethodGet, base+"/{delivery}", authz.InvestigationRead,
			http.HandlerFunc(h.get)),
		authz.Privileged(http.MethodPost, base+"/{delivery}/replay", authz.WebhookDeliveryReplay,
			http.HandlerFunc(h.replay)),
	}
}

type deliveryView struct {
	ID               string  `json:"id"`
	IntegrationID    string  `json:"integrationId"`
	ProviderIdentity string  `json:"providerIdentity"`
	LifecyclePhase   string  `json:"lifecyclePhase,omitempty"`
	RequestID        string  `json:"requestId,omitempty"`
	Status           string  `json:"status"`
	Attempts         int     `json:"attempts"`
	FailureCategory  string  `json:"failureCategory,omitempty"`
	ReceivedAt       string  `json:"receivedAt"`
	LastAttemptAt    *string `json:"lastAttemptAt"`
	NextEligibleAt   *string `json:"nextEligibleAt"`
}

func viewOfDelivery(delivery storage.WebhookDelivery) deliveryView {
	view := deliveryView{
		ID: delivery.ID.String(), IntegrationID: delivery.IntegrationID.String(),
		ProviderIdentity: delivery.ProviderIdentity, LifecyclePhase: delivery.LifecyclePhase,
		RequestID: delivery.RequestID, Status: string(delivery.State), Attempts: delivery.Attempts,
		FailureCategory: delivery.FailureClass,
		ReceivedAt:      delivery.ReceivedAt.UTC().Format(time.RFC3339),
	}
	if delivery.LastAttemptAt != nil {
		formatted := delivery.LastAttemptAt.UTC().Format(time.RFC3339)
		view.LastAttemptAt = &formatted
	}
	if delivery.NextEligibleAt != nil {
		formatted := delivery.NextEligibleAt.UTC().Format(time.RFC3339)
		view.NextEligibleAt = &formatted
	}
	return view
}

func (h DeliveryHandlers) list(writer http.ResponseWriter, request *http.Request) {
	organization, ok := deliveryOrganization(writer, request)
	if !ok {
		return
	}
	query, err := table.Parse(request.URL.Query(), deliverySpec)
	if err != nil {
		writeDeliveryJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !query.Sort.Descending {
		writeDeliveryJSON(writer, http.StatusBadRequest,
			map[string]string{"error": "webhook deliveries support descending receivedAt order only"})
		return
	}
	state := storage.WebhookDeliveryState(query.Filter("status"))
	if !validDeliveryState(state) {
		writeDeliveryJSON(writer, http.StatusBadRequest,
			map[string]string{"error": "status must be accepted, processing, succeeded, or failed"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	page, err := h.Database.WebhookDeliveries(ctx, organization, state,
		storage.Page{Limit: query.Limit, After: query.Cursor})
	if err != nil {
		if errors.Is(err, storage.ErrBadCursor) {
			writeDeliveryJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		h.fail(writer, err)
		return
	}
	views := make([]deliveryView, 0, len(page.Deliveries))
	for _, delivery := range page.Deliveries {
		views = append(views, viewOfDelivery(delivery))
	}
	writeDeliveryJSON(writer, http.StatusOK, table.Answer(views, page.Next, nil))
}

func validDeliveryState(state storage.WebhookDeliveryState) bool {
	return state == "" || state == storage.WebhookDeliveryAccepted ||
		state == storage.WebhookDeliveryProcessing || state == storage.WebhookDeliverySucceeded ||
		state == storage.WebhookDeliveryFailed
}

func (h DeliveryHandlers) get(writer http.ResponseWriter, request *http.Request) {
	organization, deliveryID, ok := addressedDelivery(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	delivery, err := h.Database.WebhookDeliveryByID(ctx, organization, deliveryID)
	if errors.Is(err, storage.ErrWebhookDeliveryUnknown) {
		writeDeliveryJSON(writer, http.StatusNotFound, map[string]string{"error": "delivery not found"})
		return
	}
	if err != nil {
		h.fail(writer, err)
		return
	}
	writeDeliveryJSON(writer, http.StatusOK, viewOfDelivery(delivery))
}

func (h DeliveryHandlers) replay(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authz.Of(request)
	if !ok {
		writeDeliveryJSON(writer, http.StatusInternalServerError,
			map[string]string{"error": "request failed"})
		return
	}
	organization, deliveryID, ok := addressedDelivery(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	if err := h.Database.ReplayWebhookDelivery(ctx, principal, organization, deliveryID); err != nil {
		if errors.Is(err, storage.ErrWebhookDeliveryUnknown) {
			writeDeliveryJSON(writer, http.StatusConflict,
				map[string]string{"error": "only failed deliveries can be replayed"})
			return
		}
		h.fail(writer, err)
		return
	}
	h.Counters.Count(ctx, "replayed")
	writer.WriteHeader(http.StatusNoContent)
}

func addressedDelivery(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, uuid.UUID, bool) {
	organization, ok := deliveryOrganization(writer, request)
	if !ok {
		return tenancy.Organization{}, uuid.Nil, false
	}
	id, err := uuid.Parse(request.PathValue("delivery"))
	if err != nil {
		writeDeliveryJSON(writer, http.StatusBadRequest,
			map[string]string{"error": "delivery is not an identity"})
		return tenancy.Organization{}, uuid.Nil, false
	}
	return organization, id, true
}

func deliveryOrganization(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, bool) {
	organization, ok := authz.ActiveOrganizationFrom(request.Context())
	if !ok {
		writeDeliveryJSON(writer, http.StatusInternalServerError,
			map[string]string{"error": "request failed"})
		return tenancy.Organization{}, false
	}
	return organization, true
}

func (h DeliveryHandlers) fail(writer http.ResponseWriter, err error) {
	if h.Logger != nil {
		h.Logger.Error("webhook delivery operator request failed", slog.String("error", err.Error()))
	}
	writeDeliveryJSON(writer, http.StatusInternalServerError, map[string]string{"error": "request failed"})
}

func writeDeliveryJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
