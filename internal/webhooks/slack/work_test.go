package slack

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type jobStoreStub struct{ applied bool }

func (s *jobStoreStub) ApplySlackWebhookJob(
	context.Context, tenancy.Organization, storage.WebhookJob, time.Duration, int,
) error {
	s.applied = true
	return nil
}

type referenceStub struct{ err error }

func (r referenceStub) Resolve(context.Context, storage.WebhookJob) error { return r.err }

func TestPermalinkFailureCannotBlockAcceptedSlackJob(t *testing.T) {
	t.Parallel()

	store := &jobStoreStub{}
	handler := JobHandler{
		Jobs: store, References: referenceStub{err: errors.New("slack unavailable")},
	}
	if err := handler.Handle(context.Background(), storage.WebhookJob{}); err != nil {
		t.Fatalf("handling accepted job: %v", err)
	}
	if !store.applied {
		t.Fatal("accepted Slack job was lost when optional provenance lookup failed")
	}
}
