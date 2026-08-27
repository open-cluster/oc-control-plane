package postmortem

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

type Corrections struct {
	Title               *string           `json:"title,omitempty"`
	ExecutiveSummary    *string           `json:"executiveSummary,omitempty"`
	Impact              *string           `json:"impact,omitempty"`
	Detection           *string           `json:"detection,omitempty"`
	Timeline            *[]TimelineEntry  `json:"timeline,omitempty"`
	RootCauses          *[]CitedStatement `json:"rootCauses,omitempty"`
	ContributingFactors *[]CitedStatement `json:"contributingFactors,omitempty"`
	Resolution          *string           `json:"resolution,omitempty"`
	WhatWentWell        *[]string         `json:"whatWentWell,omitempty"`
	WhatWentWrong       *[]string         `json:"whatWentWrong,omitempty"`
	DetectionGaps       *[]string         `json:"detectionGaps,omitempty"`
	ActionItems         *[]ActionItem     `json:"actionItems,omitempty"`
	OpenQuestions       *[]string         `json:"openQuestions,omitempty"`
}

type Store interface {
	GenerationInput(ctx context.Context, org tenancy.Organization,
		incident uuid.UUID) (GenerationInput, error)
	Postmortem(ctx context.Context, org tenancy.Organization,
		incident uuid.UUID) (Postmortem, error)
	CreateDraft(ctx context.Context, who authz.Principal, org tenancy.Organization,
		draft Postmortem) (Postmortem, error)
	ReplaceDraft(ctx context.Context, who authz.Principal, org tenancy.Organization,
		draft Postmortem) (Postmortem, error)
	Correct(ctx context.Context, who authz.Principal, org tenancy.Organization,
		incident uuid.UUID, corrections Corrections) (Postmortem, error)
	Review(ctx context.Context, who authz.Principal, org tenancy.Organization,
		incident uuid.UUID) (Postmortem, error)
}

type Service struct{ Store Store }

func (s Service) Generate(
	ctx context.Context,
	who authz.Principal,
	organization tenancy.Organization,
	incident uuid.UUID,
	human HumanInput,
) (Postmortem, error) {
	input, err := s.Store.GenerationInput(ctx, organization, incident)
	if err != nil {
		return Postmortem{}, err
	}
	input.IncidentID = incident
	input.Human = human
	return s.Store.CreateDraft(ctx, who, organization, DraftFrom(input))
}

func (s Service) Regenerate(
	ctx context.Context,
	who authz.Principal,
	organization tenancy.Organization,
	incident uuid.UUID,
	human HumanInput,
) (Postmortem, error) {
	current, err := s.Store.Postmortem(ctx, organization, incident)
	if err != nil {
		return Postmortem{}, err
	}
	input, err := s.Store.GenerationInput(ctx, organization, incident)
	if err != nil {
		return Postmortem{}, err
	}
	input.IncidentID = incident
	input.Human = human
	draft := DraftFrom(input)
	draft.Revision = current.Revision + 1
	return s.Store.ReplaceDraft(ctx, who, organization, draft)
}

func (s Service) Correct(
	ctx context.Context, who authz.Principal, organization tenancy.Organization,
	incident uuid.UUID, corrections Corrections,
) (Postmortem, error) {
	return s.Store.Correct(ctx, who, organization, incident, corrections)
}

func (s Service) Review(
	ctx context.Context, who authz.Principal, organization tenancy.Organization,
	incident uuid.UUID,
) (Postmortem, error) {
	return s.Store.Review(ctx, who, organization, incident)
}
