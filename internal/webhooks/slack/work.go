package slack

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type WorkStore interface {
	ApplySlackWebhookWork(context.Context, tenancy.Organization, storage.WebhookWork, time.Duration, int) error
}

type ReferenceResolver interface {
	Resolve(context.Context, storage.WebhookWork) error
}

type WorkHandler struct {
	Work            WorkStore
	References      ReferenceResolver
	WindowLead      time.Duration
	MaxWaitingTurns int
	Logger          *slog.Logger
}

func (h WorkHandler) Handle(ctx context.Context, work storage.WebhookWork) error {
	if h.References != nil {
		lookup, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := h.References.Resolve(lookup, work)
		cancel()
		if err != nil {
			logger := h.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.WarnContext(ctx, "slack message provenance lookup failed",
				slog.String("delivery_id", work.DeliveryID.String()))
		}
	}
	if h.Work == nil {
		return fmt.Errorf("slack webhook delivery: no store")
	}
	return h.Work.ApplySlackWebhookWork(ctx, work.Organization, work, h.WindowLead, h.MaxWaitingTurns)
}
