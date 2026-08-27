package slack

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type workStoreStub struct{ applied bool }

func (s *workStoreStub) ApplySlackWebhookWork(
	context.Context, tenancy.Organization, storage.WebhookWork, time.Duration, int,
) error {
	s.applied = true
	return nil
}

type referenceStub struct{ err error }

func (r referenceStub) Resolve(context.Context, storage.WebhookWork) error { return r.err }

func TestPermalinkFailureCannotBlockAcceptedSlackWork(t *testing.T) {
	t.Parallel()

	store := &workStoreStub{}
	handler := WorkHandler{
		Work: store, References: referenceStub{err: errors.New("slack unavailable")},
	}
	if err := handler.Handle(context.Background(), storage.WebhookWork{}); err != nil {
		t.Fatalf("handling accepted work: %v", err)
	}
	if !store.applied {
		t.Fatal("accepted Slack work was lost when optional provenance lookup failed")
	}
}
