package slack

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	providerslack "github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type ReferenceStore interface {
	SlackMessageProviderReference(context.Context, storage.WebhookJob) (string, string, string, error)
	Integration(context.Context, storage.WebhookJob) (integrations.Integration, error)
	SetSlackMessageSourceReference(context.Context, storage.WebhookJob, string) error
	RecordCredentialUnseal(context.Context, storage.WebhookJob, string) error
}

// ReferenceDatabase narrows storage to the safe, Organization-scoped references needed by
// the post-acknowledgement provider lookup.
type ReferenceDatabase struct{ Database *storage.Database }

func (d ReferenceDatabase) SlackMessageProviderReference(
	ctx context.Context, work storage.WebhookJob,
) (string, string, string, error) {
	return d.Database.SlackMessageProviderReference(
		ctx, work.Organization, work.ConversationID, work.MessageSequence)
}

func (d ReferenceDatabase) Integration(
	ctx context.Context, work storage.WebhookJob,
) (integrations.Integration, error) {
	return d.Database.Integration(ctx, work.Organization, work.IntegrationID)
}

func (d ReferenceDatabase) SetSlackMessageSourceReference(
	ctx context.Context, work storage.WebhookJob, reference string,
) error {
	return d.Database.SetSlackMessageSourceReference(
		ctx, work.Organization, work.ConversationID, work.MessageSequence, reference, work)
}

func (d ReferenceDatabase) RecordCredentialUnseal(
	ctx context.Context, work storage.WebhookJob, purpose string,
) error {
	return d.Database.RecordCredentialUnseal(ctx, work.Organization, work.IntegrationID, purpose)
}

type SlackReferenceResolver struct {
	Store  ReferenceStore
	Client *providerslack.Client
	Sealer seal.Sealer
}

func (r SlackReferenceResolver) Resolve(ctx context.Context, work storage.WebhookJob) error {
	channel, message, existing, err := r.Store.SlackMessageProviderReference(ctx, work)
	if err != nil || existing != "" || channel == "" || message == "" {
		return err
	}
	integration, err := r.Store.Integration(ctx, work)
	if err != nil {
		return err
	}
	if err = r.Store.RecordCredentialUnseal(ctx, work, "slack message source reference"); err != nil {
		return errors.New("slack message provenance credential use could not be audited")
	}
	credential, err := r.Sealer.Open(integration.CredentialSealed,
		integrations.CredentialBinding(integration.ID))
	if err != nil {
		return errors.New("slack message provenance credential could not be opened")
	}
	workspace := r.Client.WorkspaceURL(ctx, credential)
	if workspace == "" {
		return fmt.Errorf("slack message provenance workspace lookup failed")
	}
	return r.Store.SetSlackMessageSourceReference(ctx, work,
		providerslack.Permalink(workspace, channel, message))
}
