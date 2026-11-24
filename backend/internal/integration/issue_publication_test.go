package integration

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestIssuePublicationUsesEffectiveOwnedBranchFromSQLite(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, 11, 24, 21, 20, 0, 0, time.UTC)
	const (
		repoKey         = "github|github.com|acme/demo"
		rootBranch      = "ao/praxas-mini-canary-1-1/root"
		effectiveBranch = "ao/praxas-mini-canary-1-1/completion-percentage"
		headSHA         = "3ecaf90aae882083d6b9a6a374b8b9725c3776d7"
		prURL           = "https://github.com/acme/demo/pull/39"
	)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: "/repo/p", RepoOriginURL: "https://github.com/acme/demo.git", RegisteredAt: now,
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	rec, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID:     "p",
		Kind:          domain.KindWorker,
		Harness:       domain.HarnessCodex,
		IssueID:       "github:acme/demo#38",
		Activity:      domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		FirstSignalAt: now,
		Metadata: domain.SessionMetadata{
			Branch:                      rootBranch,
			ConversationCheckpointState: domain.ConversationCheckpointPrompt,
		},
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.SetSessionBranchState(ctx, rec.ID, domain.SessionBranchState{
		Branch: rootBranch, HeadSHA: headSHA, Commits: 1, Unpushed: 1,
	}); err != nil {
		t.Fatalf("SetSessionBranchState: %v", err)
	}
	if _, err := store.RecordSessionBranchTip(ctx, rec.ID, repoKey, effectiveBranch, headSHA, "/ws/p"); err != nil {
		t.Fatalf("RecordSessionBranchTip: %v", err)
	}
	if _, err := store.RegisterSessionPROwnership(ctx, rec.InstanceID, repoKey, prURL, effectiveBranch, headSHA, now); err != nil {
		t.Fatalf("RegisterSessionPROwnership: %v", err)
	}
	if err := store.WriteSCMObservation(ctx, domain.PullRequest{
		URL: prURL, SessionID: rec.ID, Number: 39, Provider: "github", Host: "github.com", Repo: "acme/demo",
		SourceBranch: effectiveBranch, TargetBranch: "pilot/cohort-1-base", HeadSHA: headSHA,
		CI: domain.CIPassing, Title: "feat: completion percentage", CreatedAtProvider: now, UpdatedAt: now,
	}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatalf("WriteSCMObservation: %v", err)
	}

	svc := session.NewWithDeps(session.Deps{Store: store, DataDir: t.TempDir(), Clock: func() time.Time { return now }})
	got, err := svc.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("session.Get: %v", err)
	}
	if got.IssuePublication == nil {
		t.Fatal("issue publication is nil")
	}
	pub := got.IssuePublication
	if pub.State != domain.IssuePublicationPRReady || pub.Reason != "" {
		t.Fatalf("publication = %+v, want pr_ready", pub)
	}
	if pub.Turn != domain.IssuePublicationTurnUnknown {
		t.Fatalf("turn = %q, want unknown; idle must not infer turn completion", pub.Turn)
	}
	if !pub.OwnershipVerified || !pub.BranchPushed || pub.Push != domain.IssuePublicationPushUnknown {
		t.Fatalf("evidence = %+v, want exact ownership with unknown push", pub)
	}
	if pub.PRURL != prURL || pub.HeadSHA != headSHA {
		t.Fatalf("publication PR = %q/%q, want %q/%q", pub.PRURL, pub.HeadSHA, prURL, headSHA)
	}
}
