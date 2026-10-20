package chat_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// claimStepStore opens deterministic windows around the durable resume claim.
// beforeClaim runs immediately before the CAS; afterClaim runs immediately after
// a successful CAS and before dispatchTurn calls the provider.
type claimStepStore struct {
	chatsvc.Store
	mu          sync.Mutex
	beforeClaim func() error
	afterClaim  func()
}

func (s *claimStepStore) ClaimQueuedTurnForResume(
	ctx context.Context,
	conversationID, turnID, expectedControllerGeneration string,
	now time.Time,
) (bool, error) {
	s.mu.Lock()
	before, after := s.beforeClaim, s.afterClaim
	s.mu.Unlock()
	if before != nil {
		if err := before(); err != nil {
			return false, err
		}
	}
	claimed, err := s.Store.ClaimQueuedTurnForResume(ctx, conversationID, turnID, expectedControllerGeneration, now)
	if claimed && err == nil && after != nil {
		after()
	}
	return claimed, err
}

func (s *claimStepStore) setHooks(before func() error, after func()) {
	s.mu.Lock()
	s.beforeClaim, s.afterClaim = before, after
	s.mu.Unlock()
}

// generationStepStore pauses Start inside its durable generation claim while it
// holds controllerGate, so a test can prove the resume blocks on that gate.
type generationStepStore struct {
	chatsvc.Store
	armed   chan struct{}
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *generationStepStore) arm() {
	close(s.armed)
}

func (s *generationStepStore) ClaimChatControllerGeneration(ctx context.Context, id domain.SessionID, generation string) error {
	select {
	case <-s.armed:
		s.once.Do(func() {
			close(s.entered)
			<-s.release
		})
	default:
	}
	return s.Store.ClaimChatControllerGeneration(ctx, id, generation)
}

// queueTwoBehindFailingRoot queues two automation turns behind a running root
// turn and fails the root, leaving both held. emit is the conversation the
// harness is actually driving, which is not h.conv for a custom provider double.
func queueTwoBehindFailingRoot(t *testing.T, h *harness, emit func(...ports.ChatEvent)) (domain.ConversationTurn, domain.ConversationTurn) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "root", ClientMessageID: "c1", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("send root: %v", err)
	}
	emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateRunning
	})
	first, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "automation", ClientMessageID: "c2", Origin: domain.MessageOriginAutomation,
	})
	if err != nil {
		t.Fatalf("queue automation: %v", err)
	}
	second, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "automation2", ClientMessageID: "c3", Origin: domain.MessageOriginAutomation,
	})
	if err != nil {
		t.Fatalf("queue automation2: %v", err)
	}
	emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateFailed,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateFailed
	})
	return first, second
}

// TestResumeHoldClearedWhenClaimLosesToCancel arms the one-shot and then makes a
// competing transition cancel the named row between the head read and the CAS.
// The resume must return stale_queue_state, call the provider zero times, clear
// the marker, ignore an unrelated terminal event, and leave a later resume of the
// new head fully functional.
func TestResumeHoldClearedWhenClaimLosesToCancel(t *testing.T) {
	claim := &claimStepStore{}
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		claim.Store = st
		return claim
	})
	first, second := queueTwoBehindFailingRoot(t, h, h.conv.emit)
	ctx := context.Background()

	claim.setHooks(func() error {
		return h.st.CancelQueuedTurnByID(ctx, h.ctrl.ConversationID(), first.ID, h.now())
	}, nil)

	result, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeStaleQueueState {
		t.Fatalf("outcome = %q, want stale_queue_state", result.Outcome)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want no release after a lost claim", got)
	}
	if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != "" {
		t.Fatalf("one-shot marker survived a lost claim: armed for %q", armed)
	}

	// An unrelated terminal event must not dispatch anything, because no
	// one-shot turn is active.
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "unrelated-turn",
		TurnState: domain.TurnStateCompleted,
	})
	time.Sleep(100 * time.Millisecond)
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v after an unrelated terminal event", got)
	}
	assertTurnStaysQueued(t, h, "automation2")

	// A later valid resume of the new head must still work.
	claim.setHooks(nil, nil)
	result, err = h.svc.ResumeHeldQueue(ctx, testSession, second.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeResumed || result.TurnID != second.ID {
		t.Fatalf("second resume = %+v, want resumed %s", result, second.ID)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation2"}) {
		t.Fatalf("provider received %v, want exactly the new head released once", got)
	}
}

// TestResumeHoldClearedWhenClaimStoreFails proves a store error before the CAS
// also retires the marker and never reaches the provider.
func TestResumeHoldClearedWhenClaimStoreFails(t *testing.T) {
	claim := &claimStepStore{}
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		claim.Store = st
		return claim
	})
	first, _ := queueTwoBehindFailingRoot(t, h, h.conv.emit)

	claim.setHooks(func() error { return errors.New("store unavailable") }, nil)

	_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
	if err == nil {
		t.Fatalf("resume succeeded despite a claim store error")
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want no release after a store error", got)
	}
	if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != "" {
		t.Fatalf("one-shot marker survived a claim store error: armed for %q", armed)
	}
	if state := turnStateByID(t, h, first.ID); state != domain.TurnStateQueued {
		t.Fatalf("turn state = %q, want still queued", state)
	}
}

// TestResumeHoldsGateAcrossClaimAndSend proves the production synchronization,
// not a simulated store write: the resume holds controllerGate from validation
// through the provider boundary, so a real Start replacement blocks on it and
// cannot change controller_generation until the send has happened exactly once.
func TestResumeHoldsGateAcrossClaimAndSend(t *testing.T) {
	claim := &claimStepStore{}
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		claim.Store = st
		return claim
	})
	first, _ := queueTwoBehindFailingRoot(t, h, h.conv.emit)
	ctx := context.Background()
	originalGeneration := currentGeneration(h)

	claimed := make(chan struct{})
	release := make(chan struct{})
	claim.setHooks(nil, func() {
		close(claimed)
		<-release
	})

	resumeDone := make(chan error, 1)
	go func() {
		_, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, originalGeneration)
		resumeDone <- err
	}()

	select {
	case <-claimed:
	case <-time.After(5 * time.Second):
		t.Fatalf("resume never reached the post-claim window")
	}

	// The row is running and the resume is paused before SendTurn.
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v before the resume was released", got)
	}

	// The real replacement path must block on controllerGate.
	startDone := make(chan error, 1)
	go func() {
		_, err := restartChat(t, h)
		startDone <- err
	}()
	select {
	case err := <-startDone:
		t.Fatalf("Start finished while the resume held controllerGate: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if err := <-resumeDone; err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation"}) {
		t.Fatalf("provider received %v, want the resumed turn sent exactly once", got)
	}
	if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != first.ID {
		t.Fatalf("one-shot marker = %q, want %s after a successful resume", armed, first.ID)
	}

	// Only now may the replacement proceed. Start finds the controller still live,
	// so it returns it; the point is that it could not even read the registry
	// (and therefore could not claim a generation) while the resume held the gate.
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Start after the resume released the gate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Start did not proceed after the resume released controllerGate")
	}
}

// TestReplacementWinsGateBeforeResume is the complementary interleaving: a real
// Start that changes controller_generation holds controllerGate before the resume
// is admitted. The resume blocks, revalidates after the replacement, and is
// refused without calling the provider.
func TestReplacementWinsGateBeforeResume(t *testing.T) {
	gen := &generationStepStore{
		armed: make(chan struct{}), entered: make(chan struct{}), release: make(chan struct{}),
	}
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		gen.Store = st
		return gen
	})
	_, _ = queueTwoBehindFailingRoot(t, h, h.conv.emit)
	ctx := context.Background()
	originalGeneration := currentGeneration(h)

	// Stop retires the live controller so Start must actually claim a new
	// generation, and queue fresh work the replacement session still owns.
	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if created, err := h.st.AppendUserMessage(ctx, h.ctrl.ConversationID(), testSession, originalGeneration,
		domain.ConversationMessage{
			ID: "message-fresh", Origin: domain.MessageOriginAutomation, Text: "fresh", ClientMessageID: "client-fresh",
		}, "turn-fresh", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}
	// Stop closed the fake's event stream. Give the replacement a fresh stream so
	// the production Start path installs a genuinely live new controller; this is
	// test plumbing for the provider double, not part of the production path.
	h.conv.mu.Lock()
	h.conv.events = make(chan ports.ChatEvent, 64)
	h.conv.mu.Unlock()
	h.conv.closeOnce = sync.Once{}
	h.conv.closeSignalOnce = sync.Once{}
	gen.arm()

	startDone := make(chan error, 1)
	go func() {
		_, err := restartChat(t, h)
		startDone <- err
	}()
	select {
	case <-gen.entered:
	case err := <-startDone:
		t.Fatalf("Start finished before reaching its generation claim: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("Start never reached its generation claim")
	}

	resumeDone := make(chan struct {
		res chatsvc.QueueResumeResult
		err error
	}, 1)
	go func() {
		res, err := h.svc.ResumeHeldQueue(ctx, testSession, "turn-fresh", originalGeneration)
		resumeDone <- struct {
			res chatsvc.QueueResumeResult
			err error
		}{res, err}
	}()
	select {
	case got := <-resumeDone:
		t.Fatalf("resume finished while the replacement held controllerGate: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}

	close(gen.release)
	if err := <-startDone; err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := <-resumeDone
	if !errors.Is(got.err, chatsvc.ErrStaleControllerGeneration) {
		t.Fatalf("err = %v, want ErrStaleControllerGeneration after the replacement won the gate", got.err)
	}
	if text := h.conv.sentTexts(); !reflect.DeepEqual(text, []string{"root"}) {
		t.Fatalf("provider received %v, want no release after losing the gate", text)
	}
	if now := durableGeneration(t, h); now == originalGeneration {
		t.Fatalf("durable generation did not change: %q", now)
	}
}

// restartChat runs the production replacement path for the harness session.
func restartChat(t *testing.T, h *harness) (*chatsvc.Controller, error) {
	t.Helper()
	rec, found, err := h.st.GetSession(context.Background(), testSession)
	if err != nil || !found {
		t.Fatalf("read session for restart: found=%v err=%v", found, err)
	}
	providerID := rec.Metadata.ProviderConversationID
	if providerID == "" {
		// The harness's fake driver does not persist the provider handle on the
		// session record; the live controller still knows it.
		providerID = h.ctrl.ProviderConversationID()
	}
	return h.svc.Start(context.Background(), chatsvc.StartConfig{
		SessionID:               testSession,
		ProjectID:               testProject,
		Harness:                 rec.Harness,
		WorkspacePath:           rec.Metadata.WorkspacePath,
		ProviderConversationID:  providerID,
		ExpectedControllerOwner: rec.ControllerOwner(),
	})
}

// turnStateByID reads the durable state of one turn.
func turnStateByID(t *testing.T, h *harness, id string) domain.TurnState {
	t.Helper()
	turn, err := h.st.TurnByID(context.Background(), id)
	if err != nil {
		t.Fatalf("TurnByID %s: %v", id, err)
	}
	return turn.State
}

func durableGeneration(t *testing.T, h *harness) string {
	t.Helper()
	rec, found, err := h.st.GetSession(context.Background(), testSession)
	if err != nil || !found {
		t.Fatalf("read session: found=%v err=%v", found, err)
	}
	return rec.Metadata.ControllerGeneration
}

// TestResumeDeferredStartFailureSettlesTurnAndClearsHold proves a deferred
// StartDeferredTurn failure settles the claimed row failed, keeps the rest of the
// queue, and retires the marker.
func TestResumeDeferredStartFailureSettlesTurnAndClearsHold(t *testing.T) {
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	conv.start = func(string) error { return nil }
	h := newHarnessWithConversation(t, conv)
	first, second := queueTwoBehindFailingRoot(t, h, conv.emit)

	conv.start = func(string) error { return errors.New("provider rejected the deferred prompt") }

	_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
	if !errors.Is(err, chatsvc.ErrQueueDispatchFailed) {
		t.Fatalf("err = %v, want ErrQueueDispatchFailed", err)
	}
	if state := turnStateByID(t, h, first.ID); state != domain.TurnStateFailed {
		t.Fatalf("turn state = %q, want failed", state)
	}
	if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != "" {
		t.Fatalf("one-shot marker survived a deferred start failure: armed for %q", armed)
	}
	// SendTurn prepared the turn (the fake records the preparation) and
	// StartDeferredTurn refused the boundary; the durable outcome is failed.
	if got := conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation"}) {
		t.Fatalf("prepared sends = %v, want the refused deferred turn prepared once", got)
	}
	assertTurnStaysQueued(t, h, "automation2")
	if second.ID == "" {
		t.Fatalf("second queued turn has no id")
	}
}

// TestResumeDeferredSendFailureSettlesTurnAndClearsHold proves a deferred
// SendTurn (prepare) failure settles the claimed row failed and retires the
// marker before any provider boundary.
func TestResumeDeferredSendFailureSettlesTurnAndClearsHold(t *testing.T) {
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	conv.start = func(string) error { return nil }
	h := newHarnessWithConversation(t, conv)
	first, _ := queueTwoBehindFailingRoot(t, h, conv.emit)

	conv.mu.Lock()
	conv.sendErr = errors.New("provider unavailable")
	conv.mu.Unlock()

	_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
	if !errors.Is(err, chatsvc.ErrQueueDispatchFailed) {
		t.Fatalf("err = %v, want ErrQueueDispatchFailed", err)
	}
	if state := turnStateByID(t, h, first.ID); state != domain.TurnStateFailed {
		t.Fatalf("turn state = %q, want failed", state)
	}
	if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != "" {
		t.Fatalf("one-shot marker survived a deferred send failure: armed for %q", armed)
	}
	assertTurnStaysQueued(t, h, "automation2")
}

// TestResumeDeferredBindsBeforeProviderBoundary proves the deferred path binds
// the AO turn to the provider id before StartDeferredTurn crosses the provider
// boundary, which is what lets a synchronous completion be correlated.
func TestResumeDeferredBindsBeforeProviderBoundary(t *testing.T) {
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	conv.start = func(string) error { return nil }
	h := newHarnessWithConversation(t, conv)
	first, _ := queueTwoBehindFailingRoot(t, h, conv.emit)

	boundAtBoundary := false
	conv.start = func(providerTurnID string) error {
		turn, err := h.st.TurnByID(context.Background(), first.ID)
		boundAtBoundary = err == nil && turn.ProviderTurnID == providerTurnID && turn.State == domain.TurnStateRunning
		return nil
	}

	if _, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !boundAtBoundary {
		t.Fatalf("StartDeferredTurn ran before the provider id was durably bound")
	}
	if state := turnStateByID(t, h, first.ID); state != domain.TurnStateRunning {
		t.Fatalf("turn state = %q, want running", state)
	}
}
