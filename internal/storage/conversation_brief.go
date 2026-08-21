package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The conversation brief and the running summary: what a follow-up turn is told, and where
// the memory of everything older lives.
//
// Nothing here copies a tool payload. A finding already carries the ordinals of the runs
// that established it and those runs are still in the record, so the brief carries the
// REFERENCE. Copying the evidence would double a long conversation's context to repeat
// something the citation already says.

// ConversationBrief reads what a conversation contributes to its next turn.
func (p *Placements) ConversationBrief(
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
	if brief.Summary, err = newestSummary(ctx, pool, organization, id); err != nil {
		return investigation.Brief{}, err
	}

	messages, err := conversationMessages(ctx, pool, organization, id, tail)
	if err != nil {
		return investigation.Brief{}, err
	}
	for _, message := range messages {
		// A message the summary already accounts for is not carried twice. The tail is
		// what is still verbatim; everything behind it is the summary's job.
		if message.Sequence <= brief.Summary.CoversThrough {
			continue
		}
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
	rows, err := pool.Query(ctx, `
		SELECT turn, findings
		  FROM investigation
		 WHERE org_id = $1 AND conversation_id = $2 AND status = 2
		 ORDER BY turn`, organization.String(), id)
	if err != nil {
		return fmt.Errorf("reading a conversation's prior findings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			turn     int
			findings []byte
		)
		if err = rows.Scan(&turn, &findings); err != nil {
			return fmt.Errorf("scanning a prior turn: %w", err)
		}
		if len(findings) == 0 {
			continue
		}
		var decoded []struct {
			Statement  string `json:"statement"`
			Kind       string `json:"kind"`
			Confidence string `json:"confidence"`
			Sources    []int  `json:"sources"`
		}
		if err = json.Unmarshal(findings, &decoded); err != nil {
			return fmt.Errorf("decoding a prior turn's findings: %w", err)
		}
		for _, finding := range decoded {
			brief.Findings = append(brief.Findings, investigation.PriorFinding{
				Turn: turn, Statement: finding.Statement, Kind: finding.Kind,
				Confidence: finding.Confidence, Runs: finding.Sources,
			})
		}
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
		 ORDER BY turn.turn, run.ordinal`, organization.String(), id)
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

// newestSummary reads the newest running summary, or an empty one when nothing has been
// compacted yet.
func newestSummary(
	ctx context.Context, pool querier, organization tenancy.Organization, id uuid.UUID,
) (investigation.Summary, error) {
	rows, err := pool.Query(ctx, `
		SELECT version, covers_through_message_sequence, summary
		  FROM conversation_summary
		 WHERE org_id = $1 AND conversation_id = $2
		 ORDER BY version DESC
		 LIMIT 1`, organization.String(), id)
	if err != nil {
		return investigation.Summary{}, fmt.Errorf("reading a conversation summary: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return investigation.Summary{},
				fmt.Errorf("reading a conversation summary: %w", err)
		}
		return investigation.Summary{}, nil
	}
	var (
		summary investigation.Summary
		body    []byte
	)
	if err = rows.Scan(&summary.Version, &summary.CoversThrough, &body); err != nil {
		return investigation.Summary{}, fmt.Errorf("scanning a conversation summary: %w", err)
	}
	if len(body) > 0 {
		if err = json.Unmarshal(body, &summary); err != nil {
			return investigation.Summary{},
				fmt.Errorf("decoding a conversation summary: %w", err)
		}
	}
	return summary, nil
}

// RecordConversationSummary writes the next summary version.
//
// Superseded versions are KEPT. A summary that turns out to have dropped something is a
// thing to be able to read afterwards, and the row is small. The authoritative transcript
// in conversation_message is not touched at all — this is the model's working memory of
// what was said, never the record of it.
func (p *Placements) RecordConversationSummary(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	summary investigation.Summary, tokensBefore, tokensAfter int, model string,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	body, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("encoding a conversation summary: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO conversation_summary (conversation_id, org_id, version,
		                                  covers_through_message_sequence, summary,
		                                  tokens_before, tokens_after, model)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, organization.String(), summary.Version, summary.CoversThrough, body,
		tokensBefore, tokensAfter, boundedRunes(model, maxSummaryModelLength))
	if err != nil {
		if isForeignKeyViolation(err) {
			return investigationConversationUnknown
		}
		return fmt.Errorf("recording a conversation summary: %w", err)
	}
	return nil
}

// maxSummaryModelLength mirrors the column's own bound.
const maxSummaryModelLength = 128

// investigationConversationUnknown reports a summary written for a conversation this
// organization does not have.
var investigationConversationUnknown = errors.New("conversation unknown")
