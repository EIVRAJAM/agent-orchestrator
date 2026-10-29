package chat_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func deferredNotStartedError() error {
	return fmt.Errorf("%w: prepared turn missing", ports.ErrDeferredTurnNotStarted)
}

func TestDeferredNotStartedSettlementSuccess(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	conv.start = func(string) error {
		*startCalls++
		return deferredNotStartedError()
	}
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store { return st })
	_, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "not started settled", ClientMessageID: "not-started-settled",
	})
	if !errors.Is(err, ports.ErrDeferredTurnNotStarted) {
		t.Fatalf("Send error = %v, want ErrDeferredTurnNotStarted", err)
	}
	if *startCalls != 1 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want 1/0", *startCalls, *discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "not-started-settled"))
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateFailed || turn.CompletedAt == nil || turn.ProviderTurnID == "" {
		t.Fatalf("settled turn = %+v, want failed with completed_at and provider id", turn)
	}
	if pending := h.ctrl.PendingTurnIDForTest(); pending != "" {
		t.Fatalf("pendingTurnID = %q, want cleared after settlement", pending)
	}
	assertNoDispatchingTurn(t, h)
	if created, err := h.st.AppendUserMessage(context.Background(), h.ctrl.ConversationID(), testSession,
		currentGeneration(h), domain.ConversationMessage{
			ID: "message-after-not-started", Origin: domain.MessageOriginHuman,
			Text: "must stay queued", ClientMessageID: "after-not-started",
		}, "turn-after-not-started", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}
	if got := turnStateByID(t, h, "turn-after-not-started"); got != domain.TurnStateQueued {
		t.Fatalf("next queued = %q, want queued", got)
	}
	if got := conv.sentTexts(); len(got) != 1 || got[0] != "not started settled" {
		t.Fatalf("provider received %v, want only the one prepared prompt", got)
	}
}

func TestDeferredNotStartedSettlementFailureRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	conv.start = func(string) error {
		*startCalls++
		return deferredNotStartedError()
	}
	cleanup := newCleanupFailureStore(false)
	close(cleanup.stopRelease)
	cleanup.settleErr = errors.New("injected not-started settlement failure")
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	_, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "not started settlement fails", ClientMessageID: "not-started-fails",
	})
	if !errors.Is(err, chatsvc.ErrDeferredDispatchCleanupIncomplete) {
		t.Fatalf("Send error = %v, want ErrDeferredDispatchCleanupIncomplete", err)
	}
	if !errors.Is(err, ports.ErrDeferredTurnNotStarted) {
		t.Fatalf("Send error = %v, want wrapped ErrDeferredTurnNotStarted", err)
	}
	if *startCalls != 1 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want 1/0", *startCalls, *discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "not-started-fails"))
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID == "" || turn.StartedAt == nil ||
		turn.CompletedAt != nil {
		t.Fatalf("turn after failed settlement = %+v, want running/prepared without completion", turn)
	}
	pending := h.ctrl.PendingTurnIDForTest()
	if pending == "" || pending != turn.ProviderTurnID {
		t.Fatalf("pendingTurnID = %q, want retained provider id %q", pending, turn.ProviderTurnID)
	}
	ids := h.ctrl.DispatchingTurnIDs()
	if len(ids) < 1 || ids[0] != turnIDByClientID(t, h, "not-started-fails") {
		t.Fatalf("sending tracking = %v, want retained turn", ids)
	}
	if !h.ctrl.Busy() {
		t.Fatal("controller is not busy after a failed not-started settlement")
	}
	if created, err := h.st.AppendUserMessage(context.Background(), h.ctrl.ConversationID(), testSession,
		currentGeneration(h), domain.ConversationMessage{
			ID: "message-after-failed-not-started", Origin: domain.MessageOriginHuman,
			Text: "must stay queued after failed settlement", ClientMessageID: "after-failed-not-started",
		}, "turn-after-failed-not-started", h.now()); err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}
	if got := turnStateByID(t, h, "turn-after-failed-not-started"); got != domain.TurnStateQueued {
		t.Fatalf("next queued = %q, want queued", got)
	}
	if got := conv.sentTexts(); len(got) != 1 {
		t.Fatalf("provider received %v, want no retry or next dispatch", got)
	}
}

func TestDeferredPreclaimedNotStartedSettlementSuccess(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store { return st })
	first, second := queueTwoBehindFailingRoot(t, h, conv.emit)
	*startCalls, *discarded = 0, 0
	conv.start = func(string) error {
		*startCalls++
		return deferredNotStartedError()
	}
	_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
	if !errors.Is(err, chatsvc.ErrQueueDispatchFailed) {
		t.Fatalf("resume error = %v, want ErrQueueDispatchFailed", err)
	}
	if *startCalls != 1 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want 1/0", *startCalls, *discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateFailed || turn.CompletedAt == nil || turn.ProviderTurnID == "" {
		t.Fatalf("preclaimed settled turn = %+v, want failed/completed/provider-bound", turn)
	}
	if pending := h.ctrl.PendingTurnIDForTest(); pending != "" {
		t.Fatalf("pendingTurnID = %q, want cleared", pending)
	}
	if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != "" {
		t.Fatalf("one-shot hold = %q, want cleared", armed)
	}
	if got := turnStateByID(t, h, second.ID); got != domain.TurnStateQueued {
		t.Fatalf("next head = %q, want queued", got)
	}
	assertNoDispatchingTurn(t, h)
}

func TestDeferredPreclaimedNotStartedSettlementFailureRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	cleanup := newCleanupFailureStore(false)
	close(cleanup.stopRelease)
	cleanup.settleErr = errors.New("injected preclaimed not-started settlement failure")
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	first, second := queueTwoBehindFailingRoot(t, h, conv.emit)
	*startCalls, *discarded = 0, 0
	conv.start = func(string) error {
		*startCalls++
		return deferredNotStartedError()
	}
	_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
	if !errors.Is(err, chatsvc.ErrQueueDispatchFailed) ||
		!errors.Is(err, chatsvc.ErrDeferredDispatchCleanupIncomplete) {
		t.Fatalf("resume error = %v, want dispatch failed + cleanup incomplete", err)
	}
	if *startCalls != 1 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want 1/0", *startCalls, *discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID == "" {
		t.Fatalf("preclaimed turn after failed settlement = %+v, want running/provider-bound", turn)
	}
	if pending := h.ctrl.PendingTurnIDForTest(); pending != turn.ProviderTurnID {
		t.Fatalf("pendingTurnID = %q, want %q", pending, turn.ProviderTurnID)
	}
	if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != first.ID {
		t.Fatalf("one-shot hold = %q, want %q retained", armed, first.ID)
	}
	if got := turnStateByID(t, h, second.ID); got != domain.TurnStateQueued {
		t.Fatalf("next head = %q, want queued", got)
	}
	if got := conv.sentTexts(); len(got) != 2 { // root, then the one prepared head
		t.Fatalf("provider received %v, want no next-head dispatch", got)
	}
}

func TestDeferredNotStartedSettlementDeadlineRetainsTracking(t *testing.T) {
	conv, startCalls, discarded := newCountingDeferredConversation()
	conv.start = func(string) error {
		*startCalls++
		return deferredNotStartedError()
	}
	cleanup := newCleanupFailureStore(false)
	close(cleanup.stopRelease)
	cleanup.settleErr = context.DeadlineExceeded
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		cleanup.Store = st
		return cleanup
	})
	_, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "settlement deadline", ClientMessageID: "not-started-deadline",
	})
	if !errors.Is(err, chatsvc.ErrDeferredDispatchCleanupIncomplete) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send error = %v, want cleanup incomplete + deadline", err)
	}
	if *startCalls != 1 || *discarded != 0 {
		t.Fatalf("start=%d discard=%d, want 1/0", *startCalls, *discarded)
	}
	turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "not-started-deadline"))
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID == "" {
		t.Fatalf("deadline turn = %+v, want running/provider-bound", turn)
	}
	if pending := h.ctrl.PendingTurnIDForTest(); pending != turn.ProviderTurnID {
		t.Fatalf("pendingTurnID = %q, want retained %q", pending, turn.ProviderTurnID)
	}
	ids := h.ctrl.DispatchingTurnIDs()
	if len(ids) < 1 || ids[0] != turnIDByClientID(t, h, "not-started-deadline") {
		t.Fatalf("sending tracking = %v, want retained turn", ids)
	}
}

func TestDeferredNotStartedSettlementStopRace(t *testing.T) {
	for _, settleFailure := range []bool{false, true} {
		name := "settled"
		if settleFailure {
			name = "settle_failed"
		}
		t.Run(name, func(t *testing.T) {
			conv, startCalls, discarded := newCountingDeferredConversation()
			conv.start = func(string) error {
				*startCalls++
				return deferredNotStartedError()
			}
			cleanup := newCleanupFailureStore(false)
			close(cleanup.stopRelease)
			cleanup.settleBlocked = true
			cleanup.settleEntered = make(chan struct{})
			cleanup.settleRelease = make(chan struct{})
			if settleFailure {
				cleanup.settleErr = errors.New("injected settlement failure after Stop")
			}
			h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
				cleanup.Store = st
				return cleanup
			})
			sent := make(chan error, 1)
			go func() {
				_, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
					Text: "not started stop race", ClientMessageID: "not-started-stop-race",
				})
				sent <- err
			}()
			<-cleanup.settleEntered
			if created, err := h.st.AppendUserMessage(context.Background(), h.ctrl.ConversationID(), testSession,
				currentGeneration(h), domain.ConversationMessage{
					ID: "message-behind-stop-race", Origin: domain.MessageOriginHuman,
					Text: "must stay behind Stop", ClientMessageID: "behind-stop-race",
				}, "turn-behind-stop-race", h.now()); err != nil || !created {
				close(cleanup.settleRelease)
				t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
			}
			if err := h.ctrl.RecordStopIntentForTest(context.Background(), domain.ChatStopReasonUserStop); err != nil {
				close(cleanup.settleRelease)
				t.Fatalf("RecordStopIntentForTest: %v", err)
			}
			close(cleanup.settleRelease)
			err := <-sent
			if settleFailure {
				if !errors.Is(err, chatsvc.ErrDeferredDispatchCleanupIncomplete) {
					t.Fatalf("Send error = %v, want cleanup incomplete", err)
				}
			} else if !errors.Is(err, ports.ErrDeferredTurnNotStarted) {
				t.Fatalf("Send error = %v, want start error after settlement", err)
			}
			if *startCalls != 1 || *discarded != 0 {
				t.Fatalf("start=%d discard=%d, want exactly one start and zero discard", *startCalls, *discarded)
			}
			turn, err := h.st.TurnByID(context.Background(), turnIDByClientID(t, h, "not-started-stop-race"))
			if err != nil {
				t.Fatalf("TurnByID: %v", err)
			}
			if settleFailure {
				if turn.State != domain.TurnStateRunning || turn.ProviderTurnID == "" {
					t.Fatalf("turn after failed settlement = %+v, want running provider-bound", turn)
				}
				if pending := h.ctrl.PendingTurnIDForTest(); pending != turn.ProviderTurnID {
					t.Fatalf("pendingTurnID = %q, want retained %q", pending, turn.ProviderTurnID)
				}
			} else {
				if turn.State != domain.TurnStateFailed || turn.CompletedAt == nil {
					t.Fatalf("turn after settlement = %+v, want failed", turn)
				}
				if pending := h.ctrl.PendingTurnIDForTest(); pending != "" {
					t.Fatalf("pendingTurnID = %q, want cleared", pending)
				}
			}
			if got := turnStateByID(t, h, "turn-behind-stop-race"); got != domain.TurnStateInterrupted {
				t.Fatalf("next queued = %q, want interrupted by Stop", got)
			}
			if got := conv.sentTexts(); len(got) != 1 {
				t.Fatalf("provider received %v, want no second start or resend", got)
			}
		})
	}
}
