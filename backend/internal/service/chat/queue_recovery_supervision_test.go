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

// harnessSessionReaderOverride lets a test wrap the session reader the shared
// harness installs, so it can simulate a durable fact (a replaced controller's
// generation, or a terminated session) changing between two reads. Nil keeps the
// plain store. Tests in this package do not run in parallel, so a single
// package-level override is safe.
var harnessSessionReaderOverride func(chatsvc.SessionReader) chatsvc.SessionReader

func harnessSessionReader(reader chatsvc.SessionReader) chatsvc.SessionReader {
	if harnessSessionReaderOverride != nil {
		return harnessSessionReaderOverride(reader)
	}
	return reader
}

// currentGeneration is the live controller's generation, the value a client
// echoes back as expectedControllerGeneration. Tests that prove a refusal for a
// stale snapshot pass a different value instead.
func currentGeneration(h *harness) string { return h.ctrl.Generation() }

func withSessionReader(t *testing.T, wrap func(chatsvc.SessionReader) chatsvc.SessionReader) {
	t.Helper()
	previous := harnessSessionReaderOverride
	harnessSessionReaderOverride = wrap
	t.Cleanup(func() { harnessSessionReaderOverride = previous })
}

// driftingSessionReader reports a different durable controller generation once
// armed, which is what a controller replacement leaves behind: the live
// controller's generation no longer matches the durable row. It is installed
// before the harness is built (which reads the session many times) and armed
// only once the test is ready to simulate the replacement.
type driftingSessionReader struct {
	inner      chatsvc.SessionReader
	generation string
	mu         sync.Mutex
	active     bool
}

func (r *driftingSessionReader) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	record, found, err := r.inner.GetSession(ctx, id)
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	if err == nil && found && active {
		record.Metadata.ControllerGeneration = r.generation
	}
	return record, found, err
}

func (r *driftingSessionReader) arm() {
	r.mu.Lock()
	r.active = true
	r.mu.Unlock()
}

// terminatedSessionReader reports every session as terminated once armed. It is
// installed before the harness is built and armed after, so the harness can
// start the session first.
type terminatedSessionReader struct {
	inner  chatsvc.SessionReader
	mu     sync.Mutex
	active bool
}

func (r *terminatedSessionReader) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	record, found, err := r.inner.GetSession(ctx, id)
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	if err == nil && found && active {
		record.IsTerminated = true
	}
	return record, found, err
}

func (r *terminatedSessionReader) arm() {
	r.mu.Lock()
	r.active = true
	r.mu.Unlock()
}

// queueWhileRunningTwice queues two automation turns behind a running root turn
// and fails the root, leaving both held. It returns both queued turns in order.
func queueWhileRunningTwice(t *testing.T, h *harness) (domain.ConversationTurn, domain.ConversationTurn) {
	t.Helper()
	first := queueWhileRunning(t, h, "c1", "automation", "c2")
	second, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "automation2", ClientMessageID: "c3", Origin: domain.MessageOriginAutomation,
	})
	if err != nil {
		t.Fatalf("queue automation2: %v", err)
	}
	if second.State != domain.TurnStateQueued {
		t.Fatalf("second queued turn state = %q, want queued", second.State)
	}
	failRootTurn(t, h)
	return first, second
}

// TestSecondResumeReleasesTheNextTurn proves a second explicit action can release
// the turn behind the first, while the first resume still released only one.
func TestSecondResumeReleasesTheNextTurn(t *testing.T) {
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
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["automation"] == domain.TurnStateCompleted
	})
	assertTurnStaysQueued(t, h, "automation2")

	result, err := h.svc.ResumeHeldQueue(ctx, testSession, second.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeResumed || result.TurnID != second.ID {
		t.Fatalf("second resume = %+v, want resumed %s", result, second.ID)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation", "automation2"}) {
		t.Fatalf("provider received %v, want the two released turns in order", got)
	}
}

// TestResumeAfterImmediateCompletionDoesNotAdvance covers the case the reviewer
// called out: the first dispatch completes immediately, and the second request
// then names the turn that already finished. It must not release the next turn.
func TestResumeAfterImmediateCompletionDoesNotAdvance(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, _ := queueWhileRunningTwice(t, h)

	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
		t.Fatalf("first resume: %v", err)
	}
	// The resumed turn completes before the second request is issued.
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2",
		TurnState: domain.TurnStateCompleted,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["automation"] == domain.TurnStateCompleted
	})

	result, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if result.Outcome == chatsvc.QueueResumeResumed {
		t.Fatalf("outcome = %q, want a refusal for the already-completed turn", result.Outcome)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation"}) {
		t.Fatalf("provider received %v, want no second release", got)
	}
}

// TestResumeHeldQueueStaleClientGenerationRefused proves the optimistic client
// generation check refuses a stale snapshot without dispatching.
func TestResumeHeldQueueStaleClientGenerationRefused(t *testing.T) {
	h := newHarness(t)
	first, _ := queueWhileRunningTwice(t, h)

	_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, "generation-that-is-not-current")
	if !errors.Is(err, chatsvc.ErrStaleControllerGeneration) {
		t.Fatalf("err = %v, want ErrStaleControllerGeneration", err)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want no release for a stale generation", got)
	}
}

// TestResumeHeldQueueReplacedControllerRefused simulates a controller
// replacement between the admission read and the re-validation: the durable
// generation no longer matches the live controller, so the resume refuses.
func TestResumeHeldQueueReplacedControllerRefused(t *testing.T) {
	drift := &driftingSessionReader{generation: "replacement-generation"}
	withSessionReader(t, func(reader chatsvc.SessionReader) chatsvc.SessionReader {
		drift.inner = reader
		return drift
	})
	h := newHarness(t)
	first, _ := queueWhileRunningTwice(t, h)

	// The controller is replaced after the queue was held and before the resume.
	drift.arm()

	_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
	if !errors.Is(err, chatsvc.ErrStaleControllerGeneration) {
		t.Fatalf("err = %v, want ErrStaleControllerGeneration for a replaced controller", err)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want no release after a controller replacement", got)
	}
}

// TestResumeHeldQueueHandoffRefused proves an armed handoff refuses the resume.
func TestResumeHeldQueueHandoffRefused(t *testing.T) {
	h := newHarness(t)
	first, _ := queueWhileRunningTwice(t, h)
	if err := h.ctrl.ArmHandoff(context.Background(), domain.SessionInterfaceTransitionDrain); err != nil {
		t.Fatalf("ArmHandoff: %v", err)
	}

	result, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeHandoffInProgress {
		t.Fatalf("outcome = %q, want handoff_in_progress", result.Outcome)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want no release during a handoff", got)
	}
}

// TestResumeHeldQueueCompactionRefused proves an in-flight compaction refuses the
// resume.
func TestResumeHeldQueueCompactionRefused(t *testing.T) {
	// The compacting conversation is the provider double itself, so events are
	// emitted on it rather than on the harness's plain fake.
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
	queued, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "automation", ClientMessageID: "c2", Origin: domain.MessageOriginAutomation,
	})
	if err != nil {
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

	result, err := h.svc.ResumeHeldQueue(ctx, testSession, queued.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeCompactionInProgress {
		t.Fatalf("outcome = %q, want compaction_in_progress", result.Outcome)
	}
	if got := conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want no release during a compaction", got)
	}
}

// TestResumeHeldQueueControllerAbsentRequiresRecovery proves a session with no
// live controller asks for recovery instead of dispatching.
func TestResumeHeldQueueControllerAbsentRequiresRecovery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, _ := queueWhileRunningTwice(t, h)

	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	result, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeControllerRecoveryRequired {
		t.Fatalf("outcome = %q, want controller_recovery_required", result.Outcome)
	}
}

// TestResumeHeldQueueTerminatedRefused proves a terminated session refuses the
// resume before any controller work.
func TestResumeHeldQueueTerminatedRefused(t *testing.T) {
	terminated := &terminatedSessionReader{}
	withSessionReader(t, func(reader chatsvc.SessionReader) chatsvc.SessionReader {
		terminated.inner = reader
		return terminated
	})
	h := newHarness(t)
	first, _ := queueWhileRunningTwice(t, h)

	// The session is terminated after the queue was held and before the resume.
	terminated.arm()

	result, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeSessionTerminated {
		t.Fatalf("outcome = %q, want session_terminated", result.Outcome)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want no release for a terminated session", got)
	}
}

// TestResumeDispatchFailureIsUncertainKeepsQueue proves an eager dispatch error
// that may have crossed the provider boundary leaves the released turn
// running/unbound and the rest of the queue untouched, rather than settling it
// failed and letting a later event advance the queue.
func TestResumeDispatchFailureIsUncertainKeepsQueue(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second := queueWhileRunningTwice(t, h)

	h.conv.mu.Lock()
	h.conv.sendErr = errors.New("provider disconnected")
	h.conv.mu.Unlock()

	result, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeStateUncertain {
		t.Fatalf("outcome = %q, want state_uncertain", result.Outcome)
	}
	// The row stays running/unbound; the one-shot is still armed.
	turn, err := h.st.TurnByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID != "" {
		t.Fatalf("released turn = state %q provider id %q, want running/unbound", turn.State, turn.ProviderTurnID)
	}
	if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != first.ID {
		t.Fatalf("one-shot marker = %q, want %s", armed, first.ID)
	}
	if queue := snapshotQueue(t, h); queue.State != chatsvc.QueueStateUncertain {
		t.Fatalf("snapshot state = %q, want state_uncertain", queue.State)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want the failed send not replayed", got)
	}
	assertTurnStaysQueued(t, h, "automation2")

	snapshot, err := h.svc.Snapshot(ctx, testSession)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Queue.PendingCount != 1 || snapshot.Queue.HeadTurnID != second.ID {
		t.Fatalf("queue = %+v, want one pending head %s", snapshot.Queue, second.ID)
	}
}

// TestRateLimitEventDoesNotDispatchQueuedWork proves a rate-limit report alone
// never releases queued work, and a turn that ends rate-limited re-holds the
// queue rather than cascading through it.
func TestRateLimitEventDoesNotDispatchQueuedWork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second := queueWhileRunningTwice(t, h)

	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventRateLimits, RateLimits: &ports.ChatRateLimits{
		PrimaryUsedPercent: 100, SecondaryUsedPercent: -1, PrimaryResetsInSeconds: 120, PlanLabel: "pro",
	}})
	assertTurnStaysQueued(t, h, "automation")

	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// The released turn ends rate-limited; the next turn must stay held.
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2",
		TurnState: domain.TurnStateFailed, Err: errors.New("rate limited"),
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["automation"] == domain.TurnStateFailed
	})
	assertTurnStaysQueued(t, h, "automation2")
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation"}) {
		t.Fatalf("provider received %v, want no cascade past the rate-limited turn", got)
	}
	if second.ID == "" {
		t.Fatalf("second queued turn has no id")
	}
}
