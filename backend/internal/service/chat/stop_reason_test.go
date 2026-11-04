package chat_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type stopIntentRecord struct {
	reason   domain.ChatStopReason
	episode  string
	cutoff   int64
	callRows int64
}

type recordingStopStore struct {
	*sqlite.Store
	mu    sync.Mutex
	calls []stopIntentRecord
}

func (s *recordingStopStore) RecordStopIntentAndCancelQueued(
	ctx context.Context,
	conversationID string,
	session domain.SessionID,
	reviewID, generation string,
	reason domain.ChatStopReason,
	episodeID string,
	now time.Time,
) (domain.ChatStopIntent, error) {
	intent, err := s.Store.RecordStopIntentAndCancelQueued(
		ctx, conversationID, session, reviewID, generation, reason, episodeID, now,
	)
	if err == nil {
		s.mu.Lock()
		s.calls = append(s.calls, stopIntentRecord{
			reason: reason, episode: intent.EpisodeID, cutoff: intent.CutoffRowID, callRows: intent.CancelledRows,
		})
		s.mu.Unlock()
	}
	return intent, err
}

func (s *recordingStopStore) records() []stopIntentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stopIntentRecord(nil), s.calls...)
}

func newStopReasonHarness(t *testing.T) (*harness, *recordingStopStore) {
	t.Helper()
	var recorder *recordingStopStore
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		recorder = &recordingStopStore{Store: st}
		return recorder
	})
	return h, recorder
}

func startRunningStopReasonTurn(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	const providerTurnID = "provider-stop-reason-1"
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "work that must be interrupted", ClientMessageID: "stop-reason-1",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: providerTurnID})
	h.awaitSnapshot(t, func(snapshot store.ConversationSnapshot) bool {
		return turnStateByText(t, snapshot)["work that must be interrupted"] == domain.TurnStateRunning
	})
}

func TestHandoffCleanupPreservesDurableStopReason(t *testing.T) {
	h, recorder := newStopReasonHarness(t)
	ctx := context.Background()
	startRunningStopReasonTurn(t, h)
	if err := h.svc.PrepareChatHandoff(ctx, testSession, domain.SessionInterfaceTransitionInterrupt); err != nil {
		t.Fatalf("PrepareChatHandoff: %v", err)
	}
	records := recorder.records()
	if len(records) != 1 || records[0].reason != domain.ChatStopReasonHandoffInterrupt {
		t.Fatalf("handoff stop records = %+v, want one handoff_interrupt", records)
	}
	first := records[0]
	if err := h.svc.StopChatAfterStopIntent(ctx, testSession); err != nil {
		t.Fatalf("StopChatAfterStopIntent: %v", err)
	}
	h.awaitSnapshot(t, func(snapshot store.ConversationSnapshot) bool {
		return turnStateByText(t, snapshot)["work that must be interrupted"].Terminal()
	})
	records = recorder.records()
	if len(records) != 1 {
		t.Fatalf("cleanup wrote an extra stop record: %+v", records)
	}
	if records[0] != first {
		t.Fatalf("cleanup changed stop record: before=%+v after=%+v", first, records[0])
	}
}

func TestAgentSwitchCleanupPreservesDurableStopReason(t *testing.T) {
	h, recorder := newStopReasonHarness(t)
	ctx := context.Background()
	startRunningStopReasonTurn(t, h)
	if err := h.svc.PrepareChatHandoffForReason(
		ctx, testSession, domain.SessionInterfaceTransitionInterrupt, domain.ChatStopReasonAgentSwitch,
	); err != nil {
		t.Fatalf("PrepareChatHandoffForReason: %v", err)
	}
	records := recorder.records()
	if len(records) != 1 || records[0].reason != domain.ChatStopReasonAgentSwitch {
		t.Fatalf("agent-switch stop records = %+v, want one agent_switch", records)
	}
	first := records[0]
	if err := h.svc.StopChatAfterStopIntent(ctx, testSession); err != nil {
		t.Fatalf("StopChatAfterStopIntent: %v", err)
	}
	h.awaitSnapshot(t, func(snapshot store.ConversationSnapshot) bool {
		return turnStateByText(t, snapshot)["work that must be interrupted"].Terminal()
	})
	records = recorder.records()
	if len(records) != 1 || records[0] != first {
		t.Fatalf("agent-switch cleanup changed stop record: before=%+v after=%+v", first, records)
	}
}

func TestExplicitTerminationPreservesSessionTerminatedReason(t *testing.T) {
	h, recorder := newStopReasonHarness(t)
	ctx := context.Background()
	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	records := recorder.records()
	if len(records) != 1 || records[0].reason != domain.ChatStopReasonSessionTerminated {
		t.Fatalf("explicit stop records = %+v, want one session_terminated", records)
	}
}
