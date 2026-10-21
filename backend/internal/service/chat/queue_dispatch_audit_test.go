package chat_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// failedTurnID returns the first failed turn, used as a retry source.
func failedTurnID(t *testing.T, h *harness) string {
	t.Helper()
	snapshot, err := h.st.LoadConversationSnapshot(context.Background(), h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("LoadConversationSnapshot: %v", err)
	}
	for _, turn := range snapshot.Turns {
		if turn.State == domain.TurnStateFailed {
			return turn.ID
		}
	}
	t.Fatalf("no failed turn to retry")
	return ""
}

// TestSendJoinsExistingDurableBacklog proves the historical jump is closed: a
// new Send while the controller is idle but a durable queued head exists is
// appended behind that head and never dispatched directly.
func TestSendJoinsExistingDurableBacklog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if created, err := h.st.AppendUserMessage(ctx, h.ctrl.ConversationID(), testSession, currentGeneration(h),
		domain.ConversationMessage{
			ID: "message-existing", Origin: domain.MessageOriginHuman, Text: "existing", ClientMessageID: "client-existing",
		}, "turn-existing", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}

	turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "new", ClientMessageID: "client-new", Origin: domain.MessageOriginHuman,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if turn.State != domain.TurnStateQueued {
		t.Fatalf("new turn state = %q, want queued behind the durable head", turn.State)
	}
	if h.conv.sendCallCount() != 0 {
		t.Fatalf("provider calls = %d, want 0 (the head belongs to the drain)", h.conv.sendCallCount())
	}
	if head, err := h.st.NextQueuedTurn(ctx, h.ctrl.ConversationID()); err != nil || head.TurnID != "turn-existing" {
		t.Fatalf("head = %+v err=%v, want turn-existing", head, err)
	}
	if queue := snapshotQueue(t, h); queue.HeadTurnID != "turn-existing" || queue.PendingCount != 2 {
		t.Fatalf("queue = %+v, want head turn-existing and two pending", queue)
	}
}

// TestSendJoinsTerminalBacklog proves a failure/recovered/interrupted hold is
// respected: a new Send joins the queued head rather than dispatching in front.
func TestSendJoinsTerminalBacklog(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state domain.TurnState
		err   error
	}{
		{"failed", domain.TurnStateFailed, errProviderFailed},
		{"recovered", domain.TurnStateRecovered, nil},
		{"interrupted", domain.TurnStateInterrupted, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			first := queueWhileRunning(t, h, "c1", "queued-head", "c2")
			h.conv.emit(ports.ChatEvent{
				Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
				TurnState: tc.state, Err: tc.err,
			})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return turnStateByText(t, s)["root"] == tc.state
			})

			turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
				Text: "new", ClientMessageID: "c3", Origin: domain.MessageOriginHuman,
			})
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if turn.State != domain.TurnStateQueued {
				t.Fatalf("new turn state = %q, want queued", turn.State)
			}
			if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
				t.Fatalf("provider received %v, want no new dispatch", got)
			}
			if queue := snapshotQueue(t, h); queue.HeadTurnID != first.ID {
				t.Fatalf("head = %q, want %s", queue.HeadTurnID, first.ID)
			}
		})
	}
}

// errProviderFailed is a plain provider failure used to settle a turn failed.
var errProviderFailed = &providerFailure{"provider failed"}

type providerFailure struct{ message string }

func (e *providerFailure) Error() string { return e.message }

// TestRetryTurnQueuesBehindRetainedBacklog proves a retry joins the retained
// backlog instead of dispatching ahead of its head, and does not clear the
// retention.
func TestRetryTurnQueuesBehindRetainedBacklog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second, third := setupRetainedBacklog(t, h)
	source := failedTurnID(t, h)
	before := h.conv.sentTexts()

	retry, err := h.svc.RetryTurn(ctx, testSession, source)
	if err != nil {
		t.Fatalf("RetryTurn: %v", err)
	}
	if retry.State != domain.TurnStateQueued {
		t.Fatalf("retry state = %q, want queued behind the retained head", retry.State)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, before) {
		t.Fatalf("provider received %v after a retained retry, want no dispatch", got)
	}
	if retained := h.ctrl.RetainedQueueTurnIDForTest(); retained != first.ID {
		t.Fatalf("retention = %q, want %s", retained, first.ID)
	}
	if head, err := h.st.NextQueuedTurn(ctx, h.ctrl.ConversationID()); err != nil || head.TurnID != second.ID {
		t.Fatalf("head = %+v err=%v, want %s", head, err, second.ID)
	}
	for _, turn := range []domain.ConversationTurn{second, third, retry} {
		if state := turnStateByID(t, h, turn.ID); state != domain.TurnStateQueued {
			t.Fatalf("turn %s state = %q, want queued", turn.ID, state)
		}
	}

	// The explicit resume releases the original head, not the retry.
	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, second.ID, currentGeneration(h)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got := h.conv.sentTexts()
	if got[len(got)-1] != "automation2" {
		t.Fatalf("provider received %v, want the original head released", got)
	}
}

// TestSteerOrSendDuringRetentionQueuesBehindHead covers the queueWhenBusy=false
// fallback: with no active turn it must join the retained backlog, not dispatch.
func TestSteerOrSendDuringRetentionQueuesBehindHead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, second, _ := setupRetainedBacklog(t, h)
	before := h.conv.sentTexts()

	result, err := h.svc.SteerOrSend(ctx, testSession, ports.ChatUserMessage{
		Text: "steer-new", ClientMessageID: "c5", Origin: domain.MessageOriginHuman,
	}, false)
	if err != nil {
		t.Fatalf("SteerOrSend: %v", err)
	}
	if result.Steered {
		t.Fatalf("steer with no active turn steered instead of queueing")
	}
	if result.Turn.State != domain.TurnStateQueued {
		t.Fatalf("result turn state = %q, want queued", result.Turn.State)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, before) {
		t.Fatalf("provider received %v after SteerOrSend, want no dispatch", got)
	}
	if queue := snapshotQueue(t, h); queue.HeadTurnID != second.ID {
		t.Fatalf("head = %q, want %s", queue.HeadTurnID, second.ID)
	}
}

// TestReorderRetainedBacklogKeepsRetention proves reordering changes the head
// without dispensing with the retention or auto-dispatching the new head.
func TestReorderRetainedBacklogKeepsRetention(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second, third := setupRetainedBacklog(t, h)
	before := h.conv.sentTexts()

	if err := h.svc.ReorderQueuedTurns(ctx, testSession, []string{third.ID, second.ID}); err != nil {
		t.Fatalf("ReorderQueuedTurns: %v", err)
	}
	if retained := h.ctrl.RetainedQueueTurnIDForTest(); retained != first.ID {
		t.Fatalf("retention = %q, want %s", retained, first.ID)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, before) {
		t.Fatalf("provider received %v after a reorder, want no dispatch", got)
	}
	queue := snapshotQueue(t, h)
	if queue.State != chatsvc.QueueStateHeldAfterManualResume || queue.HeadTurnID != third.ID {
		t.Fatalf("queue = %+v, want held_after_manual_resume with head %s", queue, third.ID)
	}

	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, third.ID, currentGeneration(h)); err != nil {
		t.Fatalf("resume new head: %v", err)
	}
	got := h.conv.sentTexts()
	if got[len(got)-1] != "automation3" {
		t.Fatalf("provider received %v, want the reordered head released", got)
	}
}

// TestEditQueuedTurnDuringRetentionDoesNotDispatch proves editing a retained
// queued turn does not dispatch or clear the retention.
func TestEditQueuedTurnDuringRetentionDoesNotDispatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second, _ := setupRetainedBacklog(t, h)
	before := h.conv.sentTexts()

	if err := h.svc.EditQueuedTurn(ctx, testSession, second.ID, chatsvc.QueuedMessageEdit{
		Text: "edited automation2",
	}); err != nil {
		t.Fatalf("EditQueuedTurn: %v", err)
	}
	if retained := h.ctrl.RetainedQueueTurnIDForTest(); retained != first.ID {
		t.Fatalf("retention = %q, want %s", retained, first.ID)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, before) {
		t.Fatalf("provider received %v after an edit, want no dispatch", got)
	}
	if queue := snapshotQueue(t, h); queue.HeadTurnID != second.ID {
		t.Fatalf("head = %q, want %s", queue.HeadTurnID, second.ID)
	}
}

// TestPromoteDuringRetentionDoesNotDispatch proves promotion cannot run with no
// active turn and never releases the retained backlog.
func TestPromoteDuringRetentionDoesNotDispatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second, _ := setupRetainedBacklog(t, h)
	before := h.conv.sentTexts()

	if _, err := h.svc.PromoteQueuedTurn(ctx, testSession, second.ID); err == nil {
		t.Fatalf("PromoteQueuedTurn succeeded with no active turn")
	}
	if retained := h.ctrl.RetainedQueueTurnIDForTest(); retained != first.ID {
		t.Fatalf("retention = %q, want %s", retained, first.ID)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, before) {
		t.Fatalf("provider received %v after a refused promote, want no dispatch", got)
	}
}

// TestAutomaticDrainBlockedDuringRetention proves an unrelated provider
// completion that would normally drain the queue cannot pass the retained head.
func TestAutomaticDrainBlockedDuringRetention(t *testing.T) {
	h := newHarness(t)
	_, second, _ := setupRetainedBacklog(t, h)
	before := h.conv.sentTexts()

	// An out-of-band provider turn (adopted, then completed) would normally drain.
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-99"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) > 0 })
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-99",
		TurnState: domain.TurnStateCompleted,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		for _, turn := range s.Turns {
			if turn.ProviderTurnID == "provider-turn-99" && turn.State == domain.TurnStateCompleted {
				return true
			}
		}
		return false
	})
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, before) {
		t.Fatalf("provider received %v, want the retained head left queued", got)
	}
	if state := turnStateByID(t, h, second.ID); state != domain.TurnStateQueued {
		t.Fatalf("retained head state = %q, want queued", state)
	}
}
