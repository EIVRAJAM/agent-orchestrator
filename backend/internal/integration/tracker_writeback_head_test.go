package integration

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/trackerwriteback"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const trackerHeadPRURL = "https://github.com/acme/demo/pull/1"
const trackerHeadRepoKey = "github|github.com|acme/demo"

type recordingTrackerCommenter struct {
	calls int
	urls  []string
}

func (c *recordingTrackerCommenter) CreateIssueComment(_ context.Context, _ domain.TrackerID, body string) (ports.TrackerComment, error) {
	c.calls++
	c.urls = append(c.urls, body)
	return ports.TrackerComment{ID: "comment-1", URL: "https://example.test/comment/1"}, nil
}

type staticPRRefresher struct {
	observation ports.SCMObservation
}

func (r *staticPRRefresher) FetchPullRequestsFresh(_ context.Context, _ []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	return []ports.SCMObservation{r.observation}, nil
}

type trackerHeadFixture struct {
	store     *sqlite.Store
	session   domain.SessionRecord
	lcm       *lifecycle.Manager
	service   *trackerwriteback.Service
	refresher *staticPRRefresher
	commenter *recordingTrackerCommenter
}

func newTrackerHeadFixture(t *testing.T) *trackerHeadFixture {
	t.Helper()
	ctx := context.Background()
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID:           "mer",
		Path:         "/repo/mer",
		RegisteredAt: now,
		Config: domain.ProjectConfig{
			TrackerWriteback: domain.TrackerWritebackConfig{Enabled: true},
		},
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	session, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "mer",
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessClaudeCode,
		IssueID:   "github:acme/demo#25",
		Activity:  domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{Branch: "feat/x", WorkspacePath: "/ws/mer"},
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.RecordSessionBranchTip(ctx, session.ID, trackerHeadRepoKey, "feat/x", "sha-expected", "/ws/mer"); err != nil {
		t.Fatalf("RecordSessionBranchTip: %v", err)
	}
	if _, err := store.RegisterSessionPROwnership(ctx, session.InstanceID, trackerHeadRepoKey, trackerHeadPRURL, "feat/x", "sha-expected", now); err != nil {
		t.Fatalf("RegisterSessionPROwnership: %v", err)
	}
	if err := store.WriteSCMObservation(ctx, domain.PullRequest{
		URL:               trackerHeadPRURL,
		SessionID:         session.ID,
		Number:            1,
		Provider:          "github",
		Host:              "github.com",
		Repo:              "acme/demo",
		SourceBranch:      "feat/x",
		TargetBranch:      "main",
		HeadSHA:           "sha-expected",
		Title:             "PR",
		CreatedAtProvider: now,
		UpdatedAt:         now,
	}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatalf("WriteSCMObservation: %v", err)
	}

	commenter := &recordingTrackerCommenter{}
	refresher := &staticPRRefresher{observation: trackerHeadObservation()}
	service := trackerwriteback.NewWithRefresher(store, refresher, commenter, nil, time.Minute)
	lcm := lifecycle.New(store, &captureMessenger{})
	lcm.SetTrackerWritebackSink(service)
	return &trackerHeadFixture{
		store:     store,
		session:   session,
		lcm:       lcm,
		service:   service,
		refresher: refresher,
		commenter: commenter,
	}
}

func trackerHeadObservation() ports.SCMObservation {
	return ports.SCMObservation{
		Fetched:  true,
		Provider: "github",
		Host:     "github.com",
		Repo:     "acme/demo",
		PR: ports.SCMPRObservation{
			URL: trackerHeadPRURL, Number: 1, State: "open",
			SourceBranch: "feat/x", HeadSHA: "sha-expected",
		},
		CI: ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "sha-expected"},
	}
}

func TestLifecycleTrackerWritebackPersistsObservedHead(t *testing.T) {
	f := newTrackerHeadFixture(t)
	ctx := context.Background()
	observation := trackerHeadObservation()

	if err := f.lcm.ApplySCMObservation(ctx, f.session.ID, observation); err != nil {
		t.Fatalf("first lifecycle observation: %v", err)
	}
	rows, err := f.store.ListTrackerWritebacks(ctx, "", 10, 0)
	if err != nil {
		t.Fatalf("ListTrackerWritebacks: %v", err)
	}
	if len(rows) != 1 || rows[0].HeadSHA != "sha-expected" {
		t.Fatalf("rows after first observation = %+v, want one row with head sha-expected", rows)
	}

	// Re-delivery of the same observation is idempotent and cannot change the
	// original durable head.
	if err := f.lcm.ApplySCMObservation(ctx, f.session.ID, observation); err != nil {
		t.Fatalf("second lifecycle observation: %v", err)
	}
	rows, err = f.store.ListTrackerWritebacks(ctx, "", 10, 0)
	if err != nil {
		t.Fatalf("ListTrackerWritebacks after duplicate: %v", err)
	}
	if len(rows) != 1 || rows[0].HeadSHA != "sha-expected" {
		t.Fatalf("rows after duplicate = %+v, want one unchanged row", rows)
	}

	if err := f.service.ProcessOnce(ctx); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if f.commenter.calls != 1 {
		t.Fatalf("comment calls = %d, want 1", f.commenter.calls)
	}
	rows, err = f.store.ListTrackerWritebacks(ctx, "", 10, 0)
	if err != nil {
		t.Fatalf("ListTrackerWritebacks after delivery: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != domain.TrackerWritebackSent {
		t.Fatalf("rows after delivery = %+v, want one sent row", rows)
	}
}

func TestLifecycleTrackerWritebackRejectsMissingHead(t *testing.T) {
	f := newTrackerHeadFixture(t)
	observation := trackerHeadObservation()
	observation.PR.HeadSHA = ""

	if err := f.lcm.ApplySCMObservation(context.Background(), f.session.ID, observation); err == nil {
		t.Fatal("ApplySCMObservation returned nil for a ready observation without a head")
	}
	rows, err := f.store.ListTrackerWritebacks(context.Background(), "", 10, 0)
	if err != nil {
		t.Fatalf("ListTrackerWritebacks: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none without an observed head", rows)
	}
}

func TestTrackerWritebackFreshHeadChangeFailsClosed(t *testing.T) {
	f := newTrackerHeadFixture(t)
	ctx := context.Background()
	if err := f.lcm.ApplySCMObservation(ctx, f.session.ID, trackerHeadObservation()); err != nil {
		t.Fatalf("lifecycle observation: %v", err)
	}
	f.refresher.observation = trackerHeadObservation()
	f.refresher.observation.PR.HeadSHA = "sha-other"
	f.refresher.observation.CI.HeadSHA = "sha-other"

	if err := f.service.ProcessOnce(ctx); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if f.commenter.calls != 0 {
		t.Fatalf("comment calls = %d, want 0 after a fresh head change", f.commenter.calls)
	}
	rows, err := f.store.ListTrackerWritebacks(ctx, "", 10, 0)
	if err != nil {
		t.Fatalf("ListTrackerWritebacks: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != domain.TrackerWritebackFailed || rows[0].FailureCode != "pr_head_changed" {
		t.Fatalf("rows = %+v, want failed pr_head_changed", rows)
	}
}
