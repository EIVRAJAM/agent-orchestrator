package pushobservation

import (
	"context"
	"errors"
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
	if err := os.WriteFile(filepath.Join(root, attemptsFile), []byte("inst-1\tgen-1\tsha-1\trefs/heads/ao/ao-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if ok, err := Attempted(dir, sessionID, "inst-1", "gen-1", "ao/ao-1", "sha-1"); err != nil || !ok {
		t.Fatalf("exact attempt = %v, %v; want true", ok, err)
	}
	if ok, err := Attempted(dir, sessionID, "inst-1", "gen-1", "ao/ao-1", "sha-3"); err != nil || ok {
		t.Fatalf("other head = %v, %v; want false", ok, err)
	}
	if ok, err := Attempted(dir, sessionID, "inst-1", "gen-1", "ao/other", "sha-1"); err != nil || ok {
		t.Fatalf("other branch = %v, %v; want false", ok, err)
	}
	if ok, err := Attempted(dir, sessionID, "inst-2", "gen-1", "ao/ao-1", "sha-1"); err != nil || ok {
		t.Fatalf("other instance = %v, %v; want false", ok, err)
	}
	if ok, err := Attempted(dir, sessionID, "inst-1", "gen-2", "ao/ao-1", "sha-1"); err != nil || ok {
		t.Fatalf("other generation = %v, %v; want false", ok, err)
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
	if err := Install(context.Background(), repo, dataDir, sessionID, "inst-1"); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "push", "-u", "origin", "feature")

	head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	if ok, err := Attempted(dataDir, sessionID, "inst-1", "gen-1", "feature", head); err != nil || !ok {
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
	if err := Install(context.Background(), repo, dataDir, sessionID, "inst-1"); err != nil {
		t.Fatal(err)
	}
	push := exec.Command("git", "-C", repo, "push", "origin", "feature")
	push.Env = append(os.Environ(), "AO_RUNTIME_LAUNCH_ID=gen-1")
	if err := push.Run(); err == nil {
		t.Fatal("push unexpectedly succeeded, want rejection")
	}
	if ok, err := Attempted(dataDir, sessionID, "inst-1", "gen-1", "feature", head); err != nil || !ok {
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
	cmd.Env = append(os.Environ(), "AO_RUNTIME_LAUNCH_ID=gen-1")
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

func TestHookPreservesPreviousHookStdinAndRecordsAttempt(t *testing.T) {
	repo, _ := newPushableRepo(t)
	inputLog := filepath.Join(t.TempDir(), "stdin")
	argsLog := filepath.Join(t.TempDir(), "args")
	original := filepath.Join(repo, ".git", "hooks", "pre-push")
	writeExecutable(t, original, "#!/bin/sh\ncat > "+shellQuote(inputLog)+"\nprintf '%s\\n' \"$@\" > "+shellQuote(argsLog)+"\n")

	dataDir := t.TempDir()
	id := domain.SessionID("ao-1")
	if err := Install(context.Background(), repo, dataDir, id, "inst-1"); err != nil {
		t.Fatal(err)
	}
	input := "refs/heads/feature abc refs/heads/feature 000\nsecond line\n"
	if code := runHookScript(t, hookPath(dataDir, id), []string{"origin", "https://example.invalid/repo"}, input, nil); code != 0 {
		t.Fatalf("hook exit = %d, want 0", code)
	}
	got, err := os.ReadFile(inputLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != input {
		t.Fatalf("previous hook stdin = %q, want %q", got, input)
	}
	gotArgs, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotArgs) != "origin\nhttps://example.invalid/repo\n" {
		t.Fatalf("previous hook args = %q", gotArgs)
	}
	if ok, err := Attempted(dataDir, id, "inst-1", "gen-1", "feature", "abc"); err != nil || !ok {
		t.Fatalf("attempt = %v, %v; want true", ok, err)
	}
}

func TestHookRunsPreviousHookUnfilteredWhenCaptureCannotBeCreated(t *testing.T) {
	repo, _ := newPushableRepo(t)
	inputLog := filepath.Join(t.TempDir(), "stdin")
	original := filepath.Join(repo, ".git", "hooks", "pre-push")
	writeExecutable(t, original, "#!/bin/sh\ncat > "+shellQuote(inputLog)+"\n")
	dataDir := t.TempDir()
	id := domain.SessionID("ao-1")
	if err := Install(context.Background(), repo, dataDir, id, "inst-1"); err != nil {
		t.Fatal(err)
	}
	saved := saveHookAndMakeCaptureDirUnavailable(t, dataDir, id)
	input := "refs/heads/feature abc refs/heads/feature 000\nunfiltered input\n"
	if code := runHookScript(t, saved, nil, input, nil); code != 0 {
		t.Fatalf("hook exit = %d, want 0", code)
	}
	got, err := os.ReadFile(inputLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != input {
		t.Fatalf("previous hook stdin after capture setup failure = %q, want %q", got, input)
	}
	if ok, _ := Attempted(dataDir, id, "inst-1", "gen-1", "feature", "abc"); ok {
		t.Fatal("attempt = true, want absent")
	}
}

func TestHookWithoutPreviousHookDoesNotBlockWhenCaptureCannotBeCreated(t *testing.T) {
	repo, _ := newPushableRepo(t)
	dataDir := t.TempDir()
	id := domain.SessionID("ao-1")
	if err := Install(context.Background(), repo, dataDir, id, "inst-1"); err != nil {
		t.Fatal(err)
	}
	saved := saveHookAndMakeCaptureDirUnavailable(t, dataDir, id)
	if code := runHookScript(t, saved, nil, "refs/heads/feature abc\n", nil); code != 0 {
		t.Fatalf("hook exit = %d, want 0", code)
	}
	if ok, _ := Attempted(dataDir, id, "inst-1", "gen-1", "feature", "abc"); ok {
		t.Fatal("attempt = true, want absent")
	}
}

func TestHookPreservesStdinWhenCaptureWriterFails(t *testing.T) {
	repo, _ := newPushableRepo(t)
	inputLog := filepath.Join(t.TempDir(), "stdin")
	original := filepath.Join(repo, ".git", "hooks", "pre-push")
	writeExecutable(t, original, "#!/bin/sh\ncat > "+shellQuote(inputLog)+"\n")
	dataDir := t.TempDir()
	id := domain.SessionID("ao-1")
	if err := Install(context.Background(), repo, dataDir, id, "inst-1"); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	teeInput := filepath.Join(t.TempDir(), "tee-input")
	fakeTee := "#!/bin/sh\ncat > " + shellQuote(teeInput) + "\nhead -c 5 " + shellQuote(teeInput) + " > \"$1\"\ncat " + shellQuote(teeInput) + "\nexit 1\n"
	writeExecutable(t, filepath.Join(bin, "tee"), fakeTee)

	input := "refs/heads/feature abc refs/heads/feature 000\ncomplete input\n"
	env := map[string]string{
		"AOTEST_TEE_INPUT": teeInput,
		"PATH":             bin + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	if code := runHookScript(t, hookPath(dataDir, id), nil, input, env); code != 0 {
		t.Fatalf("hook exit = %d, want previous-hook exit 0", code)
	}
	got, err := os.ReadFile(inputLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != input {
		t.Fatalf("previous hook stdin after capture write failure = %q, want %q", got, input)
	}
	if ok, err := Attempted(dataDir, id, "inst-1", "gen-1", "feature", "abc"); err != nil || ok {
		t.Fatalf("attempt = %v, %v; want absent", ok, err)
	}
}

func TestHookPreservesPreviousHookExitCode(t *testing.T) {
	repo, _ := newPushableRepo(t)
	original := filepath.Join(repo, ".git", "hooks", "pre-push")
	writeExecutable(t, original, "#!/bin/sh\ncat >/dev/null\nexit 42\n")
	dataDir := t.TempDir()
	id := domain.SessionID("ao-1")
	if err := Install(context.Background(), repo, dataDir, id, "inst-1"); err != nil {
		t.Fatal(err)
	}
	if code := runHookScript(t, hookPath(dataDir, id), nil, "refs/heads/feature abc\n", nil); code != 42 {
		t.Fatalf("hook exit = %d, want previous hook exit 42", code)
	}
	if ok, err := Attempted(dataDir, id, "inst-1", "gen-1", "feature", "abc"); err != nil || !ok {
		t.Fatalf("attempt = %v, %v; want true", ok, err)
	}
}

func hookPath(dataDir string, id domain.SessionID) string {
	return filepath.Join(Dir(dataDir, id), hooksDirName, prePushName)
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func saveHookAndMakeCaptureDirUnavailable(t *testing.T, dataDir string, id domain.SessionID) string {
	t.Helper()
	hook := hookPath(dataDir, id)
	raw, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(t.TempDir(), "pre-push")
	writeExecutable(t, saved, string(raw))
	if err := os.RemoveAll(Dir(dataDir, id)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Dir(dataDir, id), []byte("capture path is not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	return saved
}

func runHookScript(t *testing.T, path string, args []string, input string, extraEnv map[string]string) int {
	t.Helper()
	cmd := exec.Command("sh", append([]string{path}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "AO_RUNTIME_LAUNCH_ID=gen-1")
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	t.Fatalf("run hook: %v", err)
	return -1
}
