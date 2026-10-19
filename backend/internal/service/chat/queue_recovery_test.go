package chat_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// queueWhileRunning sends a message behind an active root turn and returns the
// queued turn. It leaves the controller busy.
func queueWhileRunning(t *testing.T, h *harness, rootClientID, text, clientID string) domain.ConversationTurn {
	t.Helper()
	ctx := context.Background()
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "root", ClientMessageID: rootClientID, Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("send root: %v", err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateRunning
	})
	queued, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: text, ClientMessageID: clientID, Origin: domain.MessageOriginAutomation,
	})
	if err != nil {
		t.Fatalf("queue %s: %v", text, err)
	}
	if queued.State != domain.TurnStateQueued {
		t.Fatalf("queued turn state = %q, want queued", queued.State)
	}
	return queued
}

func failRootTurn(t *testing.T, h *harness) {
	t.Helper()
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateFailed,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateFailed
	})
}

// TestResumeHeldQueueReleasesExactlyOneTurn is the core one-shot guarantee: a
// manual resume dispatches the named never-sent turn and nothing else.
func TestResumeHeldQueueReleasesExactlyOneTurn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	queued := queueWhileRunning(t, h, "c1", "automation", "c2")
	failRootTurn(t, h)
	assertTurnStaysQueued(t, h, "automation")

	result, err := h.svc.ResumeHeldQueue(ctx, testSession, queued.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("ResumeHeldQueue: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeResumed || result.TurnID != queued.ID {
		t.Fatalf("result = %+v, want resumed %s", result, queued.ID)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation"}) {
		t.Fatalf("provider received %v, want exactly root then automation", got)
	}
}

// TestResumeHeldQueueReHoldsAfterTheResumedTurnCompletes proves the one-shot is
// bound to the resumed turn: a completed resume holds the queue instead of
// cascading into the next turn (issue #4861 must not be defeated by a resume).
func TestResumeHeldQueueReHoldsAfterTheResumedTurnCompletes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	queued := queueWhileRunning(t, h, "c1", "automation", "c2")
	// A second queued turn proves the one-shot does not release the rest.
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "automation2", ClientMessageID: "c3", Origin: domain.MessageOriginAutomation,
	}); err != nil {
		t.Fatalf("queue automation2: %v", err)
	}
	failRootTurn(t, h)
	assertTurnStaysQueued(t, h, "automation")

	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, queued.ID, currentGeneration(h)); err != nil {
		t.Fatalf("ResumeHeldQueue: %v", err)
	}
	// The resumed turn is the fake's second provider turn.
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2",
		TurnState: domain.TurnStateCompleted,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["automation"] == domain.TurnStateCompleted
	})
	assertTurnStaysQueued(t, h, "automation2")
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation"}) {
		t.Fatalf("provider received %v, want the resumed turn only", got)
	}
}

// TestOrdinaryCompletedTurnStillDrainsTheQueue guards that the one-shot did not
// change the automatic hold/continue rule for ordinary turns.
func TestOrdinaryCompletedTurnStillDrainsTheQueue(t *testing.T) {
	h := newHarness(t)
	queueWhileRunning(t, h, "c1", "automation", "c2")
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateCompleted,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["automation"] == domain.TurnStateRunning
	})
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation"}) {
		t.Fatalf("provider received %v, want the queue to continue after a completed turn", got)
	}
}

// TestConcurrentResumeDispatchesAtMostOneTurn proves the expectedTurnId
// compare-and-set: a second concurrent request never releases the next turn.
func TestConcurrentResumeDispatchesAtMostOneTurn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	queued := queueWhileRunning(t, h, "c1", "automation", "c2")
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "automation2", ClientMessageID: "c3", Origin: domain.MessageOriginAutomation,
	}); err != nil {
		t.Fatalf("queue automation2: %v", err)
	}
	failRootTurn(t, h)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []chatsvc.QueueResumeOutcome
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := h.svc.ResumeHeldQueue(ctx, testSession, queued.ID, currentGeneration(h))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results = append(results, chatsvc.QueueResumeOutcome("error:"+err.Error()))
				return
			}
			results = append(results, res.Outcome)
		}()
	}
	wg.Wait()

	resumed := 0
	for _, outcome := range results {
		if outcome == chatsvc.QueueResumeResumed {
			resumed++
		}
	}
	if resumed != 1 {
		t.Fatalf("resumed outcomes = %d (%v), want exactly 1", resumed, results)
	}
	// Only the named turn was released; automation2 must stay queued.
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation"}) {
		t.Fatalf("provider received %v, want only the named turn", got)
	}
}

func TestResumeHeldQueueStaleHeadIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	queueWhileRunning(t, h, "c1", "automation", "c2")
	failRootTurn(t, h)

	result, err := h.svc.ResumeHeldQueue(ctx, testSession, "not-the-head", currentGeneration(h))
	if err != nil {
		t.Fatalf("ResumeHeldQueue: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeStaleQueueState {
		t.Fatalf("outcome = %q, want stale_queue_state", result.Outcome)
	}
	if got := h.conv.sentTexts(); len(got) != 1 {
		t.Fatalf("provider received %v, want no release", got)
	}
}

func TestResumeHeldQueueNoPendingWork(t *testing.T) {
	h := newHarness(t)
	result, err := h.svc.ResumeHeldQueue(context.Background(), testSession, "any-turn", currentGeneration(h))
	if err != nil {
		t.Fatalf("ResumeHeldQueue: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeNoPendingWork {
		t.Fatalf("outcome = %q, want no_pending_work", result.Outcome)
	}
}

func TestResumeHeldQueueBusyIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	queued := queueWhileRunning(t, h, "c1", "automation", "c2")
	// The root turn is still running.
	result, err := h.svc.ResumeHeldQueue(ctx, testSession, queued.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("ResumeHeldQueue: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeSessionBusy {
		t.Fatalf("outcome = %q, want session_busy", result.Outcome)
	}
	if got := h.conv.sentTexts(); len(got) != 1 {
		t.Fatalf("provider received %v, want no release while busy", got)
	}
}

func TestResumeHeldQueueRequiresExpectedTurnID(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, "  ", currentGeneration(h))
	if !errors.Is(err, chatsvc.ErrResumeTurnIDRequired) {
		t.Fatalf("err = %v, want ErrResumeTurnIDRequired", err)
	}
}

// TestResumeHeldQueueRefusesUnconfirmedRunningTurn proves an unbound running row
// (dispatch started, delivery unconfirmed) blocks the release.
func TestResumeHeldQueueRefusesUnconfirmedRunningTurn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	conversationID := h.ctrl.ConversationID()
	generation := h.ctrl.Generation()
	now := h.now()
	if _, err := h.st.AppendUserMessage(ctx, conversationID, testSession, generation, domain.ConversationMessage{
		ID: "unbound-message", Text: "in flight", Origin: domain.MessageOriginHuman,
	}, "unbound-turn", now); err != nil {
		t.Fatalf("AppendUserMessage: %v", err)
	}
	if err := h.st.MarkTurnDispatching(ctx, "unbound-turn"); err != nil {
		t.Fatalf("MarkTurnDispatching: %v", err)
	}
	result, err := h.svc.ResumeHeldQueue(ctx, testSession, "unbound-turn", currentGeneration(h))
	if err != nil {
		t.Fatalf("ResumeHeldQueue: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeStateUncertain {
		t.Fatalf("outcome = %q, want state_uncertain", result.Outcome)
	}
}

// TestSnapshotReportsHeldQueue proves the derived observable state distinguishes
// a held queue from an idle session and points at the resumable head.
func TestSnapshotReportsHeldQueue(t *testing.T) {
	h := newHarness(t)
	queued := queueWhileRunning(t, h, "c1", "automation", "c2")
	failRootTurn(t, h)
	assertTurnStaysQueued(t, h, "automation")

	snapshot, err := h.svc.Snapshot(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Queue.State != chatsvc.QueueStateHeldAfterFailure {
		t.Fatalf("queue state = %q, want held_after_terminal_failure", snapshot.Queue.State)
	}
	if snapshot.Queue.PendingCount != 1 || snapshot.Queue.HeadTurnID != queued.ID {
		t.Fatalf("queue = %+v, want one pending head %s", snapshot.Queue, queued.ID)
	}
	if snapshot.Queue.ResumeSafety != chatsvc.QueueResumeResumed {
		t.Fatalf("resume safety = %q, want resumed", snapshot.Queue.ResumeSafety)
	}
	if !snapshot.Queue.ControllerAvailable {
		t.Fatalf("controller available = false, want true")
	}
	if snapshot.Queue.ActiveTurnID != "" {
		t.Fatalf("active turn = %q, want none", snapshot.Queue.ActiveTurnID)
	}
}

// TestSnapshotReportsIdleQueue proves an empty, idle session is not labelled as
// blocked or held.
func TestSnapshotReportsIdleQueue(t *testing.T) {
	h := newHarness(t)
	snapshot, err := h.svc.Snapshot(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Queue.State != chatsvc.QueueStateIdle {
		t.Fatalf("queue state = %q, want idle", snapshot.Queue.State)
	}
	if snapshot.Queue.ResumeSafety != chatsvc.QueueResumeNoPendingWork {
		t.Fatalf("resume safety = %q, want no_pending_work", snapshot.Queue.ResumeSafety)
	}
}

// TestSnapshotReportsActiveTurnKeepsQueueWaiting distinguishes queued work behind
// a live turn from a held failure.
func TestSnapshotReportsActiveTurnKeepsQueueWaiting(t *testing.T) {
	h := newHarness(t)
	queueWhileRunning(t, h, "c1", "automation", "c2")
	snapshot, err := h.svc.Snapshot(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Queue.State != chatsvc.QueueStateWaitingForActiveTurn {
		t.Fatalf("queue state = %q, want queued_waiting_for_active_turn", snapshot.Queue.State)
	}
	if snapshot.Queue.ResumeSafety != chatsvc.QueueResumeSessionBusy {
		t.Fatalf("resume safety = %q, want session_busy", snapshot.Queue.ResumeSafety)
	}
}
