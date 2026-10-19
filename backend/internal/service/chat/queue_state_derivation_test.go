package chat_test

import (
	"context"
	"testing"

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
