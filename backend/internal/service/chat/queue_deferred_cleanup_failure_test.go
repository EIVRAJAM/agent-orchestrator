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

type deferredCleanupFailureStore struct {
	*pendingDeferredStopStore

	mu                   sync.Mutex
	failPrepared         error
	failClaimed          error
	zeroPrepared         bool
	zeroPreparedTerminal bool
	blockPrepared        bool
	settleErr            error
	settleBlocked        bool
	settleEntered        chan struct{}
	settleRelease        chan struct{}
	settleOnce           sync.Once
}

func (s *deferredCleanupFailureStore) SettleTurnByID(
	ctx context.Context, turnID string, state domain.TurnState, errMessage string, now time.Time,
) error {
	s.mu.Lock()
	blocked, settleErr := s.settleBlocked, s.settleErr
	s.mu.Unlock()
	if blocked {
		s.settleOnce.Do(func() { close(s.settleEntered) })
		select {
		case <-s.settleRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if settleErr != nil {
		return settleErr
	}
	return s.Store.SettleTurnByID(ctx, turnID, state, errMessage, now)
}

func (s *deferredCleanupFailureStore) InterruptPreparedDeferredTurn(
	ctx context.Context,
	conversationID string,
	session domain.SessionID,
	reviewID, generation, turnID, providerTurnID string,
	now time.Time,
) (bool, error) {
	s.mu.Lock()
	fail, zero, terminal, block := s.failPrepared, s.zeroPrepared, s.zeroPreparedTerminal, s.blockPrepared
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if fail != nil {
		return false, fail
	}
	if terminal {
		_, err := s.Store.InterruptPreparedDeferredTurn(
			ctx, conversationID, session, reviewID, generation, turnID, providerTurnID, now)
		if err != nil {
			return false, err
		}
		return false, nil
	}
	if zero {
		return false, nil
	}
	return s.Store.InterruptPreparedDeferredTurn(
		ctx, conversationID, session, reviewID, generation, turnID, providerTurnID, now)
}

func (s *deferredCleanupFailureStore) InterruptClaimedUndeliveredTurn(
	ctx context.Context,
	conversationID string,
	session domain.SessionID,
	reviewID, generation, turnID string,
	now time.Time,
) (bool, error) {
	s.mu.Lock()
	fail := s.failClaimed
	s.mu.Unlock()
	if fail != nil {
		return false, fail
	}
	return s.Store.InterruptClaimedUndeliveredTurn(
		ctx, conversationID, session, reviewID, generation, turnID, now)
}

func newCleanupFailureStore(failFirstStop bool) *deferredCleanupFailureStore {
	return &deferredCleanupFailureStore{
		pendingDeferredStopStore: newPendingDeferredStopStore(failFirstStop),
	}
}

func TestDeferredPendingStopPreparedCleanupFailureRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(true)
	cleanup.failPrepared = errors.New("injected prepared transition failure")
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	ctx, cancel := context.WithCancel(context.Background())
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	cleanup.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
			Text: "prepared cleanup failure", ClientMessageID: "prepared-cleanup-failure",
		})
		sent <- deferredSendResult{turn: turn, err: err}
	}()
	<-bound
	stopResult := make(chan error, 1)
	go func() {
		stopResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-cleanup.stopEntered
	close(releaseBind)
	cancel()
	close(cleanup.stopRelease)
	if err := <-stopResult; err == nil {
		t.Fatal("failed Stop unexpectedly succeeded")
	}
	result := <-sent
	assertCleanupIncomplete(t, result.err)
	if *startCalls != 0 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want no start and no discard before durable success", *startCalls, *discarded)
	}
	turnID := turnIDByClientID(t, h, "prepared-cleanup-failure")
	turn, err := h.st.TurnByID(context.Background(), turnID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID == "" || turn.StartedAt == nil {
		t.Fatalf("failed cleanup turn = %+v, want observable running/prepared", turn)
	}
	assertDeferredTrackingRetained(t, h, turnID, turn.ProviderTurnID, "prepared")
}

func TestDeferredPreclaimedClaimedCleanupFailureRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(true)
	cleanup.failClaimed = errors.New("injected claimed transition failure")
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	first, second := queueTwoBehindFailingRoot(t, h, conv.emit)
	*startCalls, *discarded = 0, 0
	claimed, releaseClaim := make(chan struct{}), make(chan struct{})
	cleanup.afterResumeClaim = func() { close(claimed); <-releaseClaim }
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
	<-cleanup.stopEntered
	cancel()
	close(releaseClaim)
	close(cleanup.stopRelease)
	if err := <-stopResult; err == nil {
		t.Fatal("failed Stop unexpectedly succeeded")
	}
	assertCleanupIncomplete(t, <-result)
	if *startCalls != 0 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want no start and no discard before durable success", *startCalls, *discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID != "" || turn.StartedAt == nil {
		t.Fatalf("failed claimed cleanup turn = %+v, want observable running/unbound claim", turn)
	}
	assertDeferredTrackingRetained(t, h, first.ID, "", "none")
	if got := turnStateByID(t, h, second.ID); got != domain.TurnStateQueued {
		t.Fatalf("next head = %q, want queued without drain", got)
	}
}

func TestDeferredPendingStopWaitTimeoutRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	stop := newPendingDeferredStopStore(false)
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		stop.Store = st
		return stop
	})
	restore := h.ctrl.SetDeferredPendingStopLimitsForTest(40*time.Millisecond, time.Second)
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	stop.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
			Text: "wait timeout retains tracking", ClientMessageID: "wait-timeout-tracking",
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
	result := <-sent
	assertCleanupIncomplete(t, result.err)
	if *startCalls != 0 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want no start/discard on wait timeout", *startCalls, *discarded)
	}
	turnID := turnIDByClientID(t, h, "wait-timeout-tracking")
	assertDeferredTrackingRetained(t, h, turnID, "provider-turn-1", "prepared")

	// The Stop still owns the durable decision. Releasing it lets the ordinary
	// Stop helper settle the row; the retained tracking is not silently erased.
	close(stop.stopRelease)
	if err := <-stopResult; err != nil {
		t.Fatalf("confirmed Stop after timeout: %v", err)
	}
	turn, err := h.st.TurnByID(context.Background(), turnID)
	if err != nil {
		t.Fatalf("TurnByID after Stop: %v", err)
	}
	if turn.State.Terminal() {
		if turn.ProviderTurnID != "" || turn.StartedAt != nil {
			t.Fatalf("terminal turn retained provider identity: %+v", turn)
		}
	} else if turn.State != domain.TurnStateRunning {
		t.Fatalf("turn after Stop = %+v, want terminal or observable running", turn)
	}
	assertDeferredTrackingRetained(t, h, turnID, "provider-turn-1", "prepared")
}

func TestDeferredPendingStopCleanupTimeoutRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(true)
	cleanup.blockPrepared = true
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	restore := h.ctrl.SetDeferredPendingStopLimitsForTest(time.Second, 40*time.Millisecond)
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	cleanup.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
			Text: "cleanup timeout retains tracking", ClientMessageID: "cleanup-timeout-tracking",
		})
		sent <- deferredSendResult{turn: turn, err: err}
	}()
	<-bound
	stopResult := make(chan error, 1)
	go func() {
		stopResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-cleanup.stopEntered
	close(releaseBind)
	cancel()
	close(cleanup.stopRelease)
	if err := <-stopResult; err == nil {
		t.Fatal("failed Stop unexpectedly succeeded")
	}
	result := <-sent
	assertCleanupIncomplete(t, result.err)
	if *startCalls != 0 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want no start/discard on cleanup timeout", *startCalls, *discarded)
	}
	turnID := turnIDByClientID(t, h, "cleanup-timeout-tracking")
	turn, err := h.st.TurnByID(context.Background(), turnID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID == "" {
		t.Fatalf("cleanup timeout turn = %+v, want observable running/prepared", turn)
	}
	assertDeferredTrackingRetained(t, h, turnID, turn.ProviderTurnID, "prepared")
	// Let harness cleanup settle the retained tracking with a normal store call.
	cleanup.mu.Lock()
	cleanup.blockPrepared = false
	cleanup.mu.Unlock()
}

func TestDeferredPendingStopZeroRowsTerminalClearsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(true)
	cleanup.zeroPreparedTerminal = true
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	ctx, cancel := context.WithCancel(context.Background())
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	cleanup.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
			Text: "zero rows terminal", ClientMessageID: "zero-rows-terminal",
		})
		sent <- deferredSendResult{turn: turn, err: err}
	}()
	<-bound
	stopResult := make(chan error, 1)
	go func() {
		stopResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-cleanup.stopEntered
	close(releaseBind)
	cancel()
	close(cleanup.stopRelease)
	if err := <-stopResult; err == nil {
		t.Fatal("failed Stop unexpectedly succeeded")
	}
	result := <-sent
	if !errors.Is(result.err, chatsvc.ErrProviderDispatchStopped) {
		t.Fatalf("Send error = %v, want ErrProviderDispatchStopped after concurrent terminal transition", result.err)
	}
	if *startCalls != 0 || *discarded != 1 {
		t.Fatalf("start=%d discard=%d, want 0/1 for terminal zero-row cleanup", *startCalls, *discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "zero-rows-terminal"))
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	assertInterruptedUnstarted(t, turn)
	assertNoDispatchingTurn(t, h)
}

func TestDeferredPendingStopZeroRowsRunningRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(true)
	cleanup.zeroPrepared = true
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	ctx, cancel := context.WithCancel(context.Background())
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	cleanup.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
			Text: "zero rows running", ClientMessageID: "zero-rows-running",
		})
		sent <- deferredSendResult{turn: turn, err: err}
	}()
	<-bound
	stopResult := make(chan error, 1)
	go func() {
		stopResult <- h.ctrl.TerminateForStop(context.Background(), domain.ChatStopReasonSessionTerminated)
	}()
	<-cleanup.stopEntered
	close(releaseBind)
	cancel()
	close(cleanup.stopRelease)
	if err := <-stopResult; err == nil {
		t.Fatal("failed Stop unexpectedly succeeded")
	}
	result := <-sent
	assertCleanupIncomplete(t, result.err)
	if *startCalls != 0 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want 0/0 for unresolved zero-row cleanup", *startCalls, *discarded)
	}
	turnID := turnIDByClientID(t, h, "zero-rows-running")
	turn, err := h.st.TurnByID(context.Background(), turnID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID == "" {
		t.Fatalf("zero-row running turn = %+v, want still observable", turn)
	}
	assertDeferredTrackingRetained(t, h, turnID, turn.ProviderTurnID, "prepared")
}

func assertCleanupIncomplete(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, chatsvc.ErrDeferredDispatchCleanupIncomplete) {
		t.Fatalf("error = %v, want ErrDeferredDispatchCleanupIncomplete", err)
	}
}

func assertDeferredTrackingRetained(t *testing.T, h *harness, turnID, providerTurnID, phase string) {
	t.Helper()
	found := false
	for _, id := range h.ctrl.DispatchingTurnIDs() {
		if id == turnID {
			found = true
		}
	}
	if !found {
		t.Fatalf("dispatch tracking does not contain %q: %v", turnID, h.ctrl.DispatchingTurnIDs())
	}
	dispatchingTurnID, gotProvider, gotPhase := h.ctrl.DeferredDispatchTrackingForTest()
	if dispatchingTurnID != turnID || gotProvider != providerTurnID || gotPhase != phase {
		t.Fatalf("retained tracking = turn=%q provider=%q phase=%q, want turn=%q provider=%q phase=%q",
			dispatchingTurnID, gotProvider, gotPhase, turnID, providerTurnID, phase)
	}
}

func TestDeferredStoppedPreparedCleanupFailureRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(false)
	close(cleanup.stopRelease) // Stop is called synchronously via the test helper.
	cleanup.failPrepared = errors.New("injected stopped prepared transition failure")
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	cleanup.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "stopped prepared cleanup failure", ClientMessageID: "stopped-prepared-failure",
		})
		sent <- deferredSendResult{turn: turn, err: err}
	}()
	<-bound
	if created, err := h.st.AppendUserMessage(context.Background(), h.ctrl.ConversationID(), testSession,
		currentGeneration(h), domain.ConversationMessage{
			ID: "message-behind-stopped-prepared", Origin: domain.MessageOriginHuman,
			Text: "must not dispatch", ClientMessageID: "behind-stopped-prepared",
		}, "turn-behind-stopped-prepared", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}
	if err := h.ctrl.RecordStopIntentForTest(context.Background(), domain.ChatStopReasonUserStop); err != nil {
		t.Fatalf("RecordStopIntentForTest: %v", err)
	}
	close(releaseBind)
	result := <-sent
	assertCleanupIncomplete(t, result.err)
	if *startCalls != 0 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want no start/discard on stopped cleanup failure", *startCalls, *discarded)
	}
	turnID := turnIDByClientID(t, h, "stopped-prepared-failure")
	turn, err := h.st.TurnByID(context.Background(), turnID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID == "" || turn.StartedAt == nil {
		t.Fatalf("turn after stopped cleanup failure = %+v, want running/prepared", turn)
	}
	assertDeferredTrackingRetained(t, h, turnID, turn.ProviderTurnID, "prepared")
	if got := turnStateByID(t, h, "turn-behind-stopped-prepared"); got != domain.TurnStateInterrupted {
		t.Fatalf("next queued = %q, want interrupted by confirmed Stop", got)
	}
}

func TestDeferredStoppedPreclaimedClaimedCleanupFailureRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(false)
	close(cleanup.stopRelease)
	cleanup.failClaimed = errors.New("injected stopped claimed transition failure")
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	first, second := queueTwoBehindFailingRoot(t, h, conv.emit)
	*startCalls, *discarded = 0, 0
	beforeSends := conv.sendCallCount()
	claimed, releaseClaim := make(chan struct{}), make(chan struct{})
	cleanup.afterResumeClaim = func() { close(claimed); <-releaseClaim }
	result := make(chan error, 1)
	go func() {
		_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
		result <- err
	}()
	<-claimed
	if err := h.ctrl.RecordStopIntentForTest(context.Background(), domain.ChatStopReasonUserStop); err != nil {
		t.Fatalf("RecordStopIntentForTest: %v", err)
	}
	close(releaseClaim)
	assertCleanupIncomplete(t, <-result)
	if got := conv.sendCallCount(); got != beforeSends {
		t.Fatalf("SendTurn calls = %d, want %d (no prepare after stopped admission)", got, beforeSends)
	}
	if *startCalls != 0 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want no start/discard", *startCalls, *discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID != "" || turn.StartedAt == nil {
		t.Fatalf("stopped claimed turn = %+v, want running/unbound", turn)
	}
	assertDeferredTrackingRetained(t, h, first.ID, "", "none")
	if got := turnStateByID(t, h, second.ID); got != domain.TurnStateInterrupted {
		t.Fatalf("next head = %q, want interrupted by confirmed Stop", got)
	}
}

func TestDeferredStoppedDuringPrepareCleanupFailureRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(false)
	close(cleanup.stopRelease)
	cleanup.failClaimed = errors.New("injected prepare claimed transition failure")
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	first, second := queueTwoBehindFailingRoot(t, h, conv.emit)
	*startCalls, *discarded = 0, 0
	preparing, releasePrepare := make(chan struct{}), make(chan struct{})
	conv.onSend = func(string) { close(preparing); <-releasePrepare }
	result := make(chan error, 1)
	go func() {
		_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
		result <- err
	}()
	<-preparing
	if err := h.ctrl.RecordStopIntentForTest(context.Background(), domain.ChatStopReasonUserStop); err != nil {
		t.Fatalf("RecordStopIntentForTest: %v", err)
	}
	close(releasePrepare)
	assertCleanupIncomplete(t, <-result)
	if *startCalls != 0 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want no start/discard", *startCalls, *discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID != "" || turn.StartedAt == nil {
		t.Fatalf("stopped during prepare turn = %+v, want running/unbound claim", turn)
	}
	assertDeferredTrackingRetained(t, h, first.ID, "", "preparing")
	if got := turnStateByID(t, h, second.ID); got != domain.TurnStateInterrupted {
		t.Fatalf("next head = %q, want interrupted by confirmed Stop", got)
	}
}

func TestDeferredStoppedZeroRowsTerminalClearsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(false)
	close(cleanup.stopRelease)
	cleanup.zeroPreparedTerminal = true
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	cleanup.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "stopped zero rows terminal", ClientMessageID: "stopped-zero-terminal",
		})
		sent <- deferredSendResult{turn: turn, err: err}
	}()
	<-bound
	if err := h.ctrl.RecordStopIntentForTest(context.Background(), domain.ChatStopReasonUserStop); err != nil {
		t.Fatalf("RecordStopIntentForTest: %v", err)
	}
	close(releaseBind)
	result := <-sent
	if !errors.Is(result.err, chatsvc.ErrProviderDispatchStopped) {
		t.Fatalf("Send error = %v, want ErrProviderDispatchStopped", result.err)
	}
	if *startCalls != 0 || *discarded != 1 {
		t.Fatalf("start=%d discard=%d, want 0/1 for terminal stopped cleanup", *startCalls, *discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "stopped-zero-terminal"))
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	assertInterruptedUnstarted(t, turn)
	assertNoDispatchingTurn(t, h)
}

func TestDeferredStoppedZeroRowsRunningRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(false)
	close(cleanup.stopRelease)
	cleanup.zeroPrepared = true
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	bound, releaseBind := make(chan struct{}), make(chan struct{})
	cleanup.afterDeferred = func() { close(bound); <-releaseBind }
	sent := make(chan deferredSendResult, 1)
	go func() {
		turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
			Text: "stopped zero rows running", ClientMessageID: "stopped-zero-running",
		})
		sent <- deferredSendResult{turn: turn, err: err}
	}()
	<-bound
	if err := h.ctrl.RecordStopIntentForTest(context.Background(), domain.ChatStopReasonUserStop); err != nil {
		t.Fatalf("RecordStopIntentForTest: %v", err)
	}
	close(releaseBind)
	result := <-sent
	assertCleanupIncomplete(t, result.err)
	if *startCalls != 0 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want 0/0 for unresolved stopped cleanup", *startCalls, *discarded)
	}
	turnID := turnIDByClientID(t, h, "stopped-zero-running")
	turn, err := h.st.TurnByID(context.Background(), turnID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID == "" {
		t.Fatalf("stopped zero-row running turn = %+v, want observable", turn)
	}
	assertDeferredTrackingRetained(t, h, turnID, turn.ProviderTurnID, "prepared")
}
