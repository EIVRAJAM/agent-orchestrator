package trackerwriteback

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeStore struct {
	mu            sync.Mutex
	sessions      map[domain.SessionID]domain.SessionRecord
	projects      map[string]domain.ProjectRecord
	prs           map[string]domain.PullRequest
	ownership     map[string]domain.SessionPROwnership
	fresh         ports.SCMObservation
	freshErr      error
	rows          map[string]domain.TrackerWriteback
	key           map[string]string
	finishErr     error
	finishUpdated *bool
	finishCalls   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions:  map[domain.SessionID]domain.SessionRecord{},
		projects:  map[string]domain.ProjectRecord{},
		prs:       map[string]domain.PullRequest{},
		ownership: map[string]domain.SessionPROwnership{},
		rows:      map[string]domain.TrackerWriteback{},
		key:       map[string]string{},
	}
}

func (f *fakeStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.sessions[id]
	return r, ok, nil
}

func (f *fakeStore) GetPR(_ context.Context, url string) (domain.PullRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.prs[url]
	return pr, ok, nil
}

func (f *fakeStore) GetSessionPROwnership(_ context.Context, prURL string) (domain.SessionPROwnership, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ownership, ok := f.ownership[prURL]
	return ownership, ok, nil
}

func (f *fakeStore) FetchPullRequestsFresh(_ context.Context, _ []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.freshErr != nil {
		return nil, f.freshErr
	}
	return []ports.SCMObservation{f.fresh}, nil
}

func (f *fakeStore) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.projects[id]
	return r, ok, nil
}

func (f *fakeStore) EnqueueTrackerWriteback(_ context.Context, w domain.TrackerWriteback) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := string(w.ProjectID) + "\x00" + string(w.IssueID) + "\x00" + w.PRURL
	if _, exists := f.key[k]; exists {
		return false, nil
	}
	f.key[k] = w.ID
	f.rows[w.ID] = w
	return true, nil
}

func (f *fakeStore) ClaimTrackerWriteback(_ context.Context, now time.Time) (domain.TrackerWriteback, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, row := range f.rows {
		if row.Status != domain.TrackerWritebackPending {
			continue
		}
		row.Status = domain.TrackerWritebackClaimed
		row.Attempts++
		row.ClaimedAt = &now
		row.UpdatedAt = now
		f.rows[id] = row
		return row, true, nil
	}
	return domain.TrackerWriteback{}, false, nil
}

func (f *fakeStore) FinishTrackerWriteback(_ context.Context, id, status, commentURL, failureCode string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishCalls++
	if f.finishErr != nil {
		return false, f.finishErr
	}
	if f.finishUpdated != nil && !*f.finishUpdated {
		return false, nil
	}
	row, ok := f.rows[id]
	if !ok || row.Status != domain.TrackerWritebackClaimed {
		return false, nil
	}
	row.Status = status
	row.CommentURL = commentURL
	row.FailureCode = failureCode
	row.UpdatedAt = now
	f.rows[id] = row
	return true, nil
}

func (f *fakeStore) RecoverStaleTrackerWritebacks(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, row := range f.rows {
		if row.Status == domain.TrackerWritebackClaimed {
			row.Status = domain.TrackerWritebackUncertain
			row.FailureCode = "restart"
			row.UpdatedAt = now
			f.rows[id] = row
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) row(id string) domain.TrackerWriteback {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id]
}

func (f *fakeStore) rowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

type httpPRRefresher struct {
	url         string
	observation ports.SCMObservation
}

func (r *httpPRRefresher) FetchPullRequestsFresh(ctx context.Context, _ []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return nil, errors.New("unexpected fake SCM status")
	}
	return []ports.SCMObservation{r.observation}, nil
}

type dualPRRefresher struct {
	cached      ports.SCMObservation
	fresh       ports.SCMObservation
	cachedCalls int
	freshCalls  int
}

func (r *dualPRRefresher) FetchPullRequests(_ context.Context, _ []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	r.cachedCalls++
	return []ports.SCMObservation{r.cached}, nil
}

func (r *dualPRRefresher) FetchPullRequestsFresh(_ context.Context, _ []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	r.freshCalls++
	return []ports.SCMObservation{r.fresh}, nil
}

type fakeCommenter struct {
	mu       sync.Mutex
	calls    int
	bodies   []string
	err      error
	url      string
	emptyURL bool
}

func (f *fakeCommenter) CreateIssueComment(_ context.Context, _ domain.TrackerID, body string) (ports.TrackerComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.bodies = append(f.bodies, body)
	if f.err != nil {
		return ports.TrackerComment{}, f.err
	}
	url := f.url
	if url == "" && !f.emptyURL {
		url = "https://example.test/comment/1"
	}
	return ports.TrackerComment{ID: "comment-1", URL: url}, nil
}

func seedWriteback(t *testing.T, enabled bool) (*Service, *fakeStore, *fakeCommenter) {
	t.Helper()
	st := newFakeStore()
	st.projects["proj"] = domain.ProjectRecord{
		ID:     "proj",
		Config: domain.ProjectConfig{TrackerWriteback: domain.TrackerWritebackConfig{Enabled: enabled}},
	}
	st.sessions["proj-1"] = domain.SessionRecord{
		ID: "proj-1", InstanceID: "inst-proj-1", ProjectID: "proj", IssueID: "github:acme/demo#12", Kind: domain.KindWorker,
		CreatedAt: time.Unix(100, 0).UTC(),
	}
	prURL := "https://github.com/acme/demo/pull/1"
	st.prs[prURL] = domain.PullRequest{
		URL: prURL, SessionID: "proj-1", Number: 1, Provider: "github", Host: "github.com", Repo: "acme/demo",
		SourceBranch: "feat/x", HeadSHA: "sha1", CreatedAtProvider: time.Unix(101, 0).UTC(),
	}
	st.ownership[prURL] = domain.SessionPROwnership{
		PRURL: prURL, SessionInstanceID: "inst-proj-1", RepoKey: "github|github.com|acme/demo",
		SourceBranch: "feat/x", HeadSHA: "sha1", RegisteredAt: time.Unix(101, 0).UTC(),
	}
	st.fresh = ports.SCMObservation{
		Fetched: true, Provider: "github", Host: "github.com", Repo: "acme/demo",
		PR: ports.SCMPRObservation{URL: prURL, Number: 1, State: "open", SourceBranch: "feat/x", HeadSHA: "sha1"},
		CI: ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "sha1"},
	}
	commenter := &fakeCommenter{}
	svc := NewWithRefresher(st, st, commenter, nil, time.Minute)
	svc.clock = func() time.Time { return time.Unix(100, 0).UTC() }
	svc.newID = func() string { return "wb-1" }
	return svc, st, commenter
}

func TestEnqueueIsOptInAndDeduplicated(t *testing.T) {
	svc, st, _ := seedWriteback(t, false)
	if err := svc.Enqueue(context.Background(), "proj-1", "https://github.com/acme/demo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if st.rowCount() != 0 {
		t.Fatal("disabled write-back inserted a row")
	}

	svc, st, commenter := seedWriteback(t, true)
	prURL := "https://github.com/acme/demo/pull/1"
	if err := svc.Enqueue(context.Background(), "proj-1", prURL); err != nil {
		t.Fatal(err)
	}
	if err := svc.Enqueue(context.Background(), "proj-1", prURL); err != nil {
		t.Fatal(err)
	}
	if st.rowCount() != 1 {
		t.Fatalf("rows = %d, want 1 after duplicate enqueue", st.rowCount())
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 1 {
		t.Fatalf("comment calls = %d, want 1", commenter.calls)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackSent || got.CommentURL != "https://example.test/comment/1" {
		t.Fatalf("settled row = %+v", got)
	}
}

func TestRemoteErrorBecomesUncertainAndIsNeverRetried(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	commenter.err = errors.New("response lost")
	if err := svc.Enqueue(context.Background(), "proj-1", "https://github.com/acme/demo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackUncertain || got.FailureCode != "remote_uncertain" {
		t.Fatalf("row = %+v, want uncertain", got)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 1 {
		t.Fatalf("comment calls = %d, want no retry", commenter.calls)
	}
}

func TestStartupRecoveryDoesNotReplayAClaim(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	st.rows["wb-1"] = domain.TrackerWriteback{
		ID: "wb-1", ProjectID: "proj", SessionID: "proj-1", IssueID: "github:acme/demo#12",
		PRURL: "https://github.com/acme/demo/pull/1", Status: domain.TrackerWritebackClaimed,
	}
	if n, err := st.RecoverStaleTrackerWritebacks(context.Background(), svc.now()); err != nil || n != 1 {
		t.Fatalf("recover = %d, %v", n, err)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 0 {
		t.Fatalf("comment calls = %d, want none after restart recovery", commenter.calls)
	}
}

func TestEnqueueSkipsNonIssueOrTerminatedSessions(t *testing.T) {
	svc, st, _ := seedWriteback(t, true)
	st.sessions["proj-2"] = domain.SessionRecord{ID: "proj-2", ProjectID: "proj", Kind: domain.KindWorker}
	st.sessions["proj-3"] = domain.SessionRecord{ID: "proj-3", ProjectID: "proj", IssueID: "github:acme/demo#12", Kind: domain.KindWorker, IsTerminated: true}
	for _, id := range []domain.SessionID{"proj-2", "proj-3"} {
		if err := svc.Enqueue(context.Background(), id, "https://github.com/acme/demo/pull/1"); err != nil {
			t.Fatal(err)
		}
	}
	if st.rowCount() != 0 {
		t.Fatalf("rows = %d, want none", st.rowCount())
	}
}

func TestMissingCommentReceiptBecomesUncertainWithoutRetry(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	commenter.emptyURL = true
	if err := svc.Enqueue(context.Background(), "proj-1", "https://github.com/acme/demo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackUncertain || got.FailureCode != "missing_comment_receipt" {
		t.Fatalf("row = %+v, want uncertain missing_comment_receipt", got)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 1 {
		t.Fatalf("comment calls = %d, want no retry", commenter.calls)
	}
}

func TestAdapterReceiptErrorMapsToMissingReceipt(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	commenter.err = ports.ErrTrackerCommentReceipt
	if err := svc.Enqueue(context.Background(), "proj-1", "https://github.com/acme/demo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackUncertain || got.FailureCode != "missing_comment_receipt" {
		t.Fatalf("row = %+v, want missing_comment_receipt", got)
	}
}

func TestSettlementFailureAfterPostDoesNotResend(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	updated := false
	st.finishUpdated = &updated
	if err := svc.Enqueue(context.Background(), "proj-1", "https://github.com/acme/demo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 1 {
		t.Fatalf("comment calls = %d, want one POST", commenter.calls)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackClaimed {
		t.Fatalf("row = %+v, want claimed/unconfirmed", got)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 1 {
		t.Fatalf("comment calls = %d, want no second POST", commenter.calls)
	}
}

func TestTerminalLoggingIsSanitized(t *testing.T) {
	var buf bytes.Buffer
	st := newFakeStore()
	st.projects["proj"] = domain.ProjectRecord{ID: "proj", Config: domain.ProjectConfig{TrackerWriteback: domain.TrackerWritebackConfig{Enabled: true}}}
	st.sessions["proj-1"] = domain.SessionRecord{ID: "proj-1", InstanceID: "inst-proj-1", ProjectID: "proj", IssueID: "github:acme/demo#12", Kind: domain.KindWorker, CreatedAt: time.Unix(100, 0).UTC()}
	st.projects["proj"] = domain.ProjectRecord{ID: "proj", Config: domain.ProjectConfig{TrackerWriteback: domain.TrackerWritebackConfig{Enabled: true}}}
	prURL := "https://github.com/acme/demo/pull/1"
	st.prs[prURL] = domain.PullRequest{
		URL: prURL, SessionID: "proj-1", Number: 1, Provider: "github", Host: "github.com", Repo: "acme/demo",
		SourceBranch: "feat/x", HeadSHA: "sha1", CreatedAtProvider: time.Unix(101, 0).UTC(),
	}
	st.ownership[prURL] = domain.SessionPROwnership{PRURL: prURL, SessionInstanceID: "inst-proj-1", RepoKey: "github|github.com|acme/demo", SourceBranch: "feat/x", HeadSHA: "sha1"}
	st.fresh = ports.SCMObservation{
		Fetched: true, Provider: "github", Host: "github.com", Repo: "acme/demo",
		PR: ports.SCMPRObservation{URL: prURL, Number: 1, State: "open", SourceBranch: "feat/x", HeadSHA: "sha1"},
		CI: ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "sha1"},
	}
	commenter := &fakeCommenter{}
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	svc := NewWithRefresher(st, st, commenter, logger, time.Minute)
	svc.clock = func() time.Time { return time.Unix(100, 0).UTC() }
	svc.newID = func() string { return "wb-log" }
	if err := svc.Enqueue(context.Background(), "proj-1", prURL); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "tracker writeback: settled") || !strings.Contains(logs, "status=sent") || !strings.Contains(logs, "writeback_id=wb-log") {
		t.Fatalf("missing terminal log: %s", logs)
	}
	for _, secret := range []string{prURL, "acme/demo", "IssueID", "body"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log leaked %q: %s", secret, logs)
		}
	}
}

func (f *fakeStore) ListTrackerWritebacks(_ context.Context, status string, limit, offset int) ([]domain.TrackerWriteback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := make([]domain.TrackerWriteback, 0, len(f.rows))
	for _, row := range f.rows {
		if status == "" || row.Status == status {
			rows = append(rows, row)
		}
	}
	// Stable order is enough for the fake; the SQL query owns production order.
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if offset >= len(rows) {
		return nil, nil
	}
	rows = rows[offset:]
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func TestListReturnsBoundedSanitizedPage(t *testing.T) {
	svc, st, _ := seedWriteback(t, true)
	now := time.Unix(100, 0).UTC()
	for _, id := range []string{"wb-a", "wb-b", "wb-c"} {
		st.rows[id] = domain.TrackerWriteback{
			ID: id, ProjectID: "proj", SessionID: "proj-1", IssueID: "github:acme/demo#12",
			PRURL: "https://github.com/acme/demo/pull/1", Status: domain.TrackerWritebackSent,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	rows, hasMore, err := svc.List(context.Background(), domain.TrackerWritebackSent, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !hasMore {
		t.Fatalf("page = %d hasMore=%v, want 2/true", len(rows), hasMore)
	}
	rows, hasMore, err = svc.List(context.Background(), domain.TrackerWritebackSent, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || hasMore {
		t.Fatalf("tail = %d hasMore=%v, want 1/false", len(rows), hasMore)
	}
	if _, _, err := svc.List(context.Background(), "nope", 2, 0); err == nil {
		t.Fatal("invalid status accepted")
	}
}

func TestSettlementErrorAfterPostDoesNotResend(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	st.finishErr = errors.New("db unavailable")
	if err := svc.Enqueue(context.Background(), "proj-1", "https://github.com/acme/demo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 1 {
		t.Fatalf("comment calls = %d, want one POST", commenter.calls)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackClaimed {
		t.Fatalf("row = %+v, want claimed after settlement error", got)
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 1 {
		t.Fatalf("comment calls = %d, want no second POST", commenter.calls)
	}
}

func TestFailedTerminalLogIsVisibleWithoutIssueContent(t *testing.T) {
	var buf bytes.Buffer
	st := newFakeStore()
	st.sessions["proj-1"] = domain.SessionRecord{ID: "proj-1", InstanceID: "inst-proj-1", ProjectID: "proj", IssueID: "github:acme/demo#12", Kind: domain.KindWorker, CreatedAt: time.Unix(100, 0).UTC()}
	st.projects["proj"] = domain.ProjectRecord{ID: "proj", Config: domain.ProjectConfig{TrackerWriteback: domain.TrackerWritebackConfig{Enabled: true}}}
	prURL := "https://github.com/acme/demo/pull/1"
	st.prs[prURL] = domain.PullRequest{
		URL: prURL, SessionID: "proj-1", Number: 1, Provider: "github", Host: "github.com", Repo: "acme/demo",
		SourceBranch: "feat/x", HeadSHA: "sha1", CreatedAtProvider: time.Unix(101, 0).UTC(),
	}
	st.ownership[prURL] = domain.SessionPROwnership{PRURL: prURL, SessionInstanceID: "inst-proj-1", RepoKey: "github|github.com|acme/demo", SourceBranch: "feat/x", HeadSHA: "sha1"}
	st.fresh = ports.SCMObservation{
		Fetched: true, Provider: "github", Host: "github.com", Repo: "acme/demo",
		PR: ports.SCMPRObservation{URL: prURL, Number: 1, State: "open", SourceBranch: "feat/x", HeadSHA: "sha1"},
		CI: ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "sha1"},
	}
	st.rows["wb-failed"] = domain.TrackerWriteback{
		ID: "wb-failed", ProjectID: "proj", SessionID: "proj-1", IssueID: "github:acme/demo#12",
		PRURL: "https://github.com/acme/demo/pull/1", HeadSHA: "sha1", Status: domain.TrackerWritebackPending,
	}
	svc := NewWithRefresher(st, st, nil, slog.New(slog.NewTextHandler(&buf, nil)), time.Minute)
	svc.clock = func() time.Time { return time.Unix(100, 0).UTC() }
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "status=failed") || !strings.Contains(logs, "code=tracker_unsupported") {
		t.Fatalf("missing failed terminal log: %s", logs)
	}
	for _, forbidden := range []string{"acme/demo", "IssueID", "PRURL", "prompt"} {
		if strings.Contains(logs, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, logs)
		}
	}
}

func TestListExposesSentAndFailedWithoutContent(t *testing.T) {
	svc, st, _ := seedWriteback(t, true)
	now := time.Unix(100, 0).UTC()
	for _, row := range []domain.TrackerWriteback{
		{ID: "wb-sent", ProjectID: "proj", SessionID: "proj-1", Status: domain.TrackerWritebackSent, CreatedAt: now, UpdatedAt: now},
		{ID: "wb-failed", ProjectID: "proj", SessionID: "proj-1", Status: domain.TrackerWritebackFailed, FailureCode: "tracker_unsupported", CreatedAt: now, UpdatedAt: now},
	} {
		st.rows[row.ID] = row
	}
	rows, _, err := svc.List(context.Background(), "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, row := range rows {
		statuses[row.ID] = row.Status
	}
	if statuses["wb-sent"] != domain.TrackerWritebackSent || statuses["wb-failed"] != domain.TrackerWritebackFailed {
		t.Fatalf("statuses = %+v", statuses)
	}
}

func TestEnqueueRequiresExactDurableOwnership(t *testing.T) {
	for name, mutate := range map[string]func(*fakeStore, *domain.PullRequest){
		"missing ownership": func(st *fakeStore, _ *domain.PullRequest) {
			delete(st.ownership, "https://github.com/acme/demo/pull/1")
		},
		"other session": func(_ *fakeStore, pr *domain.PullRequest) { pr.SessionID = "other" },
		"wrong head":    func(_ *fakeStore, pr *domain.PullRequest) { pr.HeadSHA = "other" },
		"wrong instance": func(st *fakeStore, pr *domain.PullRequest) {
			own := st.ownership[pr.URL]
			own.SessionInstanceID = "inst-other"
			st.ownership[pr.URL] = own
		},
		"wrong repository": func(_ *fakeStore, pr *domain.PullRequest) {
			pr.Repo = "acme/other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, st, _ := seedWriteback(t, true)
			pr := st.prs["https://github.com/acme/demo/pull/1"]
			mutate(st, &pr)
			st.prs[pr.URL] = pr
			if err := svc.Enqueue(context.Background(), "proj-1", pr.URL); err != nil {
				t.Fatal(err)
			}
			if st.rowCount() != 0 {
				t.Fatal("ambiguous PR association was enqueued")
			}
		})
	}
}

func TestProcessOnceRejectsHeadMovedAfterEnqueue(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	prURL := "https://github.com/acme/demo/pull/1"
	if err := svc.Enqueue(context.Background(), "proj-1", prURL); err != nil {
		t.Fatal(err)
	}
	pr := st.prs[prURL]
	pr.HeadSHA = "sha2"
	st.prs[prURL] = pr
	own := st.ownership[prURL]
	own.HeadSHA = "sha2"
	st.ownership[prURL] = own
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 0 {
		t.Fatalf("comment calls = %d, want 0", commenter.calls)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackFailed || got.FailureCode != "pr_head_changed" {
		t.Fatalf("row = %+v, want failed pr_head_changed", got)
	}
}

func TestProcessOnceUsesExplicitFreshProviderRead(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	prURL := "https://github.com/acme/demo/pull/1"
	if err := svc.Enqueue(context.Background(), "proj-1", prURL); err != nil {
		t.Fatal(err)
	}
	cached := st.fresh
	cached.PR.Closed = true
	refresher := &dualPRRefresher{cached: cached, fresh: st.fresh}
	svc.refresher = refresher
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refresher.cachedCalls != 0 || refresher.freshCalls != 1 {
		t.Fatalf("cached calls=%d fresh calls=%d, want 0/1", refresher.cachedCalls, refresher.freshCalls)
	}
	if commenter.calls != 1 {
		t.Fatalf("comment calls = %d, want 1", commenter.calls)
	}
}

func TestProcessOnceUsesFreshHTTPReadBeforeComment(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	svc, st, commenter := seedWriteback(t, true)
	prURL := "https://github.com/acme/demo/pull/1"
	if err := svc.Enqueue(context.Background(), "proj-1", prURL); err != nil {
		t.Fatal(err)
	}
	svc.refresher = &httpPRRefresher{url: server.URL, observation: st.fresh}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || commenter.calls != 1 {
		t.Fatalf("fresh calls=%d comment calls=%d, want 1/1", calls, commenter.calls)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackSent {
		t.Fatalf("row = %+v, want sent", got)
	}
}

func TestProcessOnceFailsClosedOnFreshPRReadFailure(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	prURL := "https://github.com/acme/demo/pull/1"
	if err := svc.Enqueue(context.Background(), "proj-1", prURL); err != nil {
		t.Fatal(err)
	}
	st.freshErr = errors.New("provider timeout; URL must not leak")
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 0 {
		t.Fatalf("comment calls = %d, want 0", commenter.calls)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackFailed || got.FailureCode != "fresh_pr_unavailable" {
		t.Fatalf("row = %+v, want failed fresh_pr_unavailable", got)
	}
}

func TestProcessOnceFailsClosedWhenFreshPRIsNotReady(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*domain.PullRequest, *ports.SCMObservation)
		code   string
	}{
		"closed":  {func(_ *domain.PullRequest, fresh *ports.SCMObservation) { fresh.PR.Closed = true }, "pr_not_ready"},
		"merged":  {func(_ *domain.PullRequest, fresh *ports.SCMObservation) { fresh.PR.Merged = true }, "pr_not_ready"},
		"draft":   {func(_ *domain.PullRequest, fresh *ports.SCMObservation) { fresh.PR.Draft = true }, "pr_not_ready"},
		"ci":      {func(_ *domain.PullRequest, fresh *ports.SCMObservation) { fresh.CI.Summary = string(domain.CIFailing) }, "pr_not_ready"},
		"ci head": {func(_ *domain.PullRequest, fresh *ports.SCMObservation) { fresh.CI.HeadSHA = "other" }, "pr_head_changed"},
		"head": {func(pr *domain.PullRequest, fresh *ports.SCMObservation) {
			pr.HeadSHA = "sha2"
			fresh.PR.HeadSHA = "sha2"
		}, "pr_head_changed"},
		"repository": {func(_ *domain.PullRequest, fresh *ports.SCMObservation) { fresh.Repo = "acme/other" }, "pr_repository_changed"},
		"url": {func(_ *domain.PullRequest, fresh *ports.SCMObservation) {
			fresh.PR.URL = "https://github.com/acme/demo/pull/2"
		}, "pr_identity_changed"},
	} {
		t.Run(name, func(t *testing.T) {
			svc, st, commenter := seedWriteback(t, true)
			prURL := "https://github.com/acme/demo/pull/1"
			if err := svc.Enqueue(context.Background(), "proj-1", prURL); err != nil {
				t.Fatal(err)
			}
			pr := st.prs[prURL]
			fresh := st.fresh
			tc.mutate(&pr, &fresh)
			st.prs[prURL] = pr
			st.fresh = fresh
			if name == "head" {
				own := st.ownership[prURL]
				own.HeadSHA = "sha2"
				st.ownership[prURL] = own
			}
			if err := svc.ProcessOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if commenter.calls != 0 {
				t.Fatalf("comment calls = %d, want 0", commenter.calls)
			}
			if got := st.row("wb-1"); got.Status != domain.TrackerWritebackFailed || got.FailureCode != tc.code {
				t.Fatalf("row = %+v, want failed %s", got, tc.code)
			}
		})
	}
}

func TestProcessOnceRevalidatesOwnershipImmediatelyBeforePost(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	prURL := "https://github.com/acme/demo/pull/1"
	if err := svc.Enqueue(context.Background(), "proj-1", prURL); err != nil {
		t.Fatal(err)
	}
	delete(st.ownership, prURL)
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 0 {
		t.Fatalf("comment calls = %d, want 0", commenter.calls)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackFailed || got.FailureCode != "pr_association_invalid" {
		t.Fatalf("row = %+v, want failed pr_association_invalid", got)
	}
}

func TestProcessOnceRejectsAmbiguousPRAssociationBeforeComment(t *testing.T) {
	svc, st, commenter := seedWriteback(t, true)
	prURL := "https://github.com/acme/demo/pull/1"
	pr := st.prs[prURL]
	pr.SessionID = "other"
	st.prs[prURL] = pr
	st.rows["wb-1"] = domain.TrackerWriteback{
		ID: "wb-1", ProjectID: "proj", SessionID: "proj-1", IssueID: "github:acme/demo#12",
		PRURL: prURL, HeadSHA: "sha1", Status: domain.TrackerWritebackPending,
	}
	if err := svc.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commenter.calls != 0 {
		t.Fatalf("comment calls = %d, want 0", commenter.calls)
	}
	if got := st.row("wb-1"); got.Status != domain.TrackerWritebackFailed || got.FailureCode != "pr_association_invalid" {
		t.Fatalf("row = %+v, want failed pr_association_invalid", got)
	}
}
