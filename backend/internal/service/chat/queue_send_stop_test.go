package chat_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type appendFenceHookStore struct {
	chatsvc.Store
	beforeAppend func()
	afterAppend  func()
}

func (s *appendFenceHookStore) AppendUserMessageFenced(
	ctx context.Context,
	conversationID string,
	session domain.SessionID,
	generation, expectedStopEpisode string,
	msg domain.ConversationMessage,
	turnID string,
	now time.Time,
) (bool, error) {
	if s.beforeAppend != nil {
		s.beforeAppend()
	}
	created, err := s.Store.AppendUserMessageFenced(
		ctx, conversationID, session, generation, expectedStopEpisode, msg, turnID, now)
	if s.afterAppend != nil {
		s.afterAppend()
	}
	return created, err
}

func newAppendFenceHarness(t *testing.T) (*harness, *appendFenceHookStore) {
	t.Helper()
	hook := &appendFenceHookStore{}
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		hook.Store = st
		return hook
	})
	return h, hook
}

// Stop wins before the append. The Send must fail its transaction, create no
// row, and never call the provider.
func TestSendPausedBeforeAppendLosesToTermination(t *testing.T) {
	h, hook := newAppendFenceHarness(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	hook.beforeAppend = func() {
		close(entered)
		<-release
	}

	result := make(chan error, 1)
	go func() {
		_, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "must not be inserted", ClientMessageID: "send-stop-before-append",
		})
		result <- err
	}()
	<-entered
	if err := h.svc.Stop(context.Background(), testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, store.ErrChatIntakeClosed) {
		t.Fatalf("Send after losing append race = %v, want ErrChatIntakeClosed", err)
	}
	if got := h.conv.sendCallCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	snapshot, err := h.st.LoadConversationSnapshot(context.Background(), h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snapshot.Turns) != 0 || len(snapshot.Messages) != 0 {
		t.Fatalf("rejected Send left rows: turns=%d messages=%d", len(snapshot.Turns), len(snapshot.Messages))
	}
}

// Append wins, then Stop runs. The row is inside the durable cutoff, becomes
// interrupted, and the subsequent dispatch must be fenced before the provider.
func TestAppendThatWinsBeforeStopIsCancelledAndNeverDispatched(t *testing.T) {
	h, hook := newAppendFenceHarness(t)
	appended := make(chan struct{})
	release := make(chan struct{})
	hook.afterAppend = func() {
		close(appended)
		<-release
	}

	result := make(chan error, 1)
	go func() {
		_, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "inserted before stop", ClientMessageID: "send-stop-after-append",
		})
		result <- err
	}()
	<-appended
	if err := h.svc.Stop(context.Background(), testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("Send dispatched a row cancelled by the Stop cutoff")
	}
	if got := h.conv.sendCallCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	snapshot, err := h.st.LoadConversationSnapshot(context.Background(), h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snapshot.Turns) != 1 {
		t.Fatalf("turns after append/stop = %d, want one durable row", len(snapshot.Turns))
	}
	if snapshot.Turns[0].State != domain.TurnStateInterrupted {
		t.Fatalf("turn state = %q, want interrupted", snapshot.Turns[0].State)
	}
}
