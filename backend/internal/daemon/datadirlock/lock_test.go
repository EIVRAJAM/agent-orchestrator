package datadirlock

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireReleaseAndMetadata(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	path := Path(dataDir)
	if got := filepath.Join(dataDir, "daemon.lock"); got != path {
		t.Fatalf("lock path = %q, want %q", path, got)
	}

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first.Path() != path {
		t.Fatalf("Path() = %q, want %q", first.Path(), path)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire = %v, want ErrLocked", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if metadata["pid"] != float64(os.Getpid()) || metadata["version"] != float64(1) {
		t.Fatalf("metadata = %#v", metadata)
	}
	if strings.Contains(strings.ToLower(string(data)), "token") ||
		strings.Contains(strings.ToLower(string(data)), "prompt") ||
		strings.Contains(strings.ToLower(string(data)), "credential") {
		t.Fatalf("metadata contains sensitive-looking data: %s", data)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire after close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResidualFileDoesNotBlockAcquire(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "data"))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale lock metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire over residual file: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireCreatesParentAndReportsInvalidPath(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "nested", "data"))
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire with missing parent: %v", err)
	}
	if info, statErr := os.Stat(filepath.Dir(path)); statErr != nil || !info.IsDir() {
		t.Fatalf("parent was not created: info=%v err=%v", info, statErr)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	notDir := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(filepath.Join(notDir, "daemon.lock")); err == nil {
		t.Fatal("invalid parent path was accepted")
	}
	if _, err := Acquire(""); err == nil {
		t.Fatal("empty lock path was accepted")
	}
}

// TestDataDirLockChild is the subprocess entrypoint used by the cross-process
// tests. A parent test runs this same test binary with this environment set.
func TestDataDirLockChild(t *testing.T) {
	if os.Getenv("AO_DATADIRLOCK_CHILD") != "1" {
		return
	}
	path := os.Getenv("AO_DATADIRLOCK_PATH")
	lock, err := Acquire(path)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			_, _ = fmt.Fprintln(os.Stdout, "busy")
			os.Exit(0)
		}
		_, _ = fmt.Fprintf(os.Stderr, "acquire: %v\n", err)
		os.Exit(1)
	}
	_, _ = fmt.Fprintln(os.Stdout, "locked")
	if os.Getenv("AO_DATADIRLOCK_MODE") == "crash" {
		os.Exit(23) // deliberately no Close: kernel releases on process exit
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := lock.Close(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "close: %v\n", err)
		os.Exit(1)
	}
	_, _ = fmt.Fprintln(os.Stdout, "closed")
	os.Exit(0)
}

type lockChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr bytes.Buffer
}

func startLockChild(t *testing.T, path, mode string) *lockChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDataDirLockChild$")
	cmd.Env = append(os.Environ(),
		"AO_DATADIRLOCK_CHILD=1",
		"AO_DATADIRLOCK_PATH="+path,
		"AO_DATADIRLOCK_MODE="+mode,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child := &lockChild{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	cmd.Stderr = &child.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.cmd.ProcessState != nil {
			return
		}
		_ = child.stdin.Close()
		_ = child.cmd.Process.Kill()
		_ = child.cmd.Wait()
	})
	return child
}

func (c *lockChild) line(t *testing.T) string {
	t.Helper()
	line := make(chan string, 1)
	go func() {
		value, _ := c.stdout.ReadString('\n')
		line <- strings.TrimSpace(value)
	}()
	select {
	case value := <-line:
		if value == "" {
			t.Fatalf("child produced no status line; stderr=%s", c.stderr.String())
		}
		return value
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for child; stderr=%s", c.stderr.String())
		return ""
	}
}

func (c *lockChild) release(t *testing.T) {
	t.Helper()
	_ = c.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = c.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("child did not exit; stderr=%s", c.stderr.String())
	}
}

func TestProcessLockIsExclusiveAcrossProcesses(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "data"))
	holder := startLockChild(t, path, "hold")
	if got := holder.line(t); got != "locked" {
		t.Fatalf("holder status = %q, want locked", got)
	}

	if _, err := Acquire(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("parent acquire while child holds lock = %v, want ErrLocked", err)
	}
	holder.release(t)

	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire after child close: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessCrashReleasesKernelLock(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "data"))
	child := startLockChild(t, path, "crash")
	if got := child.line(t); got != "locked" {
		t.Fatalf("crash child status = %q, want locked", got)
	}
	// The child exits without Close. The kernel releases the lock, and the
	// residual file must not prevent a new owner.
	done := make(chan struct{})
	go func() {
		_ = child.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("crash child did not exit")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("residual lock file missing: %v", err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire after crash: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSimultaneousProcessesHaveExactlyOneOwner(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "data"))
	first := startLockChild(t, path, "hold")
	second := startLockChild(t, path, "hold")

	statuses := []string{first.line(t), second.line(t)}
	holders, losers := 0, 0
	var holder, loser *lockChild
	for i, status := range statuses {
		switch status {
		case "locked":
			holders++
			if i == 0 {
				holder = first
			} else {
				holder = second
			}
		case "busy":
			losers++
			if i == 0 {
				loser = first
			} else {
				loser = second
			}
		default:
			t.Fatalf("unexpected child status %q", status)
		}
	}
	if holders != 1 || losers != 1 {
		t.Fatalf("statuses = %v, want exactly one locked and one busy", statuses)
	}
	_ = loser // already exited after reporting busy
	holder.release(t)
}

func TestLockPathAndMetadataAreLocalOnly(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	path := Path(dataDir)
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if !strings.HasPrefix(path, dataDir+string(filepath.Separator)) {
		t.Fatalf("lock path escapes data dir: %q", path)
	}
}
