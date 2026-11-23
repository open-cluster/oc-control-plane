package storage

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/postmortem"
)

const toolRunOwnerJoins = `
	LEFT JOIN agent_execution e ON e.org_id = r.org_id AND e.execution_id = r.execution_id
	LEFT JOIN investigation i ON i.org_id = r.org_id AND i.investigation_id = coalesce(r.investigation_id, e.investigation_id)
	LEFT JOIN conversation source ON source.org_id = r.org_id
		AND source.conversation_id = coalesce(e.conversation_id, i.conversation_id)`

const webToolRunSource = "(source.conversation_id IS NULL OR source.source = 'web')"

const citationUUID = `CASE WHEN pg_input_is_valid(ref.value #>> '{}', 'uuid')
	THEN (ref.value #>> '{}')::uuid END`

const citedInvestigations = `
	FROM jsonb_path_query(doc.body, '$.**.investigationId') ref(value)
	JOIN investigation i ON i.org_id = doc.org_id AND i.investigation_id = ` + citationUUID

const citedToolRunConversations = `
	SELECT source.conversation_id
	  FROM jsonb_path_query(doc.body, '$.**.toolRunId') ref(value)
	  JOIN investigation_tool_run r ON r.org_id = doc.org_id AND r.tool_run_id = ` + citationUUID + toolRunOwnerJoins

const privateInvestigationSources = `
	SELECT 1 FROM conversation private
	 WHERE private.org_id = i.org_id AND private.source = 'slack'
	   AND (private.conversation_id = i.conversation_id OR private.conversation_id IN (
	        SELECT cited.conversation_id
	          FROM (SELECT i.org_id, i.conclusion AS body) doc
	          CROSS JOIN LATERAL (
	              SELECT i.conversation_id ` + citedInvestigations + `
	              UNION ALL ` + citedToolRunConversations + `
	          ) cited))`

const webInvestigationSource = "NOT EXISTS (" + privateInvestigationSources + ")"

func (p *Database) EvidenceMissing(ctx context.Context,
	organization, conversationID uuid.UUID,
	refs []investigation.EvidenceRef) (bool, error) {
	db, err := p.poolForOrganization(organization)
	if err != nil {
		return false, err
	}
	return evidenceMissing(ctx, db, organization, conversationID, refs, false)
}

func evidenceMissing(ctx context.Context, pool querier, org, conversationID uuid.UUID,
	refs []investigation.EvidenceRef, allowFailed bool,
) (bool, error) {
	if len(refs) == 0 {
		return false, nil
	}
	ids := make([]uuid.UUID, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ToolRunID)
	}
	var missing bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM unnest($2::uuid[]) AS reference(tool_run_id)
		WHERE NOT EXISTS (SELECT 1 FROM investigation_tool_run r `+toolRunOwnerJoins+`
			 WHERE r.org_id = $1 AND r.tool_run_id = reference.tool_run_id
			   AND r.outcome IS NOT NULL AND (r.outcome = $3 OR $5)
			   AND (`+webToolRunSource+` OR source.conversation_id = $4)
			   AND NOT EXISTS (`+privateInvestigationSources+`
			       AND private.conversation_id IS DISTINCT FROM $4)))`,
		org, ids, int16(investigation.RunSucceeded), nullableUUID(conversationID), allowFailed).Scan(&missing)
	return missing, err
}

func historicalEvidenceMissing(ctx context.Context, pool querier, org, conversationID uuid.UUID,
	refs []investigation.HistoricalEvidenceRef,
) (bool, error) {
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
			 WHERE r.org_id = $1 AND r.outcome = $5
			   AND (reference.tool_run_id IS NULL OR r.tool_run_id = reference.tool_run_id)
			   AND CASE WHEN reference.investigation_id IS NULL
			            THEN reference.tool_run_id IS NOT NULL AND reference.ordinal = 0
			            ELSE r.investigation_id = reference.investigation_id AND r.ordinal = reference.ordinal END
			   AND (`+webToolRunSource+` OR source.conversation_id = $6)
			   AND NOT EXISTS (`+privateInvestigationSources+`
			       AND private.conversation_id IS DISTINCT FROM $6)))`,
		org, runIDs, investigationIDs, ordinals, int16(investigation.RunSucceeded),
		nullableUUID(conversationID)).Scan(&missing)
	return missing, err
}

func validateAgentConclusionEvidence(ctx context.Context, tx querier, org uuid.UUID, execution AgentExecution,
	conclusion investigation.Conclusion,
) error {
	type citations struct {
		refs           []investigation.EvidenceRef
		requireSuccess bool
	}
	sections := []citations{{conclusion.Impact.EvidenceRefs, true}}
	for _, finding := range conclusion.Findings {
		if len(finding.EvidenceRefs) == 0 {
			return ErrAgentEvidenceUnavailable
		}
		sections = append(sections, citations{finding.EvidenceRefs, true})
	}
	for _, hypothesis := range conclusion.Hypotheses {
		sections = append(sections, citations{hypothesis.EvidenceRefs, true})
	}
	for _, action := range conclusion.Actions {
		sections = append(sections, citations{action.EvidenceRefs, true})
	}
	for _, limitation := range conclusion.Limitations {
		sections = append(sections, citations{limitation.EvidenceRefs, false})
	}
	var ids []uuid.UUID
	var requireSuccess []bool
	for _, section := range sections {
		seen := make(map[uuid.UUID]bool, len(section.refs))
		for _, ref := range section.refs {
			if ref.ToolRunID == uuid.Nil || seen[ref.ToolRunID] {
				return ErrAgentEvidenceUnavailable
			}
			seen[ref.ToolRunID] = true
			ids = append(ids, ref.ToolRunID)
			requireSuccess = append(requireSuccess, section.requireSuccess)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var missing bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM unnest($3::uuid[], $4::boolean[]) ref(tool_run_id, require_success)
		WHERE NOT EXISTS (SELECT 1 FROM investigation_tool_run r
			WHERE r.org_id = $1 AND r.tool_run_id = ref.tool_run_id
			  AND (r.execution_id = $2 OR EXISTS (
			      SELECT 1 FROM agent_execution previous
			      JOIN investigation prior ON prior.org_id = previous.org_id AND prior.investigation_id = previous.investigation_id
			      JOIN investigation current ON current.org_id = prior.org_id AND current.investigation_id = $6
			      WHERE previous.org_id = r.org_id AND previous.execution_id = r.execution_id
			        AND previous.status = 'completed' AND prior.status = $7
			        AND prior.conversation_id = current.conversation_id AND prior.turn < current.turn))
			  AND r.outcome IS NOT NULL AND (NOT ref.require_success OR r.outcome = $5)))`,
		org, execution.ID, ids, requireSuccess, int16(investigation.RunSucceeded), execution.InvestigationID,
		int16(investigation.StatusConcluded)).Scan(&missing)
	if err == nil && missing {
		return ErrAgentEvidenceUnavailable
	}
	return err
}

func resultEvidence(found investigation.Investigation) ([]investigation.EvidenceRef, []investigation.HistoricalEvidenceRef) {
	var refs []investigation.EvidenceRef
	var historical []investigation.HistoricalEvidenceRef
	add := func(section string, index int, evidence []investigation.EvidenceRef) {
		entries := found.Historical[section]
		if index < len(entries) {
			historical = append(historical, entries[index].EvidenceRefs...)
			for _, ordinal := range entries[index].RunRefs {
				historical = append(historical, investigation.HistoricalEvidenceRef{InvestigationID: found.ID, ToolRunOrdinal: ordinal})
			}
			return
		}
		refs = append(refs, evidence...)
	}
	add("impact", 0, found.Conclusion.Impact.EvidenceRefs)
	for index, finding := range found.Conclusion.Findings {
		add("findings", index, finding.EvidenceRefs)
	}
	for index, hypothesis := range found.Conclusion.Hypotheses {
		add("hypotheses", index, hypothesis.EvidenceRefs)
	}
	for index, action := range found.Conclusion.Actions {
		add("actions", index, action.EvidenceRefs)
	}
	return refs, historical
}

type historicalRun struct {
	investigation uuid.UUID
	ordinal       int
}

func resolveHistoricalPostmortemEvidence(result *postmortem.InvestigationResult,
	retained investigation.HistoricalCitations, runs map[historicalRun]uuid.UUID,
) {
	resolve := func(section string, index int, refs *[]investigation.EvidenceRef) {
		entries := retained[section]
		if index >= len(entries) {
			return
		}
		entry := entries[index]
		*refs = make([]investigation.EvidenceRef, 0, len(entry.EvidenceRefs)+len(entry.RunRefs))
		for _, ref := range entry.EvidenceRefs {
			id := ref.ToolRunID
			if ref.ToolRunOrdinal != 0 {
				resolved := runs[historicalRun{investigation: ref.InvestigationID, ordinal: ref.ToolRunOrdinal}]
				if id == uuid.Nil {
					id = resolved
				} else if id != resolved {
					id = uuid.Nil
				}
			}
			*refs = append(*refs, investigation.EvidenceRef{ToolRunID: id})
		}
		for _, ordinal := range entry.RunRefs {
			id := runs[historicalRun{investigation: result.InvestigationID, ordinal: ordinal}]
			*refs = append(*refs, investigation.EvidenceRef{ToolRunID: id})
		}
	}
	resolve("impact", 0, &result.Conclusion.Impact.EvidenceRefs)
	for i := range result.Conclusion.Findings {
		resolve("findings", i, &result.Conclusion.Findings[i].EvidenceRefs)
	}
	for i := range result.Conclusion.Actions {
		resolve("actions", i, &result.Conclusion.Actions[i].EvidenceRefs)
	}
}
