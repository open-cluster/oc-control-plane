package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const (
	metadataOrganization   = "opencluster-org-id"
	metadataBootstrapToken = "opencluster-bootstrap-token"
)

const protocolVersion = 1

func supportsProtocol(version uint32) bool { return version >= protocolVersion }

const credentialBytes = 32

const refusalMessage = "registration refused"

type RegistrationService struct {
	relayv1.UnimplementedRelayRegistrationServiceServer

	database *storage.Database
	spkiPins []string
	logger   *slog.Logger
	flood    *floodLimiter
}

var DefaultFloodLimits = FloodLimits{
	Window:                  time.Minute,
	PerOrganization:         20,
	Global:                  200,
	MaxTrackedOrganizations: 10_000,
}

func NewRegistrationService(
	database *storage.Database,
	spkiPins []string,
	logger *slog.Logger,
) *RegistrationService {
	return &RegistrationService{
		database: database,
		spkiPins: spkiPins,
		logger:   logger,
		flood:    newFloodLimiter(DefaultFloodLimits, time.Now),
	}
}

func (s *RegistrationService) Register(
	ctx context.Context,
	request *relayv1.RegisterRequest,
) (*relayv1.RegisterResponse, error) {
	organization, token, err := callerIdentity(ctx)
	if err != nil {
		s.audit(ctx, "", "malformed registration metadata")
		return nil, status.Error(codes.FailedPrecondition, refusalMessage)
	}

	if !s.flood.allow(organization.String()) {
		s.audit(ctx, organization.String(), "shed by flood control")
		return nil, status.Error(codes.ResourceExhausted, "registration temporarily unavailable")
	}
	if !supportsProtocol(request.GetProtocolVersion()) {
		s.audit(ctx, organization.String(), "unsupported protocol version")
		return nil, status.Error(codes.FailedPrecondition, refusalMessage)
	}

	credential, digest, err := mintCredential()
	if err != nil {
		return nil, status.Error(codes.Internal, "registration unavailable")
	}
	capabilities, err := encodeCapabilities(request.GetCapabilities())
	if err != nil {
		s.audit(ctx, organization.String(), "unencodable capability roster")
		return nil, status.Error(codes.FailedPrecondition, refusalMessage)
	}

	registrationID, refusal, err := s.database.EnrolRelay(ctx, organization, storage.RelayEnrolment{
		TokenDigest:        digestOf(token),
		CredentialDigest:   digest,
		ClusterFingerprint: request.GetClusterFingerprint(),
		RelayVersion:       request.GetRelayVersion(),
		ProtocolVersion:    request.GetProtocolVersion(),
		Capabilities:       capabilities,
	})
	switch {
	case errors.Is(err, storage.ErrEnrolmentRefused):
		s.audit(ctx, organization.String(), refusal.String())
		return nil, status.Error(codes.FailedPrecondition, refusalMessage)
	case errors.Is(err, storage.ErrUnknownOrganization):
		s.audit(ctx, organization.String(), "organization has no database")
		return nil, status.Error(codes.FailedPrecondition, refusalMessage)
	case err != nil:
		s.logger.ErrorContext(ctx, "relay enrolment failed",
			slog.String("organization", organization.String()),
			slog.String("error", err.Error()))
		return nil, status.Error(codes.Unavailable, "registration unavailable")
	}

	s.logger.InfoContext(ctx, "relay registered",
		slog.String("organization", organization.String()),
		slog.String("registration_id", registrationID.String()))

	return &relayv1.RegisterResponse{
		OrgId:           organization.String(),
		RegistrationId:  registrationID.String(),
		Credential:      credential,
		SpkiPins:        s.spkiPins,
		ProtocolVersion: protocolVersion,
	}, nil
}

func (s *RegistrationService) audit(ctx context.Context, organization, reason string) {
	s.logger.WarnContext(ctx, "relay enrolment refused",
		slog.String("organization", organization),
		slog.String("reason", reason))
}

func callerIdentity(ctx context.Context) (uuid.UUID, string, error) {
	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return uuid.UUID{}, "", errors.New("no call metadata")
	}
	token := firstValue(incoming, metadataBootstrapToken)
	if token == "" {
		return uuid.UUID{}, "", errors.New("no bootstrap token")
	}
	organization, err := uuid.Parse(strings.TrimSpace(firstValue(incoming, metadataOrganization)))
	if err != nil || organization == uuid.Nil {
		return uuid.UUID{}, "", errors.New("invalid organization identifier")
	}
	return organization, token, nil
}

func firstValue(incoming metadata.MD, key string) string {
	values := incoming.Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func mintCredential() (string, []byte, error) {
	raw := make([]byte, credentialBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	credential := base64.StdEncoding.EncodeToString(raw)
	return credential, digestOf(credential), nil
}

func digestOf(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

func encodeCapabilities(descriptors []*relayv1.CapabilityDescriptor) ([]byte, error) {
	type capability struct {
		ID      string `json:"id"`
		Version uint32 `json:"version"`
	}
	roster := make([]capability, 0, len(descriptors))
	for _, descriptor := range descriptors {
		roster = append(roster, capability{
			ID:      descriptor.GetCapabilityId(),
			Version: descriptor.GetCapabilityVersion(),
		})
	}
	return json.Marshal(roster)
}
