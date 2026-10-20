package store_test

import (
	"context"
	"sort"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// activeRunningIDs is the durable dispatch/resume guard's exact scope.
func activeRunningIDs(t *testing.T, s *sqlite.Store, conversationID string, session domain.SessionID, generation string) []string {
	t.Helper()
	ids, err := s.ListActiveRunningTurnProviderIDs(context.Background(), conversationID, session, generation)
	if err != nil {
		t.Fatalf("ListActiveRunningTurnProviderIDs: %v", err)
	}
	sort.Strings(ids)
	return ids
}

// TestActiveRunningTurnsAreScopedToTheControllerEpoch proves the running guard
// only considers running work owned by the current session and generation, and
// ignores terminal, rolled-back, promoted, foreign-conversation and obsolete
// generation rows.
func TestActiveRunningTurnsAreScopedToTheControllerEpoch(t *testing.T) {
	s, session, conversation := seededChatConversation(t)
	ctx := context.Background()
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-active"); err != nil {
		t.Fatalf("claim generation: %v", err)
	}

	// Active generation, bound.
	seedQueuedTurn(t, s, session, conversation.ID, "turn-bound", "gen-active")
	if err := s.BindTurnToProvider(ctx, "turn-bound", "provider-bound", testNow); err != nil {
		t.Fatalf("bind turn-bound: %v", err)
	}
	// Active generation, unbound (a dispatch whose binding failed).
	seedQueuedTurn(t, s, session, conversation.ID, "turn-unbound", "gen-active")
	if err := s.MarkTurnDispatching(ctx, "turn-unbound"); err != nil {
		t.Fatalf("mark turn-unbound dispatching: %v", err)
	}
	// Active generation, terminal.
	seedQueuedTurn(t, s, session, conversation.ID, "turn-done", "gen-active")
	if err := s.BindTurnToProvider(ctx, "turn-done", "provider-done", testNow); err != nil {
		t.Fatalf("bind turn-done: %v", err)
	}
	if err := s.SettleTurnByID(ctx, "turn-done", domain.TurnStateCompleted, "", testNow); err != nil {
		t.Fatalf("settle turn-done: %v", err)
	}
	// Active generation, rolled back.
	seedQueuedTurn(t, s, session, conversation.ID, "turn-rolled", "gen-active")
	if err := s.MarkTurnDispatching(ctx, "turn-rolled"); err != nil {
		t.Fatalf("mark turn-rolled dispatching: %v", err)
	}
	if _, err := s.RollbackTurns(ctx, conversation.ID, "turn-rolled", testNow); err != nil {
		t.Fatalf("rollback turn-rolled: %v", err)
	}
	// Obsolete generation, still running.
	seedQueuedTurn(t, s, session, conversation.ID, "turn-old", "gen-old")
	if err := s.MarkTurnDispatching(ctx, "turn-old"); err != nil {
		t.Fatalf("mark turn-old dispatching: %v", err)
	}

	got := activeRunningIDs(t, s, conversation.ID, session.ID, "gen-active")
	want := []string{"provider-bound", ""} // bound id plus the unbound empty id
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("active running = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("active running = %v, want %v", got, want)
		}
	}

	// The obsolete generation still reports its own row for whatever handles it.
	if ids := activeRunningIDs(t, s, conversation.ID, session.ID, "gen-old"); len(ids) != 1 {
		t.Fatalf("old-generation running = %v, want one row", ids)
	}
}

// TestActiveRunningTurnsIgnoreAnotherConversation proves the guard is scoped by
// conversation, not just by generation.
func TestActiveRunningTurnsIgnoreAnotherConversation(t *testing.T) {
	s, session, conversation := seededChatConversation(t)
	ctx := context.Background()
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-active"); err != nil {
		t.Fatalf("claim generation: %v", err)
	}
	// A session-scoped conversation is created per session, so use another one.
	rec2 := sampleRecord("branches")
	rec2.Mode = domain.SessionModeChat
	rec2.Metadata.ControllerGeneration = "gen-active"
	session2, err := s.CreateSession(ctx, rec2)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	other, err := s.CreateConversation(ctx, "conversation-other", domain.ConversationScopeSession, "branches", session2.ID, testNow)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	seedQueuedTurn(t, s, session2, other.ID, "turn-other", "gen-active")
	if err := s.MarkTurnDispatching(ctx, "turn-other"); err != nil {
		t.Fatalf("mark turn-other dispatching: %v", err)
	}

	if ids := activeRunningIDs(t, s, conversation.ID, session.ID, "gen-active"); len(ids) != 0 {
		t.Fatalf("running turns in another conversation leaked into the guard: %q", ids)
	}
	if ids := activeRunningIDs(t, s, other.ID, session2.ID, "gen-active"); len(ids) != 1 {
		t.Fatalf("other conversation running = %v, want one row", ids)
	}
}
