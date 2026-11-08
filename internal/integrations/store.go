package integrations

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

type Store interface {
	CreateIntegration(ctx context.Context, who authz.Principal, org uuid.UUID,
		wanted NewIntegration) (Integration, error)
	IntegrationByID(ctx context.Context, id uuid.UUID) (Integration, error)
	Integration(ctx context.Context, org uuid.UUID, id uuid.UUID) (Integration, error)
	IntegrationByInstallation(ctx context.Context, provider Provider,
		key InstallationKey) (Integration, Installation, error)
	QueryIntegrations(ctx context.Context, who authz.Principal, org uuid.UUID,
		query Query) (List, error)
	CountIntegrationsByProvider(ctx context.Context, who authz.Principal,
		org uuid.UUID) ([]ProviderCount, error)
	UpdateIntegration(ctx context.Context, who authz.Principal, org uuid.UUID,
		id uuid.UUID, update IntegrationUpdate) (Integration, error)
	DisconnectIntegration(ctx context.Context, who authz.Principal, org uuid.UUID,
		id uuid.UUID) error
	RotateIntegrationWebhookSecret(ctx context.Context, who authz.Principal,
		org uuid.UUID, id uuid.UUID, digest []byte) error
	RecordCredentialUnseal(ctx context.Context, org uuid.UUID, id uuid.UUID,
		purpose string) error
	ReplaceIntegrationCredential(ctx context.Context, who authz.Principal,
		org uuid.UUID, expectedIntegration Integration, update IntegrationUpdate, sealed []byte,
		verification Verification, installed *Installation) (Integration, error)
	RecordIntegrationVerification(ctx context.Context, who authz.Principal,
		org uuid.UUID, expectedIntegration Integration, verification Verification) (Integration, error)
	IntegrationRelayStatus(ctx context.Context, org uuid.UUID,
		relayID uuid.UUID) (RelayStatus, error)
	LastAcceptedDelivery(ctx context.Context, org uuid.UUID,
		id uuid.UUID) (time.Time, error)

	StartConnectFlow(ctx context.Context, org uuid.UUID, flow ConnectFlow,
		state string) error
	RedeemConnectFlow(ctx context.Context, state string) (ConnectFlow, error)
}
