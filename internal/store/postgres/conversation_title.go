package storage

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
)

func (p *Database) RenameConversation(ctx context.Context, principal authz.Principal, org, id uuid.UUID, title string) (conversation.Conversation, error) {
	if !principal.HavePermission(authz.ConversationWrite) {
		return conversation.Conversation{}, ErrNotAMember
	}
	return audited(ctx, p, principal, org, audit.ActionConversationRenamed, func(ctx context.Context, tx pgx.Tx) (
		conversation.Conversation, audit.Target, audit.Detail, error,
	) {
		found, err := scanConversation(tx.QueryRow(ctx, `UPDATE conversation SET title = $3, custom_title = true
			WHERE org_id = $1 AND conversation_id = $2 AND source = 'web' AND deleted_at IS NULL
			RETURNING `+conversationColumns, org, id, title))
		if errors.Is(err, pgx.ErrNoRows) {
			err = conversation.ErrUnknown
		}
		return found, audit.Target{Kind: audit.TargetConversation, ID: id.String()}, nil, err
	})
}

func setChatFallbackTitle(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, sequence int64) (string, error) {
	var text string
	err := tx.QueryRow(ctx, `
		SELECT m.text FROM conversation c
		  JOIN conversation_message m USING (org_id, conversation_id)
		 WHERE c.org_id = $1 AND c.conversation_id = $2 AND c.source = 'web'
		   AND NOT c.custom_title AND c.title = '' AND m.sequence = $3
		   AND NOT EXISTS (SELECT 1 FROM agent_execution e
		        WHERE e.org_id = $1 AND e.conversation_id = $2
		          AND e.message_sequence < $3 AND e.status = 'completed')`, org, id, sequence).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	fallback := conversation.CleanAutomaticTitle(text)
	if fallback == "" {
		fallback = "New conversation"
	}
	_, err = tx.Exec(ctx, `UPDATE conversation SET title = $3
		WHERE org_id = $1 AND conversation_id = $2`, org, id, fallback)
	return fallback, err
}

func (p *Database) ChatTitleIsCurrent(ctx context.Context, org, id uuid.UUID, fallback string) (bool, error) {
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return false, err
	}
	var eligible bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation
		WHERE org_id = $1 AND conversation_id = $2 AND source = 'web' AND deleted_at IS NULL AND NOT custom_title AND title = $3)`,
		org, id, fallback).Scan(&eligible)
	return eligible, err
}

func (p *Database) SetAutomaticChatTitle(ctx context.Context, org, id uuid.UUID, fallback, title string) error {
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `UPDATE conversation SET title = $4
		WHERE org_id = $1 AND conversation_id = $2 AND source = 'web' AND deleted_at IS NULL AND NOT custom_title AND title = $3`,
		org, id, fallback, title)
	return err
}
