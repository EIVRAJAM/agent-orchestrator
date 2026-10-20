package chat_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// TestResumeEagerSendErrorClassification proves the eager SendTurn error split:
// a definitive local/rejection error settles the turn failed, while a transport
// uncertainty leaves it running/unbound and refuses any further release.
func TestResumeEagerSendErrorClassification(t *testing.T) {
	type want struct {
		outcome    chatsvc.QueueResumeOutcome
		definitive bool
	}
	for _, tc := range []struct {
		name string
		err  error
		want want
	}{
		{"not delivered", ports.ErrTurnNotDelivered, want{definitive: true}},
		{"rpc rejected", ports.ErrTurnRejected, want{definitive: true}},
		{"eof", io.EOF, want{outcome: chatsvc.QueueResumeStateUncertain}},
		{"timeout", context.DeadlineExceeded, want{outcome: chatsvc.QueueResumeStateUncertain}},
		{"cancelled", context.Canceled, want{outcome: chatsvc.QueueResumeStateUncertain}},
		{"undecodable", errors.New("decode result: invalid character"), want{outcome: chatsvc.QueueResumeStateUncertain}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			first, _ := queueTwoBehindFailingRoot(t, h, h.conv.emit)

			h.conv.mu.Lock()
			h.conv.sendErr = tc.err
			h.conv.mu.Unlock()

			result, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h))
			if tc.want.definitive {
				if !errors.Is(err, chatsvc.ErrQueueDispatchFailed) {
					t.Fatalf("err = %v, want ErrQueueDispatchFailed", err)
				}
				if state := turnStateByID(t, h, first.ID); state != domain.TurnStateFailed {
					t.Fatalf("turn state = %q, want failed", state)
				}
				if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != "" {
					t.Fatalf("one-shot marker = %q, want empty after a definitive failure", armed)
				}
			} else {
				if err != nil {
					t.Fatalf("resume: %v", err)
				}
				if result.Outcome != tc.want.outcome {
					t.Fatalf("outcome = %q, want %q", result.Outcome, tc.want.outcome)
				}
				turn, err := h.st.TurnByID(ctx, first.ID)
				if err != nil {
					t.Fatalf("TurnByID: %v", err)
				}
				if turn.State != domain.TurnStateRunning || turn.ProviderTurnID != "" {
					t.Fatalf("turn = state %q provider id %q, want running/unbound", turn.State, turn.ProviderTurnID)
				}
				if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != first.ID {
					t.Fatalf("one-shot marker = %q, want %s", armed, first.ID)
				}
				if queue := snapshotQueue(t, h); queue.State != chatsvc.QueueStateUncertain {
					t.Fatalf("snapshot state = %q, want state_uncertain", queue.State)
				}
				// A second resume sends nothing.
				again, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h))
				if err != nil {
					t.Fatalf("second resume: %v", err)
				}
				if again.Outcome == chatsvc.QueueResumeResumed {
					t.Fatalf("second outcome = %q, want a refusal", again.Outcome)
				}
			}
			if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
				t.Fatalf("provider received %v, want no replay", got)
			}
			assertTurnStaysQueued(t, h, "automation2")
		})
	}
}

// TestResumeEagerUncertainTurnStartedBindsAndLateTerminalSettles proves a late
// TurnStarted persists the provider binding after an ambiguous eager send, and
// the matching terminal then settles the exact row without releasing the next.
func TestResumeEagerUncertainTurnStartedBindsAndLateTerminalSettles(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, _ := queueTwoBehindFailingRoot(t, h, h.conv.emit)

	h.conv.mu.Lock()
	h.conv.sendErr = io.EOF
	h.conv.mu.Unlock()

	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// The provider reveals the turn id later: bind it durably and on the one-shot.
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-2"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		turn, err := h.st.TurnByID(ctx, first.ID)
		return err == nil && turn.ProviderTurnID == "provider-turn-2"
	})

	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2",
		TurnState: domain.TurnStateCompleted,
	})
	turn := waitTurnState(t, h, first.ID, domain.TurnStateCompleted)
	waitResumeHoldCleared(t, h)
	if turn.ProviderTurnID != "provider-turn-2" {
		t.Fatalf("provider id = %q, want provider-turn-2", turn.ProviderTurnID)
	}
	assertTurnStaysQueued(t, h, "automation2")
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want no replay", got)
	}
}

// TestResumeEagerUncertainWithoutEventsFallsToRecovery proves an unresolved
// ambiguous send is left to the normal controller cleanup and never re-dispatched.
func TestResumeEagerUncertainWithoutEventsFallsToRecovery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, _ := queueTwoBehindFailingRoot(t, h, h.conv.emit)

	h.conv.mu.Lock()
	h.conv.sendErr = io.EOF
	h.conv.mu.Unlock()

	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if state := turnStateByID(t, h, first.ID); state == domain.TurnStateRunning {
		t.Fatalf("turn still running after controller cleanup; recovery did not settle it")
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want no replay", got)
	}
}
