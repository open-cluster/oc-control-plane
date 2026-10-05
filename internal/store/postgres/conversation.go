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
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

var _ conversation.Store = (*Database)(nil)

const conversationColumns = `conversation_id, incident_id, surface, subject, state,
	       created_by, created_at, last_activity_at`

func (p *Database) OpenConversation(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
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
				uuid.New(), organization, nullableUUID(wanted.IncidentID),
				int16(wanted.Surface), wanted.Subject, wanted.CreatedBy)

			opened, err := scanConversation(row, organization.String())
			if err != nil {
				if isForeignKeyViolation(err) {
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

func (p *Database) Conversation(
	ctx context.Context, organization uuid.UUID, id uuid.UUID,
) (conversation.Conversation, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return conversation.Conversation{}, err
	}
	row := pool.QueryRow(ctx, `
		SELECT `+conversationColumns+`
		  FROM conversation
		 WHERE conversation_id = $1 AND org_id = $2`, id, organization)
	found, err := scanConversation(row, organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return conversation.Conversation{}, conversation.ErrUnknown
	}
	if err != nil {
		return conversation.Conversation{}, fmt.Errorf("reading a conversation: %w", err)
	}
	return found, nil
}

func (p *Database) QueryConversations(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	page conversation.Page,
) (conversation.List, error) {
	if principal.Organization() != organization {
		return conversation.List{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return conversation.List{}, err
	}
	sortField := page.Sort
	descending := page.Descending
	if sortField == "" {
		sortField = "lastActivityAt"
		descending = true
	}
	if sortField != "lastActivityAt" {
		return conversation.List{}, fmt.Errorf("conversation listing cannot order by %q", sortField)
	}
	scope := sortScope(sortField, descending)
	limit := pageLimit(page.Limit)
	cursorAt, cursorID, err := decodeTimeSortCursor(page.After, scope)
	if err != nil {
		return conversation.List{}, conversation.ErrBadCursor
	}

	arguments := []any{organization, limit + 1}
	narrowing := ""
	if cursorID != nil {
		arguments = append(arguments, *cursorAt, *cursorID)
		comparison := ">"
		if descending {
			comparison = "<"
		}
		narrowing = fmt.Sprintf(
			" AND (last_activity_at, conversation_id) %s ($%d, $%d)",
			comparison, len(arguments)-1, len(arguments))
	}
	if page.Search != "" {
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

	direction := "ASC"
	if descending {
		direction = "DESC"
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT `+conversationColumns+`
		  FROM conversation
		 WHERE org_id = $1`+narrowing+`
		 ORDER BY last_activity_at %s, conversation_id %s
		 LIMIT $2`, direction, direction), arguments...)
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
			list.Next = encodeTimeSortCursor(scope, last.LastActivityAt, last.ID)
			break
		}
		list.Conversations = append(list.Conversations, found)
	}
	if err := rows.Err(); err != nil {
		return conversation.List{}, fmt.Errorf("listing conversations: %w", err)
	}
	return list, nil
}

func (p *Database) ConversationDetail(
	ctx context.Context, organization uuid.UUID, id uuid.UUID, messages int,
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

	turns, err := readConversationTurns(ctx, pool, organization, id, defaultPageSize, "")
	if err != nil {
		return conversation.Detail{}, err
	}
	return conversation.Detail{Conversation: found, Messages: said, Turns: turns.Turns, TurnsNext: turns.Next}, nil
}

func conversationMessages(
	ctx context.Context, queries querier, organization uuid.UUID,
	id uuid.UUID, limit int,
) ([]conversation.Message, error) {
	if limit <= 0 {
		limit = defaultPageSize
	}
	rows, err := queries.Query(ctx, `
		SELECT sequence, role, actor_kind, actor_id, actor_display, text, source_reference,
		       investigation_id, created_at, window_from, window_until
		  FROM (SELECT sequence, role, actor_kind, actor_id, actor_display, text, source_reference,
		               investigation_id, created_at, window_from, window_until
		          FROM conversation_message
		         WHERE org_id = $1 AND conversation_id = $2
		         ORDER BY sequence DESC
		         LIMIT $3) newest
		 ORDER BY sequence`, organization, id, limit)
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

func (p *Database) AppendMessage(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	id uuid.UUID, said conversation.NewMessage,
) (conversation.Message, error) {
	return audited(ctx, p, principal, organization, audit.ActionConversationMessage,
		func(ctx context.Context, transaction pgx.Tx) (
			conversation.Message, audit.Target, audit.Detail, error,
		) {
			written, err := appendMessage(ctx, transaction, organization, id, said, conversation.DefaultIncidentWindowLead)
			if err != nil {
				return conversation.Message{}, audit.Target{}, nil, err
			}
			return written,
				audit.Target{Kind: audit.TargetConversation, ID: id.String()},
				audit.Detail{"sequence": written.Sequence}, nil
		})
}

type acceptedMessage struct {
	message conversation.Message
	turn    conversation.Turn
	opened  bool
}

func (p *Database) AppendMessageAndOpenTurn(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	id uuid.UUID, said conversation.NewMessage, lead time.Duration, maxPending int,
) (conversation.Message, conversation.Turn, bool, error) {

	accepted, err := audited(ctx, p, principal, organization, audit.ActionConversationMessage,
		func(ctx context.Context, transaction pgx.Tx) (
			acceptedMessage, audit.Target, audit.Detail, error,
		) {
			if err := reserveWaitingInvestigations(ctx, transaction, organization, maxPending, 1); err != nil {
				if errors.Is(err, ErrInvestigationCapacity) {
					return acceptedMessage{}, audit.Target{}, nil, conversation.ErrQueueFull
				}
				return acceptedMessage{}, audit.Target{}, nil, err
			}
			written, err := appendMessage(ctx, transaction, organization, id, said, lead)
			if err != nil {
				return acceptedMessage{}, audit.Target{}, nil, err
			}
			turn, opened, err := openTurn(ctx, transaction, organization, id, lead)
			if err != nil {
				return acceptedMessage{}, audit.Target{}, nil, err
			}
			return acceptedMessage{message: written, turn: turn, opened: opened},
				audit.Target{Kind: audit.TargetConversation, ID: id.String()},
				audit.Detail{"sequence": written.Sequence}, nil
		})
	return accepted.message, accepted.turn, accepted.opened, err
}

func appendMessage(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	id uuid.UUID, said conversation.NewMessage, lead time.Duration,
) (conversation.Message, error) {
	if said.Role == conversation.RolePerson {
		if err := reserveQueuedMessage(ctx, transaction, organization); err != nil {
			return conversation.Message{}, err
		}
	}
	state, err := lockConversation(ctx, transaction, organization, id)
	if err != nil {
		return conversation.Message{}, err
	}
	if state != conversation.StateOpen {
		return conversation.Message{}, conversation.ErrClosed
	}
	window, err := acceptedWindow(ctx, transaction, organization, id, said.Window, lead)
	if err != nil {
		return conversation.Message{}, err
	}
	row := transaction.QueryRow(ctx, `
		INSERT INTO conversation_message (conversation_id, org_id, sequence, role,
		                                  actor_kind, actor_id, actor_display, text, window_from, window_until)
		SELECT $1, $2,
		       coalesce((SELECT max(sequence) FROM conversation_message
		                  WHERE org_id = $2 AND conversation_id = $1), 0) + 1,
		       $3, $4, $5, $6, $7, $8, $9
		RETURNING sequence, role, actor_kind, actor_id, actor_display, text, source_reference,
		          investigation_id, created_at, window_from, window_until`,
		id, organization, int16(said.Role), int16(said.ActorKind),
		said.ActorID, said.ActorDisplay, said.Text, window.From, window.Until)
	written, err := scanMessage(row)
	if err != nil {
		return conversation.Message{}, fmt.Errorf("appending a message: %w", err)
	}
	if _, err := transaction.Exec(ctx, `
		UPDATE conversation SET last_activity_at = now()
		 WHERE conversation_id = $1 AND org_id = $2`, id, organization); err != nil {
		return conversation.Message{}, fmt.Errorf("stamping a conversation: %w", err)
	}
	return written, nil
}

func (p *Database) WaitingTurns(
	ctx context.Context, organization uuid.UUID,
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
		organization).Scan(&waiting); err != nil {
		return 0, fmt.Errorf("counting waiting turns: %w", err)
	}
	return waiting, nil
}

func (p *Database) OpenTurn(
	ctx context.Context, organization uuid.UUID, id uuid.UUID,
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

func (p *Database) DrainConversation(
	ctx context.Context, organization uuid.UUID, id uuid.UUID,
	lead time.Duration, maxPending int,
) (bool, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return false, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if err = reserveWaitingInvestigations(ctx, transaction, organization, maxPending, 1); err != nil {
		if errors.Is(err, ErrInvestigationCapacity) {
			return false, conversation.ErrQueueFull
		}
		return false, err
	}
	_, opened, err := openTurn(ctx, transaction, organization, id, lead)
	if err != nil || !opened {
		return false, err
	}
	if err = transaction.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

func (p *Database) DrainQueuedConversation(
	ctx context.Context, lead time.Duration, maxPending int,
) (bool, error) {
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var organization uuid.UUID
	var conversationID uuid.UUID
	err = transaction.QueryRow(ctx, `
		SELECT conversation.org_id, conversation.conversation_id
		  FROM conversation
		  JOIN conversation_message message
		    ON message.org_id = conversation.org_id
		   AND message.conversation_id = conversation.conversation_id
		   AND message.investigation_id IS NULL AND message.role = 1
		 WHERE conversation.state = 1
		   AND NOT EXISTS (
		       SELECT 1 FROM investigation
		        WHERE investigation.org_id = conversation.org_id
		          AND investigation.conversation_id = conversation.conversation_id
		          AND investigation.status = 1)
		   AND ($1 <= 0 OR (
		       SELECT count(*) FROM investigation waiting
		        WHERE waiting.org_id = conversation.org_id
		          AND waiting.status = 1 AND waiting.lease_worker = '') < $1)
		 ORDER BY message.created_at, conversation.conversation_id
		 LIMIT 1`, maxPending).Scan(&organization, &conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("finding queued Conversation work: %w", err)
	}
	if err = reserveWaitingInvestigations(ctx, transaction, organization, maxPending, 1); err != nil {
		if errors.Is(err, ErrInvestigationCapacity) {
			return false, nil
		}
		return false, err
	}
	_, opened, err := openTurn(ctx, transaction, organization, conversationID, lead)
	if err != nil || !opened {
		return false, err
	}
	if err = transaction.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

func openTurn(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	id uuid.UUID, lead time.Duration,
) (conversation.Turn, bool, error) {
	var (
		state      int16
		incidentID *uuid.UUID
		subject    string
	)
	if err := transaction.QueryRow(ctx, `
		SELECT state, incident_id, subject
		  FROM conversation
		 WHERE conversation_id = $1 AND org_id = $2
		   FOR UPDATE`, id, organization).Scan(
		&state, &incidentID, &subject); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return conversation.Turn{}, false, conversation.ErrUnknown
		}
		return conversation.Turn{}, false, fmt.Errorf("locking a conversation: %w", err)
	}
	if conversation.State(state) != conversation.StateOpen {
		return conversation.Turn{}, false, conversation.ErrClosed
	}
	var running bool
	if err := transaction.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM investigation
		 WHERE org_id = $1 AND conversation_id = $2 AND status = 1)`,
		organization, id).Scan(&running); err != nil {
		return conversation.Turn{}, false, fmt.Errorf("checking active turn: %w", err)
	}
	if running {
		return conversation.Turn{}, false, nil
	}

	question, lastSequence, opener, err := queuedQuestion(ctx, transaction, organization, id)
	if err != nil {
		return conversation.Turn{}, false, err
	}
	if lastSequence == 0 {
		return conversation.Turn{}, false, nil
	}

	var from, until time.Time
	if err = transaction.QueryRow(ctx, `SELECT window_from, window_until FROM conversation_message
		WHERE org_id = $1 AND conversation_id = $2 AND investigation_id IS NULL AND role = 1
		ORDER BY sequence LIMIT 1`, organization, id).Scan(&from, &until); err != nil {
		return conversation.Turn{}, false, err
	}

	investigationID := uuid.New()
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
		investigationID, organization, incidentID, question, subject, from, until,
		id, opener).Scan(&ordinal, &createdAt)
	if err != nil {
		return conversation.Turn{}, false, fmt.Errorf("opening a turn: %w", err)
	}

	if _, err := transaction.Exec(ctx, `
		UPDATE conversation_message
		   SET investigation_id = $1
		 WHERE org_id = $2 AND conversation_id = $3 AND investigation_id IS NULL
		   AND role = 1 AND sequence <= $4`,
		investigationID, organization, id, lastSequence); err != nil {
		return conversation.Turn{}, false, fmt.Errorf("attaching queued messages: %w", err)
	}

	return conversation.Turn{
		InvestigationID: investigationID,
		Ordinal:         ordinal,
		Status:          investigationStatusWord(int16(investigation.StatusRunning)),
		CreatedAt:       createdAt,
	}, true, nil
}

func queuedQuestion(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	id uuid.UUID,
) (string, int64, string, error) {
	rows, err := transaction.Query(ctx, `
		SELECT text, sequence, actor_id
		  FROM conversation_message
		 WHERE org_id = $1 AND conversation_id = $2 AND investigation_id IS NULL
		   AND role = 1
		 ORDER BY sequence LIMIT $3`, organization, id, maxQueuedMessages)
	if err != nil {
		return "", 0, "", fmt.Errorf("reading queued messages: %w", err)
	}
	defer rows.Close()

	question := ""
	var lastSequence int64
	var actor string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text, &lastSequence, &actor); err != nil {
			return "", 0, "", fmt.Errorf("scanning a queued message: %w", err)
		}
		if question != "" {
			question += "\n"
		}
		question += text
	}
	if err := rows.Err(); err != nil {
		return "", 0, "", fmt.Errorf("reading queued messages: %w", err)
	}
	return boundedRunes(question, maxQuestionLength), lastSequence, actor, nil
}

func turnWindow(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	incidentID *uuid.UUID, lead time.Duration,
) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	if incidentID == nil {
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
		*incidentID, organization).Scan(
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

func lockConversation(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	id uuid.UUID,
) (conversation.State, error) {
	var state int16
	if err := transaction.QueryRow(ctx, `
		SELECT state
		  FROM conversation
		 WHERE conversation_id = $1 AND org_id = $2
		   FOR UPDATE`, id, organization).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, conversation.ErrUnknown
		}
		return 0, fmt.Errorf("locking a conversation: %w", err)
	}
	return conversation.State(state), nil
}

const maxQuestionLength = 1024

func boundedRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

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
		from, until     *time.Time
	)
	if err := row.Scan(&message.Sequence, &role, &actorKind, &message.ActorID,
		&message.ActorDisplay, &message.Text, &message.SourceReference, &investigationID,
		&message.CreatedAt, &from, &until); err != nil {
		return conversation.Message{}, fmt.Errorf("scanning a message: %w", err)
	}
	message.Role = conversation.Role(role)
	message.ActorKind = conversation.ActorKind(actorKind)
	if investigationID != nil {
		message.InvestigationID = *investigationID
	}
	if from != nil {
		message.WindowFrom, message.WindowUntil = *from, *until
	}
	return message, nil
}
