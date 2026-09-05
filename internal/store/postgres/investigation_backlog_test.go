package storage_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestCreateInvestigationEnforcesBacklogAtomically(t *testing.T) {
	database, organization, _ := twoOrganizationsInOneDatabase(t)
	principal := ownerOf(t, organization)
	wanted := investigation.NewInvestigation{
		Question: "what changed?", Subject: "service",
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(), CreatedBy: principal.ID(),
	}

	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, err := database.CreateInvestigation(context.Background(), principal, organization, wanted, 1)
			errorsSeen <- err
		}()
	}
	ready.Wait()
	close(start)

	accepted, refused := 0, 0
	for range 2 {
		err := <-errorsSeen
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, investigation.ErrQueueFull):
			refused++
		default:
			t.Fatalf("CreateInvestigation: %v", err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("accepted=%d refused=%d, want one of each", accepted, refused)
	}
}

func TestConversationAndDirectIngressShareOneAtomicBacklog(t *testing.T) {
	database, organization, _ := twoOrganizationsInOneDatabase(t)
	principal := ownerOf(t, organization)
	chat := openConversation(t, database, organization, "service")
	wanted := investigation.NewInvestigation{
		Question: "what changed?", Subject: "service",
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(), CreatedBy: principal.ID(),
	}

	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	go func() {
		ready.Done()
		<-start
		_, err := database.CreateInvestigation(context.Background(), principal, organization, wanted, 1)
		errorsSeen <- err
	}()
	go func() {
		ready.Done()
		<-start
		_, _, _, err := database.AppendMessageAndOpenTurn(context.Background(), principal,
			organization, chat.ID, conversation.NewMessage{
				Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
				ActorID: principal.ID(), Text: "please investigate",
			}, time.Hour, 1)
		errorsSeen <- err
	}()
	ready.Wait()
	close(start)

	accepted, refused := 0, 0
	for range 2 {
		err := <-errorsSeen
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, investigation.ErrQueueFull), errors.Is(err, conversation.ErrQueueFull):
			refused++
		default:
			t.Fatalf("opening work: %v", err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("accepted=%d refused=%d, want one of each", accepted, refused)
	}
}

func TestConversationDrainDoesNotExceedBacklog(t *testing.T) {
	database, organization, _ := twoOrganizationsInOneDatabase(t)
	principal := ownerOf(t, organization)
	chat := openConversation(t, database, organization, "service")
	if _, err := database.AppendMessage(context.Background(), principal, organization, chat.ID,
		conversation.NewMessage{Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
			ActorID: principal.ID(), Text: "first"}); err != nil {
		t.Fatal(err)
	}
	turn, opened, err := database.OpenTurn(context.Background(), organization, chat.ID, time.Hour)
	if err != nil || !opened {
		t.Fatalf("opening first turn: opened=%t err=%v", opened, err)
	}
	if _, _, claimed, err := database.ClaimInvestigation(context.Background(), aClaim("worker")); err != nil || !claimed {
		t.Fatalf("claiming first turn: claimed=%t err=%v", claimed, err)
	}
	if _, err := database.AppendMessage(context.Background(), principal, organization, chat.ID,
		conversation.NewMessage{Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
			ActorID: principal.ID(), Text: "follow up"}); err != nil {
		t.Fatal(err)
	}
	if err := database.ConcludeInvestigation(context.Background(), organization,
		turn.InvestigationID, investigation.Conclusion{Summary: "done"}, "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateInvestigation(context.Background(), principal, organization,
		investigation.NewInvestigation{Question: "other", Subject: "other",
			WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(), CreatedBy: principal.ID()}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DrainConversation(context.Background(), organization, chat.ID, time.Hour, 1); !errors.Is(err, conversation.ErrQueueFull) {
		t.Fatalf("DrainConversation error = %v, want queue full", err)
	}
	if _, _, claimed, err := database.ClaimInvestigation(context.Background(), aClaim("other-worker")); err != nil || !claimed {
		t.Fatalf("claiming queued work: claimed=%t err=%v", claimed, err)
	}
	if drained, err := database.DrainQueuedConversation(context.Background(), time.Hour, 1); err != nil || !drained {
		t.Fatalf("retrying durable drain: drained=%t err=%v", drained, err)
	}
	waiting, err := database.WaitingTurns(context.Background(), organization)
	if err != nil || waiting != 1 {
		t.Fatalf("waiting=%d err=%v, want the follow-up Investigation", waiting, err)
	}
}
