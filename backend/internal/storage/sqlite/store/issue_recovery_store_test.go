package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestIssueRecoveryAttemptSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sqlitetest.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, store, "recovery")
	rec := sampleRecord("recovery")
	rec.IssueID = "github:acme/demo#1"
	rec.Kind = domain.KindWorker
	created, err := store.CreateSession(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !created.RecoveryEligible {
		t.Fatal("new issue worker was not marked recovery eligible")
	}
	now := time.Now().UTC().Truncate(time.Second)
	row := domain.IssueRecovery{
		SessionID: created.ID, IssueID: created.IssueID, InstanceID: created.InstanceID,
		State: domain.IssueRecoveryObserving, Reason: domain.IssueRecoveryReasonIssuePRMissing,
		LocalCommits: 1, LocalChanges: true, LocalChangesKnown: true, Push: domain.IssuePublicationPushUnknown, Turn: domain.IssuePublicationTurnUnknown,
		ObservedAt: now, UpdatedAt: now,
	}
	if inserted, err := store.EnsureIssueRecovery(ctx, row); err != nil || !inserted {
		t.Fatalf("ensure = inserted=%v err=%v", inserted, err)
	}
	if claimed, err := store.ClaimIssueRecovery(ctx, created.ID, created.InstanceID, now); err != nil || !claimed {
		t.Fatalf("claim = claimed=%v err=%v", claimed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	reloaded, found, err := reopened.GetSession(ctx, created.ID)
	if err != nil || !found || !reloaded.RecoveryEligible {
		t.Fatalf("reopened session eligibility = found=%v eligible=%v err=%v", found, reloaded.RecoveryEligible, err)
	}
	got, found, err := reopened.GetIssueRecovery(ctx, created.ID)
	if err != nil || !found {
		t.Fatalf("get after reopen = found=%v err=%v", found, err)
	}
	if got.Attempts != 1 || got.State != domain.IssueRecoveryContinued || got.AttemptedAt == nil || !got.LocalChanges || !got.LocalChangesKnown {
		t.Fatalf("persisted recovery = %+v, want one continued attempt", got)
	}
	if claimed, err := reopened.ClaimIssueRecovery(ctx, created.ID, created.InstanceID, now.Add(time.Minute)); err != nil || claimed {
		t.Fatalf("second claim after reopen = claimed=%v err=%v, want false", claimed, err)
	}
	got.State = domain.IssueRecoveryIncomplete
	got.Reason = domain.IssueRecoveryReasonExhausted
	if finished, err := reopened.FinishIssueRecovery(ctx, got, now.Add(time.Minute)); err != nil || !finished {
		t.Fatalf("finish = finished=%v err=%v", finished, err)
	}
	if claimed, err := reopened.ClaimIssueRecovery(ctx, created.ID, created.InstanceID, now.Add(2*time.Minute)); err != nil || claimed {
		t.Fatalf("claim after terminal = claimed=%v err=%v, want false", claimed, err)
	}
}
