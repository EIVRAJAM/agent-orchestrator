package chat_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func seedBoundaryRecovery(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	st := openStore(t)
	ctx := context.Background()
	if err := st.ClaimChatControllerGeneration(ctx, testSession, "gen-old"); err != nil {
		t.Fatalf("seed generation: %v", err)
	}
	conversation, err := st.CreateConversation(
		ctx, "conversation-boundary", domain.ConversationScopeSession,
		testProject, testSession, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	for _, item := range []struct {
		turn, text, state string
	}{
		{"turn-boundary-running", "running predecessor", "running"},
		{"turn-boundary-queued", "queued predecessor", "queued"},
	} {
		created, err := st.AppendUserMessage(ctx, conversation.ID, testSession, "gen-old",
			domain.ConversationMessage{
				ID: "message-" + item.turn, Text: item.text, Origin: domain.MessageOriginHuman,
				ClientMessageID: "client-" + item.turn,
			}, item.turn, time.Now().UTC())
		if err != nil || !created {
			t.Fatalf("AppendUserMessage %s: created=%v err=%v", item.turn, created, err)
		}
		if item.state == "running" {
			if err := st.MarkTurnDispatching(ctx, item.turn); err != nil {
				t.Fatalf("mark running: %v", err)
			}
		}
	}
	return st, conversation.ID
}

func TestProviderBoundaryAdoptsQueuedAndHoldsForManualResume(t *testing.T) {
	st, _ := seedBoundaryRecovery(t)
	ctx := context.Background()
	provider := newFakeConversation()
	svc := chatsvc.New(chatsvc.Options{
		Store: st, Sessions: st, Reader: fullSnapshotReader(st),
		Drivers: fakeRegistry{driver: fakeDriver{conv: provider}},
		Log:     slog.New(slog.DiscardHandler),
		NewID: func() string {
			return fmt.Sprintf("boundary-%d", time.Now().UnixNano())
		},
		Now: func() time.Time { return time.Now().UTC() },
	})
	t.Cleanup(func() { svc.StopAll(context.Background()) })

	ctrl, err := svc.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		WorkspacePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("boundary Start: %v", err)
	}
	if got := provider.sentTexts(); len(got) != 0 {
		t.Fatalf("boundary start dispatched preserved queue: %v", got)
	}
	snapshot, err := svc.Snapshot(ctx, testSession)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Queue == nil || snapshot.Queue.State != chatsvc.QueueStateControllerRecoveryRequired {
		t.Fatalf("queue = %+v, want controller_recovery_required", snapshot.Queue)
	}
	if snapshot.Queue.HeadTurnID != "turn-boundary-queued" {
		t.Fatalf("head = %q, want queued predecessor", snapshot.Queue.HeadTurnID)
	}
	conversation, err := st.ConversationForSession(ctx, testSession)
	if err != nil {
		t.Fatalf("ConversationForSession: %v", err)
	}
	if conversation.ActiveBranchID == conversation.ID+":root" {
		t.Fatal("provider fallback did not activate a new boundary")
	}
	running, err := st.TurnByID(ctx, "turn-boundary-running")
	if err != nil || running.State != domain.TurnStateFailed {
		t.Fatalf("orphaned running = %+v err=%v, want failed", running, err)
	}
	queued, err := st.TurnByID(ctx, "turn-boundary-queued")
	if err != nil || queued.State != domain.TurnStateQueued {
		t.Fatalf("queued = %+v err=%v, want preserved", queued, err)
	}
	result, err := svc.ResumeHeldQueue(ctx, testSession, "turn-boundary-queued", ctrl.Generation())
	if err != nil || result.Outcome != chatsvc.QueueResumeResumed {
		t.Fatalf("resume = %+v err=%v, want resumed", result, err)
	}
	if got := provider.sentTexts(); len(got) != 1 || got[0] != "queued predecessor" {
		t.Fatalf("provider received %v, want only the preserved head", got)
	}
}

type failBoundaryStore struct {
	*sqlite.Store
}

func (s *failBoundaryStore) CreateAndActivateConversationBranchAndAdoptQueued(
	context.Context, domain.SessionID, domain.ConversationBranch, string, string, time.Time,
) error {
	return errors.New("injected provider-boundary publication failure")
}

func TestProviderBoundaryFailureLeavesBranchGenerationAndQueueUntouched(t *testing.T) {
	st, conversationID := seedBoundaryRecovery(t)
	ctx := context.Background()
	wrapped := &failBoundaryStore{Store: st}
	svc := chatsvc.New(chatsvc.Options{
		Store: wrapped, Sessions: st, Reader: fullSnapshotReader(st),
		Drivers: fakeRegistry{driver: fakeDriver{conv: newFakeConversation()}},
		Log:     slog.New(slog.DiscardHandler),
		NewID:   func() string { return fmt.Sprintf("boundary-fail-%d", time.Now().UnixNano()) },
		Now:     func() time.Time { return time.Now().UTC() },
	})
	t.Cleanup(func() { svc.StopAll(context.Background()) })
	if _, err := svc.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		WorkspacePath: t.TempDir(),
	}); err == nil {
		t.Fatal("Start succeeded despite injected boundary failure")
	}
	conversation, err := st.ConversationForSession(ctx, testSession)
	if err != nil || conversation.ID != conversationID {
		t.Fatalf("conversation after failure = %+v err=%v", conversation, err)
	}
	if conversation.ActiveBranchID != conversation.ID+":root" {
		t.Fatalf("active branch after rollback = %q, want original root", conversation.ActiveBranchID)
	}
	rec, found, err := st.GetSession(ctx, testSession)
	if err != nil || !found || rec.Metadata.ControllerGeneration != "gen-old" {
		t.Fatalf("generation after rollback = %+v found=%v err=%v", rec.Metadata, found, err)
	}
	if got, err := st.TurnByID(ctx, "turn-boundary-queued"); err != nil || got.State != domain.TurnStateQueued {
		t.Fatalf("queued after rollback = %+v err=%v", got, err)
	}
	if got, err := st.TurnByID(ctx, "turn-boundary-running"); err != nil || got.State != domain.TurnStateRunning {
		t.Fatalf("running after rollback = %+v err=%v", got, err)
	}
	facts, err := st.ConversationQueueFacts(ctx, conversationID)
	if err != nil || facts.QueueRecoveryRequired {
		t.Fatalf("hold after rollback = %+v err=%v", facts, err)
	}
}

func TestProviderBoundaryControllerReadyFailureLeavesNoPartialPublication(t *testing.T) {
	st, conversationID := seedBoundaryRecovery(t)
	ctx := context.Background()
	svc := chatsvc.New(chatsvc.Options{
		Store: st, Sessions: st, Reader: fullSnapshotReader(st),
		Drivers: fakeRegistry{driver: fakeDriver{conv: newFakeConversation()}},
		Log:     slog.New(slog.DiscardHandler),
		NewID:   func() string { return fmt.Sprintf("ready-fail-%d", time.Now().UnixNano()) },
		Now:     func() time.Time { return time.Now().UTC() },
	})
	t.Cleanup(func() { svc.StopAll(context.Background()) })
	_, err := svc.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		WorkspacePath: t.TempDir(),
		ControllerReady: func(chatsvc.StartResult) (chatsvc.ControllerCommit, error) {
			return chatsvc.ControllerCommit{}, errors.New("injected controller-ready failure")
		},
	})
	if err == nil {
		t.Fatal("Start succeeded despite injected controller-ready failure")
	}
	conversation, err := st.ConversationForSession(ctx, testSession)
	if err != nil || conversation.ID != conversationID {
		t.Fatalf("conversation after ready failure = %+v err=%v", conversation, err)
	}
	if conversation.ActiveBranchID != conversation.ID+":root" {
		t.Fatalf("active branch after ready failure = %q, want original root", conversation.ActiveBranchID)
	}
	rec, found, err := st.GetSession(ctx, testSession)
	if err != nil || !found || rec.Metadata.ControllerGeneration != "gen-old" {
		t.Fatalf("generation after ready failure = %+v found=%v err=%v", rec.Metadata, found, err)
	}
	if got, err := st.TurnByID(ctx, "turn-boundary-running"); err != nil || got.State != domain.TurnStateRunning {
		t.Fatalf("running after ready failure = %+v err=%v", got, err)
	}
	if got, err := st.TurnByID(ctx, "turn-boundary-queued"); err != nil || got.State != domain.TurnStateQueued {
		t.Fatalf("queued after ready failure = %+v err=%v", got, err)
	}
	facts, err := st.ConversationQueueFacts(ctx, conversationID)
	if err != nil || facts.QueueRecoveryRequired {
		t.Fatalf("hold after ready failure = %+v err=%v", facts, err)
	}
}
