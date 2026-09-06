package storage_test

import (
	"context"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestExpiredLeaseCannotBeRenewedBeforeRecovery(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	ctx := context.Background()
	conversation := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, conversation.ID, "what changed?")
	turn, took, err := database.OpenTurn(ctx, organization, conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	claim := aClaim("original-worker")
	if _, _, took, err = database.ClaimInvestigation(ctx, claim); err != nil || !took {
		t.Fatalf("claiming: took=%v err=%v", took, err)
	}
	expireInvestigationLease(t, database, organization, turn.InvestigationID)
	claim.Token = claimToken(t, database, organization, turn.InvestigationID)
	if held, err := database.Heartbeat(ctx, organization, turn.InvestigationID, claim); err != nil || held {
		t.Fatalf("expired lease renewed: held=%v err=%v", held, err)
	}
	if recovered, err := database.RecoverStale(ctx, investigation.RecoveryReason, 10); err != nil || recovered != 1 {
		t.Fatalf("recovering expired lease: recovered=%d err=%v", recovered, err)
	}
	found, err := database.Investigation(ctx, organization, turn.InvestigationID)
	if err != nil || found.Status != investigation.StatusFailed {
		t.Fatalf("recovered status=%v err=%v", found.Status, err)
	}
}
