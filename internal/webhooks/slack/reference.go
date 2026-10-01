package slack

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	providerslack "github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type SlackReferenceResolver struct {
	Database *storage.Database
	Client   *providerslack.Client
	Sealer   seal.Sealer
}

func (r SlackReferenceResolver) Resolve(ctx context.Context, work storage.SlackMessageWork) error {
	channel, message, existing, err := r.Database.SlackMessageProviderReference(
		ctx, work.Organization, work.ConversationID, work.MessageSequence)
	if err != nil || existing != "" || channel == "" || message == "" {
		return err
	}
	integration, err := r.Database.Integration(ctx, work.Organization, work.IntegrationID)
	if err != nil {
		return err
	}
	if err = r.Database.RecordCredentialUnseal(ctx, work.Organization, work.IntegrationID,
		"slack message source reference"); err != nil {
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
	return r.Database.SetSlackMessageSourceReference(ctx, work.Organization,
		work.ConversationID, work.MessageSequence, providerslack.Permalink(workspace, channel, message), work)
}
