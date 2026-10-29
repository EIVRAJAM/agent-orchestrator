package chat_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// A clean daemon shutdown is not a user Stop: running work is honestly settled,
// queued work survives, and the replacement controller holds it until the user
// names the head in an explicit resume.
func TestRestartPreservesQueuedWorkAndRequiresManualResume(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "root", ClientMessageID: "restart-root", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("send root: %v", err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateRunning
	})
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "queued", ClientMessageID: "restart-queued", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("queue second: %v", err)
	}
	before := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["queued"] == domain.TurnStateQueued
	})
	if len(before.Turns) != 2 {
		t.Fatalf("turns before restart = %d, want 2", len(before.Turns))
	}

	// A clean daemon shutdown uses Close, not Stop. The provider stream ending
	// settles the running row but must not cancel the queued row.
	h.svc.StopAll(ctx)
	afterStop := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		states := turnStateByText(t, s)
		return states["root"] == domain.TurnStateFailed && states["queued"] == domain.TurnStateQueued
	})
	if len(afterStop.Turns) != 2 {
		t.Fatalf("turns after shutdown = %d, want the preserved queue", len(afterStop.Turns))
	}

	conv := newFakeConversation()
	conv.providerConversationID = h.ctrl.ProviderConversationID()
	// The old root already bound provider-turn-1; a replacement process names
	// its turns independently, so avoid a test-only collision with that old id.
	conv.turnSeq = 10
	restarted := newRestartedEditService(t, h, fakeDriver{conv: conv}, "restart-generation")
	ctrl, err := restarted.Start(ctx, chatsvc.StartConfig{
		SessionID:              testSession,
		ProjectID:              testProject,
		Harness:                domain.HarnessCodex,
		WorkspacePath:          t.TempDir(),
		ProviderConversationID: h.ctrl.ProviderConversationID(),
	})
	if err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	if got := conv.sentTexts(); len(got) != 0 {
		t.Fatalf("restart drained preserved work automatically: %v", got)
	}
	snapshot, err := restarted.Snapshot(ctx, testSession)
	if err != nil {
		t.Fatalf("restart Snapshot: %v", err)
	}
	if snapshot.Queue == nil || snapshot.Queue.State != chatsvc.QueueStateControllerRecoveryRequired {
		t.Fatalf("queue state after restart = %+v, want controller_recovery_required", snapshot.Queue)
	}
	if snapshot.Queue.PendingCount != 1 || snapshot.Queue.HeadTurnID == "" {
		t.Fatalf("preserved queue = %+v, want one named head", snapshot.Queue)
	}
	if ctrl.Generation() != snapshot.Queue.ControllerGeneration {
		t.Fatalf("live generation %q != durable %q", ctrl.Generation(), snapshot.Queue.ControllerGeneration)
	}
	joined, err := restarted.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "joins behind held head", ClientMessageID: "restart-join", Origin: domain.MessageOriginHuman,
	})
	if err != nil {
		t.Fatalf("send during recovery hold: %v", err)
	}
	if joined.State != domain.TurnStateQueued {
		t.Fatalf("send during recovery hold state = %q, want queued", joined.State)
	}
	if got := conv.sentTexts(); len(got) != 0 {
		t.Fatalf("send during recovery hold reached provider: %v", got)
	}

	var providerTurnID string
	conv.onSend = func(id string) { providerTurnID = id }
	result, err := restarted.ResumeHeldQueue(
		ctx, testSession, snapshot.Queue.HeadTurnID, snapshot.Queue.ControllerGeneration)
	if err != nil {
		t.Fatalf("ResumeHeldQueue: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeResumed || result.TurnID != before.Turns[1].ID {
		t.Fatalf("resume = %+v, want the preserved head", result)
	}
	if got := conv.sentTexts(); len(got) != 1 || got[0] != "queued" {
		t.Fatalf("provider received %v, want only the preserved head", got)
	}
	for _, text := range conv.sentTexts() {
		if text == "root" {
			t.Fatal("restart re-sent the orphaned running turn")
		}
	}
	if providerTurnID == "" {
		t.Fatal("resumed turn never reached the provider")
	}
	conv.emit(
		ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: providerTurnID},
		ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: providerTurnID, TurnState: domain.TurnStateCompleted},
	)
	after := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		states := turnStateByText(t, s)
		return states["queued"] == domain.TurnStateCompleted &&
			states["joins behind held head"] == domain.TurnStateQueued
	})
	if got := turnStateByText(t, after)["queued"]; got != domain.TurnStateCompleted {
		t.Fatalf("resumed head state = %q, want completed", got)
	}
	if got := conv.sentTexts(); len(got) != 1 || got[0] != "queued" {
		t.Fatalf("provider received %v after the one-shot; the backlog must stay held", got)
	}
}

// Stop is durable at the cutoff: pre-Stop queue rows are cancelled, a message
// accepted after the cutoff survives and is released only once the interrupted
// root has settled.
func TestStopCancelsOnlyWorkThroughDurableCutoff(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "running", ClientMessageID: "stop-root", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("send root: %v", err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["running"] == domain.TurnStateRunning
	})
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "before stop", ClientMessageID: "stop-before", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("queue before stop: %v", err)
	}
	if err := h.svc.Interrupt(ctx, testSession); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "after stop", ClientMessageID: "stop-after", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("queue after stop: %v", err)
	}
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateInterrupted,
	})

	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["after stop"] == domain.TurnStateRunning
	})
	states := turnStateByText(t, snapshot)
	if states["before stop"] != domain.TurnStateInterrupted {
		t.Fatalf("pre-stop queue state = %q, want interrupted", states["before stop"])
	}
	if got := h.conv.sentTexts(); len(got) != 2 || got[0] != "running" || got[1] != "after stop" {
		t.Fatalf("provider received %v, want the root and only the post-cutoff message", got)
	}
}

// Termination can arrive when no controller is live, for example after a crash
// and before any restart. It still owns the durable queue and must cancel it.
func TestTerminationWithoutControllerCancelsQueuedWork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "queued before crash", ClientMessageID: "no-controller-queued",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// This first send actually runs; make it a queued row by stopping the
	// controller cleanly and leaving the durable row in place.
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["queued before crash"] == domain.TurnStateRunning
	})
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "second queued", ClientMessageID: "no-controller-queued-2",
	}); err != nil {
		t.Fatalf("second send: %v", err)
	}
	h.svc.StopAll(ctx)
	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatalf("Stop without controller: %v", err)
	}
	snapshot, err := h.st.LoadConversationSnapshot(ctx, h.ctrl.ConversationID())
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	for _, turn := range snapshot.Turns {
		if turn.State == domain.TurnStateQueued {
			t.Fatalf("termination without controller left queued turn %s", turn.ID)
		}
	}
}

// Session termination is an intentional stop: it cancels the whole durable
// queue and never lets the replacement provider replay it.
func TestTerminationCancelsDurableQueue(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "running", ClientMessageID: "terminate-root", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("send root: %v", err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["running"] == domain.TurnStateRunning
	})
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "must be cancelled", ClientMessageID: "terminate-queued", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("queue before termination: %v", err)
	}

	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["must be cancelled"].Terminal()
	})
	if got := turnStateByText(t, snapshot)["must be cancelled"]; got != domain.TurnStateInterrupted {
		t.Fatalf("terminated queue state = %q, want interrupted", got)
	}
	if got := h.conv.sentTexts(); len(got) != 1 || got[0] != "running" {
		t.Fatalf("provider received %v; termination must not dispatch the cancelled queue", got)
	}
}
