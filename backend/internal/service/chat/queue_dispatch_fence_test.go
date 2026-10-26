package chat_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type dispatchFenceHookStore struct {
	chatsvc.Store
	mu             sync.Mutex
	beforeClaim    func()
	beforeEager    func()
	beforeDeferred func()
	afterDeferred  func()
}

func (s *dispatchFenceHookStore) hook(fn func()) func() {
	if fn == nil {
		return func() {}
	}
	return fn
}

func (s *dispatchFenceHookStore) ClaimQueuedTurnForDispatch(
	ctx context.Context, conversationID string, session domain.SessionID,
	reviewID, generation, turnID string, now time.Time,
) (bool, error) {
	s.mu.Lock()
	hook := s.beforeClaim
	s.mu.Unlock()
	s.hook(hook)()
	return s.Store.ClaimQueuedTurnForDispatch(ctx, conversationID, session, reviewID, generation, turnID, now)
}

func (s *dispatchFenceHookStore) BindPreparedDeferredTurn(
	ctx context.Context, conversationID string, session domain.SessionID,
	reviewID, generation, turnID, providerTurnID string, now time.Time,
) (bool, error) {
	s.mu.Lock()
	before, after := s.beforeDeferred, s.afterDeferred
	s.mu.Unlock()
	s.hook(before)()
	bound, err := s.Store.BindPreparedDeferredTurn(ctx, conversationID, session, reviewID, generation, turnID, providerTurnID, now)
	s.hook(after)()
	return bound, err
}

func (s *dispatchFenceHookStore) BindClaimedEagerTurn(
	ctx context.Context, conversationID string, session domain.SessionID,
	reviewID, generation, turnID, providerTurnID string, now time.Time,
) error {
	s.mu.Lock()
	hook := s.beforeEager
	s.mu.Unlock()
	s.hook(hook)()
	return s.Store.BindClaimedEagerTurn(ctx, conversationID, session, reviewID, generation, turnID, providerTurnID, now)
}

func TestEagerDispatchClaimLosesToStopBeforeProvider(t *testing.T) {
	hook := &dispatchFenceHookStore{}
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		hook.Store = st
		return hook
	})
	entered, release := make(chan struct{}), make(chan struct{})
	hook.beforeClaim = func() { close(entered); <-release }
	result := make(chan error, 1)
	go func() {
		_, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "eager must not dispatch", ClientMessageID: "eager-stop-claim",
		})
		result <- err
	}()
	<-entered
	if err := h.svc.Stop(context.Background(), testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("Send succeeded after losing the eager claim to Stop")
	}
	if got := h.conv.sendCallCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	snapshot, err := h.st.LoadConversationSnapshot(context.Background(), h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].State != domain.TurnStateInterrupted {
		t.Fatalf("turns after eager Stop = %+v, want one interrupted row", snapshot.Turns)
	}
}

func TestDeferredStopBeforeBindDiscardsWithoutStartingProvider(t *testing.T) {
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	startCalls := 0
	discarded := 0
	conv.start = func(string) error { startCalls++; return nil }
	conv.discard = func(string) { discarded++ }
	hook := &dispatchFenceHookStore{}
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		hook.Store = st
		return hook
	})
	entered, release := make(chan struct{}), make(chan struct{})
	hook.beforeDeferred = func() { close(entered); <-release }
	result := make(chan error, 1)
	go func() {
		_, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "deferred must be discarded", ClientMessageID: "deferred-stop-bind",
		})
		result <- err
	}()
	<-entered
	if err := h.svc.Stop(context.Background(), testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, store.ErrTurnDispatchFenced) {
		t.Fatalf("deferred Send error = %v, want ErrTurnDispatchFenced", err)
	}
	if startCalls != 0 {
		t.Fatalf("StartDeferredTurn calls = %d, want 0", startCalls)
	}
	if discarded != 1 {
		t.Fatalf("DiscardDeferredTurn calls = %d, want 1", discarded)
	}
	snapshot, err := h.st.LoadConversationSnapshot(context.Background(), h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snapshot.Turns) != 1 {
		t.Fatalf("turns after deferred Stop = %d, want one", len(snapshot.Turns))
	}
	turn := snapshot.Turns[0]
	if turn.State != domain.TurnStateInterrupted || turn.ProviderTurnID != "" || turn.StartedAt != nil {
		t.Fatalf("deferred turn after Stop = %+v, want interrupted/unbound/unstarted", turn)
	}
}

func TestDeferredBindWinsThenStopUsesRunningSemantics(t *testing.T) {
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	startCalls := 0
	conv.start = func(string) error { startCalls++; return nil }
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store { return st })
	if _, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "deferred bind wins", ClientMessageID: "deferred-bind-wins",
	}); err != nil {
		t.Fatalf("deferred Send: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("StartDeferredTurn calls = %d, want 1", startCalls)
	}
	snapshot, err := h.st.LoadConversationSnapshot(context.Background(), h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].State != domain.TurnStateRunning ||
		snapshot.Turns[0].ProviderTurnID == "" || snapshot.Turns[0].StartedAt == nil {
		t.Fatalf("deferred winner = %+v", snapshot.Turns)
	}
	if err := h.svc.Stop(context.Background(), testSession); err != nil {
		t.Fatalf("Stop after deferred bind: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("StartDeferredTurn called twice: %d", startCalls)
	}
}

// reviewFenceStore pauses the review append transaction so the Stop race is
// deterministic at the durable boundary.
type reviewFenceStore struct {
	*sqlite.Store
	beforeAppend func()
	afterAppend  func()
}

func (s *reviewFenceStore) AppendReviewUserMessageFenced(
	ctx context.Context, conversationID string, session domain.SessionID,
	reviewID, generation, expectedStopEpisode string,
	msg domain.ConversationMessage, turnID string, now time.Time,
) (bool, error) {
	if s.beforeAppend != nil {
		s.beforeAppend()
	}
	created, err := s.Store.AppendReviewUserMessageFenced(
		ctx, conversationID, session, reviewID, generation, expectedStopEpisode, msg, turnID, now)
	if s.afterAppend != nil {
		s.afterAppend()
	}
	return created, err
}

func newReviewFenceService(t *testing.T, hook *reviewFenceStore) (*chatsvc.Service, domain.ConversationOwner, *fakeConversation) {
	t.Helper()
	st := openStore(t)
	now := time.Now().UTC()
	reviewID := "review-fence"
	if err := st.UpsertReview(context.Background(), domain.Review{
		ID: reviewID, SessionID: testSession, ProjectID: testProject,
		Harness: domain.ReviewerCodex, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertReview: %v", err)
	}
	conv := newFakeConversation()
	hook.Store = st
	var ids int
	svc := chatsvc.New(chatsvc.Options{
		Store: hook, Sessions: st, Reader: fullSnapshotReader(st),
		Drivers: fakeRegistry{driver: fakeDriver{conv: conv}},
		Log:     slog.New(slog.DiscardHandler),
		NewID:   func() string { ids++; return fmt.Sprintf("review-fence-id-%d", ids) },
		Now:     func() time.Time { return time.Now().UTC() },
	})
	t.Cleanup(func() { svc.StopAll(context.Background()) })
	owner := domain.ReviewConversationOwner(reviewID)
	_, err := svc.Start(context.Background(), chatsvc.StartConfig{
		Owner: owner, SessionID: testSession, ProjectID: testProject,
		Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("review Start: %v", err)
	}
	return svc, owner, conv
}

func TestReviewStopWinsBeforeAppend(t *testing.T) {
	hook := &reviewFenceStore{}
	svc, owner, conv := newReviewFenceService(t, hook)
	entered, release := make(chan struct{}), make(chan struct{})
	hook.beforeAppend = func() { close(entered); <-release }
	result := make(chan error, 1)
	go func() {
		_, err := svc.SendForOwner(context.Background(), owner, ports.ChatUserMessage{
			Text: "review must not insert", ClientMessageID: "review-stop-before-append",
		})
		result <- err
	}()
	<-entered
	if err := svc.StopForOwner(context.Background(), owner); err != nil {
		t.Fatalf("review Stop: %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, store.ErrChatIntakeClosed) {
		t.Fatalf("review Send error = %v, want ErrChatIntakeClosed", err)
	}
	if got := conv.sendCallCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	snapshot, err := svc.SnapshotForReview(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("review snapshot: %v", err)
	}
	if len(snapshot.Turns) != 0 || len(snapshot.Messages) != 0 {
		t.Fatalf("review Stop left rows: turns=%d messages=%d", len(snapshot.Turns), len(snapshot.Messages))
	}
}

func TestReviewAppendWinsThenStopCancelsAndFencesDispatch(t *testing.T) {
	hook := &reviewFenceStore{}
	svc, owner, conv := newReviewFenceService(t, hook)
	appended, release := make(chan struct{}), make(chan struct{})
	hook.afterAppend = func() { close(appended); <-release }
	result := make(chan error, 1)
	go func() {
		_, err := svc.SendForOwner(context.Background(), owner, ports.ChatUserMessage{
			Text: "review append wins", ClientMessageID: "review-append-wins",
		})
		result <- err
	}()
	<-appended
	if err := svc.StopForOwner(context.Background(), owner); err != nil {
		t.Fatalf("review Stop: %v", err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("review Send dispatched a row cancelled by Stop")
	}
	if got := conv.sendCallCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	snapshot, err := svc.SnapshotForReview(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("review snapshot: %v", err)
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].State != domain.TurnStateInterrupted {
		t.Fatalf("review turns after Stop = %+v, want one interrupted", snapshot.Turns)
	}
}
