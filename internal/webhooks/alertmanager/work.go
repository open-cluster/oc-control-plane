package alertmanager

import (
	"context"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/storage"
)

type WorkHandler struct {
	Database        *storage.Database
	WindowLead      time.Duration
	MaxWaitingTurns int
}

func (h WorkHandler) Handle(ctx context.Context, work storage.WebhookWork) error {
	_, err := h.Database.ApplyAlertWebhookWork(ctx, work.Organization, work,
		h.WindowLead, h.MaxWaitingTurns)
	return err
}
