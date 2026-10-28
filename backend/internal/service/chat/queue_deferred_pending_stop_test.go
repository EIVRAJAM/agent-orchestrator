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

type pendingDeferredStopStore struct {
	*dispatchFenceHookStore

	mu               sync.Mutex
	calls            int
	failFirst        bool
	stopEntered      chan struct{}
	stopRelease      chan struct{}
	enteredOnce      sync.Once
	afterResumeClaim func()
}

func (s *pendingDeferredStopStore) RecordStopIntentAndCancelQueued(
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
	fail := s.failFirst && call == 1
	s.mu.Unlock()
	s.enteredOnce.Do(func() { close(s.stopEntered) })
	<-s.stopRelease
	if fail {
		return domain.ChatStopIntent{}, errors.New("injected pending Stop failure")
	}
	return s.Store.RecordStopIntentAndCancelQueued(
		ctx, conversationID, session, reviewID, generation, reason, episodeID, now)
}

func (s *pendingDeferredStopStore) ClaimQueuedTurnForResume(
	ctx context.Context, conversationID, turnID, expectedGeneration string, now time.Time,
) (bool, error) {
	claimed, err := s.Store.ClaimQueuedTurnForResume(ctx, conversationID, turnID, expectedGeneration, now)
	if claimed && err == nil && s.afterResumeClaim != nil {
		s.afterResumeClaim()
	}
	return claimed, err
}

func (s *pendingDeferredStopStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newPendingDeferredStopStore(failFirst bool) *pendingDeferredStopStore {
	return &pendingDeferredStopStore{
		dispatchFenceHookStore: &dispatchFenceHookStore{},
		failFirst:              failFirst,
		stopEntered:            make(chan struct{}),
		stopRelease:            make(chan struct{}),
	}
}

type deferredSendResult struct {
	turn domain.ConversationTurn
	err  error
}

func TestDeferredPendingStopContextCancelledConfirmed(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	stop := newPendingDeferredStopStore(false)
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		stop.Store = st
		return stop
	})
	ctx, cancel := context.WithCancel(context.Background())
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	stop.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
			Text: "pending stop confirmed", ClientMessageID: "pending-stop-confirmed",
		})
		sent <- deferredSendResult{turn: turn, err: err}
	}()
	<-bound
	if created, err := h.st.AppendUserMessage(context.Background(), h.ctrl.ConversationID(), testSession,
		currentGeneration(h), domain.ConversationMessage{
			ID: "message-behind-pending-stop", Origin: domain.MessageOriginHuman,
			Text: "must not dispatch", ClientMessageID: "behind-pending-stop",
		}, "turn-behind-pending-stop", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}

	stopResult := make(chan error, 1)
	go func() {
		stopResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-stop.stopEntered
	close(releaseBind)
	cancel()
	close(stop.stopRelease)
	if err := <-stopResult; err != nil {
		t.Fatalf("confirmed Stop: %v", err)
	}
	result := <-sent
	if !errors.Is(result.err, chatsvc.ErrProviderDispatchStopped) {
		t.Fatalf("Send error = %v, want ErrProviderDispatchStopped", result.err)
	}
	assertNoDeferredStartOrProvider(t, *startCalls, *discarded)
	turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "pending-stop-confirmed"))
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	assertInterruptedUnstarted(t, turn)
	if got := turnStateByID(t, h, "turn-behind-pending-stop"); got != domain.TurnStateInterrupted {
		t.Fatalf("next queued state = %q, want interrupted by confirmed Stop", got)
	}
	assertNoDispatchingTurn(t, h)
}

func TestDeferredPendingStopContextCancelledFailed(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	stop := newPendingDeferredStopStore(true)
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		stop.Store = st
		return stop
	})
	ctx, cancel := context.WithCancel(context.Background())
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	stop.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
			Text: "pending stop failed", ClientMessageID: "pending-stop-failed",
		})
		sent <- deferredSendResult{turn: turn, err: err}
	}()
	<-bound
	stopResult := make(chan error, 1)
	go func() {
		stopResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-stop.stopEntered
	close(releaseBind)
	cancel()
	close(stop.stopRelease)
	if err := <-stopResult; err == nil {
		t.Fatal("failed Stop unexpectedly succeeded")
	}
	result := <-sent
	if !errors.Is(result.err, chatsvc.ErrProviderDispatchStopped) {
		t.Fatalf("Send error = %v, want ErrProviderDispatchStopped", result.err)
	}
	assertNoDeferredStartOrProvider(t, *startCalls, *discarded)
	turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "pending-stop-failed"))
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	assertInterruptedUnstarted(t, turn)
	episode, err := h.st.ConversationStopEpisode(context.Background(), h.ctrl.ConversationID())
	if err != nil || episode != "" {
		t.Fatalf("failed Stop episode = %q err=%v, want empty", episode, err)
	}
	// A failed Stop cannot leave the controller's admission closed.
	stop.afterDeferred = nil
	if _, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "fresh context after failed Stop", ClientMessageID: "fresh-after-cancelled-stop",
	}); err != nil {
		t.Fatalf("Send after failed pending Stop: %v", err)
	}
	if *startCalls != 1 {
		t.Fatalf("StartDeferredTurn after fresh Send = %d, want 1", startCalls)
	}
	if err := h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestDeferredPreclaimedPendingStopContextCancelledConfirmed(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	stop := newPendingDeferredStopStore(false)
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		stop.Store = st
		return stop
	})
	first, second := queueTwoBehindFailingRoot(t, h, conv.emit)
	*startCalls, *discarded = 0, 0
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	stop.afterEager = func() { close(bound); <-releaseBind }
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h))
		result <- err
	}()
	<-bound
	stopResult := make(chan error, 1)
	go func() {
		stopResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-stop.stopEntered
	close(releaseBind)
	cancel()
	close(stop.stopRelease)
	if err := <-stopResult; err != nil {
		t.Fatalf("confirmed Stop: %v", err)
	}
	if err := <-result; !errors.Is(err, chatsvc.ErrQueueDispatchFailed) {
		t.Fatalf("resume error = %v, want ErrQueueDispatchFailed", err)
	}
	assertNoDeferredStartOrProvider(t, *startCalls, *discarded)
	turn, err := h.st.TurnByID(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	assertInterruptedUnstarted(t, turn)
	if got := turnStateByID(t, h, second.ID); got != domain.TurnStateInterrupted {
		t.Fatalf("next head state = %q, want interrupted by confirmed Stop", got)
	}
	assertNoDispatchingTurn(t, h)
}

func TestDeferredPreclaimedPendingStopContextCancelledDuringPrepare(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "confirmed"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			conv, startCalls, discarded := newCountingDeferredConversation()
			stop := newPendingDeferredStopStore(fail)
			h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
				stop.Store = st
				return stop
			})
			first, second := queueTwoBehindFailingRoot(t, h, conv.emit)
			*startCalls, *discarded = 0, 0
			preparing, releasePrepare := make(chan struct{}), make(chan struct{})
			conv.onSend = func(string) { close(preparing); <-releasePrepare }
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				_, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h))
				result <- err
			}()
			<-preparing
			stopResult := make(chan error, 1)
			go func() {
				stopResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
			}()
			<-stop.stopEntered
			cancel()
			close(releasePrepare)
			close(stop.stopRelease)
			stopErr := <-stopResult
			if fail && stopErr == nil {
				t.Fatal("failed Stop unexpectedly succeeded")
			}
			if !fail && stopErr != nil {
				t.Fatalf("confirmed Stop: %v", stopErr)
			}
			if err := <-result; err == nil {
				t.Fatal("resume succeeded after its context was canceled")
			}
			assertNoDeferredStartOrProvider(t, *startCalls, *discarded)
			turn, err := h.st.TurnByID(context.Background(), first.ID)
			if err != nil {
				t.Fatalf("TurnByID: %v", err)
			}
			assertInterruptedUnstarted(t, turn)
			if fail {
				if got := turnStateByID(t, h, second.ID); got != domain.TurnStateQueued {
					t.Fatalf("next head after failed Stop = %q, want queued", got)
				}
				episode, err := h.st.ConversationStopEpisode(context.Background(), h.ctrl.ConversationID())
				if err != nil || episode != "" {
					t.Fatalf("failed Stop episode = %q err=%v, want empty", episode, err)
				}
				if _, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
					Text: "fresh after preclaimed cancelled Stop", ClientMessageID: "fresh-preclaimed-after-stop",
				}); err != nil {
					t.Fatalf("Send after failed preclaimed Stop: %v", err)
				}
			} else if got := turnStateByID(t, h, second.ID); got != domain.TurnStateInterrupted {
				t.Fatalf("next head after confirmed Stop = %q, want interrupted", got)
			}
		})
	}
}

func TestDeferredPreclaimedPendingStopContextCancelledBeforePrepare(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "confirmed"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			conv, startCalls, discarded := newCountingDeferredConversation()
			stop := newPendingDeferredStopStore(fail)
			h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
				stop.Store = st
				return stop
			})
			first, second := queueTwoBehindFailingRoot(t, h, conv.emit)
			*startCalls, *discarded = 0, 0
			claimed, releaseClaim := make(chan struct{}), make(chan struct{})
			stop.afterResumeClaim = func() { close(claimed); <-releaseClaim }
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				_, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h))
				result <- err
			}()
			<-claimed
			stopResult := make(chan error, 1)
			go func() {
				stopResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
			}()
			<-stop.stopEntered
			cancel()
			close(releaseClaim)
			close(stop.stopRelease)
			stopErr := <-stopResult
			if fail && stopErr == nil {
				t.Fatal("failed Stop unexpectedly succeeded")
			}
			if !fail && stopErr != nil {
				t.Fatalf("confirmed Stop: %v", stopErr)
			}
			if err := <-result; err == nil {
				t.Fatal("resume succeeded after its context was canceled")
			}
			if *startCalls != 0 {
				t.Fatalf("StartDeferredTurn calls = %d, want 0", *startCalls)
			}
			if *discarded != 0 {
				t.Fatalf("DiscardDeferredTurn calls = %d, want 0 before prepare", *discarded)
			}
			turn, err := h.st.TurnByID(context.Background(), first.ID)
			if err != nil {
				t.Fatalf("TurnByID: %v", err)
			}
			assertInterruptedUnstarted(t, turn)
			if fail {
				if got := turnStateByID(t, h, second.ID); got != domain.TurnStateQueued {
					t.Fatalf("next head after failed Stop = %q, want queued", got)
				}
			} else if got := turnStateByID(t, h, second.ID); got != domain.TurnStateInterrupted {
				t.Fatalf("next head after confirmed Stop = %q, want interrupted", got)
			}
		})
	}
}

type concurrentPendingStopStore struct {
	*dispatchFenceHookStore
	mu            sync.Mutex
	calls         int
	firstEntered  chan struct{}
	secondEntered chan struct{}
	releaseFirst  chan struct{}
	releaseSecond chan struct{}
}

func (s *concurrentPendingStopStore) RecordStopIntentAndCancelQueued(
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

func TestDeferredPendingStopConcurrentStopsContextCancelled(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	stop := &concurrentPendingStopStore{
		dispatchFenceHookStore: &dispatchFenceHookStore{},
		firstEntered:           make(chan struct{}),
		secondEntered:          make(chan struct{}),
		releaseFirst:           make(chan struct{}),
		releaseSecond:          make(chan struct{}),
	}
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		stop.Store = st
		return stop
	})
	ctx, cancel := context.WithCancel(context.Background())
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	stop.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
			Text: "concurrent pending Stops", ClientMessageID: "concurrent-pending-stops",
		})
		sent <- deferredSendResult{turn: turn, err: err}
	}()
	<-bound
	firstStop := make(chan error, 1)
	secondStop := make(chan error, 1)
	go func() {
		firstStop <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-stop.firstEntered
	go func() {
		secondStop <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-stop.secondEntered
	close(releaseBind)
	cancel()
	close(stop.releaseFirst)
	if err := <-firstStop; err == nil {
		t.Fatal("first Stop unexpectedly succeeded")
	}
	close(stop.releaseSecond)
	if err := <-secondStop; err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	result := <-sent
	if !errors.Is(result.err, chatsvc.ErrProviderDispatchStopped) {
		t.Fatalf("Send error = %v, want ErrProviderDispatchStopped", result.err)
	}
	assertNoDeferredStartOrProvider(t, *startCalls, *discarded)
	turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "concurrent-pending-stops"))
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	assertInterruptedUnstarted(t, turn)
	episode, err := h.st.ConversationStopEpisode(context.Background(), h.ctrl.ConversationID())
	if err != nil || episode == "" {
		t.Fatalf("confirmed concurrent Stop episode = %q err=%v, want non-empty", episode, err)
	}
	assertNoDispatchingTurn(t, h)
}

func newCountingDeferredConversation() (*deferredConversation, *int, *int) {
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	startCalls, discarded := 0, 0
	conv.start = func(string) error { startCalls++; return nil }
	conv.discard = func(string) { discarded++ }
	return conv, &startCalls, &discarded
}

func assertNoDeferredStartOrProvider(t *testing.T, startCalls, discarded int) {
	t.Helper()
	if startCalls != 0 {
		t.Fatalf("StartDeferredTurn calls = %d, want 0", startCalls)
	}
	if discarded != 1 {
		t.Fatalf("DiscardDeferredTurn calls = %d, want exactly 1", discarded)
	}
}

func assertInterruptedUnstarted(t *testing.T, turn domain.ConversationTurn) {
	t.Helper()
	if turn.State != domain.TurnStateInterrupted || turn.ProviderTurnID != "" ||
		turn.StartedAt != nil || turn.CompletedAt == nil {
		t.Fatalf("turn = %+v, want interrupted with no provider identity and completed_at", turn)
	}
}

func assertNoDispatchingTurn(t *testing.T, h *harness) {
	t.Helper()
	ids := h.ctrl.DispatchingTurnIDs()
	if len(ids) > 0 && ids[len(ids)-1] != "" {
		t.Fatalf("dispatching turn tracking survived finalization: %v", ids)
	}
}
