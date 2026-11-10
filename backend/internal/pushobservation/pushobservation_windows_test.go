//go:build windows

package pushobservation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestWindowsPushObservationDirectFallbackPreservesHookContract(t *testing.T) {
	shell := windowsShell(t)
	probe := exec.Command(shell, "-c", "command -v mkfifo; command -v tee")
	probe.Env = os.Environ()
	probeOut, _ := probe.CombinedOutput()
	t.Logf("Windows hook environment: sh=%s probe=%q", shell, probeOut)

	repo, _ := newPushableRepo(t)
	inputLog := filepath.Join(repo, "push-stdin.log")
	argsLog := filepath.Join(repo, "push-args.log")
	original := filepath.Join(repo, ".git", "hooks", "pre-push")
	writeExecutable(t, original, "#!/bin/sh\ncat > push-stdin.log\nprintf '%s\\n' \"$@\" > push-args.log\nexit 7\n")

	dataDir := t.TempDir()
	id := domain.SessionID("ao-1")
	if err := Install(context.Background(), repo, dataDir, id, "inst-1"); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	sentinel := filepath.Join(bin, "aux-invoked")
	writeExecutable(t, filepath.Join(bin, "tee"), "#!/bin/sh\nprintf tee > "+shellQuote(sentinel)+"\ncat\n")
	writeExecutable(t, filepath.Join(bin, "mkfifo"), "#!/bin/sh\nprintf mkfifo > "+shellQuote(sentinel)+"\n")
	env := append(os.Environ(), "AO_RUNTIME_LAUNCH_ID=gen-1", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	input := "refs/heads/feature abc refs/heads/feature 000\nwindows stdin\n"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, filepath.ToSlash(hookPath(dataDir, id)), "origin", "https://example.invalid/repo")
	setHookProcessGroup(cmd)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = env
	runErr := cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("Windows hook blocked until context timeout")
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("Windows hook error = %v, want previous hook exit 7", runErr)
	}
	gotInput, readErr := os.ReadFile(inputLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(gotInput) != input {
		t.Fatalf("Windows previous hook stdin = %q, want %q", gotInput, input)
	}
	gotArgs, readErr := os.ReadFile(argsLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(gotArgs) != "origin\nhttps://example.invalid/repo\n" {
		t.Fatalf("Windows previous hook args = %q", gotArgs)
	}
	if ok, _ := Attempted(dataDir, id, "inst-1", "gen-1", "feature", "abc"); ok {
		t.Fatal("Windows direct fallback fabricated an attempt marker")
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatalf("Windows direct fallback invoked tee/mkfifo: %s", sentinel)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal(statErr)
	}
	assertNoHookArtifacts(t, dataDir, id)
}

func TestWindowsPushObservationDirectFallbackDoesNotReadStdin(t *testing.T) {
	shell := windowsShell(t)
	repo, _ := newPushableRepo(t)
	argsLog := filepath.Join(repo, "push-args.log")
	original := filepath.Join(repo, ".git", "hooks", "pre-push")
	writeExecutable(t, original, "#!/bin/sh\nexec 0<&0\nprintf '%s\\n' \"$@\" > push-args.log\nexit 9\n")

	dataDir := t.TempDir()
	id := domain.SessionID("ao-1")
	if err := Install(context.Background(), repo, dataDir, id, "inst-1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, filepath.ToSlash(hookPath(dataDir, id)), "origin", "https://example.invalid/repo")
	setHookProcessGroup(cmd)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader("refs/heads/feature abc refs/heads/feature 000\nsmall input\n")
	cmd.Env = append(os.Environ(), "AO_RUNTIME_LAUNCH_ID=gen-1")
	runErr := cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("Windows hook blocked until context timeout")
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 9 {
		t.Fatalf("Windows hook error = %v, want previous hook exit 9", runErr)
	}
	gotArgs, readErr := os.ReadFile(argsLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(gotArgs) != "origin\nhttps://example.invalid/repo\n" {
		t.Fatalf("Windows previous hook args = %q", gotArgs)
	}
	if ok, _ := Attempted(dataDir, id, "inst-1", "gen-1", "feature", "abc"); ok {
		t.Fatal("Windows direct fallback fabricated an attempt marker")
	}
	assertNoHookArtifacts(t, dataDir, id)
}

func windowsShell(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"sh", "bash"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	programFiles := os.Getenv("ProgramFiles")
	candidates := []string{
		filepath.Join(programFiles, "Git", "usr", "bin", "sh.exe"),
		filepath.Join(programFiles, "Git", "bin", "bash.exe"),
		filepath.Join(programFiles, "Git", "usr", "bin", "bash.exe"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Fatalf("Git Bash shell unavailable; checked PATH and %v", candidates)
	return ""
}
