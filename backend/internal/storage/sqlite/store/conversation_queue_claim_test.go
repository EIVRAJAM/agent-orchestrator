package store_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// seedQueuedTurn appends a never-sent queued turn with an explicit controller
// generation, so a test can make the row's own generation disagree with the
// handling session's live generation.
func seedQueuedTurn(t *testing.T, s *sqlite.Store, session domain.SessionRecord, conversationID, turnID, generation string) {
	t.Helper()
	created, err := s.AppendUserMessage(context.Background(), conversationID, session.ID, generation, domain.ConversationMessage{
		ID:              "message-" + turnID,
		Origin:          domain.MessageOriginAutomation,
		Text:            "queued " + turnID,
		ClientMessageID: "client-" + turnID,
	}, turnID, testNow)
	if err != nil || !created {
		t.Fatalf("AppendUserMessage %s: created=%v err=%v", turnID, created, err)
	}
}

func turnState(t *testing.T, s *sqlite.Store, turnID string) domain.TurnState {
	t.Helper()
	turn, err := s.TurnByID(context.Background(), turnID)
	if err != nil {
		t.Fatalf("TurnByID %s: %v", turnID, err)
	}
	return turn.State
}

// TestClaimQueuedTurnForResumeClaimsExactTurn proves the CAS promotes exactly the
// named turn and leaves its sibling queued.
func TestClaimQueuedTurnForResumeClaimsExactTurn(t *testing.T) {
	s, session, conversation := seededChatConversation(t)
	ctx := context.Background()
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-live"); err != nil {
		t.Fatalf("claim generation: %v", err)
	}
	seedQueuedTurn(t, s, session, conversation.ID, "turn-a", "gen-live")
	seedQueuedTurn(t, s, session, conversation.ID, "turn-b", "gen-live")

	claimed, err := s.ClaimQueuedTurnForResume(ctx, conversation.ID, "turn-a", "gen-live", testNow)
	if err != nil || !claimed {
		t.Fatalf("claim turn-a: claimed=%v err=%v, want true", claimed, err)
	}
	if got := turnState(t, s, "turn-a"); got != domain.TurnStateRunning {
		t.Fatalf("turn-a state = %q, want running", got)
	}
	if got := turnState(t, s, "turn-b"); got != domain.TurnStateQueued {
		t.Fatalf("turn-b state = %q, want still queued", got)
	}
}

// TestClaimQueuedTurnForResumeRefusesStaleTurnGeneration proves a row that
// belongs to an older controller epoch cannot be claimed for the live one: the
// row's own generation is compared, not only the session's.
func TestClaimQueuedTurnForResumeRefusesStaleTurnGeneration(t *testing.T) {
	s, session, conversation := seededChatConversation(t)
	ctx := context.Background()
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-live"); err != nil {
		t.Fatalf("claim generation: %v", err)
	}
	// The row was queued under the previous epoch; the session has since moved on.
	seedQueuedTurn(t, s, session, conversation.ID, "turn-old", "gen-old")

	claimed, err := s.ClaimQueuedTurnForResume(ctx, conversation.ID, "turn-old", "gen-live", testNow)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatalf("claimed a row from an older epoch; want refusal")
	}
	if got := turnState(t, s, "turn-old"); got != domain.TurnStateQueued {
		t.Fatalf("turn state = %q, want still queued", got)
	}
}

// TestClaimQueuedTurnForResumeRefusesChangedLiveGeneration proves the live
// session generation is checked inside the same statement: a row whose own
// generation matches the caller is still refused once the session advanced.
func TestClaimQueuedTurnForResumeRefusesChangedLiveGeneration(t *testing.T) {
	s, session, conversation := seededChatConversation(t)
	ctx := context.Background()
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-live"); err != nil {
		t.Fatalf("claim generation: %v", err)
	}
	seedQueuedTurn(t, s, session, conversation.ID, "turn-a", "gen-live")

	// The controller is replaced after the row was queued but before the claim.
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-new"); err != nil {
		t.Fatalf("advance generation: %v", err)
	}

	claimed, err := s.ClaimQueuedTurnForResume(ctx, conversation.ID, "turn-a", "gen-live", testNow)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatalf("claimed after the live generation changed; want refusal")
	}
	if got := turnState(t, s, "turn-a"); got != domain.TurnStateQueued {
		t.Fatalf("turn state = %q, want still queued", got)
	}
}

// TestClaimQueuedTurnForResumeRefusesWrongNamedGeneration proves a correct turn
// id with a wrong generation is rejected.
func TestClaimQueuedTurnForResumeRefusesWrongNamedGeneration(t *testing.T) {
	s, session, conversation := seededChatConversation(t)
	ctx := context.Background()
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-live"); err != nil {
		t.Fatalf("claim generation: %v", err)
	}
	seedQueuedTurn(t, s, session, conversation.ID, "turn-a", "gen-live")

	claimed, err := s.ClaimQueuedTurnForResume(ctx, conversation.ID, "turn-a", "gen-wrong", testNow)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatalf("claimed with a wrong named generation; want refusal")
	}
	if got := turnState(t, s, "turn-a"); got != domain.TurnStateQueued {
		t.Fatalf("turn state = %q, want still queued", got)
	}
}

// TestClaimQueuedTurnForResumeNeverClaimsAnotherTurn proves the CAS is bound to
// the exact id: once claimed, the same id cannot be claimed again, and a sibling
// in the same state and generation is untouched until it is named.
func TestClaimQueuedTurnForResumeNeverClaimsAnotherTurn(t *testing.T) {
	s, session, conversation := seededChatConversation(t)
	ctx := context.Background()
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-live"); err != nil {
		t.Fatalf("claim generation: %v", err)
	}
	seedQueuedTurn(t, s, session, conversation.ID, "turn-a", "gen-live")
	seedQueuedTurn(t, s, session, conversation.ID, "turn-b", "gen-live")

	first, err := s.ClaimQueuedTurnForResume(ctx, conversation.ID, "turn-a", "gen-live", testNow)
	if err != nil || !first {
		t.Fatalf("first claim: claimed=%v err=%v", first, err)
	}
	second, err := s.ClaimQueuedTurnForResume(ctx, conversation.ID, "turn-a", "gen-live", testNow)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second {
		t.Fatalf("re-claimed the same turn; want refusal")
	}
	if got := turnState(t, s, "turn-b"); got != domain.TurnStateQueued {
		t.Fatalf("sibling turn-b state = %q, want still queued", got)
	}
}
