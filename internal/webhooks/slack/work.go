package slack

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type JobStore interface {
	ApplySlackWebhookJob(context.Context, tenancy.Organization, storage.WebhookJob, time.Duration, int) error
}

type ReferenceResolver interface {
	Resolve(context.Context, storage.WebhookJob) error
}

type JobHandler struct {
	Jobs            JobStore
	References      ReferenceResolver
	WindowLead      time.Duration
	MaxWaitingTurns int
	Logger          *slog.Logger
}

func (h JobHandler) Handle(ctx context.Context, job storage.WebhookJob) error {
	if h.References != nil {
		lookup, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := h.References.Resolve(lookup, job)
		cancel()
		if err != nil {
			logger := h.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.WarnContext(ctx, "slack message provenance lookup failed",
				slog.String("delivery_id", job.DeliveryID.String()))
		}
	}
	if h.Jobs == nil {
		return fmt.Errorf("slack webhook delivery: no store")
	}
	return h.Jobs.ApplySlackWebhookJob(ctx, job.Organization, job, h.WindowLead, h.MaxWaitingTurns)
}
