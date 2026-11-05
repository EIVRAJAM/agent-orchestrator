package trackerintake

import (
	"context"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type sessionRecordingSpawner struct {
	store *fakeStore
	mu    sync.Mutex
	calls int
}

func (s *sessionRecordingSpawner) Spawn(_ context.Context, cfg ports.SpawnConfig) (domain.Session, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.store.sessions = append(s.store.sessions, domain.SessionRecord{
		ID:        domain.SessionID("demo-" + string(rune('0'+s.calls))),
		ProjectID: cfg.ProjectID, IssueID: cfg.IssueID, Kind: cfg.Kind,
	})
	return domain.Session{SessionRecord: domain.SessionRecord{ID: domain.SessionID("demo-1")}}, 0, 0, nil
}

func TestConcurrentPollSpawnsIssueOnce(t *testing.T) {
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID: "demo", RepoOriginURL: "https://github.com/acme/demo.git",
		Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
	}}}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:    domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#12"},
		Title: "duplicate", State: domain.IssueOpen, Assignees: []string{"alice"},
	}}}
	spawner := &sessionRecordingSpawner{store: store}
	observer := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := observer.Poll(context.Background()); err != nil {
				t.Errorf("Poll: %v", err)
			}
		}()
	}
	wg.Wait()
	if spawner.calls != 1 {
		t.Fatalf("spawn calls = %d, want 1", spawner.calls)
	}
}
