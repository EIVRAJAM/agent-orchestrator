package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestTrackerWritebackOutboxIsUniqueClaimedAndAtMostOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	session, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	row := domain.TrackerWriteback{
		ID: "wb-1", ProjectID: "mer", SessionID: session.ID,
		IssueID: "github:acme/demo#12", PRURL: "https://github.com/acme/demo/pull/1",
		HeadSHA: "sha-one",
		Status:  domain.TrackerWritebackPending, CreatedAt: now, UpdatedAt: now,
	}
	inserted, err := s.EnqueueTrackerWriteback(ctx, row)
	if err != nil || !inserted {
		t.Fatalf("first enqueue = inserted=%v err=%v", inserted, err)
	}
	duplicate := row
	duplicate.ID = "wb-2"
	inserted, err = s.EnqueueTrackerWriteback(ctx, duplicate)
	if err != nil || inserted {
		t.Fatalf("duplicate enqueue = inserted=%v err=%v", inserted, err)
	}

	claimed, ok, err := s.ClaimTrackerWriteback(ctx, now)
	if err != nil || !ok || claimed.ID != "wb-1" || claimed.Status != domain.TrackerWritebackClaimed {
		t.Fatalf("claim = %+v ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := s.ClaimTrackerWriteback(ctx, now); err != nil || ok {
		t.Fatalf("second claim = ok=%v err=%v, want no row", ok, err)
	}
	if ok, err := s.FinishTrackerWriteback(ctx, "wb-1", domain.TrackerWritebackSent, "https://example.test/c/1", "", now); err != nil || !ok {
		t.Fatalf("finish = ok=%v err=%v", ok, err)
	}
	if ok, err := s.FinishTrackerWriteback(ctx, "wb-1", domain.TrackerWritebackUncertain, "", "remote_uncertain", now); err != nil || ok {
		t.Fatalf("second finish = ok=%v err=%v, want terminal guard", ok, err)
	}
	if _, ok, err := s.ClaimTrackerWriteback(ctx, now); err != nil || ok {
		t.Fatalf("claim after sent = ok=%v err=%v, want no row", ok, err)
	}
}

func TestTrackerWritebackPersistsExactHeadAndRejectsEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	session, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	row := domain.TrackerWriteback{
		ID: "wb-head", ProjectID: "mer", SessionID: session.ID,
		IssueID: "github:acme/demo#12", PRURL: "https://github.com/acme/demo/pull/1",
		HeadSHA: "sha-expected", Status: domain.TrackerWritebackPending, CreatedAt: now, UpdatedAt: now,
	}
	inserted, err := s.EnqueueTrackerWriteback(ctx, row)
	if err != nil || !inserted {
		t.Fatalf("enqueue = inserted=%v err=%v", inserted, err)
	}

	rows, err := s.ListTrackerWritebacks(ctx, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].HeadSHA != "sha-expected" {
		t.Fatalf("rows = %+v, want one row with head sha-expected", rows)
	}

	// The idempotency key preserves the original head even if a later
	// observation supplies a different one.
	duplicate := row
	duplicate.ID = "wb-head-2"
	duplicate.HeadSHA = "sha-other"
	inserted, err = s.EnqueueTrackerWriteback(ctx, duplicate)
	if err != nil || inserted {
		t.Fatalf("duplicate enqueue = inserted=%v err=%v", inserted, err)
	}
	rows, err = s.ListTrackerWritebacks(ctx, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].HeadSHA != "sha-expected" {
		t.Fatalf("after duplicate rows = %+v, want original head", rows)
	}

	invalid := row
	invalid.ID = "wb-empty"
	invalid.PRURL = "https://github.com/acme/demo/pull/2"
	invalid.HeadSHA = ""
	inserted, err = s.EnqueueTrackerWriteback(ctx, invalid)
	if err == nil || inserted {
		t.Fatalf("empty head enqueue = inserted=%v err=%v, want explicit rejection", inserted, err)
	}
	rows, err = s.ListTrackerWritebacks(ctx, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("after empty-head rejection rows = %d, want 1", len(rows))
	}
}

func TestTrackerWritebackRecoveryMarksClaimedRowsUncertain(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	session, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.EnqueueTrackerWriteback(ctx, domain.TrackerWriteback{
		ID: "wb-1", ProjectID: "mer", SessionID: session.ID,
		IssueID: "github:acme/demo#12", PRURL: "https://github.com/acme/demo/pull/1",
		HeadSHA: "sha-one", Status: domain.TrackerWritebackPending, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimTrackerWriteback(ctx, now); err != nil || !ok {
		t.Fatalf("claim = ok=%v err=%v", ok, err)
	}
	if n, err := s.RecoverStaleTrackerWritebacks(ctx, now); err != nil || n != 1 {
		t.Fatalf("recover = %d err=%v", n, err)
	}
	if _, ok, err := s.ClaimTrackerWriteback(ctx, now); err != nil || ok {
		t.Fatalf("claim after recovery = ok=%v err=%v, want no row", ok, err)
	}
}
