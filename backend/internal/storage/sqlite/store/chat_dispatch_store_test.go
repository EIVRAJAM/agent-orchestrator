package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	storepkg "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestClaimQueuedTurnForDispatchIsExclusive(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "dispatch-claim", "generation-old")

	claimed, err := s.ClaimQueuedTurnForDispatch(
		ctx, conversation.ID, session.ID, "", "generation-old", "dispatch-claim", testNow)
	if err != nil || !claimed {
		t.Fatalf("first dispatch claim = %v, %v", claimed, err)
	}
	second, err := s.ClaimQueuedTurnForDispatch(
		ctx, conversation.ID, session.ID, "", "generation-old", "dispatch-claim", testNow)
	if !errors.Is(err, storepkg.ErrTurnDispatchFenced) || second {
		t.Fatalf("second dispatch claim = %v, %v, want ErrTurnDispatchFenced", second, err)
	}
	if got := turnState(t, s, "dispatch-claim"); got != domain.TurnStateRunning {
		t.Fatalf("turn state = %q, want running", got)
	}
}

func TestBindPreparedDeferredIsAtomicAndStopFenced(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "deferred-bind", "generation-old")

	if err := s.CancelQueuedTurnByID(ctx, conversation.ID, "deferred-bind", testNow); err != nil {
		t.Fatalf("cancel deferred turn: %v", err)
	}
	bound, err := s.BindPreparedDeferredTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "deferred-bind", "provider-deferred", testNow)
	if !errors.Is(err, storepkg.ErrTurnDispatchFenced) || bound {
		t.Fatalf("deferred bind after Stop = %v, %v, want ErrTurnDispatchFenced", bound, err)
	}
	turn, err := s.TurnByID(ctx, "deferred-bind")
	if err != nil {
		t.Fatalf("read turn: %v", err)
	}
	if turn.State != domain.TurnStateCancelled || turn.ProviderTurnID != "" || turn.StartedAt != nil {
		t.Fatalf("fenced deferred turn = %+v, want cancelled/unbound/unstarted", turn)
	}

	seedQueuedTurn(t, s, session, conversation.ID, "deferred-win", "generation-old")
	bound, err = s.BindPreparedDeferredTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "deferred-win", "provider-deferred-win", testNow)
	if err != nil || !bound {
		t.Fatalf("deferred bind = %v, %v", bound, err)
	}
	turn, err = s.TurnByID(ctx, "deferred-win")
	if err != nil {
		t.Fatalf("read winning turn: %v", err)
	}
	if turn.State != domain.TurnStateRunning || turn.ProviderTurnID != "provider-deferred-win" || turn.StartedAt == nil {
		t.Fatalf("deferred winner = %+v, want running with provider id and started_at", turn)
	}
}

func TestBindClaimedEagerRequiresRunningAndRefusesRebind(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "eager-bind", "generation-old")

	if err := s.BindClaimedEagerTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "eager-bind", "provider-eager", testNow); !errors.Is(err, storepkg.ErrTurnDispatchFenced) {
		t.Fatalf("eager bind before claim = %v, want ErrTurnDispatchFenced", err)
	}
	if claimed, err := s.ClaimQueuedTurnForDispatch(
		ctx, conversation.ID, session.ID, "", "generation-old", "eager-bind", testNow); err != nil || !claimed {
		t.Fatalf("claim eager turn = %v, %v", claimed, err)
	}
	if err := s.BindClaimedEagerTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "eager-bind", "provider-eager", testNow); err != nil {
		t.Fatalf("bind claimed eager turn: %v", err)
	}
	if err := s.BindClaimedEagerTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "eager-bind", "provider-eager-2", testNow); !errors.Is(err, storepkg.ErrTurnProviderIDConflict) {
		t.Fatalf("second eager bind = %v, want ErrTurnProviderIDConflict", err)
	}
	turn, err := s.TurnByID(ctx, "eager-bind")
	if err != nil || turn.State != domain.TurnStateRunning || turn.ProviderTurnID != "provider-eager" {
		t.Fatalf("eager turn = %+v err=%v", turn, err)
	}
}

func TestDeferredBindRollsBackAllFieldsOnFault(t *testing.T) {
	s, raw := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "deferred-fault", "generation-old")
	mustExecRaw(t, raw, `
CREATE TRIGGER fail_deferred_bind
BEFORE UPDATE OF provider_turn_id ON conversation_turns
WHEN NEW.id = 'deferred-fault'
BEGIN
    SELECT RAISE(ABORT, 'injected deferred bind failure');
END;`)
	if _, err := s.BindPreparedDeferredTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "deferred-fault", "provider-fault", testNow); err == nil {
		t.Fatal("deferred bind succeeded despite injected failure")
	}
	turn, err := s.TurnByID(ctx, "deferred-fault")
	if err != nil {
		t.Fatalf("read faulted turn: %v", err)
	}
	if turn.State != domain.TurnStateQueued || turn.ProviderTurnID != "" || turn.StartedAt != nil {
		t.Fatalf("faulted deferred turn = %+v, want queued/unbound/unstarted", turn)
	}
}

func TestConcurrentDispatchClaimHasOneWinner(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "concurrent-claim", "generation-old")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ClaimQueuedTurnForDispatch(
				ctx, conversation.ID, session.ID, "", "generation-old", "concurrent-claim", testNow)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes, fenced := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storepkg.ErrTurnDispatchFenced):
			fenced++
		default:
			t.Fatalf("concurrent claim error = %v", err)
		}
	}
	if successes != 1 || fenced != 1 {
		t.Fatalf("concurrent claims = %d success, %d fenced; want 1/1", successes, fenced)
	}
}

func TestOwnerLostFencesDispatchAndStop(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedProject(t, st, "owner-lost")
	first := newScopedChatSession(t, st, "owner-lost", "gen-a")
	second := newScopedChatSession(t, st, "owner-lost", "gen-b")
	conversation, err := st.CreateConversation(ctx, "owner-lost-conversation", domain.ConversationScopeProject, "owner-lost", first.ID, testNow)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	seedScopedQueued(t, st, conversation.ID, first, "owner-queued", "gen-a")
	// A->B rebind.
	if _, err := st.CreateProjectConversationWithContextReset(ctx, "rebind-placeholder", "owner-lost", second.ID,
		domain.ConversationActivity{}, testNow.Add(time.Minute)); err != nil {
		t.Fatalf("rebind conversation: %v", err)
	}
	if _, err := st.ClaimChatControllerGenerationAndAdoptQueued(ctx, domain.ChatControllerClaim{
		SessionID: first.ID, ConversationID: conversation.ID,
		ExpectedGeneration: "gen-a", Generation: "gen-a-next",
		AdoptQueued: true, RecoveryRequired: true, SettleRunning: true, Now: testNow,
	}); !errors.Is(err, storepkg.ErrChatOwnershipLost) {
		t.Fatalf("stale claim error = %v, want ErrChatOwnershipLost", err)
	}
	if _, err := st.RecordStopIntentAndCancelQueued(ctx, conversation.ID, first.ID, "", "gen-a",
		domain.ChatStopReasonUserStop, "stale-stop", testNow); !errors.Is(err, storepkg.ErrChatOwnershipLost) {
		t.Fatalf("stale stop error = %v, want ErrChatOwnershipLost", err)
	}
	if got := turnState(t, st, "owner-queued"); got != domain.TurnStateQueued {
		t.Fatalf("stale owner changed queued state to %q", got)
	}
	facts, err := st.ConversationQueueFacts(ctx, conversation.ID)
	if err != nil || facts.QueueRecoveryRequired {
		t.Fatalf("stale owner changed hold = %+v err=%v", facts, err)
	}
}

func TestStaleOwnerCannotPublishProviderBoundary(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedProject(t, st, "boundary-stale")
	first := newScopedChatSession(t, st, "boundary-stale", "gen-a")
	second := newScopedChatSession(t, st, "boundary-stale", "gen-b")
	conversation, err := st.CreateConversation(ctx, "boundary-stale-conversation", domain.ConversationScopeProject, "boundary-stale", first.ID, testNow)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	seedScopedQueued(t, st, conversation.ID, first, "boundary-stale-queued", "gen-a")
	branch := domain.ConversationBranch{
		ID: "stale-boundary", ConversationID: conversation.ID, SessionID: first.ID,
		ProviderConversationID: "thread-stale", ParentBranchID: conversation.ActiveBranchID,
		ForkAfterSequence: conversation.LatestSequence, ProviderScopeID: "stale-boundary",
		CreatedAt: testNow,
	}
	if _, err := st.CreateProjectConversationWithContextReset(ctx, "boundary-rebind", "boundary-stale", second.ID,
		domain.ConversationActivity{}, testNow.Add(time.Minute)); err != nil {
		t.Fatalf("rebind conversation: %v", err)
	}
	if err := st.CreateAndActivateConversationBranchAndAdoptQueued(
		ctx, first.ID, branch, "gen-a", "gen-a-next", testNow.Add(2*time.Minute)); !errors.Is(err, storepkg.ErrChatOwnershipLost) {
		t.Fatalf("stale boundary error = %v, want ErrChatOwnershipLost", err)
	}
	if _, err := st.ConversationBranch(ctx, conversation.ID, branch.ID); !errors.Is(err, domain.ErrNoConversationBranch) {
		t.Fatalf("stale boundary branch error = %v, want absent", err)
	}
	rec, found, err := st.GetSession(ctx, first.ID)
	if err != nil || !found || rec.Metadata.ControllerGeneration != "gen-a" {
		t.Fatalf("stale owner generation = %q found=%v err=%v", rec.Metadata.ControllerGeneration, found, err)
	}
	if got := turnState(t, st, "boundary-stale-queued"); got != domain.TurnStateQueued {
		t.Fatalf("stale boundary queue = %q, want untouched", got)
	}
}

func TestConcurrentControllerClaimsHaveOneWinner(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "controller-race-queue", "generation-old")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, generation := range []string{"generation-a", "generation-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ClaimChatControllerGenerationAndAdoptQueued(ctx, domain.ChatControllerClaim{
				SessionID: session.ID, ConversationID: conversation.ID,
				ExpectedGeneration: "generation-old", Generation: generation,
				AdoptQueued: true, RecoveryRequired: true, SettleRunning: true, Now: testNow,
			})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, domain.ErrChatControllerGenerationChanged) &&
			!errors.Is(err, storepkg.ErrChatOwnershipLost) &&
			!errors.Is(err, domain.ErrChatControllerRunningTurn) {
			t.Fatalf("concurrent controller claim error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent controller claims succeeded %d times, want exactly 1", successes)
	}
	rec, found, err := s.GetSession(ctx, session.ID)
	if err != nil || !found {
		t.Fatalf("GetSession: found=%v err=%v", found, err)
	}
	if rec.Metadata.ControllerGeneration != "generation-a" && rec.Metadata.ControllerGeneration != "generation-b" {
		t.Fatalf("winner generation = %q", rec.Metadata.ControllerGeneration)
	}
	turn, err := s.TurnByID(ctx, "controller-race-queue")
	if err != nil || turn.State != domain.TurnStateQueued {
		t.Fatalf("queued winner state = %+v err=%v", turn, err)
	}
}

func TestBindClaimedEagerIsIdempotentOnlyForSameProviderID(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "eager-idempotent", "generation-old")
	if claimed, err := s.ClaimQueuedTurnForDispatch(
		ctx, conversation.ID, session.ID, "", "generation-old", "eager-idempotent", testNow); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := s.BindClaimedEagerTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "eager-idempotent", "provider-same", testNow); err != nil {
		t.Fatalf("first eager bind: %v", err)
	}
	if err := s.BindClaimedEagerTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "eager-idempotent", "provider-same", testNow.Add(time.Minute)); err != nil {
		t.Fatalf("idempotent eager bind: %v", err)
	}
	turn, err := s.TurnByID(ctx, "eager-idempotent")
	if err != nil || turn.ProviderTurnID != "provider-same" || turn.StartedAt == nil {
		t.Fatalf("idempotent eager turn = %+v err=%v", turn, err)
	}
	originalStarted := *turn.StartedAt
	if err := s.BindClaimedEagerTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "eager-idempotent", "provider-other", testNow.Add(2*time.Minute)); !errors.Is(err, storepkg.ErrTurnProviderIDConflict) {
		t.Fatalf("conflicting eager bind = %v, want ErrTurnProviderIDConflict", err)
	}
	turn, err = s.TurnByID(ctx, "eager-idempotent")
	if err != nil || turn.ProviderTurnID != "provider-same" || !turn.StartedAt.Equal(originalStarted) {
		t.Fatalf("conflicting bind changed identity = %+v err=%v", turn, err)
	}
}

func TestTerminatedOwnerCannotDispatchButCanCancelOwnQueue(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "terminated-queued", "generation-old")
	seedQueuedTurn(t, s, session, conversation.ID, "terminated-running", "generation-old")
	rec, found, err := s.GetSession(ctx, session.ID)
	if err != nil || !found {
		t.Fatalf("GetSession: found=%v err=%v", found, err)
	}
	rec.IsTerminated = true
	if err := s.UpdateSession(ctx, rec); err != nil {
		t.Fatalf("terminate session: %v", err)
	}
	if claimed, err := s.ClaimQueuedTurnForDispatch(
		ctx, conversation.ID, session.ID, "", "generation-old", "terminated-queued", testNow); claimed || !errors.Is(err, storepkg.ErrChatOwnershipLost) {
		t.Fatalf("terminated dispatch claim = %v, %v, want ErrChatOwnershipLost", claimed, err)
	}
	if bound, err := s.BindPreparedDeferredTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "terminated-queued", "provider-terminal", testNow); bound || !errors.Is(err, storepkg.ErrChatOwnershipLost) {
		t.Fatalf("terminated deferred bind = %v, %v, want ErrChatOwnershipLost", bound, err)
	}
	if err := s.MarkTurnDispatching(ctx, "terminated-running"); err != nil {
		t.Fatalf("setup running: %v", err)
	}
	if err := s.BindClaimedEagerTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "terminated-running", "provider-terminal", testNow); !errors.Is(err, storepkg.ErrChatOwnershipLost) {
		t.Fatalf("terminated eager bind = %v, want ErrChatOwnershipLost", err)
	}
	if _, err := s.RecordStopIntentAndCancelQueued(ctx, conversation.ID, session.ID, "", "generation-old",
		domain.ChatStopReasonSessionTerminated, "terminated-stop", testNow); err != nil {
		t.Fatalf("terminated cleanup Stop: %v", err)
	}
	if got := turnState(t, s, "terminated-queued"); got != domain.TurnStateInterrupted {
		t.Fatalf("terminated queue state = %q, want interrupted", got)
	}
}

func TestTerminatedOwnerCannotPublishBoundary(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedProject(t, st, "terminated-boundary")
	owner := newScopedChatSession(t, st, "terminated-boundary", "gen-a")
	conversation, err := st.CreateConversation(ctx, "terminated-boundary-conversation", domain.ConversationScopeProject, "terminated-boundary", owner.ID, testNow)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	rec, found, err := st.GetSession(ctx, owner.ID)
	if err != nil || !found {
		t.Fatalf("GetSession: found=%v err=%v", found, err)
	}
	rec.IsTerminated = true
	if err := st.UpdateSession(ctx, rec); err != nil {
		t.Fatalf("terminate owner: %v", err)
	}
	branch := domain.ConversationBranch{
		ID: "terminated-boundary", ConversationID: conversation.ID, SessionID: owner.ID,
		ProviderConversationID: "thread-terminated", ParentBranchID: conversation.ActiveBranchID,
		ForkAfterSequence: conversation.LatestSequence, ProviderScopeID: "terminated-boundary",
		CreatedAt: testNow,
	}
	if err := st.CreateAndActivateConversationBranchAndAdoptQueued(
		ctx, owner.ID, branch, "gen-a", "gen-b", testNow); !errors.Is(err, storepkg.ErrChatOwnershipLost) {
		t.Fatalf("terminated boundary error = %v, want ErrChatOwnershipLost", err)
	}
	if _, err := st.ConversationBranch(ctx, conversation.ID, branch.ID); !errors.Is(err, domain.ErrNoConversationBranch) {
		t.Fatalf("terminated boundary branch error = %v, want absent", err)
	}
}

func TestInterruptPreparedDeferredTurnClearsOnlyPreparedIdentity(t *testing.T) {
	s, _ := openChatStoreWithRawDB(t)
	session, conversation := seedOpenChatSession(t, s)
	ctx := context.Background()
	seedQueuedTurn(t, s, session, conversation.ID, "prepared-stop", "generation-old")
	if bound, err := s.BindPreparedDeferredTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "prepared-stop", "provider-prepared", testNow); err != nil || !bound {
		t.Fatalf("BindPreparedDeferredTurn: bound=%v err=%v", bound, err)
	}
	if settled, err := s.InterruptPreparedDeferredTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "prepared-stop", "wrong-provider", testNow); err != nil || settled {
		t.Fatalf("wrong prepared id = settled=%v err=%v, want no mutation", settled, err)
	}
	turn, err := s.TurnByID(ctx, "prepared-stop")
	if err != nil || turn.ProviderTurnID != "provider-prepared" || turn.StartedAt == nil || turn.State != domain.TurnStateRunning {
		t.Fatalf("wrong id changed row = %+v err=%v", turn, err)
	}
	if settled, err := s.InterruptPreparedDeferredTurn(
		ctx, conversation.ID, session.ID, "", "generation-stale", "prepared-stop", "provider-prepared", testNow); !errors.Is(err, storepkg.ErrChatOwnershipLost) || settled {
		t.Fatalf("stale owner = settled=%v err=%v, want ownership lost", settled, err)
	}
	if settled, err := s.InterruptPreparedDeferredTurn(
		ctx, conversation.ID, session.ID, "", "generation-old", "prepared-stop", "provider-prepared", testNow); err != nil || !settled {
		t.Fatalf("prepared interrupt = settled=%v err=%v", settled, err)
	}
	turn, err = s.TurnByID(ctx, "prepared-stop")
	if err != nil {
		t.Fatalf("TurnByID: %v", err)
	}
	if turn.State != domain.TurnStateInterrupted || turn.ProviderTurnID != "" || turn.StartedAt != nil || turn.CompletedAt == nil {
		t.Fatalf("prepared interrupt row = %+v, want interrupted without prepared identity", turn)
	}
}
