package scm

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestPoll_RejectsOldPRFromReusedSessionPrefix(t *testing.T) {
	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "ao/praxas-mini-1/root"
	store.sessions[0].CreatedAt = time.Unix(100, 0).UTC()

	oldPR := ports.SCMPRObservation{
		URL: "https://github.com/o/r/pull/4", Number: 4, SourceBranch: "ao/praxas-mini-1/old-task",
		HeadRepo: "o/r", TargetBranch: "main", HeadSHA: "old", Author: "alice",
		CreatedAtProvider: time.Unix(50, 0).UTC(),
	}
	newPR := ports.SCMPRObservation{
		URL: "https://github.com/o/r/pull/5", Number: 5, SourceBranch: "ao/praxas-mini-1/new-task",
		HeadRepo: "o/r", TargetBranch: "main", HeadSHA: "new", Author: "alice",
		CreatedAtProvider: time.Unix(101, 0).UTC(),
	}
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs: map[string][]ports.SCMPRObservation{
			prKey(testRepo, 0): {oldPR, newPR},
		},
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(102, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, write := range store.writes {
		if write.pr.Number == 4 {
			t.Fatalf("old PR was adopted by reused session prefix: %+v", write.pr)
		}
	}
	found := false
	for _, write := range store.writes {
		if write.pr.Number == 5 {
			found = true
		}
	}
	if !found {
		t.Fatal("correctly timed PR under the session namespace was not persisted")
	}
}

func TestPRBelongsToSessionFailsClosedWithoutCreationEvidence(t *testing.T) {
	sr := sessionRepo{
		session: domain.SessionRecord{ID: "p-1", CreatedAt: time.Unix(100, 0).UTC()},
		branch:  "ao/p-1/root",
	}
	if prBelongsToSession(sr, ports.SCMPRObservation{SourceBranch: "ao/p-1/child"}) {
		t.Fatal("prefix match without PR creation evidence was accepted")
	}
	if prBelongsToSession(sr, ports.SCMPRObservation{
		SourceBranch: "ao/p-1/child", CreatedAtProvider: time.Unix(99, 0).UTC(),
	}) {
		t.Fatal("prefix match with PR created before session was accepted")
	}
	if !prBelongsToSession(sr, ports.SCMPRObservation{
		SourceBranch: "ao/p-1/child", CreatedAtProvider: time.Unix(101, 0).UTC(),
	}) {
		t.Fatal("prefix match with PR created after session was rejected")
	}
}
