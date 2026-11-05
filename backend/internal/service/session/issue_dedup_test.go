package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (f *fakeStore) GetActiveSessionByIssue(_ context.Context, project domain.ProjectID, issue domain.IssueID) (domain.SessionRecord, bool, error) {
	for _, rec := range f.sessions {
		if rec.ProjectID == project && rec.IssueID == issue && rec.Kind == domain.KindWorker && !rec.IsTerminated && !rec.IsTaskPreparation {
			return rec, true, nil
		}
	}
	return domain.SessionRecord{}, false, nil
}

func TestSpawnCoalescesConcurrentIssueSpawns(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer"}
	now := time.Now().UTC()
	cmd := &fakeCommander{}
	cmd.spawnFunc = func(cfg ports.SpawnConfig) domain.SessionRecord {
		rec, err := st.CreateSession(context.Background(), domain.SessionRecord{
			ProjectID: cfg.ProjectID,
			IssueID:   cfg.IssueID,
			Kind:      cfg.Kind,
			Harness:   cfg.Harness,
			Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
			CreatedAt: now,
			UpdatedAt: now,
		})
		if err != nil {
			panic(err)
		}
		return rec
	}
	svc := NewWithDeps(Deps{Manager: cmd, Store: st})

	const workers = 8
	start := make(chan struct{})
	results := make([]domain.Session, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], _, _, errs[i] = svc.Spawn(context.Background(), ports.SpawnConfig{
				ProjectID: "mer",
				IssueID:   "github:acme/demo#12",
				Kind:      domain.KindWorker,
				Harness:   domain.HarnessClaudeCode,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	if cmd.spawnCalls != 1 {
		t.Fatalf("manager spawn calls = %d, want 1", cmd.spawnCalls)
	}
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if results[i].ID != results[0].ID {
			t.Fatalf("worker %d session = %s, want %s", i, results[i].ID, results[0].ID)
		}
	}
}
