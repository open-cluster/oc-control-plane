package storage

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func (p *Database) ConversationHistory(ctx context.Context, org tenancy.Organization, id uuid.UUID, before int64) (investigation.HistoryPage, error) {
	page := investigation.HistoryPage{Exchange: []investigation.BriefMessage{}}
	pool, err := p.Pool(org)
	if err != nil {
		return page, err
	}
	rows, err := pool.Query(ctx, `SELECT sequence, role, actor_display, text, created_at, investigation_id
		FROM conversation_message WHERE org_id=$1 AND conversation_id=$2 AND sequence<$3
		ORDER BY sequence DESC LIMIT $4`, org.String(), id, before, investigation.BriefRecentMessages+1)
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
		message.FromPerson = role == conversationRolePerson
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
			for _, ordinal := range finding.Sources {
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

func historyAnswers(ctx context.Context, pool querier, org tenancy.Organization, id uuid.UUID, ids []uuid.UUID, through int64) ([]investigation.BriefMessage, error) {
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
		ORDER BY turn.concluded_at, turn.turn`, org.String(), id, ids, through)
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
