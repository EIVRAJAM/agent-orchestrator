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
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// pausingSessionReader pauses the Nth GetSession call after it has read the row
// but before returning it, so a test can change durable state in the window
// between a read and the write that depends on it. It is installed before the
// harness is built and armed only once the test is ready.
type pausingSessionReader struct {
	inner   chatsvc.SessionReader
	mu      sync.Mutex
	calls   int
	pauseAt int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *pausingSessionReader) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	record, found, err := r.inner.GetSession(ctx, id)
	r.mu.Lock()
	r.calls++
	call := r.calls
	pauseAt := r.pauseAt
	r.mu.Unlock()
	if err == nil && found && pauseAt > 0 && call == pauseAt {
		r.once.Do(func() {
			close(r.entered)
			<-r.release
		})
	}
	return record, found, err
}

func (r *pausingSessionReader) pauseAfter(n int) {
	r.mu.Lock()
	r.pauseAt = r.calls + n
	r.mu.Unlock()
}

// waitResumeHoldCleared waits until the one-shot marker is retired, which happens
// when its own terminal event is consumed (or a failure path clears it).
func waitResumeHoldCleared(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.ctrl.ResumeHoldTurnIDForTest() == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("one-shot marker was not consumed: still armed for %q", h.ctrl.ResumeHoldTurnIDForTest())
}

// waitTurnState waits until a turn reaches a durable terminal state, so a test
// does not race the projector after a terminal event.
func waitTurnState(t *testing.T, h *harness, id string, want domain.TurnState) domain.ConversationTurn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last domain.ConversationTurn
	for time.Now().Before(deadline) {
		turn, err := h.st.TurnByID(context.Background(), id)
		if err == nil {
			last = turn
			if turn.State == want {
				return turn
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("turn %s state = %q, want %q", id, last.State, want)
	return domain.ConversationTurn{}
}

// TestResumeHeldQueueGenerationChangeAfterValidationRefused proves the durable
// generation is checked inside the claim, not only in the Go-side validation.
//
// The resume is paused after its authoritative validation has read the session
// (still the old generation) and before the claim. The durable generation is then
// advanced. The claim must refuse, and the provider must receive nothing.
func TestResumeHeldQueueGenerationChangeAfterValidationRefused(t *testing.T) {
	pause := &pausingSessionReader{entered: make(chan struct{}), release: make(chan struct{})}
	withSessionReader(t, func(reader chatsvc.SessionReader) chatsvc.SessionReader {
		pause.inner = reader
		return pause
	})
	h := newHarness(t)
	ctx := context.Background()
	first, _ := queueWhileRunningTwice(t, h)

	// ResumeHeldQueue reads the session twice before validate (admission and the
	// working-controller check); validate's read is the third.
	pause.pauseAfter(3)

	type outcome struct {
		res chatsvc.QueueResumeResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h))
		done <- outcome{res, err}
	}()

	select {
	case <-pause.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("resume never reached the post-validation window")
	}

	// The controller is replaced durably while the resume is paused: validation
	// already saw the old generation, so only the claim can catch this.
	if err := h.st.ClaimChatControllerGeneration(ctx, testSession, "replacement-generation"); err != nil {
		t.Fatalf("advance durable generation: %v", err)
	}
	close(pause.release)

	got := <-done
	if got.err != nil {
		t.Fatalf("resume: %v", got.err)
	}
	if got.res.Outcome != chatsvc.QueueResumeStaleQueueState {
		t.Fatalf("outcome = %q, want stale_queue_state", got.res.Outcome)
	}
	if sent := h.conv.sentTexts(); !reflect.DeepEqual(sent, []string{"root"}) {
		t.Fatalf("provider received %v, want no release after the generation changed", sent)
	}
	assertTurnStaysQueued(t, h, "automation")
}

// TestResumeSynchronousCompletionStillHoldsTheQueue proves a provider that
// completes the resumed turn synchronously from the boundary-crossing call
// cannot release the next turn: the one-shot hold is bound before sendMu is
// released, so the completion is recognized as the resumed turn's own.
func TestResumeSynchronousCompletionStillHoldsTheQueue(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second := queueWhileRunningTwice(t, h)

	h.conv.mu.Lock()
	h.conv.onSend = func(providerTurnID string) {
		h.conv.emit(ports.ChatEvent{
			Kind: ports.ChatEventTurnCompleted, ProviderTurnID: providerTurnID,
			TurnState: domain.TurnStateCompleted,
		})
	}
	h.conv.mu.Unlock()

	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertTurnStaysQueued(t, h, "automation2")
	waitResumeHoldCleared(t, h)
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation"}) {
		t.Fatalf("provider received %v, want no second dispatch", got)
	}

	// A new explicit resume with the new head can still proceed.
	result, err := h.svc.ResumeHeldQueue(ctx, testSession, second.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeResumed || result.TurnID != second.ID {
		t.Fatalf("second resume = %+v, want resumed %s", result, second.ID)
	}
}

// TestResumeDeferredSynchronousCompletionStillHoldsTheQueue is the same
// guarantee on the deferred (ACP/OpenCode) path, where the provider boundary is
// StartDeferredTurn and the provider turn id is bound before it runs.
func TestResumeDeferredSynchronousCompletionStillHoldsTheQueue(t *testing.T) {
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	conv.start = func(string) error { return nil }
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
	conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateFailed,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateFailed
	})

	// The deferred boundary now emits the terminal completion synchronously.
	conv.start = func(providerTurnID string) error {
		conv.emit(ports.ChatEvent{
			Kind: ports.ChatEventTurnCompleted, ProviderTurnID: providerTurnID,
			TurnState: domain.TurnStateCompleted,
		})
		return nil
	}

	if _, err := h.svc.ResumeHeldQueue(ctx, testSession, first.ID, currentGeneration(h)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertTurnStaysQueued(t, h, "automation2")
	waitResumeHoldCleared(t, h)
	if got := conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation"}) {
		t.Fatalf("provider received %v, want no second dispatch", got)
	}

	// A new explicit resume with the new head dispatches exactly that turn.
	result, err := h.svc.ResumeHeldQueue(ctx, testSession, second.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeResumed || result.TurnID != second.ID {
		t.Fatalf("second resume = %+v, want resumed %s", result, second.ID)
	}
	if got := conv.sentTexts(); !reflect.DeepEqual(got, []string{"root", "automation", "automation2"}) {
		t.Fatalf("provider received %v, want the new head sent exactly once", got)
	}
}

// TestResumeHeldQueueEmptyGenerationRejected proves the generation is mandatory
// and an empty value is never read as "use the current generation".
func TestResumeHeldQueueEmptyGenerationRejected(t *testing.T) {
	h := newHarness(t)
	first, _ := queueWhileRunningTwice(t, h)

	_, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, "")
	if !errors.Is(err, chatsvc.ErrResumeControllerGenerationRequired) {
		t.Fatalf("err = %v, want ErrResumeControllerGenerationRequired", err)
	}
	if got := h.conv.sentTexts(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("provider received %v, want no release", got)
	}
}
