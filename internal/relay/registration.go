// Package relay serves the Relay's side of the protocol. The contract is Protobuf and lives
// in the Relay's repository; this package implements it and owns nothing about how a Relay
// executes work.
package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// Metadata keys the contract puts the caller's identity and secret in. They travel in call
// metadata rather than in message fields: fields carry attestations, metadata carries scope
// and secrets.
const (
	metadataOrganization   = "opencluster-org-id"
	metadataBootstrapToken = "opencluster-bootstrap-token"
)

// protocolVersion is the version this control plane speaks and negotiates back.
const protocolVersion = 1

func supportsProtocol(version uint32) bool { return version >= protocolVersion }

// credentialBytes is the size of the durable credential. It is server-generated and
// high-entropy, which is why its stored form is a plain digest rather than a slow key
// derivation: the cost of a derivation would fall on every session establishment, and the
// attack it defends against — offline guessing of a low-entropy human secret — does not
// apply here.
const credentialBytes = 32

// refusalMessage is what every rejected enrolment says, whatever the reason. Unknown,
// expired, already-consumed, revoked and organization-mismatched are indistinguishable from
// outside, because distinguishing them is what turns this endpoint into an oracle for
// probing token validity. The real reason goes to the audit log.
const refusalMessage = "registration refused"

// RegistrationService implements the Relay registration contract.
type RegistrationService struct {
	relayv1.UnimplementedRelayRegistrationServiceServer

	database *storage.Database
	spkiPins []string
	logger   *slog.Logger
	flood    *floodLimiter
}

// DefaultFloodLimits bound enrolment for a deployment that has not tuned them. Registration
// is a rare event per relay — once per installation — so the caps can be low enough to make
// probing impractical without affecting any legitimate operator.
var DefaultFloodLimits = FloodLimits{
	Window:                  time.Minute,
	PerOrganization:         20,
	Global:                  200,
	MaxTrackedOrganizations: 10_000,
}

// NewRegistrationService returns the service. The pins are the control plane's own public
// key digests, handed to a Relay at enrolment so every later connection is key-pinned rather
// than trusting a certificate authority.
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

// Register consumes a single-use bootstrap token and issues a durable identity. The
// credential it returns is the only time that value exists outside the Relay: it is stored
// here as a digest and there is no path that reads it back.
func (s *RegistrationService) Register(
	ctx context.Context,
	request *relayv1.RegisterRequest,
) (*relayv1.RegisterResponse, error) {
	organization, token, err := callerIdentity(ctx)
	if err != nil {
		s.audit(ctx, "", "malformed registration metadata")
		return nil, status.Error(codes.FailedPrecondition, refusalMessage)
	}

	// Shed before the token is examined. A shed must carry no information about whether the
	// token was valid, or the difference between this response and a refusal becomes an
	// oracle for probing. It is also a different, RETRYABLE status: a flood is transient,
	// while a refused token is terminal and the relay must stop rather than retry.
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
		// Indistinguishable from a bad token on purpose: an unassigned organization would
		// otherwise reveal which organizations exist.
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

// audit records why an enrolment was refused. The reason is deliberately absent from the
// response and present only here.
func (s *RegistrationService) audit(ctx context.Context, organization, reason string) {
	s.logger.WarnContext(ctx, "relay enrolment refused",
		slog.String("organization", organization),
		slog.String("reason", reason))
}

// callerIdentity reads the claimed organization and the bootstrap token from call metadata.
func callerIdentity(ctx context.Context) (tenancy.Organization, string, error) {
	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return tenancy.Organization{}, "", errors.New("no call metadata")
	}
	token := firstValue(incoming, metadataBootstrapToken)
	if token == "" {
		return tenancy.Organization{}, "", errors.New("no bootstrap token")
	}
	organization, err := tenancy.NewOrganization(firstValue(incoming, metadataOrganization))
	if err != nil {
		return tenancy.Organization{}, "", err
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

// mintCredential returns the credential to send once and the digest to keep.
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

// encodeCapabilities stores the advertised roster as it was received. It is an attestation
// rather than an authorization: what a Relay may be asked to run is decided here, not by
// what it claims to support.
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
