package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
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
	pool, err := p.poolForOrganization(organization)
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
			FromUser:        message.Role == conversationRoleUser,
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

const conversationRoleUser = 1

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
				if hypothesis.Status == investigation.HypothesisUnresolved &&
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

func (p *Database) ConversationHistory(ctx context.Context, org uuid.UUID, id uuid.UUID, before int64) (investigation.HistoryPage, error) {
	page := investigation.HistoryPage{Exchange: []investigation.BriefMessage{}}
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return page, err
	}
	rows, err := pool.Query(ctx, `SELECT sequence, role, actor_display, text, created_at, investigation_id
		FROM conversation_message WHERE org_id=$1 AND conversation_id=$2 AND sequence<$3
		ORDER BY sequence DESC LIMIT $4`, org, id, before, investigation.BriefRecentMessages+1)
	if err != nil {
		return page, fmt.Errorf("reading older Messages: %w", err)
	}
	var messages []investigation.BriefMessage
	for rows.Next() {
		var message investigation.BriefMessage
		var role int16
		var owner *uuid.UUID
		if err = rows.Scan(&message.Sequence, &role, &message.Actor, &message.Text, &message.CreatedAt, &owner); err != nil {
			rows.Close()
			return page, err
		}
		message.FromUser = role == conversationRoleUser
		message.Text = briefExchangeText(message.Text)
		if owner != nil {
			message.InvestigationID = *owner
		}
		messages = append(messages, message)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(messages) == 0 {
		return page, nil
	}
	if len(messages) > investigation.BriefRecentMessages {
		messages = messages[:investigation.BriefRecentMessages]
		page.NextBefore = messages[len(messages)-1].Sequence
	}
	slices.Reverse(messages)
	ids := make([]uuid.UUID, 0, len(messages))
	for _, message := range messages {
		if message.InvestigationID != uuid.Nil && !slices.Contains(ids, message.InvestigationID) {
			ids = append(ids, message.InvestigationID)
		}
	}
	answers, err := historyAnswers(ctx, pool, org, id, ids, messages[len(messages)-1].Sequence)
	if err != nil {
		return page, err
	}
	page.Exchange = mergeExchange(messages, answers)
	var refs []investigation.EvidenceRef
	for _, entry := range answers {
		if entry.Answer == nil {
			continue
		}
		for _, finding := range entry.Answer.Findings {
			refs = append(refs, finding.EvidenceRefs...)
			for _, ordinal := range finding.RunRefs {
				refs = append(refs, investigation.EvidenceRef{
					InvestigationID: entry.InvestigationID, ToolRunOrdinal: ordinal,
				})
			}
		}
	}
	page.MissingEvidence, err = evidenceMissing(ctx, pool, org, refs)
	if err != nil {
		return page, fmt.Errorf("checking older evidence: %w", err)
	}
	if page.MissingEvidence {
		page.Limitations = []string{investigation.MissingEvidenceStatement}
	}
	return page, nil
}

func historyAnswers(ctx context.Context, pool querier, org uuid.UUID, id uuid.UUID, ids []uuid.UUID, through int64) ([]investigation.BriefMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `SELECT turn.investigation_id, turn.concluded_at, COALESCE(turn.conclusion->>'summary',''), turn.conclusion,
		(SELECT MAX(message.sequence) FROM conversation_message message WHERE message.org_id=$1
			AND message.conversation_id=$2 AND message.investigation_id=turn.investigation_id)
		FROM investigation turn WHERE turn.org_id=$1 AND turn.conversation_id=$2
		AND turn.investigation_id=ANY($3) AND turn.status=2
		AND NOT EXISTS (SELECT 1 FROM conversation_message message WHERE message.org_id=$1
			AND message.conversation_id=$2 AND message.investigation_id=turn.investigation_id AND message.sequence>$4)
		ORDER BY turn.concluded_at, turn.turn`, org, id, ids, through)
	if err != nil {
		return nil, fmt.Errorf("reading older answers: %w", err)
	}
	defer rows.Close()
	var answers []investigation.BriefMessage
	for rows.Next() {
		var answer investigation.BriefMessage
		if err = rows.Scan(&answer.InvestigationID, &answer.CreatedAt, &answer.Text, &answer.Answer, &answer.Sequence); err != nil {
			return nil, err
		}
		answer.Text = briefExchangeText(answer.Text)
		answers = append(answers, answer)
	}
	return answers, rows.Err()
}

func mergeExchange(messages, answers []investigation.BriefMessage) []investigation.BriefMessage {
	exchange := make([]investigation.BriefMessage, 0, len(messages)+len(answers))
	for _, message := range messages {
		for len(answers) > 0 && answers[0].CreatedAt.Before(message.CreatedAt) {
			exchange = append(exchange, answers[0])
			answers = answers[1:]
		}
		exchange = append(exchange, message)
	}
	return append(exchange, answers...)
}

func (p *Database) ConversationOrigin(ctx context.Context, organization uuid.UUID, id uuid.UUID) (*investigation.ConversationOrigin, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return nil, err
	}
	var source conversation.Source
	var integration *uuid.UUID
	var channel, thread string
	err = pool.QueryRow(ctx, `SELECT c.source, i.integration_id,
		COALESCE(s.channel_id, ''), COALESCE(s.thread_ts, '')
		FROM conversation c
		LEFT JOIN slack_conversation s ON s.org_id = c.org_id AND s.conversation_id = c.conversation_id
		LEFT JOIN integration i ON i.org_id = c.org_id AND i.integration_id = s.integration_id
		WHERE c.org_id = $1 AND c.conversation_id = $2`, organization, id).
		Scan(&source, &integration, &channel, &thread)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, conversation.ErrUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("reading Conversation origin: %w", err)
	}
	if source == conversation.SourceWeb && integration == nil && channel == "" && thread == "" {
		return nil, nil
	}
	if source != conversation.SourceSlack || integration == nil || *integration == uuid.Nil || channel == "" || thread == "" {
		return nil, errors.New("Conversation provider origin is missing or inconsistent")
	}
	return &investigation.ConversationOrigin{IntegrationID: *integration, Channel: channel, Thread: thread}, nil
}

func (p *Database) InvestigationMessages(ctx context.Context, org uuid.UUID, conversationID, id uuid.UUID) ([]investigation.AssignedMessage, error) {
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM investigation
		WHERE org_id = $1 AND conversation_id = $2 AND investigation_id = $3)`,
		org, conversationID, id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, investigation.ErrUnknown
	}
	rows, err := pool.Query(ctx, `SELECT sequence, actor_display, created_at, text
		FROM conversation_message WHERE org_id = $1 AND conversation_id = $2
		AND investigation_id = $3 AND role = 1 ORDER BY sequence LIMIT $4`,
		org, conversationID, id, maxQueuedMessages+1)
	if err != nil {
		return nil, fmt.Errorf("reading assigned Messages: %w", err)
	}
	defer rows.Close()
	var messages []investigation.AssignedMessage
	for rows.Next() {
		var message investigation.AssignedMessage
		if err := rows.Scan(&message.Sequence, &message.Actor, &message.CreatedAt, &message.Text); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(messages) > maxQueuedMessages {
		return nil, fmt.Errorf("assigned Message batch exceeds %d", maxQueuedMessages)
	}
	return messages, nil
}
