package pushobservation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestAttemptedRequiresExactBranchAndHead(t *testing.T) {
	dir := t.TempDir()
	sessionID := domain.SessionID("ao-1")
	root := Dir(dir, sessionID)
	if root == "" {
		t.Fatal("Dir returned empty")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, attemptsFile), []byte("sha-1\trefs/heads/ao/ao-1\nsha-2\trefs/heads/ao/ao-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if ok, err := Attempted(dir, sessionID, "ao/ao-1", "sha-1"); err != nil || !ok {
		t.Fatalf("exact attempt = %v, %v; want true", ok, err)
	}
	if ok, err := Attempted(dir, sessionID, "ao/ao-1", "sha-3"); err != nil || ok {
		t.Fatalf("other head = %v, %v; want false", ok, err)
	}
	if ok, err := Attempted(dir, sessionID, "ao/other", "sha-1"); err != nil || ok {
		t.Fatalf("other branch = %v, %v; want false", ok, err)
	}
}

func TestInstallRecordsAttemptAndChainsExistingHook(t *testing.T) {
	repo, _ := newPushableRepo(t)
	originalMarker := filepath.Join(t.TempDir(), "original-hook")
	original := filepath.Join(repo, ".git", "hooks", "pre-push")
	if err := os.WriteFile(original, []byte("#!/bin/sh\necho original > '"+originalMarker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	sessionID := domain.SessionID("ao-1")
	if err := Install(context.Background(), repo, dataDir, sessionID); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "push", "-u", "origin", "feature")

	head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	if ok, err := Attempted(dataDir, sessionID, "feature", head); err != nil || !ok {
		t.Fatalf("recorded attempt = %v, %v; want true", ok, err)
	}
	if data, err := os.ReadFile(originalMarker); err != nil || strings.TrimSpace(string(data)) != "original" {
		t.Fatalf("original hook marker = %q, %v; want chained hook", data, err)
	}
}

func TestInstallDoesNotRecordAFailedPushAsSucceeded(t *testing.T) {
	repo, remote := newPushableRepo(t)
	update := filepath.Join(remote, "hooks", "update")
	if err := os.WriteFile(update, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "local.txt", "local")
	runGit(t, repo, "add", "local.txt")
	runGit(t, repo, "commit", "-m", "local")
	head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))

	dataDir := t.TempDir()
	sessionID := domain.SessionID("ao-1")
	if err := Install(context.Background(), repo, dataDir, sessionID); err != nil {
		t.Fatal(err)
	}
	push := exec.Command("git", "-C", repo, "push", "origin", "feature")
	if err := push.Run(); err == nil {
		t.Fatal("push unexpectedly succeeded, want rejection")
	}
	if ok, err := Attempted(dataDir, sessionID, "feature", head); err != nil || !ok {
		t.Fatalf("failed push attempt = %v, %v; want recorded attempt", ok, err)
	}
}

func newPushableRepo(t *testing.T) (repo, remote string) {
	t.Helper()
	remote = t.TempDir()
	runGit(t, remote, "init", "--bare")
	repo = t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "a@b.c")
	runGit(t, repo, "config", "user.name", "a")
	writeFile(t, repo, "base.txt", "base")
	runGit(t, repo, "add", "base.txt")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "branch", "-M", "main")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")
	runGit(t, repo, "checkout", "-b", "feature")
	return repo, remote
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
