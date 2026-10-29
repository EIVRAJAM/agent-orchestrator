package chat_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func seedRestartedQueue(t *testing.T, st *sqlite.Store) (domain.ConversationRecord, string, string) {
	t.Helper()
	ctx := context.Background()
	rec, found, err := st.GetSession(ctx, testSession)
	if err != nil || !found {
		t.Fatalf("GetSession: found=%v err=%v", found, err)
	}
	rec.Metadata.ProviderConversationID = "thread-live"
	rec.Metadata.ControllerGeneration = "gen-old"
	if err := st.UpdateSession(ctx, rec); err != nil {
		t.Fatalf("seed session provider: %v", err)
	}
	conversation, err := st.CreateConversation(
		ctx, "conversation-live-hold", domain.ConversationScopeSession,
		testProject, testSession, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	for _, id := range []struct {
		turn, text string
	}{
		{"turn-live-head", "live head"},
		{"turn-live-next", "live next"},
	} {
		created, err := st.AppendUserMessage(ctx, conversation.ID, testSession, "gen-old",
			domain.ConversationMessage{
				ID: "message-" + id.turn, Text: id.text, Origin: domain.MessageOriginHuman,
				ClientMessageID: "client-" + id.turn,
			}, id.turn, time.Now().UTC())
		if err != nil || !created {
			t.Fatalf("AppendUserMessage %s: created=%v err=%v", id.turn, created, err)
		}
	}
	if _, err := st.ClaimChatControllerGenerationAndAdoptQueued(ctx, domain.ChatControllerClaim{
		SessionID:          testSession,
		ConversationID:     conversation.ID,
		ExpectedGeneration: "gen-old",
		Generation:         "gen-live",
		AdoptQueued:        true,
		RecoveryRequired:   true,
		Now:                time.Now().UTC(),
	}); err != nil {
		t.Fatalf("claim/adopt: %v", err)
	}
	claimed, err := st.ClaimQueuedTurnForResume(ctx, conversation.ID, "turn-live-head", "gen-live", time.Now().UTC())
	if err != nil || !claimed {
		t.Fatalf("claim head: claimed=%v err=%v", claimed, err)
	}
	if err := st.BindClaimedEagerTurn(ctx, conversation.ID, testSession, "", "gen-live", "turn-live-head", "provider-live-head", time.Now().UTC()); err != nil {
		t.Fatalf("bind live head: %v", err)
	}
	return conversation, "turn-live-head", "turn-live-next"
}

func newLiveReconnectService(t *testing.T, st *sqlite.Store, conv ports.ChatConversation) *chatsvc.Service {
	t.Helper()
	var ids int
	svc := chatsvc.New(chatsvc.Options{
		Store: st, Sessions: st, Reader: fullSnapshotReader(st),
		Drivers: fakeRegistry{driver: fakeDriver{conv: conv}},
		Log:     slog.New(slog.DiscardHandler),
		NewID:   func() string { ids++; return fmt.Sprintf("live-reconnect-%d", ids) },
		Now:     func() time.Time { return time.Now().UTC() },
	})
	t.Cleanup(func() { svc.StopAll(context.Background()) })
	return svc
}

// A durable hold must survive live reconnect even when the restored controller
// has a running turn and other queued work. Completing that turn must not drain
// the preserved next head.
func TestLiveReconnectRestoresQueueRecoveryHold(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	_, headID, nextID := seedRestartedQueue(t, st)
	provider := &liveReconnectedConversation{nativeHistoryConversation: &nativeHistoryConversation{
		fakeConversation: newFakeConversation(),
		err:              ports.ErrChatHistoryUnsettled,
	}}
	provider.providerConversationID = "thread-live"
	svc := newLiveReconnectService(t, st, provider)
	ctrl, err := svc.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		WorkspacePath: t.TempDir(), ProviderConversationID: "thread-live",
	})
	if err != nil {
		t.Fatalf("live reconnect Start: %v", err)
	}
	if ctrl.Generation() != "gen-live" {
		t.Fatalf("live reconnect generation = %q, want reused gen-live", ctrl.Generation())
	}
	before, err := svc.Snapshot(ctx, testSession)
	if err != nil {
		t.Fatalf("snapshot before completion: %v", err)
	}
	if before.Queue == nil || before.Queue.State != chatsvc.QueueStateWaitingForActiveTurn ||
		before.Queue.HeadTurnID != nextID {
		t.Fatalf("queue before live completion = %+v, want the queued next head behind running", before.Queue)
	}

	provider.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-live-head",
		TurnState: domain.TurnStateCompleted,
	})
	after := awaitServiceQueue(t, svc, func(q *chatsvc.QueueState) bool {
		return q != nil && q.State == chatsvc.QueueStateControllerRecoveryRequired && q.HeadTurnID == nextID
	})
	if after.PendingCount != 1 || after.HeadTurnID != nextID {
		t.Fatalf("queue after live completion = %+v, want retained next head", after)
	}
	if got := provider.sentTexts(); len(got) != 0 {
		t.Fatalf("live completion drained queued work: %v", got)
	}
	if headID == nextID {
		t.Fatal("test did not create distinct heads")
	}

	result, err := svc.ResumeHeldQueue(ctx, testSession, nextID, "gen-live")
	if err != nil {
		t.Fatalf("ResumeHeldQueue: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeResumed || result.TurnID != nextID {
		t.Fatalf("resume = %+v, want exactly the held next head", result)
	}
	if got := provider.sentTexts(); len(got) != 1 || got[0] != "live next" {
		t.Fatalf("provider received %v, want only the next head", got)
	}
}

// The same hold must remain after a second live reconnect; a repeated restart is
// not an authorized resume.
func TestRepeatedLiveReconnectKeepsQueueRecoveryHold(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	_, _, nextID := seedRestartedQueue(t, st)

	for i := 0; i < 2; i++ {
		provider := &liveReconnectedConversation{nativeHistoryConversation: &nativeHistoryConversation{
			fakeConversation: newFakeConversation(),
			err:              ports.ErrChatHistoryUnsettled,
		}}
		provider.providerConversationID = "thread-live"
		svc := newLiveReconnectService(t, st, provider)
		if _, err := svc.Start(ctx, chatsvc.StartConfig{
			SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
			WorkspacePath: t.TempDir(), ProviderConversationID: "thread-live",
		}); err != nil {
			t.Fatalf("live reconnect %d Start: %v", i+1, err)
		}
		snapshot, err := svc.Snapshot(ctx, testSession)
		if err != nil {
			t.Fatalf("live reconnect %d snapshot: %v", i+1, err)
		}
		if snapshot.Queue == nil ||
			(snapshot.Queue.State != chatsvc.QueueStateWaitingForActiveTurn &&
				snapshot.Queue.State != chatsvc.QueueStateControllerRecoveryRequired) ||
			snapshot.Queue.HeadTurnID != nextID {
			t.Fatalf("live reconnect %d queue = %+v, want a held next head", i+1, snapshot.Queue)
		}
		if got := provider.sentTexts(); len(got) != 0 {
			t.Fatalf("live reconnect %d drained held queue: %v", i+1, got)
		}
		svc.StopAll(ctx)
		// The first reconnect leaves the restored bound turn running in SQLite;
		// the second reconnect must restore it again rather than treating the
		// queued head as ordinary work.
	}
}

func awaitServiceQueue(t *testing.T, svc *chatsvc.Service, pred func(*chatsvc.QueueState) bool) *chatsvc.QueueState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := svc.Snapshot(context.Background(), testSession)
		if err == nil && pred(snapshot.Queue) {
			return snapshot.Queue
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("service queue never satisfied condition")
	return nil
}
