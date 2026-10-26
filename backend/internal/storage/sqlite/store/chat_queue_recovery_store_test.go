package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	storepkg "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func openChatStoreWithRawDB(t *testing.T) (*sqlite.Store, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw SQLite: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return s, raw
}

func mustExecRaw(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("exec raw SQLite: %v", err)
	}
}

func turnGeneration(t *testing.T, db *sql.DB, turnID string) string {
	t.Helper()
	var generation string
	if err := db.QueryRow(`SELECT controller_generation FROM conversation_turns WHERE id = ?`, turnID).Scan(&generation); err != nil {
		t.Fatalf("read turn generation %s: %v", turnID, err)
	}
	return generation
}

func seedOpenChatSession(t *testing.T, s *sqlite.Store) (domain.SessionRecord, domain.ConversationRecord) {
	t.Helper()
	ctx := context.Background()
	seedProject(t, s, "recovery")
	rec := sampleRecord("recovery")
	rec.Mode = domain.SessionModeChat
	rec.Metadata.ProviderConversationID = "thread-recovery"
	rec.Metadata.ControllerGeneration = "generation-old"
	session, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	conversation, err := s.CreateConversation(
		ctx, "conversation-recovery", domain.ConversationScopeSession,
		"recovery", session.ID, testNow,
	)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return session, conversation
}

func TestRecordStopIntentAndCancelQueuedIsAtomicAndPreservesPostCutoff(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()

	seedQueuedTurn(t, s, session, conversation.ID, "turn-stop-1", "generation-old")
	seedQueuedTurn(t, s, session, conversation.ID, "turn-stop-2", "generation-old")

	intent, err := s.RecordStopIntentAndCancelQueued(
		ctx, conversation.ID, session.ID, "", "generation-old", domain.ChatStopReasonUserStop, "episode-1", testNow)
	if err != nil {
		t.Fatalf("record stop: %v", err)
	}
	if intent.CutoffRowID == 0 || intent.CancelledRows != 2 {
		t.Fatalf("intent = %+v, want a nonzero cutoff and two cancellations", intent)
	}
	for _, id := range []string{"turn-stop-1", "turn-stop-2"} {
		if got := turnState(t, s, id); got != domain.TurnStateInterrupted {
			t.Fatalf("%s state = %q, want interrupted", id, got)
		}
	}

	// A later accepted row is after the cutoff and must survive a second Stop
	// with its own, later cutoff.
	seedQueuedTurn(t, s, session, conversation.ID, "turn-after-stop", "generation-old")
	second, err := s.RecordStopIntentAndCancelQueued(
		ctx, conversation.ID, session.ID, "", "generation-old", domain.ChatStopReasonUserStop, "episode-2", testNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("record second stop: %v", err)
	}
	if second.CutoffRowID <= intent.CutoffRowID {
		t.Fatalf("second cutoff %d did not advance past %d", second.CutoffRowID, intent.CutoffRowID)
	}
	if got := turnState(t, s, "turn-after-stop"); got != domain.TurnStateInterrupted {
		t.Fatalf("post-first-stop row state = %q, want interrupted by second Stop", got)
	}
}

func TestRecordStopIntentRollsBackWhenQueueCancellationFails(t *testing.T) {
	s, raw := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "turn-rollback", "generation-old")

	mustExecRaw(t, raw, `
CREATE TRIGGER fail_chat_queue_stop
BEFORE UPDATE OF state ON conversation_turns
WHEN NEW.state = 'interrupted'
BEGIN
    SELECT RAISE(ABORT, 'injected stop cancellation failure');
END;`)

	if _, err := s.RecordStopIntentAndCancelQueued(
		ctx, conversation.ID, session.ID, "", "generation-old", domain.ChatStopReasonUserStop, "episode-failed", testNow); err == nil {
		t.Fatal("stop succeeded despite injected cancellation failure")
	}
	if got := turnState(t, s, "turn-rollback"); got != domain.TurnStateQueued {
		t.Fatalf("turn state = %q after rollback, want queued", got)
	}
	var reason, episode string
	var cutoff int64
	if err := raw.QueryRow(`SELECT stop_reason, stop_episode_id, stop_cutoff_rowid
        FROM conversations WHERE id = ?`, conversation.ID).Scan(&reason, &episode, &cutoff); err != nil {
		t.Fatalf("read stop intent: %v", err)
	}
	if reason != "" || episode != "" || cutoff != 0 {
		t.Fatalf("partial stop intent survived rollback: (%q, %q, %d)", reason, episode, cutoff)
	}
}

func TestClaimAndAdoptQueuedIsAtomicAndRefusesRunning(t *testing.T) {
	s, raw := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()

	seedQueuedTurn(t, s, session, conversation.ID, "turn-queued", "generation-old")
	seedQueuedTurn(t, s, session, conversation.ID, "turn-running", "generation-old")
	if err := s.MarkTurnDispatching(ctx, "turn-running"); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	claim := domain.ChatControllerClaim{
		SessionID:          session.ID,
		ConversationID:     conversation.ID,
		ExpectedGeneration: "generation-old",
		Generation:         "generation-new",
		AdoptQueued:        true,
		RecoveryRequired:   true,
		Now:                testNow,
	}
	if _, err := s.ClaimChatControllerGenerationAndAdoptQueued(ctx, claim); !errors.Is(err, domain.ErrChatControllerRunningTurn) {
		t.Fatalf("claim with running turn error = %v, want ErrChatControllerRunningTurn", err)
	}
	if got := turnState(t, s, "turn-queued"); got != domain.TurnStateQueued {
		t.Fatalf("queued state = %q after refused claim, want queued", got)
	}
	current, found, err := s.GetSession(ctx, session.ID)
	if err != nil || !found || current.Metadata.ControllerGeneration != "generation-old" {
		t.Fatalf("generation after refused claim = %q, found=%v err=%v", current.Metadata.ControllerGeneration, found, err)
	}

	claim.SettleRunning = true
	adopted, err := s.ClaimChatControllerGenerationAndAdoptQueued(ctx, claim)
	if err != nil {
		t.Fatalf("recovery claim: %v", err)
	}
	if adopted != 1 {
		t.Fatalf("adopted = %d, want 1", adopted)
	}
	if got := turnState(t, s, "turn-running"); got != domain.TurnStateFailed {
		t.Fatalf("running state = %q, want failed", got)
	}
	if got := turnState(t, s, "turn-queued"); got != domain.TurnStateQueued {
		t.Fatalf("queued state = %q, want still queued", got)
	}
	if got := turnGeneration(t, raw, "turn-queued"); got != "generation-new" {
		t.Fatalf("queued generation = %q, want new", got)
	}
	facts, err := s.ConversationQueueFacts(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("queue facts: %v", err)
	}
	if !facts.QueueRecoveryRequired || facts.PendingCount != 1 || facts.RunningCount != 0 {
		t.Fatalf("queue facts after adoption = %+v", facts)
	}
}

func TestClaimAndAdoptRefusesStaleExpectedGeneration(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "turn-stale", "generation-old")

	_, err := s.ClaimChatControllerGenerationAndAdoptQueued(ctx, domain.ChatControllerClaim{
		SessionID:          session.ID,
		ConversationID:     conversation.ID,
		ExpectedGeneration: "generation-wrong",
		Generation:         "generation-new",
		AdoptQueued:        true,
		RecoveryRequired:   true,
		Now:                testNow,
	})
	if !errors.Is(err, domain.ErrChatControllerGenerationChanged) {
		t.Fatalf("stale claim error = %v, want ErrChatControllerGenerationChanged", err)
	}
	if got := turnState(t, s, "turn-stale"); got != domain.TurnStateQueued {
		t.Fatalf("turn state after stale claim = %q, want queued", got)
	}
	current, found, readErr := s.GetSession(ctx, session.ID)
	if readErr != nil || !found || current.Metadata.ControllerGeneration != "generation-old" {
		t.Fatalf("generation after stale claim = %q, found=%v err=%v",
			current.Metadata.ControllerGeneration, found, readErr)
	}
}

func TestClaimAndAdoptRollsBackGenerationWhenAdoptionFails(t *testing.T) {
	s, raw := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "turn-adoption", "generation-old")

	mustExecRaw(t, raw, `
CREATE TRIGGER fail_chat_queue_adoption
BEFORE UPDATE OF controller_generation ON conversation_turns
WHEN NEW.id = 'turn-adoption'
BEGIN
    SELECT RAISE(ABORT, 'injected adoption failure');
END;`)

	_, err := s.ClaimChatControllerGenerationAndAdoptQueued(ctx, domain.ChatControllerClaim{
		SessionID:          session.ID,
		ConversationID:     conversation.ID,
		ExpectedGeneration: "generation-old",
		Generation:         "generation-new",
		AdoptQueued:        true,
		RecoveryRequired:   true,
		Now:                testNow,
	})
	if err == nil {
		t.Fatal("claim succeeded despite injected adoption failure")
	}
	current, found, readErr := s.GetSession(ctx, session.ID)
	if readErr != nil || !found || current.Metadata.ControllerGeneration != "generation-old" {
		t.Fatalf("generation after rollback = %q, found=%v err=%v",
			current.Metadata.ControllerGeneration, found, readErr)
	}
	turn, turnErr := s.TurnByID(ctx, "turn-adoption")
	if turnErr != nil {
		t.Fatalf("read turn after rollback: %v", turnErr)
	}
	if turn.State != domain.TurnStateQueued || turnGeneration(t, raw, "turn-adoption") != "generation-old" {
		t.Fatalf("turn after rollback = %+v, want queued under old generation", turn)
	}
	facts, factsErr := s.ConversationQueueFacts(ctx, conversation.ID)
	if factsErr != nil {
		t.Fatalf("queue facts after rollback: %v", factsErr)
	}
	if facts.QueueRecoveryRequired {
		t.Fatal("recovery hold survived rollback")
	}
}

func TestQueuedOnlyClaimKeepsRecoveryHoldAcrossRepeatedRestarts(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "turn-only", "generation-old")

	claim := domain.ChatControllerClaim{
		SessionID:          session.ID,
		ConversationID:     conversation.ID,
		ExpectedGeneration: "generation-old",
		Generation:         "generation-one",
		AdoptQueued:        true,
		RecoveryRequired:   true,
		Now:                testNow,
	}
	if adopted, err := s.ClaimChatControllerGenerationAndAdoptQueued(ctx, claim); err != nil || adopted != 1 {
		t.Fatalf("first restart: adopted=%d err=%v", adopted, err)
	}
	facts, err := s.ConversationQueueFacts(ctx, conversation.ID)
	if err != nil || !facts.QueueRecoveryRequired {
		t.Fatalf("first restart hold = %+v, err=%v", facts, err)
	}

	claim.ExpectedGeneration = "generation-one"
	claim.Generation = "generation-two"
	claim.Now = testNow.Add(time.Minute)
	if adopted, err := s.ClaimChatControllerGenerationAndAdoptQueued(ctx, claim); err != nil || adopted != 1 {
		t.Fatalf("second restart: adopted=%d err=%v", adopted, err)
	}
	facts, err = s.ConversationQueueFacts(ctx, conversation.ID)
	if err != nil || !facts.QueueRecoveryRequired || facts.PendingCount != 1 {
		t.Fatalf("second restart hold = %+v, err=%v", facts, err)
	}
	if got := turnState(t, s, "turn-only"); got != domain.TurnStateQueued {
		t.Fatalf("turn state after two restarts = %q, want queued", got)
	}
}

func TestCancelLastQueuedClearsRecoveryHold(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "turn-cancel", "generation-old")
	if _, err := s.ClaimChatControllerGenerationAndAdoptQueued(ctx, domain.ChatControllerClaim{
		SessionID:          session.ID,
		ConversationID:     conversation.ID,
		ExpectedGeneration: "generation-old",
		Generation:         "generation-new",
		AdoptQueued:        true,
		RecoveryRequired:   true,
		Now:                testNow,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.CancelQueuedTurnByID(ctx, conversation.ID, "turn-cancel", testNow); err != nil {
		t.Fatalf("cancel last queued: %v", err)
	}
	facts, err := s.ConversationQueueFacts(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("queue facts: %v", err)
	}
	if facts.QueueRecoveryRequired || facts.PendingCount != 0 {
		t.Fatalf("hold after last cancellation = %+v, want cleared", facts)
	}
}

func TestCleanupOwnedControllerWorkSettlesOnlyRunning(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "generation-live"); err != nil {
		t.Fatalf("claim live generation: %v", err)
	}
	seedQueuedTurn(t, s, session, conversation.ID, "turn-queued", "generation-live")
	seedQueuedTurn(t, s, session, conversation.ID, "turn-running", "generation-live")
	if err := s.MarkTurnDispatching(ctx, "turn-running"); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	owned, err := s.CleanupOwnedControllerWork(ctx, session.ID, conversation.ID, "generation-live", testNow)
	if err != nil || !owned {
		t.Fatalf("cleanup: owned=%v err=%v", owned, err)
	}
	if got := turnState(t, s, "turn-running"); got != domain.TurnStateFailed {
		t.Fatalf("running state = %q, want failed", got)
	}
	if got := turnState(t, s, "turn-queued"); got != domain.TurnStateQueued {
		t.Fatalf("queued state = %q, want preserved", got)
	}
}

func TestClaimAndAdoptExcludesOtherSessionConversationAndPromotion(t *testing.T) {
	s, raw := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()

	seedQueuedTurn(t, s, session, conversation.ID, "turn-adopt", "generation-old")
	seedQueuedTurn(t, s, session, conversation.ID, "turn-promoted", "generation-old")
	if _, err := s.ReserveQueuedTurnForPromotion(ctx, conversation.ID, "turn-promoted", testNow); err != nil {
		t.Fatalf("reserve promotion: %v", err)
	}

	otherSession := sampleRecord("recovery")
	otherSession.ID = "other-session"
	otherSession.Mode = domain.SessionModeChat
	other, err := s.CreateSession(ctx, otherSession)
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}
	otherConversation, err := s.CreateConversation(
		ctx, "other-conversation", domain.ConversationScopeSession,
		"recovery", other.ID, testNow)
	if err != nil {
		t.Fatalf("create other conversation: %v", err)
	}
	seedQueuedTurn(t, s, other, otherConversation.ID, "turn-other", "other-generation")

	adopted, err := s.ClaimChatControllerGenerationAndAdoptQueued(ctx, domain.ChatControllerClaim{
		SessionID:          session.ID,
		ConversationID:     conversation.ID,
		ExpectedGeneration: "generation-old",
		Generation:         "generation-new",
		AdoptQueued:        true,
		RecoveryRequired:   true,
		Now:                testNow,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if adopted != 1 {
		t.Fatalf("adopted = %d, want only the unpromoted own-session turn", adopted)
	}
	if got := turnState(t, s, "turn-promoted"); got != domain.TurnStateQueued {
		t.Fatalf("promoted source state = %q, want queued and unmodified", got)
	}
	if got := turnGeneration(t, raw, "turn-promoted"); got != "generation-old" {
		t.Fatalf("promoted source generation = %q, want old", got)
	}
	if got := turnState(t, s, "turn-other"); got != domain.TurnStateQueued {
		t.Fatalf("other session state = %q, want queued", got)
	}
	if got := turnGeneration(t, raw, "turn-other"); got != "other-generation" {
		t.Fatalf("other generation = %q, want untouched", got)
	}
}

func TestDispatchTransitionRefusesCancelledQueuedTurn(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "turn-cancel-dispatch", "generation-old")
	if err := s.CancelQueuedTurnByID(ctx, conversation.ID, "turn-cancel-dispatch", testNow); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	if err := s.MarkTurnDispatching(ctx, "turn-cancel-dispatch"); !errors.Is(err, storepkg.ErrTurnDispatchFenced) {
		t.Fatalf("MarkTurnDispatching error = %v, want ErrTurnDispatchFenced", err)
	}
	if got := turnState(t, s, "turn-cancel-dispatch"); got != domain.TurnStateCancelled {
		t.Fatalf("turn state = %q, want cancelled", got)
	}
}

func TestBranchAndQueuePublicationRollsBackOnAdoptionFailure(t *testing.T) {
	s, raw := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "branch-queue", "generation-old")
	mustExecRaw(t, raw, `
CREATE TRIGGER fail_branch_queue_adoption
BEFORE UPDATE OF controller_generation ON conversation_turns
WHEN NEW.id = 'branch-queue'
BEGIN
    SELECT RAISE(ABORT, 'injected branch adoption failure');
END;`)
	branch := domain.ConversationBranch{
		ID: "branch-atomic", ConversationID: conversation.ID, SessionID: session.ID,
		ProviderConversationID: "thread-atomic", ParentBranchID: conversation.ActiveBranchID,
		ForkAfterSequence: conversation.LatestSequence, ProviderScopeID: "branch-atomic",
		CreatedAt: testNow,
	}
	if err := s.CreateAndActivateConversationBranchAndAdoptQueued(
		ctx, session.ID, branch, "generation-old", "generation-new", testNow); err == nil {
		t.Fatal("branch publication succeeded despite injected adoption failure")
	}
	gotConversation, err := s.ConversationForSession(ctx, session.ID)
	if err != nil || gotConversation.ActiveBranchID != conversation.ActiveBranchID {
		t.Fatalf("active branch after rollback = %+v err=%v", gotConversation, err)
	}
	if _, err := s.ConversationBranch(ctx, conversation.ID, branch.ID); !errors.Is(err, domain.ErrNoConversationBranch) {
		t.Fatalf("branch after rollback error = %v, want ErrNoConversationBranch", err)
	}
	rec, found, err := s.GetSession(ctx, session.ID)
	if err != nil || !found || rec.Metadata.ControllerGeneration != "generation-old" {
		t.Fatalf("generation after rollback = %q found=%v err=%v", rec.Metadata.ControllerGeneration, found, err)
	}
	if got := turnState(t, s, "branch-queue"); got != domain.TurnStateQueued {
		t.Fatalf("queued state after rollback = %q, want queued", got)
	}
	facts, err := s.ConversationQueueFacts(ctx, conversation.ID)
	if err != nil || facts.QueueRecoveryRequired {
		t.Fatalf("hold after rollback = %+v err=%v", facts, err)
	}
}
