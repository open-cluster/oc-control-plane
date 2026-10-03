package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func (p *Database) ConversationBrief(
	ctx context.Context, organization uuid.UUID, id uuid.UUID, tail int,
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

func readRecentAnswers(ctx context.Context, pool querier, organization uuid.UUID,
	id uuid.UUID, limit int,
) ([]investigation.BriefMessage, error) {
	rows, err := pool.Query(ctx, `
		SELECT investigation_id, concluded_at, COALESCE(conclusion->>'summary', ''), conclusion
		  FROM investigation
		 WHERE org_id = $1 AND conversation_id = $2 AND status = 2
		 ORDER BY turn DESC
		 LIMIT $3`, organization, id, limit)
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

const conversationRolePerson = 1

func readPriorTurns(
	ctx context.Context, pool querier, organization uuid.UUID, id uuid.UUID,
	brief *investigation.Brief,
) error {
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
		 LIMIT $3`, organization, id, investigation.BriefMaxFindings)
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
			for _, hypothesis := range decoded.Hypotheses {
				if (hypothesis.Status == investigation.HypothesisExploring ||
					hypothesis.Status == investigation.HypothesisUnresolved) &&
					hypothesis.Statement != "" &&
					len(brief.OpenHypotheses) < investigation.BriefMaxConstraints {
					brief.OpenHypotheses = append(brief.OpenHypotheses,
						boundedRunes(hypothesis.Statement, investigation.BriefMessageBound))
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
				Kind:            string(finding.Kind),
				Runs:            finding.RunRefs,
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
