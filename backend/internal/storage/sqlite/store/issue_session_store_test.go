package store_test

import (
	"context"
	"testing"
	"time"
)

func TestGetActiveSessionByIssueUsesCanonicalLiveVisibleWorker(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	seedProject(t, s, "other")

	rec := sampleRecord("mer")
	rec.IssueID = "github:acme/demo#12"
	rec.CreatedAt = time.Now().UTC().Truncate(time.Second)
	rec.UpdatedAt = rec.CreatedAt
	created, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatalf("create issue session: %v", err)
	}

	got, ok, err := s.GetActiveSessionByIssue(ctx, "mer", rec.IssueID)
	if err != nil || !ok {
		t.Fatalf("get active issue session: ok=%v err=%v", ok, err)
	}
	if got.ID != created.ID {
		t.Fatalf("active session = %s, want %s", got.ID, created.ID)
	}

	// A different project with the same canonical issue id must not leak.
	if _, ok, err := s.GetActiveSessionByIssue(ctx, "other", rec.IssueID); err != nil || ok {
		t.Fatalf("cross-project lookup: ok=%v err=%v, want absent", ok, err)
	}

	// Hidden task-preparation rows do not own a visible issue session.
	prep := sampleRecord("mer")
	prep.IssueID = "github:acme/demo#13"
	prep.IsTaskPreparation = true
	prep.CreatedAt = rec.CreatedAt
	prep.UpdatedAt = rec.CreatedAt
	if _, err := s.CreateSession(ctx, prep); err != nil {
		t.Fatalf("create task preparation: %v", err)
	}
	if _, ok, err := s.GetActiveSessionByIssue(ctx, "mer", prep.IssueID); err != nil || ok {
		t.Fatalf("task preparation lookup: ok=%v err=%v, want absent", ok, err)
	}

	// Termination releases the issue for a deliberate later respawn.
	created.IsTerminated = true
	created.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	if err := s.UpdateSession(ctx, created); err != nil {
		t.Fatalf("terminate issue session: %v", err)
	}
	if _, ok, err := s.GetActiveSessionByIssue(ctx, "mer", rec.IssueID); err != nil || ok {
		t.Fatalf("terminated issue lookup: ok=%v err=%v, want absent", ok, err)
	}
}
