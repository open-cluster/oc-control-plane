package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// The conversation capability owns its vocabulary; this file is its persistence.
var _ conversation.Store = (*Database)(nil)

const conversationColumns = `conversation_id, incident_id, surface, subject, state,
	       created_by, created_at, last_activity_at`

// OpenConversation records one and audits the act.
func (p *Database) OpenConversation(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	wanted conversation.NewConversation,
) (conversation.Conversation, error) {
	return audited(ctx, p, principal, organization, audit.ActionConversationOpened,
		func(ctx context.Context, transaction pgx.Tx) (
			conversation.Conversation, audit.Target, audit.Detail, error,
		) {
			row := transaction.QueryRow(ctx, `
				INSERT INTO conversation (conversation_id, org_id, incident_id, surface,
				                          subject, created_by)
				VALUES ($1, $2, $3, $4, $5, $6)
				RETURNING `+conversationColumns,
				uuid.New(), organization.String(), nullableUUID(wanted.IncidentID),
				int16(wanted.Surface), wanted.Subject, wanted.CreatedBy)

			opened, err := scanConversation(row, organization.String())
			if err != nil {
				if isForeignKeyViolation(err) {
					// The only foreign key on the insert is the incident's, and it is
					// org-composite: an incident from another tenant is not distinguishable
					// here from one that does not exist, which is the answer either way.
					return conversation.Conversation{}, audit.Target{}, nil,
						conversation.ErrIncidentUnknown
				}
				return conversation.Conversation{}, audit.Target{}, nil,
					fmt.Errorf("opening a conversation: %w", err)
			}
			return opened,
				audit.Target{Kind: audit.TargetConversation, ID: opened.ID.String()},
				audit.Detail{"subject": opened.Subject}, nil
		})
}

// Conversation reads one, scoped to the tenant.
func (p *Database) Conversation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (conversation.Conversation, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return conversation.Conversation{}, err
	}
	row := pool.QueryRow(ctx, `
		SELECT `+conversationColumns+`
		  FROM conversation
		 WHERE conversation_id = $1 AND org_id = $2`, id, organization.String())
	found, err := scanConversation(row, organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return conversation.Conversation{}, conversation.ErrUnknown
	}
	if err != nil {
		return conversation.Conversation{}, fmt.Errorf("reading a conversation: %w", err)
	}
	return found, nil
}

// QueryConversations reports a page, most recently active first.
func (p *Database) QueryConversations(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	page conversation.Page,
) (conversation.List, error) {
	if !principal.MemberOf(organization) {
		return conversation.List{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return conversation.List{}, err
	}
	limit := pageLimit(page.Limit)
	cursorAt, cursorID, err := decodeCursor(page.After)
	if err != nil {
		return conversation.List{}, conversation.ErrBadCursor
	}

	arguments := []any{organization.String(), limit + 1}
	narrowing := ""
	if cursorID != nil {
		arguments = append(arguments, *cursorAt, *cursorID)
		narrowing = fmt.Sprintf(
			" AND (last_activity_at, conversation_id) < ($%d, $%d)",
			len(arguments)-1, len(arguments))
	}
	if page.Search != "" {
		// Case-insensitive containment on the subject. Deliberately dumb: a caller who
		// typed part of a subject wants the conversations whose subject contains it, and
		// anything cleverer would be an ordering nobody could explain.
		arguments = append(arguments, "%"+page.Search+"%")
		narrowing += fmt.Sprintf(" AND subject ILIKE $%d", len(arguments))
	}
	if page.Incident != uuid.Nil {
		arguments = append(arguments, page.Incident)
		narrowing += fmt.Sprintf(" AND incident_id = $%d", len(arguments))
	}
	if page.State != 0 {
		arguments = append(arguments, int16(page.State))
		narrowing += fmt.Sprintf(" AND state = $%d", len(arguments))
	}

	rows, err := pool.Query(ctx, `
		SELECT `+conversationColumns+`
		  FROM conversation
		 WHERE org_id = $1`+narrowing+`
		 ORDER BY last_activity_at DESC, conversation_id DESC
		 LIMIT $2`, arguments...)
	if err != nil {
		return conversation.List{}, fmt.Errorf("listing conversations: %w", err)
	}
	defer rows.Close()

	list := conversation.List{
		Conversations: make([]conversation.Conversation, 0, limit),
	}
	for rows.Next() {
		found, scanErr := scanConversation(rows, organization.String())
		if scanErr != nil {
			return conversation.List{}, scanErr
		}
		if len(list.Conversations) == limit {
			last := list.Conversations[limit-1]
			list.Next = encodeCursor(last.LastActivityAt, last.ID)
			break
		}
		list.Conversations = append(list.Conversations, found)
	}
	if err := rows.Err(); err != nil {
		return conversation.List{}, fmt.Errorf("listing conversations: %w", err)
	}
	return list, nil
}

// ConversationDetail reads one with its messages and the turns it opened. The message
// read is bounded and returns the NEWEST, in order — a conversation of a thousand
// messages is read from its end, and the whole transcript is not what a surface renders.
func (p *Database) ConversationDetail(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID, messages int,
) (conversation.Detail, error) {
	found, err := p.Conversation(ctx, organization, id)
	if err != nil {
		return conversation.Detail{}, err
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return conversation.Detail{}, err
	}

	said, err := conversationMessages(ctx, pool, organization, id, messages)
	if err != nil {
		return conversation.Detail{}, err
	}

	turnRows, err := pool.Query(ctx, `
		SELECT investigation_id, turn, status, coalesce(conclusion->>'summary', ''),
		       stopped_by, error, created_at,
		       concluded_at
		  FROM investigation
		 WHERE org_id = $1 AND conversation_id = $2
		 ORDER BY turn`, organization.String(), id)
	if err != nil {
		return conversation.Detail{}, fmt.Errorf("reading a conversation's turns: %w", err)
	}
	defer turnRows.Close()

	turns := make([]conversation.Turn, 0, 8)
	for turnRows.Next() {
		var (
			turn        conversation.Turn
			status      int16
			concludedAt *time.Time
		)
		if err := turnRows.Scan(&turn.InvestigationID, &turn.Ordinal, &status,
			&turn.Answer, &turn.StoppedBy, &turn.Error, &turn.CreatedAt,
			&concludedAt); err != nil {
			return conversation.Detail{}, fmt.Errorf("scanning a turn: %w", err)
		}
		turn.Status = investigationStatusWord(status)
		if concludedAt != nil {
			turn.ConcludedAt = *concludedAt
		}
		turns = append(turns, turn)
	}
	if err := turnRows.Err(); err != nil {
		return conversation.Detail{}, fmt.Errorf("reading a conversation's turns: %w", err)
	}

	return conversation.Detail{Conversation: found, Messages: said, Turns: turns}, nil
}

// conversationMessages reads the newest bounded window of a conversation's transcript, in
// order.
func conversationMessages(
	ctx context.Context, queries querier, organization tenancy.Organization,
	id uuid.UUID, limit int,
) ([]conversation.Message, error) {
	if limit <= 0 {
		limit = defaultPageSize
	}
	rows, err := queries.Query(ctx, `
		SELECT sequence, role, actor_kind, actor_id, actor_display, text, source_reference,
		       investigation_id, created_at
		  FROM (SELECT sequence, role, actor_kind, actor_id, actor_display, text, source_reference,
		               investigation_id, created_at
		          FROM conversation_message
		         WHERE org_id = $1 AND conversation_id = $2
		         ORDER BY sequence DESC
		         LIMIT $3) newest
		 ORDER BY sequence`, organization.String(), id, limit)
	if err != nil {
		return nil, fmt.Errorf("reading a conversation's messages: %w", err)
	}
	defer rows.Close()

	said := make([]conversation.Message, 0, limit)
	for rows.Next() {
		message, scanErr := scanMessage(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		said = append(said, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading a conversation's messages: %w", err)
	}
	return said, nil
}

// AppendMessage writes one message at the next sequence and stamps the conversation's
// activity.
//
// The sequence is assigned inside the same statement that reads it, from the row the
// conversation lock already serialises: two messages arriving at once take two positions
// rather than racing for one. The message is written with NO investigation — opening a
// turn is a separate decision, because whether one can be opened depends on whether
// another is running.
func (p *Database) AppendMessage(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, said conversation.NewMessage,
) (conversation.Message, error) {
	return audited(ctx, p, principal, organization, audit.ActionConversationMessage,
		func(ctx context.Context, transaction pgx.Tx) (
			conversation.Message, audit.Target, audit.Detail, error,
		) {
			state, err := lockConversation(ctx, transaction, organization, id)
			if err != nil {
				return conversation.Message{}, audit.Target{}, nil, err
			}
			if state != conversation.StateOpen {
				return conversation.Message{}, audit.Target{}, nil, conversation.ErrClosed
			}

			row := transaction.QueryRow(ctx, `
				INSERT INTO conversation_message (conversation_id, org_id, sequence, role,
				                                  actor_kind, actor_id, actor_display, text)
				SELECT $1, $2,
				       coalesce((SELECT max(sequence)
				                   FROM conversation_message
				                  WHERE org_id = $2 AND conversation_id = $1), 0) + 1,
				       $3, $4, $5, $6, $7
				RETURNING sequence, role, actor_kind, actor_id, actor_display, text, source_reference,
				          investigation_id, created_at`,
				id, organization.String(), int16(said.Role), int16(said.ActorKind),
				said.ActorID, said.ActorDisplay, said.Text)

			written, err := scanMessage(row)
			if err != nil {
				return conversation.Message{}, audit.Target{}, nil,
					fmt.Errorf("appending a message: %w", err)
			}

			if _, err := transaction.Exec(ctx, `
				UPDATE conversation
				   SET last_activity_at = now()
				 WHERE conversation_id = $1 AND org_id = $2`,
				id, organization.String()); err != nil {
				return conversation.Message{}, audit.Target{}, nil,
					fmt.Errorf("stamping a conversation: %w", err)
			}

			// The message TEXT is deliberately not in the audit detail. It is a
			// customer's own words, it can be eight kilobytes of them, and the record
			// exists to answer who said something rather than to hold a second copy of
			// what they said — the transcript is the copy.
			return written,
				audit.Target{Kind: audit.TargetConversation, ID: id.String()},
				audit.Detail{"sequence": written.Sequence}, nil
		})
}

// WaitingTurns counts this organization's investigations that are running and unleased —
// the queue, as the claimer sees it.
func (p *Database) WaitingTurns(
	ctx context.Context, organization tenancy.Organization,
) (int, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}
	var waiting int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM investigation
		 WHERE org_id = $1 AND status = 1 AND lease_worker = ''`,
		organization.String()).Scan(&waiting); err != nil {
		return 0, fmt.Errorf("counting waiting turns: %w", err)
	}
	return waiting, nil
}

// OpenTurn opens the next turn from whatever messages are queued and attaches them to it.
//
// The second return is false when a turn is already running for this conversation. That
// is the single-writer invariant refusing, and it is NOT an error: the messages stay
// queued and the drain at the running turn's terminal takes them up, which is the "next
// safe point" that keeps one conversation to one agent.
//
// lead is how far before the incident began the turn's window reaches back; a conversation
// naming no incident gets a window of that length ending now, because a follow-up question
// asked at three in the morning is about the last few hours and not about all of history.
func (p *Database) OpenTurn(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	lead time.Duration,
) (conversation.Turn, bool, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return conversation.Turn{}, false, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return conversation.Turn{}, false, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Rollback(ctx)
		}
	}()

	turn, opened, err := openTurn(ctx, transaction, organization, id, lead)
	if err != nil || !opened {
		return conversation.Turn{}, false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return conversation.Turn{}, false, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return turn, true, nil
}

// DrainConversation opens the next turn from whatever is queued, for the investigation
// runtime, which knows conversations only as an identifier its record carries.
//
// It is OpenTurn under another name on purpose: the drain at a terminal boundary and a
// message arriving to an idle conversation are the same act, and two implementations of
// "start the next turn" would be two places for the single-writer invariant to be got
// wrong.
func (p *Database) DrainConversation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	lead time.Duration,
) (bool, error) {
	_, opened, err := p.OpenTurn(ctx, organization, id, lead)
	return opened, err
}

// openTurn is the whole of opening a turn, inside somebody else's transaction: the
// conversation lock, the ordinal, the guarded insert, and the queued messages moving onto
// it. The drain at a terminal boundary runs exactly this, which is why it is a function
// rather than a method.
func openTurn(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	id uuid.UUID, lead time.Duration,
) (conversation.Turn, bool, error) {
	// The lock serialises two openers of ONE conversation so that they take different
	// ordinals rather than colliding on the turn uniqueness. It is not what makes the
	// single-writer invariant hold — the partial unique index below is — because a lock
	// held in one transaction says nothing to a replica that never took it.
	var (
		state      int16
		incidentID *uuid.UUID
		subject    string
	)
	if err := transaction.QueryRow(ctx, `
		SELECT state, incident_id, subject
		  FROM conversation
		 WHERE conversation_id = $1 AND org_id = $2
		   FOR UPDATE`, id, organization.String()).Scan(
		&state, &incidentID, &subject); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return conversation.Turn{}, false, conversation.ErrUnknown
		}
		return conversation.Turn{}, false, fmt.Errorf("locking a conversation: %w", err)
	}
	if conversation.State(state) != conversation.StateOpen {
		return conversation.Turn{}, false, conversation.ErrClosed
	}

	question, has, err := queuedQuestion(ctx, transaction, organization, id)
	if err != nil {
		return conversation.Turn{}, false, err
	}
	if !has {
		// Nothing is waiting to be asked. A drain that finds an empty queue is the
		// ordinary case, not a failure.
		return conversation.Turn{}, false, nil
	}

	from, until, err := turnWindow(ctx, transaction, organization, incidentID, lead)
	if err != nil {
		return conversation.Turn{}, false, err
	}

	investigationID := uuid.New()
	opener := turnOpener(ctx, transaction, organization, id)
	var (
		ordinal   int
		createdAt time.Time
	)
	err = transaction.QueryRow(ctx, `
		INSERT INTO investigation (investigation_id, org_id, incident_id, question,
		                           subject, window_from, window_until, conversation_id,
		                           turn, created_by)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8,
		       coalesce((SELECT max(turn)
		                   FROM investigation existing
		                  WHERE existing.org_id = $2
		                    AND existing.conversation_id = $8), 0) + 1,
		       $9
		RETURNING turn, created_at`,
		investigationID, organization.String(), incidentID, question, subject, from, until,
		id, opener).Scan(&ordinal, &createdAt)
	if err != nil {
		// The partial unique index is the single-writer invariant, and this is it
		// firing: another turn of this conversation is already running.
		if isUniqueViolation(err, "investigation_one_running_per_conversation") {
			return conversation.Turn{}, false, nil
		}
		return conversation.Turn{}, false, fmt.Errorf("opening a turn: %w", err)
	}

	if _, err := transaction.Exec(ctx, `
		UPDATE conversation_message
		   SET investigation_id = $1
		 WHERE org_id = $2 AND conversation_id = $3 AND investigation_id IS NULL`,
		investigationID, organization.String(), id); err != nil {
		return conversation.Turn{}, false, fmt.Errorf("attaching queued messages: %w", err)
	}

	return conversation.Turn{
		InvestigationID: investigationID,
		Ordinal:         ordinal,
		Status:          investigationStatusWord(int16(investigation.StatusRunning)),
		CreatedAt:       createdAt,
	}, true, nil
}

// queuedQuestion renders the messages no turn has taken up into the turn's question, in
// order. Several queued messages become one question because they are one thing to
// answer: a person who typed three lines while the agent worked asked one thing in three
// parts, and answering each separately would be three investigations of the same context.
func queuedQuestion(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	id uuid.UUID,
) (string, bool, error) {
	rows, err := transaction.Query(ctx, `
		SELECT text
		  FROM conversation_message
		 WHERE org_id = $1 AND conversation_id = $2 AND investigation_id IS NULL
		   AND role = 1
		 ORDER BY sequence`, organization.String(), id)
	if err != nil {
		return "", false, fmt.Errorf("reading queued messages: %w", err)
	}
	defer rows.Close()

	question := ""
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return "", false, fmt.Errorf("scanning a queued message: %w", err)
		}
		if question != "" {
			question += "\n"
		}
		question += text
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("reading queued messages: %w", err)
	}
	if question == "" {
		return "", false, nil
	}
	return boundedRunes(question, maxQuestionLength), true, nil
}

// turnOpener is who the turn is attributed to: the actor of the newest queued person
// message. A turn is caused by whoever asked for it, so an audit of the reads it performs
// answers "who caused this" rather than naming the process that happened to run it.
func turnOpener(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	id uuid.UUID,
) string {
	var actor string
	if err := transaction.QueryRow(ctx, `
		SELECT actor_id
		  FROM conversation_message
		 WHERE org_id = $1 AND conversation_id = $2 AND investigation_id IS NULL
		   AND role = 1
		 ORDER BY sequence DESC
		 LIMIT 1`, organization.String(), id).Scan(&actor); err != nil {
		return ""
	}
	return actor
}

// turnWindow derives the turn's time bounds. An incident-associated conversation reaches
// back before the incident began and forward to now while it is still open; one naming no
// incident gets the lead ending now.
func turnWindow(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	incidentID *uuid.UUID, lead time.Duration,
) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	if incidentID == nil {
		// No incident to anchor to, so the lead has nothing to lead: a question reaches
		// back its own documented span instead of borrowing an incident's.
		return now.Add(-conversation.QuestionWindow(lead)), now, nil
	}
	var (
		firstSeen time.Time
		lastSeen  time.Time
		status    int16
	)
	if err := transaction.QueryRow(ctx, `
		SELECT first_seen_at, last_seen_at, status
		  FROM incident
		 WHERE incident_id = $1 AND org_id = $2`,
		*incidentID, organization.String()).Scan(
		&firstSeen, &lastSeen, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return now.Add(-lead), now, nil
		}
		return time.Time{}, time.Time{}, fmt.Errorf("reading a turn's incident: %w", err)
	}
	until := lastSeen
	if status == 1 {
		until = now
	}
	return firstSeen.Add(-lead), until, nil
}

// lockConversation takes the row lock and reports the conversation's state, or that this
// organization does not have it.
func lockConversation(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	id uuid.UUID,
) (conversation.State, error) {
	var state int16
	if err := transaction.QueryRow(ctx, `
		SELECT state
		  FROM conversation
		 WHERE conversation_id = $1 AND org_id = $2
		   FOR UPDATE`, id, organization.String()).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, conversation.ErrUnknown
		}
		return 0, fmt.Errorf("locking a conversation: %w", err)
	}
	return conversation.State(state), nil
}

// maxQuestionLength mirrors the investigation question column's own bound.
const maxQuestionLength = 1024

// boundedRunes cuts text at a rune boundary inside the limit.
func boundedRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// investigationStatusWord renders a status column the way the investigation surface does.
// It DEFERS to the capability that owns the vocabulary rather than re-spelling it: a second
// copy of those words is exactly how a turn and the investigation it names come to disagree.
func investigationStatusWord(status int16) string {
	return investigation.Status(status).String()
}

func scanConversation(
	row scanned, organization string,
) (conversation.Conversation, error) {
	var (
		found      = conversation.Conversation{OrgID: organization}
		incidentID *uuid.UUID
		surface    int16
		state      int16
	)
	if err := row.Scan(&found.ID, &incidentID, &surface, &found.Subject, &state,
		&found.CreatedBy, &found.CreatedAt, &found.LastActivityAt); err != nil {
		return conversation.Conversation{}, err
	}
	found.Surface = conversation.Surface(surface)
	found.State = conversation.State(state)
	if incidentID != nil {
		found.IncidentID = *incidentID
	}
	return found, nil
}

func scanMessage(row scanned) (conversation.Message, error) {
	var (
		message         conversation.Message
		role            int16
		actorKind       int16
		investigationID *uuid.UUID
	)
	if err := row.Scan(&message.Sequence, &role, &actorKind, &message.ActorID,
		&message.ActorDisplay, &message.Text, &message.SourceReference, &investigationID,
		&message.CreatedAt); err != nil {
		return conversation.Message{}, fmt.Errorf("scanning a message: %w", err)
	}
	message.Role = conversation.Role(role)
	message.ActorKind = conversation.ActorKind(actorKind)
	if investigationID != nil {
		message.InvestigationID = *investigationID
	}
	return message, nil
}
