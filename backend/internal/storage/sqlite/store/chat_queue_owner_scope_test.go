package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	storepkg "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func newScopedChatSession(t *testing.T, s interface {
	CreateSession(context.Context, domain.SessionRecord) (domain.SessionRecord, error)
}, project, generation string) domain.SessionRecord {
	t.Helper()
	rec := sampleRecord(project)
	rec.Mode = domain.SessionModeChat
	rec.Metadata.ControllerGeneration = generation
	created, err := s.CreateSession(context.Background(), rec)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return created
}

func seedScopedQueued(t *testing.T, s interface {
	AppendUserMessage(context.Context, string, domain.SessionID, string, domain.ConversationMessage, string, time.Time) (bool, error)
}, conversationID string, session domain.SessionRecord, turnID, generation string) {
	t.Helper()
	created, err := s.AppendUserMessage(context.Background(), conversationID, session.ID, generation,
		domain.ConversationMessage{
			ID: "message-" + turnID, Text: "text " + turnID,
			ClientMessageID: "client-" + turnID, Origin: domain.MessageOriginHuman,
		}, turnID, testNow)
	if err != nil || !created {
		t.Fatalf("AppendUserMessage %s: created=%v err=%v", turnID, created, err)
	}
}

func seedRunningActivity(t *testing.T, s interface {
	BindTurnToProvider(context.Context, string, string, time.Time) error
	UpsertActivity(context.Context, string, string, domain.ConversationActivity, time.Time) error
}, conversationID, turnID, providerTurnID, activityID string) {
	t.Helper()
	if err := s.BindTurnToProvider(context.Background(), turnID, providerTurnID, testNow); err != nil {
		t.Fatalf("BindTurnToProvider %s: %v", turnID, err)
	}
	if err := s.UpsertActivity(context.Background(), conversationID, providerTurnID, domain.ConversationActivity{
		ID: activityID, Kind: domain.ActivityKindCommand, Status: domain.ActivityStatusRunning,
		Summary: "running activity", ProviderItemID: activityID,
	}, testNow); err != nil {
		t.Fatalf("UpsertActivity %s: %v", activityID, err)
	}
}

func seedReviewConversation(t *testing.T, s interface {
	UpsertReview(context.Context, domain.Review) error
	CreateReviewConversation(context.Context, string, string, domain.ProjectID, domain.SessionID, time.Time) (domain.ConversationRecord, error)
	ClaimReviewChatController(context.Context, string, string, string, time.Time) (bool, error)
	AppendReviewUserMessage(context.Context, string, domain.SessionID, string, string, domain.ConversationMessage, string, time.Time) (bool, error)
}, reviewID string, harness domain.ReviewerHarness, worker domain.SessionRecord, conversationID, turnID, generation string) domain.ConversationRecord {
	t.Helper()
	review := domain.Review{
		ID: reviewID, SessionID: worker.ID, ProjectID: worker.ProjectID,
		Harness: harness, CreatedAt: testNow, UpdatedAt: testNow,
	}
	if err := s.UpsertReview(context.Background(), review); err != nil {
		t.Fatalf("UpsertReview: %v", err)
	}
	conversation, err := s.CreateReviewConversation(context.Background(), conversationID, reviewID, worker.ProjectID, worker.ID, testNow)
	if err != nil {
		t.Fatalf("CreateReviewConversation: %v", err)
	}
	if claimed, err := s.ClaimReviewChatController(context.Background(), reviewID, "provider-"+reviewID, generation, testNow); err != nil || !claimed {
		t.Fatalf("ClaimReviewChatController: claimed=%v err=%v", claimed, err)
	}
	created, err := s.AppendReviewUserMessage(context.Background(), conversation.ID, worker.ID, reviewID, generation,
		domain.ConversationMessage{
			ID: "message-" + turnID, Text: "review " + turnID,
			ClientMessageID: "client-" + turnID, Origin: domain.MessageOriginHuman,
		}, turnID, testNow)
	if err != nil || !created {
		t.Fatalf("AppendReviewUserMessage %s: created=%v err=%v", turnID, created, err)
	}
	return conversation
}

func TestStopScopeIsPrimaryOwnerOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "scope")
	primary := newScopedChatSession(t, s, "scope", "gen-primary")
	other := newScopedChatSession(t, s, "scope", "gen-other")
	conversation, err := s.CreateConversation(ctx, "scope-conversation", domain.ConversationScopeSession, "scope", primary.ID, testNow)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	seedScopedQueued(t, s, conversation.ID, primary, "primary-queued", "gen-primary")
	seedScopedQueued(t, s, conversation.ID, other, "other-queued", "gen-other")
	reviewConversation := seedReviewConversation(t, s, "scope-review", domain.ReviewerCodex, primary, "scope-review-conversation", "review-queued", "gen-review")

	if _, err := s.RecordStopIntentAndCancelQueued(ctx, conversation.ID, primary.ID, "", "gen-primary",
		domain.ChatStopReasonUserStop, "primary-stop", testNow); err != nil {
		t.Fatalf("primary Stop: %v", err)
	}
	if got := turnState(t, s, "primary-queued"); got != domain.TurnStateInterrupted {
		t.Fatalf("primary queued state = %q, want interrupted", got)
	}
	if got := turnState(t, s, "other-queued"); got != domain.TurnStateQueued {
		t.Fatalf("other owner queued state = %q, want untouched", got)
	}
	if got := turnState(t, s, "review-queued"); got != domain.TurnStateQueued {
		t.Fatalf("review queued state = %q, want untouched by primary Stop", got)
	}

	if _, err := s.RecordStopIntentAndCancelQueued(ctx, reviewConversation.ID, primary.ID, "scope-review", "gen-review",
		domain.ChatStopReasonUserStop, "review-stop", testNow); err != nil {
		t.Fatalf("review Stop: %v", err)
	}
	if got := turnState(t, s, "review-queued"); got != domain.TurnStateInterrupted {
		t.Fatalf("review queued state = %q, want interrupted by review Stop", got)
	}
	if got := turnState(t, s, "other-queued"); got != domain.TurnStateQueued {
		t.Fatalf("other owner queued state after review Stop = %q, want untouched", got)
	}
}

func TestClaimScopeAdoptsAndSettlesOnlyTargetSession(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "claim-scope")
	primary := newScopedChatSession(t, s, "claim-scope", "gen-primary")
	other := newScopedChatSession(t, s, "claim-scope", "gen-other")
	conversation, err := s.CreateConversation(ctx, "claim-scope-conversation", domain.ConversationScopeSession, "claim-scope", primary.ID, testNow)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	seedScopedQueued(t, s, conversation.ID, primary, "primary-queued", "gen-primary")
	seedScopedQueued(t, s, conversation.ID, primary, "primary-running", "gen-primary")
	seedRunningActivity(t, s, conversation.ID, "primary-running", "provider-primary", "activity-primary")
	seedScopedQueued(t, s, conversation.ID, other, "other-queued", "gen-other")
	seedScopedQueued(t, s, conversation.ID, other, "other-running", "gen-other")
	seedRunningActivity(t, s, conversation.ID, "other-running", "provider-other", "activity-other")
	reviewConversation := seedReviewConversation(t, s, "claim-scope-review", domain.ReviewerCodex, primary, "claim-scope-review-conversation", "review-running", "gen-review")
	seedRunningActivity(t, s, reviewConversation.ID, "review-running", "provider-review", "activity-review")

	adopted, err := s.ClaimChatControllerGenerationAndAdoptQueued(ctx, domain.ChatControllerClaim{
		SessionID: primary.ID, ConversationID: conversation.ID,
		ExpectedGeneration: "gen-primary", Generation: "gen-new",
		AdoptQueued: true, RecoveryRequired: true, SettleRunning: true, Now: testNow,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if adopted != 1 {
		t.Fatalf("adopted = %d, want only the primary queued turn", adopted)
	}
	if got := turnState(t, s, "primary-running"); got != domain.TurnStateFailed {
		t.Fatalf("primary running = %q, want failed", got)
	}
	if got := turnState(t, s, "primary-queued"); got != domain.TurnStateQueued {
		t.Fatalf("primary queued = %q, want queued", got)
	}
	if got := turnState(t, s, "other-running"); got != domain.TurnStateRunning {
		t.Fatalf("other running = %q, want untouched", got)
	}
	if got := turnState(t, s, "other-queued"); got != domain.TurnStateQueued {
		t.Fatalf("other queued = %q, want untouched", got)
	}
	if got := turnState(t, s, "review-running"); got != domain.TurnStateRunning {
		t.Fatalf("review running = %q, want untouched", got)
	}
	snapshot, err := s.LoadConversationSnapshot(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("LoadConversationSnapshot: %v", err)
	}
	activities := map[string]domain.ActivityStatus{}
	for _, activity := range snapshot.Activities {
		activities[activity.ID] = activity.Status
	}
	for _, id := range []string{"activity-other"} {
		if activities[id] != domain.ActivityStatusRunning {
			t.Fatalf("%s status = %q, want running", id, activities[id])
		}
	}
	if activities["activity-primary"] != domain.ActivityStatusFailed {
		t.Fatalf("primary activity = %q, want failed", activities["activity-primary"])
	}
	reviewSnapshot, err := s.LoadConversationSnapshot(ctx, reviewConversation.ID)
	if err != nil {
		t.Fatalf("review LoadConversationSnapshot: %v", err)
	}
	var reviewStatus domain.ActivityStatus
	for _, activity := range reviewSnapshot.Activities {
		if activity.ID == "activity-review" {
			reviewStatus = activity.Status
		}
	}
	if reviewStatus != domain.ActivityStatusRunning {
		t.Fatalf("review activity = %q, want running", reviewStatus)
	}
}

func TestRetiredSessionSettlesOwnActivitiesOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "retired")
	predecessor := newScopedChatSession(t, s, "retired", "gen-predecessor")
	target := newScopedChatSession(t, s, "retired", "gen-target")
	conversation, err := s.CreateConversation(ctx, "retired-conversation", domain.ConversationScopeSession, "retired", predecessor.ID, testNow)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	seedScopedQueued(t, s, conversation.ID, predecessor, "retired-running", "gen-predecessor")
	seedRunningActivity(t, s, conversation.ID, "retired-running", "provider-retired", "activity-retired")
	seedScopedQueued(t, s, conversation.ID, target, "target-running", "gen-target")
	seedRunningActivity(t, s, conversation.ID, "target-running", "provider-target", "activity-target")
	reviewConversation := seedReviewConversation(t, s, "retired-review", domain.ReviewerCodex, predecessor, "retired-review-conversation", "review-running", "gen-review")
	seedRunningActivity(t, s, reviewConversation.ID, "review-running", "provider-review", "activity-review")

	if err := s.SettleRetiredSessionTurns(ctx, predecessor.ID, conversation.ID, testNow); err != nil {
		t.Fatalf("SettleRetiredSessionTurns: %v", err)
	}
	if got := turnState(t, s, "retired-running"); got != domain.TurnStateFailed {
		t.Fatalf("retired turn = %q, want failed", got)
	}
	if got := turnState(t, s, "target-running"); got != domain.TurnStateRunning {
		t.Fatalf("target turn = %q, want untouched", got)
	}
	if got := turnState(t, s, "review-running"); got != domain.TurnStateRunning {
		t.Fatalf("review turn = %q, want untouched", got)
	}
	snapshot, err := s.LoadConversationSnapshot(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("LoadConversationSnapshot: %v", err)
	}
	statuses := map[string]domain.ActivityStatus{}
	for _, activity := range snapshot.Activities {
		statuses[activity.ID] = activity.Status
	}
	if statuses["activity-retired"] != domain.ActivityStatusFailed {
		t.Fatalf("retired activity = %q, want failed", statuses["activity-retired"])
	}
	if statuses["activity-target"] != domain.ActivityStatusRunning {
		t.Fatalf("target activity = %q, want running", statuses["activity-target"])
	}
	reviewSnapshot, err := s.LoadConversationSnapshot(ctx, reviewConversation.ID)
	if err != nil {
		t.Fatalf("review LoadConversationSnapshot: %v", err)
	}
	var reviewStatus domain.ActivityStatus
	for _, activity := range reviewSnapshot.Activities {
		if activity.ID == "activity-review" {
			reviewStatus = activity.Status
		}
	}
	if reviewStatus != domain.ActivityStatusRunning {
		t.Fatalf("review activity = %q, want running", reviewStatus)
	}
}

func TestRetiredSettlementRollsBackOnActivityFailure(t *testing.T) {
	s, raw := openChatStoreWithRawDB(t)
	ctx := context.Background()
	seedProject(t, s, "retired-fail")
	predecessor := newScopedChatSession(t, s, "retired-fail", "gen-predecessor")
	target := newScopedChatSession(t, s, "retired-fail", "gen-target")
	conversation, err := s.CreateConversation(ctx, "retired-fail-conversation", domain.ConversationScopeSession, "retired-fail", predecessor.ID, testNow)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	seedScopedQueued(t, s, conversation.ID, predecessor, "retired-fail-running", "gen-predecessor")
	seedRunningActivity(t, s, conversation.ID, "retired-fail-running", "provider-retired-fail", "activity-retired-fail")
	seedScopedQueued(t, s, conversation.ID, target, "target-fail-running", "gen-target")
	seedRunningActivity(t, s, conversation.ID, "target-fail-running", "provider-target-fail", "activity-target-fail")

	mustExecRaw(t, raw, `
CREATE TRIGGER fail_retired_activity
BEFORE UPDATE OF status ON conversation_activities
WHEN NEW.status = 'failed'
BEGIN
    SELECT RAISE(ABORT, 'injected retired activity failure');
END;`)

	if err := s.SettleRetiredSessionTurns(ctx, predecessor.ID, conversation.ID, testNow); err == nil {
		t.Fatal("retired settlement succeeded despite injected activity failure")
	}
	if got := turnState(t, s, "retired-fail-running"); got != domain.TurnStateRunning {
		t.Fatalf("predecessor turn = %q, want rolled back to running", got)
	}
	snapshot, err := s.LoadConversationSnapshot(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("LoadConversationSnapshot: %v", err)
	}
	statuses := map[string]domain.ActivityStatus{}
	for _, activity := range snapshot.Activities {
		statuses[activity.ID] = activity.Status
	}
	if statuses["activity-retired-fail"] != domain.ActivityStatusRunning ||
		statuses["activity-target-fail"] != domain.ActivityStatusRunning {
		t.Fatalf("activities after rollback = %+v, want all running", statuses)
	}
}

func TestTwoReviewOwnersHaveIndependentStopEpisodes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "review-isolation")
	worker := newScopedChatSession(t, s, "review-isolation", "gen-worker")
	reviewOne := seedReviewConversation(t, s, "review-one", domain.ReviewerCodex, worker, "review-one-conversation", "review-one-turn", "gen-review-one")
	reviewTwo := seedReviewConversation(t, s, "review-two", domain.ReviewerClaudeCode, worker, "review-two-conversation", "review-two-turn", "gen-review-two")

	if _, err := s.RecordStopIntentAndCancelQueued(ctx, reviewOne.ID, worker.ID, "review-one", "gen-review-one",
		domain.ChatStopReasonUserStop, "review-one-stop", testNow); err != nil {
		t.Fatalf("review one Stop: %v", err)
	}
	if got := turnState(t, s, "review-one-turn"); got != domain.TurnStateInterrupted {
		t.Fatalf("review one state = %q, want interrupted", got)
	}
	if got := turnState(t, s, "review-two-turn"); got != domain.TurnStateQueued {
		t.Fatalf("review two state = %q, want untouched", got)
	}
	episodeTwo, err := s.ConversationStopEpisode(ctx, reviewTwo.ID)
	if err != nil || episodeTwo != "" {
		t.Fatalf("review two episode = %q err=%v, want empty", episodeTwo, err)
	}

	// A stale review generation cannot append or Stop after replacement.
	if _, err := s.AppendReviewUserMessageFenced(ctx, reviewTwo.ID, worker.ID, "review-two", "gen-stale", "",
		domain.ConversationMessage{
			ID: "stale-review-message", Text: "stale review", Origin: domain.MessageOriginHuman,
			ClientMessageID: "stale-review-message",
		}, "stale-review-turn", testNow); !errors.Is(err, storepkg.ErrChatIntakeClosed) {
		t.Fatalf("stale review append error = %v, want ErrChatIntakeClosed", err)
	}
	if _, err := s.RecordStopIntentAndCancelQueued(ctx, reviewTwo.ID, worker.ID, "review-two", "gen-stale",
		domain.ChatStopReasonUserStop, "stale-review-stop", testNow); !errors.Is(err, storepkg.ErrChatOwnershipLost) {
		t.Fatalf("stale review Stop error = %v, want ErrChatOwnershipLost", err)
	}
	if got := turnState(t, s, "review-two-turn"); got != domain.TurnStateQueued {
		t.Fatalf("stale review changed state = %q", got)
	}
}

func TestReviewAppendFenceRejectsChangedEpisode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "review-episode")
	worker := newScopedChatSession(t, s, "review-episode", "gen-worker")
	conversation := seedReviewConversation(t, s, "review-episode", domain.ReviewerCodex, worker, "review-episode-conversation", "review-episode-turn", "gen-review")
	if _, err := s.RecordStopIntentAndCancelQueued(ctx, conversation.ID, worker.ID, "review-episode", "gen-review",
		domain.ChatStopReasonUserStop, "review-episode-stop", testNow); err != nil {
		t.Fatalf("review Stop: %v", err)
	}
	// Captured before the Stop: the append must observe a changed episode.
	if _, err := s.AppendReviewUserMessageFenced(ctx, conversation.ID, worker.ID, "review-episode", "gen-review", "",
		domain.ConversationMessage{
			ID: "old-episode-message", Text: "old episode", Origin: domain.MessageOriginHuman,
			ClientMessageID: "old-episode-message",
		}, "old-episode-turn", testNow.Add(time.Minute)); !errors.Is(err, storepkg.ErrChatIntakeClosed) {
		t.Fatalf("append with stale review episode error = %v, want ErrChatIntakeClosed", err)
	}
	episode, err := s.ConversationStopEpisode(ctx, conversation.ID)
	if err != nil || episode == "" {
		t.Fatalf("review Stop episode = %q err=%v, want durable", episode, err)
	}
}

func TestArchivedOrNonChatReviewCannotDispatchButCleanupIsScoped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "review-archive")
	worker := newScopedChatSession(t, s, "review-archive", "gen-worker")
	conversation := seedReviewConversation(t, s, "review-archive", domain.ReviewerCodex, worker, "review-archive-conversation", "review-archive-turn", "gen-review")
	if ok, err := s.SetReviewInterfaceMode(ctx, "review-archive", domain.ReviewerInterfaceTUI, testNow.Add(time.Second)); err != nil || !ok {
		t.Fatalf("SetReviewInterfaceMode: ok=%v err=%v", ok, err)
	}
	if _, err := s.AppendReviewUserMessageFenced(ctx, conversation.ID, worker.ID, "review-archive", "gen-review", "",
		domain.ConversationMessage{
			ID: "archived-review-message", Text: "archived review", Origin: domain.MessageOriginHuman,
			ClientMessageID: "archived-review-message",
		}, "archived-review-turn", testNow); !errors.Is(err, storepkg.ErrChatIntakeClosed) {
		t.Fatalf("archived review append error = %v, want ErrChatIntakeClosed", err)
	}
	if claimed, err := s.ClaimQueuedTurnForDispatch(
		ctx, conversation.ID, worker.ID, "review-archive", "gen-review", "review-archive-turn", testNow); claimed || !errors.Is(err, storepkg.ErrChatOwnershipLost) {
		t.Fatalf("archived review dispatch claim = %v, %v, want ErrChatOwnershipLost", claimed, err)
	}
	if _, err := s.RecordStopIntentAndCancelQueued(ctx, conversation.ID, worker.ID, "review-archive", "gen-review",
		domain.ChatStopReasonUserStop, "archived-review-stop", testNow); err != nil {
		t.Fatalf("archived review cleanup Stop: %v", err)
	}
	if got := turnState(t, s, "review-archive-turn"); got != domain.TurnStateInterrupted {
		t.Fatalf("archived review queue = %q, want interrupted by scoped cleanup", got)
	}
}
