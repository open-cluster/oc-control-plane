package postmortem

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

type serviceStore struct {
	input      GenerationInput
	contextErr error
	current    Postmortem
	created    Postmortem
	replaced   Postmortem
}

func (s *serviceStore) GenerationInput(context.Context, tenancy.Organization,
	uuid.UUID) (GenerationInput, error) {
	return s.input, s.contextErr
}
func (s *serviceStore) Postmortem(context.Context, tenancy.Organization,
	uuid.UUID) (Postmortem, error) {
	if s.current.IncidentID == uuid.Nil {
		return Postmortem{}, ErrUnknown
	}
	return s.current, nil
}
func (s *serviceStore) CreateDraft(_ context.Context, _ authz.Principal,
	_ tenancy.Organization, draft Postmortem) (Postmortem, error) {
	s.created = draft
	return draft, nil
}
func (s *serviceStore) ReplaceDraft(_ context.Context, _ authz.Principal,
	_ tenancy.Organization, draft Postmortem) (Postmortem, error) {
	s.replaced = draft
	return draft, nil
}
func (s *serviceStore) Correct(context.Context, authz.Principal, tenancy.Organization,
	uuid.UUID, Corrections) (Postmortem, error) {
	return Postmortem{}, nil
}
func (s *serviceStore) Review(context.Context, authz.Principal, tenancy.Organization,
	uuid.UUID) (Postmortem, error) {
	return Postmortem{}, nil
}

func TestServiceGeneratesOnlyForResolvedIncidentsAndRevisionsRegeneration(t *testing.T) {
	t.Parallel()

	organization, _ := tenancy.NewOrganization("org-test")
	incidentID := uuid.New()
	store := &serviceStore{contextErr: ErrNotEligible}
	service := Service{Store: store}
	if _, err := service.Generate(context.Background(), authz.Principal{}, organization,
		incidentID, HumanInput{}); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("unresolved generation error = %v", err)
	}
	if store.created.IncidentID != uuid.Nil {
		t.Fatal("an unresolved incident created a postmortem")
	}

	store.contextErr = nil
	store.input = GenerationInput{IncidentID: incidentID, Title: "Checkout unavailable"}
	created, err := service.Generate(context.Background(), authz.Principal{}, organization,
		incidentID, HumanInput{})
	if err != nil || created.Revision != 1 || store.created.IncidentID != incidentID {
		t.Fatalf("created=%+v stored=%+v err=%v", created, store.created, err)
	}

	store.current = created
	replaced, err := service.Regenerate(context.Background(), authz.Principal{},
		organization, incidentID, HumanInput{Resolution: "Rolled back the deployment."})
	if err != nil || replaced.Revision != 2 || replaced.Status != StatusDraft ||
		store.replaced.Resolution != "Rolled back the deployment." {
		t.Fatalf("replaced=%+v stored=%+v err=%v", replaced, store.replaced, err)
	}
}
