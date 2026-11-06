package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestSessionInstanceIdentityIsUniqueAcrossDataDirs(t *testing.T) {
	ctx := context.Background()
	dir1, dir2 := t.TempDir(), t.TempDir()
	s1, err := sqlite.Open(dir1)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := sqlite.Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	seedProject(t, s1, "mer")
	seedProject(t, s2, "mer")
	a, err := s1.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s2.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("test precondition: session ids differ (%s vs %s)", a.ID, b.ID)
	}
	if a.InstanceID == "" || b.InstanceID == "" || a.InstanceID == b.InstanceID {
		t.Fatalf("instance ids = %q / %q, want two non-empty distinct values", a.InstanceID, b.InstanceID)
	}
}

func TestSessionPROwnershipSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, s, "mer")
	sess, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	branch, head, prURL := "ao/mer-1/root", "abc123", "https://github.com/acme/demo/pull/1"
	if ok, err := s.RecordSessionBranchTip(ctx, sess.ID, branch, head, "/ws/mer-1"); err != nil || !ok {
		t.Fatalf("record branch tip: ok=%v err=%v", ok, err)
	}
	if ok, err := s.RegisterSessionPROwnership(ctx, sess.InstanceID, prURL, branch, head, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("register ownership: ok=%v err=%v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, ok, err := s.GetSession(ctx, sess.ID)
	if err != nil || !ok || got.InstanceID != sess.InstanceID {
		t.Fatalf("reopened session = %+v ok=%v err=%v, want instance %q", got, ok, err, sess.InstanceID)
	}
	ownership, ok, err := s.GetSessionPROwnership(ctx, prURL)
	if err != nil || !ok {
		t.Fatalf("reopened ownership: ok=%v err=%v", ok, err)
	}
	if ownership.SessionInstanceID != sess.InstanceID || ownership.SourceBranch != branch || ownership.HeadSHA != head {
		t.Fatalf("ownership = %+v, want instance=%s branch=%s head=%s", ownership, sess.InstanceID, branch, head)
	}
}

func TestSessionPROwnershipRejectsMissingTipAndConflictingInstance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "mer")
	a, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	branch, head, prURL := "ao/shared", "sha1", "https://github.com/acme/demo/pull/2"
	// Ownership row exists, but no tip: the PR must not be adoptable.
	if ok, err := s.RecordSessionBranchTip(ctx, a.ID, branch, "", "/ws/a"); err != nil || !ok {
		t.Fatalf("record branch without tip: ok=%v err=%v", ok, err)
	}
	if ok, err := s.RegisterSessionPROwnership(ctx, a.InstanceID, prURL, branch, head, time.Now().UTC()); err != nil || ok {
		t.Fatalf("ownership without tip = %v, %v; want false,nil", ok, err)
	}
	// Register the same tip for both instances; the first owner wins and the
	// second must be rejected, not silently reassigned.
	if ok, err := s.RecordSessionBranchTip(ctx, a.ID, branch, head, "/ws/a"); err != nil || !ok {
		t.Fatalf("record a tip: ok=%v err=%v", ok, err)
	}
	if ok, err := s.RecordSessionBranchTip(ctx, b.ID, branch, head, "/ws/b"); err != nil || !ok {
		t.Fatalf("record b tip: ok=%v err=%v", ok, err)
	}
	if ok, err := s.RegisterSessionPROwnership(ctx, a.InstanceID, prURL, branch, head, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("first ownership: ok=%v err=%v", ok, err)
	}
	if ok, err := s.RegisterSessionPROwnership(ctx, b.InstanceID, prURL, branch, head, time.Now().UTC()); err != nil || ok {
		t.Fatalf("conflicting ownership: ok=%v err=%v; want false,nil", ok, err)
	}
	got, ok, err := s.GetSessionPROwnership(ctx, prURL)
	if err != nil || !ok || got.SessionInstanceID != a.InstanceID {
		t.Fatalf("ownership = %+v ok=%v err=%v, want first instance", got, ok, err)
	}
}
