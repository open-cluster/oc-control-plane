package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

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
	if tail <= 0 || tail > investigation.BriefRecentMessages {
		tail = investigation.BriefRecentMessages
	}
	_, err := p.Conversation(ctx, organization, id)
	if err != nil {
		return investigation.Brief{}, err
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.Brief{}, err
	}

	brief := investigation.Brief{}
	messages, err := conversationMessages(ctx, pool, organization, id, tail)
	if err != nil {
		return investigation.Brief{}, err
	}
	for _, message := range messages {
		brief.Recent = append(brief.Recent, investigation.BriefMessage{
			FromPerson:      message.Role == conversationRolePerson,
			Actor:           message.ActorDisplay,
			Text:            briefExchangeText(message.Text),
			Sequence:        message.Sequence,
			CreatedAt:       message.CreatedAt,
			InvestigationID: message.InvestigationID,
		})
	}
	answers, err := readRecentAnswers(ctx, pool, organization, id, tail)
	if err != nil {
		return investigation.Brief{}, err
	}
	// Transaction timestamps can precede lock acquisition; Message sequence stays authoritative.
	brief.Recent = mergeExchange(brief.Recent, answers)
	if len(brief.Recent) > tail {
		brief.Recent = brief.Recent[len(brief.Recent)-tail:]
	}
	if err = readPriorTurns(ctx, pool, organization, id, &brief); err != nil {
		return investigation.Brief{}, err
	}
	var refs []investigation.EvidenceRef
	for _, finding := range brief.Findings {
		refs = append(refs, finding.References()...)
	}
	brief.MissingEvidence, err = evidenceMissing(ctx, pool, organization, refs)
	if err != nil {
		return investigation.Brief{}, fmt.Errorf("checking prior evidence: %w", err)
	}
	if brief.MissingEvidence {
		brief.Limitations = append(brief.Limitations, investigation.MissingEvidenceStatement)
	}
	return brief, nil
}

func readRecentAnswers(ctx context.Context, pool querier, organization tenancy.Organization,
	id uuid.UUID, limit int,
) ([]investigation.BriefMessage, error) {
	rows, err := pool.Query(ctx, `
		SELECT investigation_id, concluded_at, COALESCE(conclusion->>'summary', ''), conclusion
		  FROM investigation
		 WHERE org_id = $1 AND conversation_id = $2 AND status = 2
		 ORDER BY turn DESC
		 LIMIT $3`, organization.String(), id, limit)
	if err != nil {
		return nil, fmt.Errorf("reading recent answers: %w", err)
	}
	defer rows.Close()
	var answers []investigation.BriefMessage
	for rows.Next() {
		var answer investigation.BriefMessage
		if err = rows.Scan(&answer.InvestigationID, &answer.CreatedAt, &answer.Text, &answer.Answer); err != nil {
			return nil, fmt.Errorf("scanning recent answer: %w", err)
		}
		answer.Text = briefExchangeText(answer.Text)
		answers = append(answers, answer)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading recent answers: %w", err)
	}
	slices.Reverse(answers)
	return answers, nil
}

func briefExchangeText(text string) string {
	if len([]rune(text)) <= investigation.BriefMessageBound {
		return text
	}
	const suffix = " [truncated]"
	return boundedRunes(text, investigation.BriefMessageBound-len(suffix)) + suffix
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
	// theirs. Citations retain Investigation identity independently of local turn ordinals.
	rows, err := pool.Query(ctx, `
		SELECT CASE WHEN turn.conversation_id = $2 THEN turn.turn ELSE 0 END,
		       turn.investigation_id, turn.conclusion, turn.concluded_at,
		       turn.window_from, turn.window_until
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
			investigationID uuid.UUID
			turn            int
			conclusion      []byte
			observedAt      time.Time
			windowFrom      time.Time
			windowUntil     time.Time
		)
		if err = rows.Scan(&turn, &investigationID, &conclusion, &observedAt, &windowFrom, &windowUntil); err != nil {
			return fmt.Errorf("scanning a prior turn: %w", err)
		}
		var decoded investigation.Conclusion
		if err = json.Unmarshal(conclusion, &decoded); err != nil {
			return fmt.Errorf("decoding a prior turn's conclusion: %w", err)
		}
		if turn != 0 {
			for _, limitation := range decoded.Limitations {
				if limitation.Statement != "" &&
					len(brief.Limitations) < investigation.BriefMaxConstraints {
					brief.Limitations = append(brief.Limitations,
						boundedRunes(limitation.Statement, investigation.BriefMessageBound))
				}
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
				InvestigationID: investigationID,
				Turn:            turn,
				Statement:       finding.Statement,
				Kind:            finding.Kind,
				Confidence:      finding.Confidence,
				Runs:            finding.Sources,
				EvidenceRefs:    finding.EvidenceRefs,
				ObservedAt:      observedAt,
				WindowFrom:      windowFrom,
				WindowUntil:     windowUntil,
			})
		}
		brief.Findings = append(prior, brief.Findings...)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("reading a conversation's prior findings: %w", err)
	}

	return nil
}
