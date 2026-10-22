package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// archiveObservation captures the durable row at the instant the diagnostic
// write is attempted, before the archive transaction begins.
type archiveObservation struct {
	turns   []domain.ConversationTurn
	pending string
	payload string
	err     error
}

type archiveOrderStore struct {
	chatsvc.Store
	st      *sqlite.Store
	pending func() string

	mu   sync.Mutex
	seen []archiveObservation
}

func (s *archiveOrderStore) ProjectProviderEvent(
	ctx context.Context,
	conversationID string,
	session domain.SessionID,
	generation, providerEventID, method, payloadJSON string,
	nowTime time.Time,
	project func(context.Context) error,
) (bool, error) {
	if method == "provider.error" {
		obs := archiveObservation{payload: payloadJSON}
		snapshot, err := s.st.LoadConversationSnapshot(ctx, conversationID)
		if err != nil {
			obs.err = err
		} else {
			obs.turns = append([]domain.ConversationTurn(nil), snapshot.Turns...)
		}
		if s.pending != nil {
			obs.pending = s.pending()
		}
		s.mu.Lock()
		s.seen = append(s.seen, obs)
		s.mu.Unlock()
	}
	return s.Store.ProjectProviderEvent(ctx, conversationID, session, generation,
		providerEventID, method, payloadJSON, nowTime, project)
}

func (s *archiveOrderStore) last(t *testing.T) archiveObservation {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 {
		t.Fatal("no provider.error archive was attempted")
	}
	return s.seen[len(s.seen)-1]
}

func turnAt(t *testing.T, turns []domain.ConversationTurn, id string) domain.ConversationTurn {
	t.Helper()
	for _, turn := range turns {
		if turn.ID == id {
			return turn
		}
	}
	t.Fatalf("turn %s not present in archive observation", id)
	return domain.ConversationTurn{}
}

func assertArchiveTurn(t *testing.T, obs archiveObservation, id string, state domain.TurnState, completed bool) {
	t.Helper()
	if obs.err != nil {
		t.Fatalf("archive snapshot: %v", obs.err)
	}
	turn := turnAt(t, obs.turns, id)
	if turn.State != state {
		t.Fatalf("turn %s at archive = %q, want %q", id, turn.State, state)
	}
	if (turn.CompletedAt != nil) != completed {
		t.Fatalf("turn %s completed_at at archive = %v, want present=%v", id, turn.CompletedAt, completed)
	}
}

func assertDiagnosticRow(t *testing.T, obs archiveObservation, turnID string) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(obs.payload), &payload); err != nil {
		t.Fatalf("diagnostic payload is not valid JSON: %v", err)
	}
	if payload["turnId"] != turnID {
		t.Fatalf("diagnostic turnId = %#v, want %q", payload["turnId"], turnID)
	}
	if _, present := payload["providerTurnId"]; present {
		t.Fatalf("diagnostic row carries a fake providerTurnId: %#v", payload)
	}
}

func evidenceFor(class domain.ProviderErrorClass, protocol, harness, operation string) domain.ProviderErrorEvidence {
	return domain.ProviderErrorEvidence{
		Class: class, Protocol: protocol, Harness: harness, Operation: operation,
	}
}

// A definitive eager SendTurn failure must settle the row before the diagnostic
// archive is attempted. The next queued turn remains queued.
func TestDefinitiveSendTurnSettlesBeforeArchivingEvidence(t *testing.T) {
	var order *archiveOrderStore
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		order = &archiveOrderStore{st: st, Store: st}
		return order
	})
	order.pending = h.ctrl.PendingTurnIDForTest
	first, second := queueTwoBehindFailingRoot(t, h, h.conv.emit)

	h.conv.sendErr = ports.WithProviderError(
		fmt.Errorf("%w: test definitive failure", ports.ErrTurnNotDelivered),
		evidenceFor(domain.ProviderErrorNotDelivered, "codex", "codex", "turn/start"),
	)
	if _, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h)); err == nil {
		t.Fatal("definitive SendTurn failure was reported as success")
	}

	obs := order.last(t)
	assertArchiveTurn(t, obs, first.ID, domain.TurnStateFailed, true)
	assertArchiveTurn(t, obs, second.ID, domain.TurnStateQueued, false)
	if obs.pending != "" {
		t.Fatalf("pendingTurnID at archive = %q, want cleared", obs.pending)
	}
	assertDiagnosticRow(t, obs, first.ID)
	assertTurnStaysQueued(t, h, "automation2")
	if got := h.conv.sentTexts(); len(got) != 1 || got[0] != "root" {
		t.Fatalf("provider received %v, want only root", got)
	}
}

// A deferred not-started error settles and clears pendingTurnID before its
// diagnostic archive is attempted. The next queued turn remains queued.
func TestDeferredNotStartedSettlesBeforeArchivingEvidence(t *testing.T) {
	var order *archiveOrderStore
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	conv.start = func(string) error { return nil }
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		order = &archiveOrderStore{st: st, Store: st}
		return order
	})
	order.pending = h.ctrl.PendingTurnIDForTest
	first, second := queueTwoBehindFailingRoot(t, h, conv.emit)

	conv.start = func(string) error {
		return ports.WithProviderError(
			fmt.Errorf("%w: test prepared turn missing", ports.ErrDeferredTurnNotStarted),
			evidenceFor(domain.ProviderErrorNotDelivered, "acp", "opencode", "session/prompt"),
		)
	}
	if _, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h)); err == nil {
		t.Fatal("deferred not-started failure was reported as success")
	}

	obs := order.last(t)
	assertArchiveTurn(t, obs, first.ID, domain.TurnStateFailed, true)
	assertArchiveTurn(t, obs, second.ID, domain.TurnStateQueued, false)
	if obs.pending != "" {
		t.Fatalf("pendingTurnID at archive = %q, want cleared", obs.pending)
	}
	assertDiagnosticRow(t, obs, first.ID)
	assertTurnStaysQueued(t, h, "automation2")
}

// An ambiguous eager SendTurn failure archives while the row is still running.
func TestAmbiguousSendTurnArchivesWhileRunning(t *testing.T) {
	var order *archiveOrderStore
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		order = &archiveOrderStore{st: st, Store: st}
		return order
	})
	order.pending = h.ctrl.PendingTurnIDForTest
	first, second := queueTwoBehindFailingRoot(t, h, h.conv.emit)

	h.conv.sendErr = ports.WithProviderError(
		errors.New("test ambiguous transport"),
		evidenceFor(domain.ProviderErrorUncertain, "codex", "codex", "turn/start"),
	)
	result, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("ambiguous SendTurn resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeStateUncertain {
		t.Fatalf("outcome = %q, want state_uncertain", result.Outcome)
	}

	obs := order.last(t)
	assertArchiveTurn(t, obs, first.ID, domain.TurnStateRunning, false)
	assertArchiveTurn(t, obs, second.ID, domain.TurnStateQueued, false)
	assertDiagnosticRow(t, obs, first.ID)
	assertTurnStaysQueued(t, h, "automation2")
}

// An ambiguous deferred start archives while the row is still running and the
// pending provider binding and one-shot hold remain intact.
func TestAmbiguousDeferredStartArchivesWhileRunningAndPending(t *testing.T) {
	var order *archiveOrderStore
	conv := &deferredConversation{fakeConversation: newFakeConversation()}
	conv.start = func(string) error { return nil }
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		order = &archiveOrderStore{st: st, Store: st}
		return order
	})
	order.pending = h.ctrl.PendingTurnIDForTest
	first, second := queueTwoBehindFailingRoot(t, h, conv.emit)

	conv.start = func(string) error {
		return ports.WithProviderError(
			io.EOF,
			evidenceFor(domain.ProviderErrorUncertain, "acp", "opencode", "session/prompt"),
		)
	}
	result, err := h.svc.ResumeHeldQueue(context.Background(), testSession, first.ID, currentGeneration(h))
	if err != nil {
		t.Fatalf("ambiguous deferred resume: %v", err)
	}
	if result.Outcome != chatsvc.QueueResumeStateUncertain {
		t.Fatalf("outcome = %q, want state_uncertain", result.Outcome)
	}

	obs := order.last(t)
	assertArchiveTurn(t, obs, first.ID, domain.TurnStateRunning, false)
	assertArchiveTurn(t, obs, second.ID, domain.TurnStateQueued, false)
	if obs.pending == "" {
		t.Fatal("pendingTurnID was cleared for an ambiguous deferred start")
	}
	assertDiagnosticRow(t, obs, first.ID)
	assertTurnStaysQueued(t, h, "automation2")
	if armed := h.ctrl.ResumeHoldTurnIDForTest(); armed != first.ID {
		t.Fatalf("one-shot hold = %q, want %s", armed, first.ID)
	}
}
