package storage

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

const toolRunOwnerJoins = `
	LEFT JOIN agent_execution e ON e.org_id = r.org_id AND e.execution_id = r.execution_id
	LEFT JOIN investigation i ON i.org_id = r.org_id AND i.investigation_id = coalesce(r.investigation_id, e.investigation_id)
	LEFT JOIN conversation source ON source.org_id = r.org_id
		AND source.conversation_id = coalesce(e.conversation_id, i.conversation_id)`

const webToolRunSource = "(source.conversation_id IS NULL OR source.source = 'web')"

const privateInvestigationSources = `
	SELECT 1 FROM conversation private
	 WHERE private.org_id = i.org_id AND private.source = 'slack'
	   AND (private.conversation_id = i.conversation_id OR private.conversation_id IN (
	        SELECT referenced.conversation_id
	          FROM jsonb_path_query(i.conclusion, '$.findings[*].evidenceRefs[*].investigationId') ref(value)
	          JOIN investigation referenced ON referenced.org_id = i.org_id
	           AND referenced.investigation_id::text = (ref.value #>> '{}')))`

const webInvestigationSource = "NOT EXISTS (" + privateInvestigationSources + ")"

type evidenceScope struct {
	conversationID uuid.UUID
	executionID    uuid.UUID
	allowFailed    bool
}

func (p *Database) EvidenceMissing(ctx context.Context,
	organization, conversationID uuid.UUID,
	refs []investigation.EvidenceRef) (bool, error) {
	db, err := p.poolForOrganization(organization)
	if err != nil {
		return false, err
	}
	return evidenceMissing(ctx, db, organization, evidenceScope{conversationID: conversationID}, refs)
}

func evidenceMissing(ctx context.Context, pool querier, org uuid.UUID, scope evidenceScope, refs []investigation.EvidenceRef) (bool, error) {
	if len(refs) == 0 {
		return false, nil
	}
	runIDs := make([]*uuid.UUID, 0, len(refs))
	investigationIDs := make([]*uuid.UUID, 0, len(refs))
	ordinals := make([]int64, 0, len(refs))
	for _, ref := range refs {
		runIDs = append(runIDs, nullableUUID(ref.ToolRunID))
		investigationIDs = append(investigationIDs, nullableUUID(ref.InvestigationID))
		ordinals = append(ordinals, int64(ref.ToolRunOrdinal))
	}
	var missing bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM unnest($2::uuid[], $3::uuid[], $4::bigint[]) AS reference(tool_run_id, investigation_id, ordinal)
		WHERE NOT EXISTS (
			SELECT 1 FROM investigation_tool_run r `+toolRunOwnerJoins+`
			 WHERE r.org_id = $1 AND r.outcome IS NOT NULL AND (r.outcome = $5 OR $8)
			   AND (reference.tool_run_id IS NULL OR r.tool_run_id = reference.tool_run_id)
			   AND ((reference.tool_run_id IS NOT NULL AND reference.investigation_id IS NULL AND reference.ordinal = 0)
			        OR (i.investigation_id = reference.investigation_id AND r.ordinal = reference.ordinal))
			   AND ($7::uuid IS NULL OR r.execution_id = $7)
			   AND (r.execution_id = $7 OR (
			        (`+webToolRunSource+` OR source.conversation_id = $6)
			         AND NOT EXISTS (`+privateInvestigationSources+`
			             AND private.conversation_id IS DISTINCT FROM $6)))))`,
		org, runIDs, investigationIDs, ordinals, int16(investigation.RunSucceeded),
		nullableUUID(scope.conversationID), nullableUUID(scope.executionID), scope.allowFailed).Scan(&missing)
	return missing, err
}

func resultEvidence(id uuid.UUID, conclusion investigation.Conclusion) []investigation.EvidenceRef {
	var refs []investigation.EvidenceRef
	for _, finding := range conclusion.Findings {
		refs = append(refs, finding.EvidenceRefs...)
		for _, ordinal := range finding.RunRefs {
			refs = append(refs, investigation.EvidenceRef{InvestigationID: id, ToolRunOrdinal: ordinal})
		}
	}
	return refs
}
