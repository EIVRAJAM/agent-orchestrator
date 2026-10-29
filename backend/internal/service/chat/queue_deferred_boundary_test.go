package chat_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestDeferredStopAfterBindBeforeStartDiscardsPreparedTurn(t *testing.T) {
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
	bound, release := make(chan struct{}), make(chan struct{})
	hook.afterDeferred = func() { close(bound); <-release }
	type sendResult struct {
		turn domain.ConversationTurn
		err  error
	}
	result := make(chan sendResult, 1)
	go func() {
		turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "deferred prepared then stopped", ClientMessageID: "deferred-after-bind",
		})
		result <- sendResult{turn: turn, err: err}
	}()
	<-bound

	if created, err := h.st.AppendUserMessage(context.Background(), h.ctrl.ConversationID(), testSession,
		currentGeneration(h), domain.ConversationMessage{
			ID: "message-after-prepared", Origin: domain.MessageOriginHuman,
			Text: "must remain queued", ClientMessageID: "queued-after-prepared",
		}, "turn-after-prepared", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}

	if err := h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated); err != nil {
		t.Fatalf("TerminateForStop: %v", err)
	}
	close(release)
	sent := <-result
	if !errors.Is(sent.err, chatsvc.ErrProviderDispatchStopped) {
		t.Fatalf("Send error = %v, want ErrProviderDispatchStopped", sent.err)
	}
	if startCalls != 0 {
		t.Fatalf("StartDeferredTurn calls = %d, want 0", startCalls)
	}
	if discarded != 1 {
		t.Fatalf("DiscardDeferredTurn calls = %d, want 1", discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "deferred-after-bind"))
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateInterrupted || turn.ProviderTurnID != "" ||
		turn.StartedAt != nil || turn.CompletedAt == nil {
		t.Fatalf("prepared turn after Stop = %+v, want interrupted without prepared identity", turn)
	}
	if got := turnStateByID(t, h, "turn-after-prepared"); got != domain.TurnStateInterrupted {
		t.Fatalf("next queued turn state = %q, want interrupted by Stop", got)
	}
	assertNoDispatchingTurn(t, h)
}

func TestDeferredPreclaimedStopAfterBindBeforeStart(t *testing.T) {
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
	first, second := queueTwoBehindFailingRoot(t, h, conv.emit)
	startCalls, discarded = 0, 0
	bound, release := make(chan struct{}), make(chan struct{})
	hook.afterEager = func() { close(bound); <-release }
	result := make(chan error, 1)
	go func() {
		_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
		result <- err
	}()
	<-bound
	if err := h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated); err != nil {
		t.Fatalf("TerminateForStop: %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, chatsvc.ErrQueueDispatchFailed) {
		t.Fatalf("resume error = %v, want ErrQueueDispatchFailed", err)
	}
	if startCalls != 0 {
		t.Fatalf("StartDeferredTurn calls = %d, want 0", startCalls)
	}
	if discarded != 1 {
		t.Fatalf("DiscardDeferredTurn calls = %d, want 1", discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateInterrupted || turn.ProviderTurnID != "" || turn.StartedAt != nil {
		t.Fatalf("preclaimed turn after Stop = %+v, want interrupted without provider identity", turn)
	}
	if got := turnStateByID(t, h, second.ID); got != domain.TurnStateInterrupted {
		t.Fatalf("next head state = %q, want interrupted by the same Stop cutoff", got)
	}
	assertNoDispatchingTurn(t, h)
}

func TestDeferredStopDuringPrepareDiscardsAndNeverStarts(t *testing.T) {
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	startCalls := 0
	discarded := 0
	conv.start = func(string) error { startCalls++; return nil }
	conv.discard = func(string) { discarded++ }
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store { return st })
	preparing, release := make(chan struct{}), make(chan struct{})
	conv.onSend = func(string) { close(preparing); <-release }
	type prepareResult struct {
		turn domain.ConversationTurn
		err  error
	}
	result := make(chan prepareResult, 1)
	go func() {
		turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "deferred prepare blocked", ClientMessageID: "deferred-during-prepare",
		})
		result <- prepareResult{turn: turn, err: err}
	}()
	<-preparing
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.ctrl.TerminateForStop(ctx, domain.ChatStopReasonSessionTerminated); err != nil {
		t.Fatalf("TerminateForStop waited behind prepare: %v", err)
	}
	close(release)
	prepared := <-result
	if !errors.Is(prepared.err, chatsvc.ErrProviderDispatchStopped) {
		t.Fatalf("Send error = %v, want ErrProviderDispatchStopped", prepared.err)
	}
	if startCalls != 0 {
		t.Fatalf("StartDeferredTurn calls = %d, want 0", startCalls)
	}
	if discarded != 1 {
		t.Fatalf("DiscardDeferredTurn calls = %d, want 1", discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "deferred-during-prepare"))
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateInterrupted || turn.ProviderTurnID != "" || turn.StartedAt != nil {
		t.Fatalf("prepared during Stop = %+v, want interrupted/unbound", turn)
	}
}

func TestDeferredStartWinsAndDoesNotBlockStop(t *testing.T) {
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	started, release := make(chan struct{}), make(chan struct{})
	startCalls, discardCalls := 0, 0
	conv.start = func(string) error { startCalls++; close(started); <-release; return nil }
	conv.discard = func(string) { discardCalls++ }
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store { return st })
	type sendResult struct {
		turn domain.ConversationTurn
		err  error
	}
	result := make(chan sendResult, 1)
	go func() {
		turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "deferred start admitted", ClientMessageID: "deferred-start-wins",
		})
		result <- sendResult{turn: turn, err: err}
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.ctrl.TerminateForStop(ctx, domain.ChatStopReasonSessionTerminated); err != nil {
		t.Fatalf("TerminateForStop waited behind StartDeferredTurn: %v", err)
	}
	close(release)
	sent := <-result
	if sent.err != nil {
		t.Fatalf("admitted deferred start error = %v", sent.err)
	}
	if startCalls != 1 {
		t.Fatalf("StartDeferredTurn calls = %d, want exactly one", startCalls)
	}
	if discardCalls != 0 {
		t.Fatalf("DiscardDeferredTurn calls = %d, want 0 after admitted start", discardCalls)
	}
	turnID := sent.turn.ID
	if turnID == "" {
		turnID = turnIDByClientID(t, h, "deferred-start-wins")
	}
	turn, err := h.st.TurnByID(context.Background(), turnID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.ProviderTurnID == "" {
		t.Fatal("provider-owned turn lost its provider id after Stop")
	}
}

func TestFailedStopDoesNotLeavePhantomAdmissionClosure(t *testing.T) {
	var flaky *failFirstQueueCancellationStore
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		flaky = &failFirstQueueCancellationStore{Store: st}
		return flaky
	})
	ctx := context.Background()
	if created, err := h.st.AppendUserMessage(ctx, h.ctrl.ConversationID(), testSession, currentGeneration(h),
		domain.ConversationMessage{
			ID: "message-survives-failed-stop", Origin: domain.MessageOriginHuman,
			Text: "queue survives failed Stop", ClientMessageID: "survives-failed-stop",
		}, "turn-survives-failed-stop", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}
	beforeEpisode, err := h.st.ConversationStopEpisode(ctx, h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("ConversationStopEpisode before: %v", err)
	}
	if err := h.ctrl.TerminateForStop(ctx, domain.ChatStopReasonSessionTerminated); err == nil {
		t.Fatal("first Stop succeeded despite injected failure")
	}
	afterEpisode, err := h.st.ConversationStopEpisode(ctx, h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("ConversationStopEpisode after: %v", err)
	}
	if afterEpisode != beforeEpisode {
		t.Fatalf("failed Stop wrote episode %q, want %q", afterEpisode, beforeEpisode)
	}
	if got := turnStateByID(t, h, "turn-survives-failed-stop"); got != domain.TurnStateQueued {
		t.Fatalf("queue after failed Stop = %q, want queued", got)
	}
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "valid after failed Stop", ClientMessageID: "valid-after-failed-stop",
	}); err != nil {
		t.Fatalf("Send after failed Stop = %v, want admission open", err)
	}
	if err := h.ctrl.TerminateForStop(ctx, domain.ChatStopReasonSessionTerminated); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

type deferredStopFailureStore struct {
	*dispatchFenceHookStore
	stopEntered chan struct{}
	stopRelease chan struct{}
	stopOnce    sync.Once
	stopCalls   int
}

func (s *deferredStopFailureStore) RecordStopIntentAndCancelQueued(
	context.Context, string, domain.SessionID, string, string,
	domain.ChatStopReason, string, time.Time,
) (domain.ChatStopIntent, error) {
	s.stopCalls++
	s.stopOnce.Do(func() { close(s.stopEntered) })
	<-s.stopRelease
	return domain.ChatStopIntent{}, errors.New("injected deferred Stop failure")
}

func TestFailedStopDuringDeferredSecondAdmissionDoesNotAbortPreparedTurn(t *testing.T) {
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	startCalls := 0
	discarded := 0
	conv.start = func(string) error { startCalls++; return nil }
	conv.discard = func(string) { discarded++ }
	hook := &deferredStopFailureStore{
		dispatchFenceHookStore: &dispatchFenceHookStore{},
		stopEntered:            make(chan struct{}),
		stopRelease:            make(chan struct{}),
	}
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		hook.Store = st
		return hook
	})
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	hook.afterDeferred = func() { close(bound); <-releaseBind }
	type sendResult struct {
		turn domain.ConversationTurn
		err  error
	}
	sent := make(chan sendResult, 1)
	go func() {
		turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "deferred survives failed stop", ClientMessageID: "deferred-failed-stop",
		})
		sent <- sendResult{turn: turn, err: err}
	}()
	<-bound
	stopResult := make(chan error, 1)
	go func() {
		stopResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-hook.stopEntered
	close(releaseBind)
	close(hook.stopRelease)
	if err := <-stopResult; err == nil {
		t.Fatal("Stop unexpectedly succeeded")
	}
	result := <-sent
	if result.err != nil {
		t.Fatalf("Send after failed Stop = %v, want admitted deferred start", result.err)
	}
	if startCalls != 1 {
		t.Fatalf("StartDeferredTurn calls = %d, want 1", startCalls)
	}
	if discarded != 0 {
		t.Fatalf("DiscardDeferredTurn calls = %d, want 0", discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), result.turn.ID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID == "" {
		t.Fatalf("turn after failed Stop = %+v, want running provider-bound", turn)
	}
	episode, err := h.st.ConversationStopEpisode(context.Background(), h.ctrl.ConversationID())
	if err != nil || episode != "" {
		t.Fatalf("failed Stop episode = %q err=%v, want empty", episode, err)
	}
	if hook.stopCalls != 1 {
		t.Fatalf("Stop calls = %d, want 1", hook.stopCalls)
	}
}

type concurrentStopStore struct {
	*sqlite.Store
	mu            sync.Mutex
	calls         int
	firstEntered  chan struct{}
	secondEntered chan struct{}
	releaseFirst  chan struct{}
	releaseSecond chan struct{}
}

func (s *concurrentStopStore) RecordStopIntentAndCancelQueued(
	ctx context.Context,
	conversationID string,
	session domain.SessionID,
	reviewID, generation string,
	reason domain.ChatStopReason,
	episodeID string,
	now time.Time,
) (domain.ChatStopIntent, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	switch call {
	case 1:
		close(s.firstEntered)
		<-s.releaseFirst
		return domain.ChatStopIntent{}, errors.New("injected first Stop failure")
	case 2:
		close(s.secondEntered)
		<-s.releaseSecond
		return s.Store.RecordStopIntentAndCancelQueued(
			ctx, conversationID, session, reviewID, generation, reason, episodeID, now)
	default:
		return s.Store.RecordStopIntentAndCancelQueued(
			ctx, conversationID, session, reviewID, generation, reason, episodeID, now)
	}
}

func TestConcurrentStopFailureDoesNotReopenConfirmedStop(t *testing.T) {
	concurrent := &concurrentStopStore{
		firstEntered:  make(chan struct{}),
		secondEntered: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		concurrent.Store = st
		return concurrent
	})
	firstResult := make(chan error, 1)
	secondResult := make(chan error, 1)
	go func() {
		firstResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-concurrent.firstEntered
	go func() {
		secondResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-concurrent.secondEntered
	close(concurrent.releaseFirst)
	if err := <-firstResult; err == nil {
		t.Fatal("first Stop unexpectedly succeeded")
	}
	close(concurrent.releaseSecond)
	if err := <-secondResult; err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	episode, err := h.st.ConversationStopEpisode(context.Background(), h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("ConversationStopEpisode: %v", err)
	}
	if episode == "" {
		t.Fatal("confirmed concurrent Stop wrote no episode")
	}
}

var _ chatsvc.Store = (*concurrentStopStore)(nil)
var _ chatsvc.Store = (*dispatchFenceHookStore)(nil)

func turnIDByClientID(t *testing.T, h *harness, clientMessageID string) string {
	t.Helper()
	messages, err := h.st.ConversationMessages(context.Background(), h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("ConversationMessages: %v", err)
	}
	for _, message := range messages {
		if message.ClientMessageID == clientMessageID {
			if message.TurnID == "" {
				t.Fatalf("message %s has no turn id", clientMessageID)
			}
			return message.TurnID
		}
	}
	t.Fatalf("message %s not found", clientMessageID)
	return ""
}
