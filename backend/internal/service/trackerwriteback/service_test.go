package trackerwriteback

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeStore struct {
	mu       sync.Mutex
	sessions map[domain.SessionID]domain.SessionRecord
	projects map[string]domain.ProjectRecord
	rows     map[string]domain.TrackerWriteback
	key      map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions: map[domain.SessionID]domain.SessionRecord{},
		projects: map[string]domain.ProjectRecord{},
		rows:     map[string]domain.TrackerWriteback{},
		key:      map[string]string{},
	}
}

func (f *fakeStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.sessions[id]
	return r, ok, nil
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

type fakeCommenter struct {
	mu     sync.Mutex
	calls  int
	bodies []string
	err    error
}

func (f *fakeCommenter) CreateIssueComment(_ context.Context, _ domain.TrackerID, body string) (ports.TrackerComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.bodies = append(f.bodies, body)
	if f.err != nil {
		return ports.TrackerComment{}, f.err
	}
	return ports.TrackerComment{ID: "comment-1", URL: "https://example.test/comment/1"}, nil
}

func seedWriteback(t *testing.T, enabled bool) (*Service, *fakeStore, *fakeCommenter) {
	t.Helper()
	st := newFakeStore()
	st.projects["proj"] = domain.ProjectRecord{
		ID:     "proj",
		Config: domain.ProjectConfig{TrackerWriteback: domain.TrackerWritebackConfig{Enabled: enabled}},
	}
	st.sessions["proj-1"] = domain.SessionRecord{
		ID: "proj-1", ProjectID: "proj", IssueID: "github:acme/demo#12", Kind: domain.KindWorker,
	}
	commenter := &fakeCommenter{}
	svc := New(st, commenter, nil, time.Minute)
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
