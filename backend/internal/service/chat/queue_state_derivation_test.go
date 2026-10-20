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
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func snapshotQueue(t *testing.T, h *harness) *chatsvc.QueueState {
	t.Helper()
	snapshot, err := h.svc.Snapshot(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snapshot.Queue
}

// TestSnapshotReportsFreshQueueNotHeld proves a queue that has not yet had its
// chance to dispatch is not labelled as held after a failure. There is no
// terminal turn at all, so there is nothing that withheld the work.
func TestSnapshotReportsFreshQueueNotHeld(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	created, err := h.st.AppendUserMessage(ctx, h.ctrl.ConversationID(), testSession, currentGeneration(h),
		domain.ConversationMessage{
			ID: "message-fresh", Origin: domain.MessageOriginAutomation, Text: "fresh", ClientMessageID: "client-fresh",
		}, "turn-fresh", h.now())
	if err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}

	queue := snapshotQueue(t, h)
	if queue.State == chatsvc.QueueStateHeldAfterFailure {
		t.Fatalf("a freshly queued turn was labelled held_after_terminal_failure: %+v", queue)
	}
	if queue.State != chatsvc.QueueStateWaitingForActiveTurn {
		t.Fatalf("state = %q, want queued_waiting_for_active_turn", queue.State)
	}
	if queue.ResumeSafety != chatsvc.QueueResumeResumed {
		t.Fatalf("resume safety = %q, want resumed", queue.ResumeSafety)
	}
}

// TestSnapshotReportsCompletedTerminalNotHeld proves a completed terminal turn
// with pending work is not labelled held: a completion drains, it does not hold.
func TestSnapshotReportsCompletedTerminalNotHeld(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	convID := h.ctrl.ConversationID()
	gen := currentGeneration(h)
	now := h.now()
	if created, err := h.st.AppendUserMessage(ctx, convID, testSession, gen, domain.ConversationMessage{
		ID: "message-done", Origin: domain.MessageOriginHuman, Text: "done", ClientMessageID: "client-done",
	}, "turn-done", now); err != nil || !created {
		t.Fatalf("AppendUserMessage done: created=%v err=%v", created, err)
	}
	if err := h.st.BindTurnToProvider(ctx, "turn-done", "provider-done", now); err != nil {
		t.Fatalf("BindTurnToProvider: %v", err)
	}
	if err := h.st.SettleTurnByID(ctx, "turn-done", domain.TurnStateCompleted, "", now); err != nil {
		t.Fatalf("SettleTurnByID: %v", err)
	}
	if created, err := h.st.AppendUserMessage(ctx, convID, testSession, gen, domain.ConversationMessage{
		ID: "message-pending", Origin: domain.MessageOriginAutomation, Text: "pending", ClientMessageID: "client-pending",
	}, "turn-pending", now); err != nil || !created {
		t.Fatalf("AppendUserMessage pending: created=%v err=%v", created, err)
	}

	queue := snapshotQueue(t, h)
	if queue.State == chatsvc.QueueStateHeldAfterFailure {
		t.Fatalf("a completed terminal turn was treated as a hold: %+v", queue)
	}
	if queue.State != chatsvc.QueueStateWaitingForActiveTurn {
		t.Fatalf("state = %q, want queued_waiting_for_active_turn", queue.State)
	}
}

// TestSnapshotReportsCompaction proves an in-flight compaction is reported as
// compacting, not as a held failure, even with pending work.
func TestSnapshotReportsCompaction(t *testing.T) {
	conv := newCompactingConversation()
	h := newHarnessWithConversation(t, conv)
	ctx := context.Background()

	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "root", ClientMessageID: "c1", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("send root: %v", err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateRunning
	})
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "automation", ClientMessageID: "c2", Origin: domain.MessageOriginAutomation,
	}); err != nil {
		t.Fatalf("queue automation: %v", err)
	}
	conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateFailed,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateFailed
	})
	if _, err := h.svc.Compact(ctx, testSession); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	queue := snapshotQueue(t, h)
	if queue.State != chatsvc.QueueStateCompacting {
		t.Fatalf("state = %q, want compacting", queue.State)
	}
	if queue.ResumeSafety != chatsvc.QueueResumeCompactionInProgress {
		t.Fatalf("resume safety = %q, want compaction_in_progress", queue.ResumeSafety)
	}
	if !queue.CompactionPending {
		t.Fatalf("compactionPending = false, want true")
	}
}

// TestSnapshotReportsHandoff proves an armed handoff wins over other states.
func TestSnapshotReportsHandoff(t *testing.T) {
	h := newHarness(t)
	first, _ := queueWhileRunningTwice(t, h)
	if err := h.ctrl.ArmHandoff(context.Background(), domain.SessionInterfaceTransitionDrain); err != nil {
		t.Fatalf("ArmHandoff: %v", err)
	}

	queue := snapshotQueue(t, h)
	if queue.State != chatsvc.QueueStateHandoffInProgress {
		t.Fatalf("state = %q, want handoff_in_progress", queue.State)
	}
	if queue.ResumeSafety != chatsvc.QueueResumeHandoffInProgress {
		t.Fatalf("resume safety = %q, want handoff_in_progress", queue.ResumeSafety)
	}
	if queue.HeadTurnID != first.ID {
		t.Fatalf("head = %q, want %s", queue.HeadTurnID, first.ID)
	}
}

// TestSnapshotReportsControllerAbsent proves pending work with no live
// controller asks for recovery, not a held failure.
func TestSnapshotReportsControllerAbsent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	queueWhileRunningTwice(t, h)
	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop closes the controller's own work; queue fresh work afterwards so the
	// session has pending turns with no controller to dispatch them.
	if created, err := h.st.AppendUserMessage(ctx, h.ctrl.ConversationID(), testSession, currentGeneration(h),
		domain.ConversationMessage{
			ID: "message-after-stop", Origin: domain.MessageOriginAutomation, Text: "after stop", ClientMessageID: "client-after-stop",
		}, "turn-after-stop", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage after Stop: created=%v err=%v", created, err)
	}

	queue := snapshotQueue(t, h)
	if queue.ControllerAvailable {
		t.Fatalf("controller available = true after Stop")
	}
	if queue.State != chatsvc.QueueStateControllerRecoveryRequired {
		t.Fatalf("state = %q, want controller_recovery_required", queue.State)
	}
	if queue.ResumeSafety != chatsvc.QueueResumeControllerRecoveryRequired {
		t.Fatalf("resume safety = %q, want controller_recovery_required", queue.ResumeSafety)
	}
}

// TestSnapshotHandoffBeatsRunningTurn proves the priority order: a handoff armed
// while a turn is in flight is reported as handoff, not as a busy queue.
func TestSnapshotHandoffBeatsRunningTurn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	queueWhileRunning(t, h, "c1", "automation", "c2")

	if err := h.ctrl.ArmHandoff(ctx, domain.SessionInterfaceTransitionDrain); err != nil {
		t.Fatalf("ArmHandoff: %v", err)
	}
	queue := snapshotQueue(t, h)
	if queue.State != chatsvc.QueueStateHandoffInProgress {
		t.Fatalf("state = %q, want handoff_in_progress", queue.State)
	}
}

// TestSnapshotHistoricalFailureDoesNotHoldPendingWork proves a failure that has
// been superseded by a later completion does not keep classifying pending work as
// held. The queue head is a normal turn waiting for its turn, not a held one.
func TestSnapshotHistoricalFailureDoesNotHoldPendingWork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second := queueWhileRunningTwice(t, h)

	// Release and complete the first turn after the historical root failure.
	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2",
		TurnState: domain.TurnStateCompleted,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["automation"] == domain.TurnStateCompleted
	})

	queue := snapshotQueue(t, h)
	if queue.State == chatsvc.QueueStateHeldAfterFailure {
		t.Fatalf("a superseded failure still labelled pending work as held: %+v", queue)
	}
	// The turn that completed was released by an explicit resume, so the remaining
	// work is held by the one-shot rule, not by a historical failure.
	if queue.State != chatsvc.QueueStateHeldAfterManualResume {
		t.Fatalf("state = %q, want held_after_manual_resume", queue.State)
	}
	if queue.HeadTurnID != second.ID {
		t.Fatalf("head = %q, want %s", queue.HeadTurnID, second.ID)
	}
	if queue.ResumeSafety != chatsvc.QueueResumeResumed {
		t.Fatalf("resume safety = %q, want resumed", queue.ResumeSafety)
	}
}

// waitRetainedQueue waits until the controller records the manual-resume
// retention for a turn.
func waitRetainedQueue(t *testing.T, h *harness, turnID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.ctrl.RetainedQueueTurnIDForTest() == turnID {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("manual-resume retention = %q, want %s", h.ctrl.RetainedQueueTurnIDForTest(), turnID)
}

// reviveFakeConversation gives the harness provider double a fresh event stream
// so a real Start can install a live replacement after Stop closed it.
func reviveFakeConversation(h *harness) {
	h.conv.mu.Lock()
	h.conv.events = make(chan ports.ChatEvent, 64)
	h.conv.mu.Unlock()
	h.conv.closeOnce = sync.Once{}
	h.conv.closeSignalOnce = sync.Once{}
}

// TestSnapshotHeldAfterManualResumeLifecycle proves a completed one-shot with
// queued work left reads as held_after_manual_resume (not as waiting on a phantom
// active turn), clears on the next explicit resume, and reappears/ends correctly.
func TestSnapshotHeldAfterManualResumeLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second := queueWhileRunningTwice(t, h)

	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
		t.Fatalf("first resume: %v", err)
	}
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2",
		TurnState: domain.TurnStateCompleted,
	})
	waitTurnState(t, h, first.ID, domain.TurnStateCompleted)
	waitRetainedQueue(t, h, first.ID)

	queue := snapshotQueue(t, h)
	if queue.State != chatsvc.QueueStateHeldAfterManualResume {
		t.Fatalf("state = %q, want held_after_manual_resume", queue.State)
	}
	if queue.ActiveTurnID != "" {
		t.Fatalf("active turn = %q, want none", queue.ActiveTurnID)
	}
	if queue.HeadTurnID != second.ID || queue.PendingCount != 1 {
		t.Fatalf("queue = %+v, want head %s and one pending", queue, second.ID)
	}
	if queue.ResumeSafety != chatsvc.QueueResumeResumed {
		t.Fatalf("resume safety = %q, want resumed", queue.ResumeSafety)
	}
	assertTurnStaysQueued(t, h, "automation2")

	// Queue a third turn while the queue is retained (inserted directly so the
	// snapshot sees queued work without an automatic dispatch).
	if created, err := h.st.AppendUserMessage(ctx, h.ctrl.ConversationID(), testSession, currentGeneration(h),
		domain.ConversationMessage{
			ID: "message-third", Origin: domain.MessageOriginAutomation, Text: "automation3", ClientMessageID: "client-third",
		}, "turn-third", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage third: created=%v err=%v", created, err)
	}

	// A new explicit resume retires the retention and runs the next turn.
	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, second.ID, currentGeneration(h)); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if retained := h.ctrl.RetainedQueueTurnIDForTest(); retained != "" {
		t.Fatalf("retention survived a new resume: %q", retained)
	}
	queue = snapshotQueue(t, h)
	if queue.State != chatsvc.QueueStateWaitingForActiveTurn {
		t.Fatalf("state while running = %q, want queued_waiting_for_active_turn", queue.State)
	}
	if queue.ResumeSafety != chatsvc.QueueResumeSessionBusy {
		t.Fatalf("resume safety while running = %q, want session_busy", queue.ResumeSafety)
	}
	if queue.ActiveTurnID != second.ID {
		t.Fatalf("active turn = %q, want %s", queue.ActiveTurnID, second.ID)
	}

	// Completing it with another turn still queued retains again.
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-3",
		TurnState: domain.TurnStateCompleted,
	})
	waitTurnState(t, h, second.ID, domain.TurnStateCompleted)
	waitRetainedQueue(t, h, second.ID)
	queue = snapshotQueue(t, h)
	if queue.State != chatsvc.QueueStateHeldAfterManualResume {
		t.Fatalf("state after the second one-shot = %q, want held_after_manual_resume", queue.State)
	}
	if queue.HeadTurnID != "turn-third" {
		t.Fatalf("head = %q, want turn-third", queue.HeadTurnID)
	}

	// The last explicit resume drains the final turn; with nothing queued the
	// session is idle.
	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, "turn-third", currentGeneration(h)); err != nil {
		t.Fatalf("third resume: %v", err)
	}
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-4",
		TurnState: domain.TurnStateCompleted,
	})
	waitTurnState(t, h, "turn-third", domain.TurnStateCompleted)
	waitResumeHoldCleared(t, h)
	if queue := snapshotQueue(t, h); queue.State != chatsvc.QueueStateIdle {
		t.Fatalf("state after the last turn = %q, want idle", queue.State)
	}
}

// TestSnapshotOneShotFailureKeepsTerminalFailureReason proves a failed or
// interrupted one-shot reports held_after_terminal_failure, not the weaker
// manual-resume retention.
func TestSnapshotOneShotFailureKeepsTerminalFailureReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state domain.TurnState
		err   error
	}{
		{"failed", domain.TurnStateFailed, errors.New("provider failed")},
		{"interrupted", domain.TurnStateInterrupted, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			first, _ := queueWhileRunningTwice(t, h)
			if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
				t.Fatalf("resume: %v", err)
			}
			h.conv.emit(ports.ChatEvent{
				Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2",
				TurnState: tc.state, Err: tc.err,
			})
			waitTurnState(t, h, first.ID, tc.state)
			waitRetainedQueue(t, h, first.ID)
			if queue := snapshotQueue(t, h); queue.State != chatsvc.QueueStateHeldAfterFailure {
				t.Fatalf("state = %q, want held_after_terminal_failure", queue.State)
			}
		})
	}
}

// TestSnapshotNormalCompletedTurnNeverRetains proves the automatic drain is
// unaffected: a normal completion releases the queued turn and never creates a
// manual-resume hold.
func TestSnapshotNormalCompletedTurnNeverRetains(t *testing.T) {
	h := newHarness(t)
	// A root turn is running and one automation turn is queued behind it.
	queueWhileRunning(t, h, "c1", "automation", "c2")
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateCompleted,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["automation"] == domain.TurnStateRunning
	})
	if retained := h.ctrl.RetainedQueueTurnIDForTest(); retained != "" {
		t.Fatalf("normal completion created a retention: %q", retained)
	}
	if queue := snapshotQueue(t, h); queue.State == chatsvc.QueueStateHeldAfterManualResume {
		t.Fatalf("normal completion reported held_after_manual_resume")
	}
}

// TestSnapshotReplacementDoesNotInheritManualResumeRetention proves the retention
// is controller-local: Stop/Terminate clears it and a replacement controller
// starts without inheriting it.
func TestSnapshotReplacementDoesNotInheritManualResumeRetention(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, _ := queueWhileRunningTwice(t, h)
	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2",
		TurnState: domain.TurnStateCompleted,
	})
	waitTurnState(t, h, first.ID, domain.TurnStateCompleted)
	waitRetainedQueue(t, h, first.ID)

	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if retained := h.ctrl.RetainedQueueTurnIDForTest(); retained != "" {
		t.Fatalf("Stop did not clear the retention: %q", retained)
	}

	reviveFakeConversation(h)
	if _, err := h.svc.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		WorkspacePath: t.TempDir(), ProviderConversationID: h.ctrl.ProviderConversationID(),
	}); err != nil {
		t.Fatalf("Start replacement: %v", err)
	}
	replacement, err := h.svc.Controller(testSession)
	if err != nil {
		t.Fatalf("Controller: %v", err)
	}
	if retained := replacement.RetainedQueueTurnIDForTest(); retained != "" {
		t.Fatalf("replacement inherited a retention: %q", retained)
	}
	if created, err := h.st.AppendUserMessage(ctx, h.ctrl.ConversationID(), testSession, replacement.Generation(),
		domain.ConversationMessage{
			ID: "message-after-restart", Origin: domain.MessageOriginAutomation, Text: "after restart", ClientMessageID: "client-after-restart",
		}, "turn-after-restart", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}
	if queue := snapshotQueue(t, h); queue.State == chatsvc.QueueStateHeldAfterManualResume {
		t.Fatalf("replacement reported held_after_manual_resume")
	}
}

// TestManualResumeRetentionClearedByHandoffAndClose proves the retention is
// retired when the controller stops serving the queue: an armed handoff and a
// controller close both clear it.
func TestManualResumeRetentionClearedByHandoffAndClose(t *testing.T) {
	setup := func(t *testing.T) (*harness, context.Context, domain.ConversationTurn) {
		t.Helper()
		h := newHarness(t)
		ctx := context.Background()
		first, _ := queueWhileRunningTwice(t, h)
		if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
			t.Fatalf("resume: %v", err)
		}
		h.conv.emit(ports.ChatEvent{
			Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2",
			TurnState: domain.TurnStateCompleted,
		})
		waitTurnState(t, h, first.ID, domain.TurnStateCompleted)
		waitRetainedQueue(t, h, first.ID)
		return h, ctx, first
	}

	t.Run("handoff", func(t *testing.T) {
		h, ctx, _ := setup(t)
		if err := h.ctrl.ArmHandoff(ctx, domain.SessionInterfaceTransitionDrain); err != nil {
			t.Fatalf("ArmHandoff: %v", err)
		}
		if retained := h.ctrl.RetainedQueueTurnIDForTest(); retained != "" {
			t.Fatalf("handoff did not clear the retention: %q", retained)
		}
		if queue := snapshotQueue(t, h); queue.State != chatsvc.QueueStateHandoffInProgress {
			t.Fatalf("state = %q, want handoff_in_progress", queue.State)
		}
	})

	t.Run("close", func(t *testing.T) {
		h, ctx, _ := setup(t)
		if err := h.ctrl.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if retained := h.ctrl.RetainedQueueTurnIDForTest(); retained != "" {
			t.Fatalf("close did not clear the retention: %q", retained)
		}
	})
}
