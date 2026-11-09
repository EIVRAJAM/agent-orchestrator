// Package pushobservation records the minimum durable evidence needed to tell
// a push that was attempted from one that never was. AO does not run git push
// for a worker; the worker does. A per-worktree pre-push hook therefore records
// only the local branch tip about to be pushed, while the branch-state observer
// separately records whether that tip reached the remote.
//
// The hook is deliberately observational: it never changes the push, never
// prints, and never stores remote URLs, stderr, stdout, prompts, or repository
// content. Git can reject a push before it invokes pre-push (for example a
// non-fast-forward refusal); that leaves no attempt marker and is reported as
// unknown, never as a failed attempt.
package pushobservation

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

const (
	dirName        = "push-observations"
	attemptsFile   = "attempts"
	hooksDirName   = "hooks"
	originalFile   = "original-hook"
	prePushName    = "pre-push"
	attemptLineSep = "\t"
)

// Dir returns the AO-owned directory for one session's push observations.
// Empty or unsafe identifiers return "".
func Dir(dataDir string, id domain.SessionID) string {
	if strings.TrimSpace(dataDir) == "" || strings.TrimSpace(string(id)) == "" {
		return ""
	}
	if filepath.Base(string(id)) != string(id) || string(id) == "." || string(id) == ".." {
		return ""
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	return filepath.Join(dataDir, dirName, string(id))
}

// Attempted reports whether the exact branch head was recorded by a pre-push
// hook. A missing file is a normal "no evidence", not an error.
func Attempted(dataDir string, id domain.SessionID, branch, headSHA string) (bool, error) {
	branch = strings.TrimSpace(branch)
	headSHA = strings.TrimSpace(headSHA)
	if branch == "" || headSHA == "" {
		return false, nil
	}
	dir := Dir(dataDir, id)
	if dir == "" {
		return false, nil
	}
	file, err := os.Open(filepath.Join(dir, attemptsFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open push observations for %s: %w", id, err)
	}
	defer func() { _ = file.Close() }()

	wantRef := "refs/heads/" + branch
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		sha, ref, ok := strings.Cut(scanner.Text(), attemptLineSep)
		if ok && sha == headSHA && ref == wantRef {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read push observations for %s: %w", id, err)
	}
	return false, nil
}

// Install installs an idempotent, worktree-local pre-push hook. It records the
// local ref and head passed to the hook, then chains the repository's previous
// pre-push hook, if any. Failures are returned for the caller to log; a failed
// installation makes push evidence unknown and must not fail the session.
func Install(ctx context.Context, workspacePath, dataDir string, id domain.SessionID) error {
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" {
		return errors.New("push observation: workspace path is required")
	}
	dir := Dir(dataDir, id)
	if dir == "" {
		return errors.New("push observation: data dir and session id are required")
	}
	hooksDir := filepath.Join(dir, hooksDirName)
	if err := os.MkdirAll(hooksDir, 0o700); err != nil {
		return fmt.Errorf("push observation: create hooks dir: %w", err)
	}
	originalPath, err := originalHookPath(ctx, workspacePath, hooksDir)
	if err != nil {
		return err
	}
	if err := writeOriginalHookPath(hooksDir, originalPath); err != nil {
		return err
	}
	if err := writeHookScript(filepath.Join(hooksDir, prePushName), filepath.Join(dir, attemptsFile), originalPath); err != nil {
		return err
	}
	// Worktree-scoped configuration keeps this hook out of the human checkout
	// and every other AO worktree. The extension is repository metadata, not a
	// remote or credential change.
	if out, err := gitOutput(ctx, workspacePath, "config", "--local", "extensions.worktreeConfig", "true"); err != nil {
		return fmt.Errorf("push observation: enable worktree config: %w (%s)", err, strings.TrimSpace(out))
	}
	if out, err := gitOutput(ctx, workspacePath, "config", "--worktree", "core.hooksPath", hooksDir); err != nil {
		return fmt.Errorf("push observation: set worktree hooks path: %w (%s)", err, strings.TrimSpace(out))
	}
	return nil
}

func originalHookPath(ctx context.Context, workspacePath, hooksDir string) (string, error) {
	// A previous installation may already point core.hooksPath at our directory.
	// Keep the original hook path we saved then instead of recursing into our
	// own wrapper.
	if data, err := os.ReadFile(filepath.Join(hooksDir, originalFile)); err == nil {
		return strings.TrimSpace(string(data)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("push observation: read original hook: %w", err)
	}

	current, err := gitOutput(ctx, workspacePath, "config", "--get", "core.hooksPath")
	if err == nil && strings.TrimSpace(current) != "" {
		resolved, pathErr := gitOutput(ctx, workspacePath, "config", "--path", "--get", "core.hooksPath")
		if pathErr != nil {
			return "", fmt.Errorf("push observation: resolve hooks path: %w", pathErr)
		}
		path := strings.TrimSpace(resolved)
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspacePath, path)
		}
		if filepath.Clean(path) == filepath.Clean(hooksDir) {
			return "", nil // already ours and no saved original
		}
		return filepath.Join(path, prePushName), nil
	}
	if err != nil && !isExitStatus(err, 1) {
		return "", fmt.Errorf("push observation: read hooks path: %w", err)
	}
	raw, err := gitOutput(ctx, workspacePath, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", fmt.Errorf("push observation: resolve git hooks: %w", err)
	}
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspacePath, path)
	}
	return filepath.Join(filepath.Clean(path), prePushName), nil
}

func writeOriginalHookPath(hooksDir, originalPath string) error {
	if err := os.WriteFile(filepath.Join(hooksDir, originalFile), []byte(originalPath+"\n"), 0o600); err != nil {
		return fmt.Errorf("push observation: save original hook: %w", err)
	}
	return nil
}

func writeHookScript(path, attemptsPath, originalPath string) error {
	original := ""
	if originalPath != "" && filepath.Clean(originalPath) != filepath.Clean(path) {
		if info, err := os.Stat(originalPath); err == nil && info.Mode().IsRegular() {
			original = originalPath
		}
	}
	script := `#!/bin/sh
# Managed by agent-orchestrator. Observes that a push was attempted; it does not
# change the push and never records URLs, output, or repository content.
set +e
umask 077
attempts=` + shellQuote(attemptsPath) + `
tmp="$attempts.$$"
mkdir -p "$(dirname "$attempts")" 2>/dev/null
if ! cat > "$tmp"; then
  tmp=/dev/null
  cat >/dev/null
fi
if [ -s "$tmp" ]; then
  while IFS=' ' read -r local_ref local_sha remote_ref remote_sha; do
    case "$local_ref" in
      refs/heads/*)
        printf '%s\t%s\n' "$local_sha" "$local_ref" >> "$attempts" 2>/dev/null
        ;;
    esac
  done < "$tmp"
fi
`
	if original != "" {
		script += `if [ -x ` + shellQuote(original) + ` ]; then
  ` + shellQuote(original) + ` "$@" < "$tmp"
  rc=$?
  rm -f "$tmp"
  exit $rc
fi
`
	}
	script += `rm -f "$tmp"
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // git requires an executable hook.
		return fmt.Errorf("push observation: write pre-push hook: %w", err)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := aoprocess.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func isExitStatus(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}
