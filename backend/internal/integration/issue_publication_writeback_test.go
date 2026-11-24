package integration

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/trackerwriteback"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestIssuePublicationAndWritebackAgreeWithoutLocalBranchHead(t *testing.T) {
	const (
		repoKey         = "github|github.com|acme/demo"
		rootBranch      = "ao/worker-1/root"
		effectiveBranch = "ao/worker-1/completion-percentage"
		headSHA         = "pr-head-1"
		prURL           = "https://github.com/acme/demo/pull/39"
	)
	for _, tc := range []struct {
		name        string
		branchState *domain.SessionBranchState
		wantCommits int
	}{
		{name: "no BranchState", branchState: nil, wantCommits: 0},
		{
			name: "root branch at another head",
			branchState: &domain.SessionBranchState{
				Branch: rootBranch, HeadSHA: "root-head-old", Commits: 2, Unpushed: 2,
			},
			wantCommits: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlitetest.Open(t.TempDir())
			if err != nil {
				t.Fatalf("sqlite.Open: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })

			now := time.Date(2026, 11, 24, 21, 20, 0, 0, time.UTC)
			if err := store.UpsertProject(ctx, domain.ProjectRecord{
				ID: "p", Path: "/repo/p", RepoOriginURL: "https://github.com/acme/demo.git", RegisteredAt: now,
				Config: domain.ProjectConfig{TrackerWriteback: domain.TrackerWritebackConfig{Enabled: true}},
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
			if tc.branchState != nil {
				if _, err := store.SetSessionBranchState(ctx, rec.ID, *tc.branchState); err != nil {
					t.Fatalf("SetSessionBranchState: %v", err)
				}
			}
			if _, err := store.RecordSessionBranchTip(ctx, rec.ID, repoKey, effectiveBranch, headSHA, "/ws/p"); err != nil {
				t.Fatalf("RecordSessionBranchTip: %v", err)
			}
			if _, err := store.RegisterSessionPROwnership(ctx, rec.InstanceID, repoKey, prURL, effectiveBranch, headSHA, now); err != nil {
				t.Fatalf("RegisterSessionPROwnership: %v", err)
			}
			if err := store.WriteSCMObservation(ctx, domain.PullRequest{
				URL: prURL, SessionID: rec.ID, Number: 39, Provider: "github", Host: "github.com", Repo: "acme/demo",
				SourceBranch: effectiveBranch, TargetBranch: "main", HeadSHA: headSHA,
				CI: domain.CIPassing, Title: "feat: completion percentage", CreatedAtProvider: now, UpdatedAt: now,
			}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
				t.Fatalf("WriteSCMObservation: %v", err)
			}

			svc := session.NewWithDeps(session.Deps{Store: store, DataDir: t.TempDir(), Clock: func() time.Time { return now }})
			got, err := svc.Get(ctx, rec.ID)
			if err != nil {
				t.Fatalf("session.Get: %v", err)
			}
			if got.IssuePublication == nil || got.IssuePublication.State != domain.IssuePublicationPRReady {
				t.Fatalf("publication = %+v, want pr_ready", got.IssuePublication)
			}
			pub := got.IssuePublication
			if pub.Turn != domain.IssuePublicationTurnUnknown {
				t.Fatalf("turn = %q, want unknown", pub.Turn)
			}
			if pub.LocalCommits != tc.wantCommits || pub.HeadSHA != headSHA || pub.PRURL != prURL {
				t.Fatalf("publication evidence = %+v, want local commits %d and PR %s/%s", pub, tc.wantCommits, prURL, headSHA)
			}

			observation := ports.SCMObservation{
				Fetched:  true,
				Provider: "github",
				Host:     "github.com",
				Repo:     "acme/demo",
				PR: ports.SCMPRObservation{
					URL: prURL, Number: 39, State: "open",
					SourceBranch: effectiveBranch, HeadSHA: headSHA,
				},
				CI: ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: headSHA},
			}
			commenter := &recordingTrackerCommenter{}
			wb := trackerwriteback.NewWithRefresher(store, &staticPRRefresher{observation: observation}, commenter, nil, time.Minute)
			if err := wb.Enqueue(ctx, rec.ID, prURL, headSHA); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			rows, err := store.ListTrackerWritebacks(ctx, "", 10, 0)
			if err != nil {
				t.Fatalf("ListTrackerWritebacks: %v", err)
			}
			if len(rows) != 1 || rows[0].Status != domain.TrackerWritebackPending {
				t.Fatalf("writeback rows = %+v, want one pending row", rows)
			}
			if err := wb.ProcessOnce(ctx); err != nil {
				t.Fatalf("ProcessOnce: %v", err)
			}
			if commenter.calls != 1 {
				t.Fatalf("comment calls = %d, want 1", commenter.calls)
			}
			rows, err = store.ListTrackerWritebacks(ctx, "", 10, 0)
			if err != nil {
				t.Fatalf("ListTrackerWritebacks after delivery: %v", err)
			}
			if len(rows) != 1 || rows[0].Status != domain.TrackerWritebackSent {
				t.Fatalf("writeback rows = %+v, want one sent row", rows)
			}
		})
	}
}
