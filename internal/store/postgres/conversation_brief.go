package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// The conversation brief carries a bounded message tail and prior cited findings.
//
// Nothing here copies a tool payload. A finding already carries the ordinals of the runs
// that established it and those runs are still in the record, so the brief carries the
// REFERENCE. Copying the evidence would double a long conversation's context to repeat
// something the citation already says.

// ConversationBrief reads what a conversation contributes to its next turn.
func (p *Database) ConversationBrief(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID, tail int,
) (investigation.Brief, error) {
	found, err := p.Conversation(ctx, organization, id)
	if err != nil {
		return investigation.Brief{}, err
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.Brief{}, err
	}

	brief := investigation.Brief{
		ConversationID: found.ID.String(),
		Subject:        found.Subject,
	}
	var originatingIntegration uuid.UUID
	err = pool.QueryRow(ctx, `
		SELECT integration_id, channel_id, thread_ts
		  FROM slack_conversation
		 WHERE org_id = $1 AND conversation_id = $2`,
		organization.String(), id).Scan(&originatingIntegration,
		&brief.OriginChannel, &brief.OriginThread)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return investigation.Brief{}, fmt.Errorf("reading a conversation's provider origin: %w", err)
	}
	if err == nil {
		brief.OriginIntegrationID = originatingIntegration.String()
	}
	messages, err := conversationMessages(ctx, pool, organization, id, tail)
	if err != nil {
		return investigation.Brief{}, err
	}
	for _, message := range messages {
		if brief.RecentFrom == 0 {
			brief.RecentFrom = message.Sequence
		}
		brief.Recent = append(brief.Recent, investigation.BriefMessage{
			FromPerson: message.Role == conversationRolePerson,
			Actor:      message.ActorDisplay,
			Text:       boundedRunes(message.Text, investigation.BriefMessageBound),
		})
	}

	if err = readPriorTurns(ctx, pool, organization, id, &brief); err != nil {
		return investigation.Brief{}, err
	}
	return brief, nil
}

// conversationRolePerson is the message role a person's own words carry. Named here rather
// than written as a literal, because the mapping between the column and the meaning is what
// the frozen-enum gate exists to protect.
const conversationRolePerson = 1

// readPriorTurns fills the brief with what the conversation's concluded turns established.
//
// Only CONCLUDED turns contribute. A running one has established nothing yet, and a failed
// one established nothing at all — carrying its findings would be carrying findings that do
// not exist.
func readPriorTurns(
	ctx context.Context, pool querier, organization tenancy.Organization, id uuid.UUID,
	brief *investigation.Brief,
) error {
	// This conversation's own concluded turns, and — when it is about an incident — the
	// concluded investigations of every OTHER conversation on that same incident.
	//
	// That second half is the whole of what conversations about one incident share.
	// FINDINGS ONLY: durable, cited, incident-level fact. Never another conversation's
	// messages and never its summary, because what somebody else asked and was told is
	// theirs. A sibling turn carries no ordinal in this conversation, so its citation
	// references turn 0 — which reads as "established elsewhere on this incident" rather
	// than as a turn of this conversation that nobody can find.
	rows, err := pool.Query(ctx, `
		SELECT CASE WHEN turn.conversation_id = $2 THEN turn.turn ELSE 0 END,
		       turn.conclusion
		  FROM investigation turn
		 WHERE turn.org_id = $1
		   AND turn.status = 2
		   AND (turn.conversation_id = $2
		        OR (turn.incident_id IS NOT NULL
		            AND turn.incident_id = (SELECT incident_id
		                                     FROM conversation
		                                    WHERE org_id          = $1
		                                      AND conversation_id = $2)))
		 ORDER BY turn.conversation_id = $2 DESC, turn.turn DESC, turn.created_at DESC
		 LIMIT $3`, organization.String(), id, investigation.BriefMaxFindings)
	if err != nil {
		return fmt.Errorf("reading a conversation's prior findings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			turn       int
			conclusion []byte
		)
		if err = rows.Scan(&turn, &conclusion); err != nil {
			return fmt.Errorf("scanning a prior turn: %w", err)
		}
		var decoded investigation.Conclusion
		if err = json.Unmarshal(conclusion, &decoded); err != nil {
			return fmt.Errorf("decoding a prior turn's conclusion: %w", err)
		}
		for _, action := range decoded.Actions {
			if len(brief.Recommended) < investigation.BriefMaxConstraints {
				brief.Recommended = append(brief.Recommended, action.Title)
			}
		}
		if len(decoded.Findings) == 0 {
			continue
		}
		remaining := investigation.BriefMaxFindings - len(brief.Findings)
		if remaining <= 0 {
			continue
		}
		if len(decoded.Findings) > remaining {
			decoded.Findings = decoded.Findings[len(decoded.Findings)-remaining:]
		}
		prior := make([]investigation.PriorFinding, 0, len(decoded.Findings))
		for _, finding := range decoded.Findings {
			prior = append(prior, investigation.PriorFinding{
				Turn: turn, Statement: finding.Statement, Kind: finding.Kind,
				Confidence: finding.Confidence, Runs: finding.Sources,
			})
		}
		brief.Findings = append(prior, brief.Findings...)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("reading a conversation's prior findings: %w", err)
	}

	return readPriorReads(ctx, pool, organization, id, brief)
}

// readPriorReads fills in what the conversation's turns actually read: the identifiers in
// play, and the reads that failed — so a gap in an answer stays explained.
func readPriorReads(
	ctx context.Context, pool querier, organization tenancy.Organization, id uuid.UUID,
	brief *investigation.Brief,
) error {
	rows, err := pool.Query(ctx, `
		SELECT run.sources, run.error
		  FROM investigation_tool_run run
		  JOIN investigation turn
		    ON turn.investigation_id = run.investigation_id
		   AND turn.org_id           = run.org_id
		 WHERE turn.org_id = $1 AND turn.conversation_id = $2
		 ORDER BY turn.turn DESC, run.ordinal DESC
		 LIMIT $3`, organization.String(), id,
		investigation.BriefMaxIdentifiers+investigation.BriefMaxConstraints)
	if err != nil {
		return fmt.Errorf("reading a conversation's prior reads: %w", err)
	}
	defer rows.Close()

	identifiers := map[string]bool{}
	failures := map[string]bool{}
	for rows.Next() {
		var (
			sources  []byte
			runError string
		)
		if err = rows.Scan(&sources, &runError); err != nil {
			return fmt.Errorf("scanning a prior read: %w", err)
		}
		read, decodeErr := decodeStringArray(sources)
		if decodeErr != nil {
			return fmt.Errorf("decoding a prior read's sources: %w", decodeErr)
		}
		for _, identifier := range read {
			if identifier != "" && !identifiers[identifier] &&
				len(brief.Identifiers) < investigation.BriefMaxIdentifiers {
				identifiers[identifier] = true
				brief.Identifiers = append(brief.Identifiers, identifier)
			}
		}
		if runError != "" && !failures[runError] &&
			len(brief.FailedReads) < investigation.BriefMaxConstraints {
			failures[runError] = true
			brief.FailedReads = append(brief.FailedReads, runError)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("reading a conversation's prior reads: %w", err)
	}
	return nil
}
