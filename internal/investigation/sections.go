package investigation

import (
	"context"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The paged reads of one case, each stamped with the case version it represents.
//
// They are their own file because they are one shape repeated: take the case, read a section,
// render it, stamp it. Keeping them together makes the repetition visible — which is the point,
// because a section that did something different would then stand out — and keeps the file that
// owns the case's LIFECYCLE about the lifecycle.

func (h Handlers) timeline(writer http.ResponseWriter, request *http.Request) {
	h.section(writer, request, func(
		ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	) (any, int64, error) {
		section, err := h.Reader.InvestigationTimeline(ctx, organization, id, pageOf(request))
		if err != nil {
			return nil, 0, err
		}
		return renderSection(section, func(item Item) evidenceView {
			return evidenceViewOf(item, false)
		}), section.CaseVersion, nil
	})
}

func (h Handlers) evidence(writer http.ResponseWriter, request *http.Request) {
	filter := EvidenceFilter{CapabilityID: request.URL.Query().Get("capability")}
	if named := request.URL.Query().Get("source"); named != "" {
		source, err := uuid.Parse(named)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: "source is not an identity"})
			return
		}
		filter.Source = source
	}
	if named := request.URL.Query().Get("stance"); named != "" {
		stance, known := ParseStance(named)
		if !known {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: `stance must be "supports", "contradicts" or "neutral"`})
			return
		}
		filter.Stance = stance
	}

	h.section(writer, request, func(
		ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	) (any, int64, error) {
		section, err := h.Reader.InvestigationEvidence(
			ctx, organization, id, filter, pageOf(request))
		if err != nil {
			return nil, 0, err
		}
		return renderSection(section, func(item Item) evidenceView {
			return evidenceViewOf(item, false)
		}), section.CaseVersion, nil
	})
}

// evidenceItem reads one item with its content. It is a separate route from the listing because the
// content is bounded but large, and a listing that carried it would be the size of its contents.
func (h Handlers) evidenceItem(writer http.ResponseWriter, request *http.Request) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	evidenceID, err := uuid.Parse(request.PathValue("evidence"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "evidence is not an identity"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	item, version, err := h.Reader.EvidenceItem(ctx, organization, id, evidenceID)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.stamp(writer, version)
	writeJSON(writer, http.StatusOK, sectionView[evidenceView]{
		Items:       []evidenceView{evidenceViewOf(item, true)},
		CaseVersion: version,
	})
}

func (h Handlers) hypotheses(writer http.ResponseWriter, request *http.Request) {
	h.section(writer, request, func(
		ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	) (any, int64, error) {
		section, err := h.Reader.InvestigationHypotheses(ctx, organization, id, pageOf(request))
		if err != nil {
			return nil, 0, err
		}
		return renderSection(section, hypothesisViewOf), section.CaseVersion, nil
	})
}

func (h Handlers) gaps(writer http.ResponseWriter, request *http.Request) {
	h.section(writer, request, func(
		ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	) (any, int64, error) {
		section, err := h.Reader.InvestigationGaps(ctx, organization, id, pageOf(request))
		if err != nil {
			return nil, 0, err
		}
		return renderSection(section, gapViewOf), section.CaseVersion, nil
	})
}

func (h Handlers) coverage(writer http.ResponseWriter, request *http.Request) {
	h.section(writer, request, func(
		ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	) (any, int64, error) {
		section, err := h.Reader.InvestigationCoverage(ctx, organization, id)
		if err != nil {
			return nil, 0, err
		}
		return renderSection(section, coverageViewOf), section.CaseVersion, nil
	})
}

func (h Handlers) activity(writer http.ResponseWriter, request *http.Request) {
	h.section(writer, request, func(
		ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	) (any, int64, error) {
		section, err := h.Reader.InvestigationActivity(ctx, organization, id, pageOf(request))
		if err != nil {
			return nil, 0, err
		}
		return renderSection(section, activityViewOf), section.CaseVersion, nil
	})
}

// caseFile answers the assembled case at a pinned version. One code path serves the shared route,
// both export formats and the harness artifact, so a share, an export and a scored artifact cannot
// diverge.
func (h Handlers) caseFile(writer http.ResponseWriter, request *http.Request) {
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), openTimeout)
	defer cancel()

	var pinned int64
	if named := request.URL.Query().Get("version"); named != "" {
		version, err := strconv.ParseInt(named, 10, 64)
		if err != nil || version < 1 {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "version is not a case version"})
			return
		}
		pinned = version
	}

	file, err := h.Reader.AssembleCaseFile(ctx, organization, id, pinned)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.stamp(writer, file.CaseVersion)
	writeJSON(writer, http.StatusOK, caseFileViewOf(file))
}
