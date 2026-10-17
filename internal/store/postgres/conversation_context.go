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
	ctx context.Context, organization, conversationID, currentInvestigationID uuid.UUID, recentMessageLimit int,
) (investigation.Brief, error) {
	if recentMessageLimit <= 0 || recentMessageLimit > investigation.BriefRecentMessages {
		recentMessageLimit = investigation.BriefRecentMessages
	}
	db, err := p.poolForOrganization(organization)
	if err != nil {
		return investigation.Brief{}, err
	}
	page, err := readPriorMessages(ctx, db, organization, conversationID, currentInvestigationID, 0, recentMessageLimit, false)
	if err != nil {
		return investigation.Brief{}, err
	}
	brief := investigation.Brief{
		Recent:     page.Exchange,
		NextBefore: page.NextBefore,
	}
	if len(brief.Recent) > 0 {
		brief.HistoryBefore = brief.Recent[len(brief.Recent)-1].Sequence + 1
	}
	if err = readPriorTurns(ctx, db, organization, conversationID, currentInvestigationID, &brief); err != nil {
		return investigation.Brief{}, err
	}
	var refs []investigation.EvidenceRef
	for _, finding := range brief.Findings {
		refs = append(refs, finding.References()...)
	}
	brief.MissingEvidence, err = evidenceMissing(ctx, db, organization, refs)
	if err != nil {
		return investigation.Brief{}, fmt.Errorf("checking prior evidence: %w", err)
	}
	if brief.MissingEvidence {
		brief.Limitations = append(brief.Limitations, investigation.MissingEvidenceStatement)
	}
	return brief, nil
}

func briefExchangeText(text string) string {
	if len([]rune(text)) <= investigation.BriefMessageBound {
		return text
	}
	const suffix = " [truncated]"
	return boundedRunes(text, investigation.BriefMessageBound-len(suffix)) + suffix
}

func readPriorTurns(
	ctx context.Context, db querier, organizationID, conversationID, currentInvestigationID uuid.UUID,
	brief *investigation.Brief,
) error {
	rows, err := db.Query(ctx, `
		SELECT CASE WHEN turn.conversation_id = $2 THEN turn.turn ELSE 0 END,
		       turn.investigation_id, turn.conclusion, turn.concluded_at,
		       turn.window_from, turn.window_until
		  FROM investigation turn
		  JOIN investigation current ON current.org_id = turn.org_id
		   AND current.conversation_id = $2 AND current.investigation_id = $3
		 WHERE turn.org_id = $1
		   AND turn.status = 2
		   AND turn.investigation_id <> $3
		   AND turn.concluded_at < current.created_at
		   AND (turn.conversation_id IS DISTINCT FROM $2 OR turn.turn < current.turn)
		   AND (turn.conversation_id = $2
		        OR (turn.incident_id IS NOT NULL
		            AND turn.incident_id = current.incident_id))
		 ORDER BY turn.conversation_id = $2 DESC, turn.turn DESC, turn.created_at DESC
		 LIMIT $4`, organizationID, conversationID, currentInvestigationID, investigation.BriefMaxFindings)
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

func (p *Database) ConversationHistory(
	ctx context.Context, organization, conversationID, currentInvestigationID uuid.UUID, beforeSequence int64,
) (investigation.HistoryPage, error) {
	db, err := p.poolForOrganization(organization)
	if err != nil {
		return investigation.HistoryPage{}, err
	}
	return readPriorMessages(ctx, db, organization, conversationID, currentInvestigationID,
		beforeSequence, investigation.BriefRecentMessages, true)
}

func readPriorMessages(
	ctx context.Context,
	db querier,
	organizationID, conversationID, currentInvestigationID uuid.UUID,
	beforeSequence int64,
	limit int,
	includeConclusions bool,
) (investigation.HistoryPage, error) {
	page := investigation.HistoryPage{Exchange: []investigation.BriefMessage{}}
	var currentTurn int
	err := db.QueryRow(ctx, `SELECT turn FROM investigation
		WHERE org_id = $1 AND conversation_id = $2 AND investigation_id = $3`,
		organizationID, conversationID, currentInvestigationID).Scan(&currentTurn)
	if errors.Is(err, pgx.ErrNoRows) {
		return page, investigation.ErrUnknown
	}
	if err != nil {
		return page, fmt.Errorf("reading current Investigation: %w", err)
	}
	rows, err := db.Query(ctx, `
		SELECT message.sequence, message.role, message.author_name, message.text,
		       message.created_at, owner.investigation_id,
		       CASE WHEN $6 AND message.role = 2 AND owner.status = 2 THEN owner.conclusion END
		  FROM conversation_message message
		  JOIN investigation owner ON owner.org_id = message.org_id
		   AND owner.conversation_id = message.conversation_id
		   AND owner.investigation_id = message.investigation_id
		 WHERE message.org_id = $1 AND message.conversation_id = $2
		   AND owner.turn < $3 AND ($4::bigint = 0 OR message.sequence < $4)
		 ORDER BY message.sequence DESC LIMIT $5`, organizationID, conversationID, currentTurn, beforeSequence, limit+1, includeConclusions)
	if err != nil {
		return page, fmt.Errorf("reading prior Messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var message investigation.BriefMessage
		var role int16
		if err = rows.Scan(&message.Sequence, &role, &message.Author, &message.Text,
			&message.CreatedAt, &message.InvestigationID, &message.Answer); err != nil {
			return page, fmt.Errorf("scanning prior Message: %w", err)
		}
		message.FromUser = role == int16(conversation.RoleUser)
		message.Text = briefExchangeText(message.Text)
		page.Exchange = append(page.Exchange, message)
	}
	if err = rows.Err(); err != nil {
		return page, fmt.Errorf("reading prior Messages: %w", err)
	}
	if len(page.Exchange) > limit {
		page.Exchange = page.Exchange[:limit]
		page.NextBefore = page.Exchange[limit-1].Sequence
	}
	slices.Reverse(page.Exchange)
	return page, nil
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
		return nil, errors.New("conversation provider origin is missing or inconsistent")
	}
	return &investigation.ConversationOrigin{
		IntegrationID: *integration,
		Channel:       channel,
		Thread:        thread,
	}, nil
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
	rows, err := pool.Query(ctx, `SELECT sequence, author_name, created_at, text
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
		if err := rows.Scan(&message.Sequence, &message.Author, &message.CreatedAt, &message.Text); err != nil {
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
