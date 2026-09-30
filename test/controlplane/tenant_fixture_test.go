package controlplane

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
)

func (p *integrationPlane) switchOrganization(t *testing.T, organization string) func() {
	t.Helper()
	ctx := context.Background()
	ensureTestOrganization(t, p.dsn, organization)
	database, err := pgx.Connect(ctx, p.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(ctx) })
	var user, original uuid.UUID
	if err := database.QueryRow(ctx, `SELECT membership.user_id, membership.org_id
		FROM organization_membership AS membership JOIN session USING (user_id)
		WHERE session.credential_digest = $1`, session.Digest(session.Token(p.sessionCookie))).
		Scan(&user, &original); err != nil {
		t.Fatal(err)
	}
	move := func(from, to uuid.UUID) {
		t.Helper()
		tag, err := database.Exec(ctx, `UPDATE organization_membership SET org_id = $3
			WHERE org_id = $1 AND user_id = $2`, from, user, to)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("moving fixture Membership: rows=%d err=%v", tag.RowsAffected(), err)
		}
	}
	target := uuid.MustParse(organization)
	move(original, target)
	return func() { move(target, original) }
}
