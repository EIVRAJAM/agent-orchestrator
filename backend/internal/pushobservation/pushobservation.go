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
	"runtime"
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

// Attempted reports whether a current-generation pre-push hook recorded the
// exact instance, branch and head. A missing file or a stale generation is a
// normal "no evidence", not an error.
func Attempted(dataDir string, id domain.SessionID, instanceID, generation, branch, headSHA string) (bool, error) {
	// Windows uses the direct delegation hook and never creates a valid
	// attempt marker. Ignore any inherited marker from an older Windows build
	// rather than attributing its attempt to the current generation.
	if runtime.GOOS == "windows" {
		return false, nil
	}
	return attemptedFromObservation(dataDir, id, instanceID, generation, branch, headSHA)
}

// attemptedFromObservation is the platform-independent marker parser. It is
// kept separate from Attempted so Linux/macOS parser coverage remains strong
// even though Windows intentionally ignores inherited markers.
func attemptedFromObservation(dataDir string, id domain.SessionID, instanceID, generation, branch, headSHA string) (bool, error) {
	instanceID = strings.TrimSpace(instanceID)
	generation = strings.TrimSpace(generation)
	branch = strings.TrimSpace(branch)
	headSHA = strings.TrimSpace(headSHA)
	if instanceID == "" || generation == "" || branch == "" || headSHA == "" {
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
		parts := strings.Split(scanner.Text(), attemptLineSep)
		if len(parts) == 4 && parts[0] == instanceID && parts[1] == generation &&
			parts[2] == headSHA && parts[3] == wantRef {
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
func Install(ctx context.Context, workspacePath, dataDir string, id domain.SessionID, instanceID string) error {
	workspacePath = strings.TrimSpace(workspacePath)
	instanceID = strings.TrimSpace(instanceID)
	if workspacePath == "" {
		return errors.New("push observation: workspace path is required")
	}
	if instanceID == "" {
		return errors.New("push observation: session instance id is required")
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
	if runtime.GOOS == "windows" {
		// Git for Windows does not provide a reliable filesystem FIFO. Keep the
		// Windows hook as a direct delegation: no tee, FIFO, marker, or auxiliary
		// process can affect the previous hook's stdin or exit status.
		if err := writeDirectHookScript(filepath.Join(hooksDir, prePushName), originalPath); err != nil {
			return err
		}
	} else if err := writeHookScript(filepath.Join(hooksDir, prePushName), filepath.Join(dir, attemptsFile), instanceID, originalPath); err != nil {
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

func writeDirectHookScript(path, originalPath string) error {
	original := ""
	if originalPath != "" {
		if info, err := os.Stat(originalPath); err == nil && info.Mode().IsRegular() {
			original = originalPath
		}
	}
	script := `#!/bin/sh
# Managed by agent-orchestrator. Windows direct fallback: delegate exactly to
# the previous hook, or consume stdin when there is no previous hook. It does
# not record an attempt or inspect repository/remote data.
set +e
original=` + shellQuote(filepath.ToSlash(original)) + `
if [ -n "$original" ] && [ -x "$original" ]; then
  "$original" "$@"
  exit $?
fi
cat >/dev/null
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // git requires an executable hook.
		return fmt.Errorf("push observation: write direct pre-push hook: %w", err)
	}
	return nil
}

func writeHookScript(path, attemptsPath, instanceID, originalPath string) error {
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
instance=` + shellQuote(instanceID) + `
tmp="$attempts.$$"
original=` + shellQuote(original) + `

run_original() {
  if [ -n "$original" ] && [ -x "$original" ]; then
    "$original" "$@"
  else
    cat >/dev/null
  fi
}

record_attempts() {
  [ -s "$tmp" ] || return 0
  while IFS=' ' read -r local_ref local_sha remote_ref remote_sha; do
    case "$local_ref" in
      refs/heads/*)
        printf '%s\t%s\t%s\t%s\n' "$instance" "${AO_RUNTIME_LAUNCH_ID-}" "$local_sha" "$local_ref" >> "$attempts" 2>/dev/null
        ;;
    esac
  done < "$tmp"
}

# Prepare the capture target before consuming stdin. If that is impossible,
# run the prior hook unfiltered; do not consume its stdin.
if ! mkdir -p "$(dirname "$attempts")" 2>/dev/null || ! : > "$tmp" 2>/dev/null; then
  rm -f "$tmp"
  run_original "$@"
  exit $?
fi

if [ -n "$original" ] && [ -x "$original" ]; then
  fifo="$attempts.$$.fifo"
  rm -f "$fifo" 2>/dev/null
  if command -v tee >/dev/null 2>&1 && mkfifo "$fifo" 2>/dev/null; then
    # Stream the original stdin to the previous hook while tee copies the same
    # bytes to the capture file. If the previous hook does not read all of its
    # stdin or tee cannot write the file, keep writing the capture and return
    # the previous hook's own status. The original hook runs in a subshell
    # with the normal SIGPIPE disposition; only tee ignores it so a hook that
    # exits without reading stdin cannot prevent the capture.
    trap '' PIPE
    ( trap - PIPE; "$original" "$@" < "$fifo" ) &
    hook_pid=$!
    tee "$tmp" > "$fifo" 2>/dev/null
    tee_rc=$?
    wait "$hook_pid"
    hook_rc=$?
    trap - PIPE
    if [ "$tee_rc" -eq 0 ]; then
      record_attempts
    fi
    rm -f "$fifo" "$tmp"
    exit "$hook_rc"
  fi
  rm -f "$tmp"
  "$original" "$@"
  exit $?
fi

if cat > "$tmp" 2>/dev/null; then
  record_attempts
fi
rm -f "$tmp"
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
