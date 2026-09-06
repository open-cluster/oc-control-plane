package storage

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func evidenceMissing(ctx context.Context, pool querier, org tenancy.Organization, refs []investigation.EvidenceRef) (bool, error) {
	if len(refs) == 0 {
		return false, nil
	}
	ids := make([]uuid.UUID, 0, len(refs))
	ordinals := make([]int64, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.InvestigationID)
		ordinals = append(ordinals, int64(ref.ToolRunOrdinal))
	}
	var missing bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM unnest($2::uuid[], $3::bigint[]) AS reference(investigation_id, ordinal)
		WHERE NOT EXISTS (SELECT 1 FROM investigation_tool_run run
			WHERE run.org_id = $1 AND run.investigation_id = reference.investigation_id
			AND run.ordinal = reference.ordinal))`, org.String(), ids, ordinals).Scan(&missing)
	return missing, err
}

func resultEvidence(found investigation.Investigation) []investigation.EvidenceRef {
	var refs []investigation.EvidenceRef
	for _, finding := range found.Conclusion.Findings {
		refs = append(refs, finding.EvidenceRefs...)
		for _, ordinal := range finding.Sources {
			refs = append(refs, investigation.EvidenceRef{InvestigationID: found.ID, ToolRunOrdinal: ordinal})
		}
	}
	return refs
}
