package alertmanager

import (
	"context"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type JobHandler struct {
	Database        *storage.Database
	WindowLead      time.Duration
	MaxWaitingTurns int
}

func (h JobHandler) Handle(ctx context.Context, job storage.WebhookJob) error {
	_, err := h.Database.ApplyAlertWebhookJob(ctx, job.Organization, job,
		h.WindowLead, h.MaxWaitingTurns)
	return err
}
