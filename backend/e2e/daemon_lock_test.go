//go:build !windows

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemon/datadirlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// A second real daemon must fail before it can bind a port, replace the
// handshake, unlink the owner's supervisor socket, or open a second store.
func TestSecondDaemonFailsBeforeMutatingOwnerState(t *testing.T) {
	dataDir := t.TempDir()
	d := startDaemon(t, dataDir)
	runPath := filepath.Join(dataDir, "running.json")
	firstRun, err := runfile.Read(runPath)
	if err != nil || firstRun == nil {
		t.Fatalf("read owner run-file: info=%v err=%v", firstRun, err)
	}
	socketPath := filepath.Join(dataDir, "supervise.sock")
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("owner supervisor socket: %v", err)
	}

	secondPort := freePort(t)
	second := runDaemonCommand(t, dataDir, runPath, secondPort)
	if second.exitCode == 0 {
		t.Fatalf("second daemon succeeded; output:\n%s", second.output)
	}
	if !strings.Contains(strings.ToLower(second.output), "already owned") &&
		!strings.Contains(strings.ToLower(second.output), "data directory") {
		t.Fatalf("second daemon error is not clear:\n%s", second.output)
	}
	afterRun, err := runfile.Read(runPath)
	if err != nil || !reflect.DeepEqual(firstRun, afterRun) {
		t.Fatalf("owner run-file changed: before=%+v after=%+v err=%v", firstRun, afterRun, err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("owner supervisor socket disappeared: %v", err)
	}
	conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", secondPort), 300*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		t.Fatal("losing daemon opened its configured port")
	}
	var settings map[string]any
	if status, callErr := d.call("GET", "/settings", nil, &settings); callErr != nil || status != 200 {
		t.Fatalf("owner daemon became unavailable: status=%d err=%v", status, callErr)
	}

	d.stop()
	// A cleanly stopped owner releases the kernel lock and the same data dir can
	// be opened again.
	restarted := startDaemon(t, dataDir)
	restarted.stop()
}

func TestDaemonLockSurvivesCrashAndCorruptDiscovery(t *testing.T) {
	t.Run("crash release", func(t *testing.T) {
		dataDir := t.TempDir()
		d := startDaemon(t, dataDir)
		d.kill()
		next := startDaemon(t, dataDir)
		next.stop()
	})

	t.Run("corrupt run-file is overwritten by the lock owner", func(t *testing.T) {
		dataDir := t.TempDir()
		runPath := filepath.Join(dataDir, "running.json")
		if err := os.WriteFile(runPath, []byte(`{"pid":not-json`), 0o600); err != nil {
			t.Fatal(err)
		}
		d := startDaemon(t, dataDir)
		if info, err := runfile.Read(runPath); err != nil || info == nil || info.PID <= 0 {
			t.Fatalf("valid run-file was not published: info=%v err=%v", info, err)
		}
		d.stop()
	})

	t.Run("stale supervisor socket is replaced", func(t *testing.T) {
		dataDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dataDir, "supervise.sock"), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
		d := startDaemon(t, dataDir)
		d.stop()
	})
}

func TestLockHolderRejectsDaemonBeforeStateMutation(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "ao.db")
	runPath := filepath.Join(dataDir, "running.json")
	socketPath := filepath.Join(dataDir, "supervise.sock")
	for path, content := range map[string]string{
		dbPath:     "sentinel database bytes",
		runPath:    `{"pid":12345,"port":1}`,
		socketPath: "sentinel socket bytes",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := readSentinelFiles(t, dbPath, runPath, socketPath)

	holder := startE2ELockHolder(t, dataDir)
	defer holder.release(t)
	port := freePort(t)
	result := runDaemonCommand(t, dataDir, runPath, port)
	if result.exitCode == 0 {
		t.Fatalf("daemon acquired a data dir already held by a lock holder:\n%s", result.output)
	}
	after := readSentinelFiles(t, dbPath, runPath, socketPath)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("losing daemon mutated state before lock rejection: before=%q after=%q", before, after)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("losing daemon opened a listener")
	}
}

func readSentinelFiles(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read sentinel %s: %v", path, err)
		}
		out[path] = string(data)
	}
	return out
}

// TestDataDirLockHolderHelper is a subprocess that holds the real
// datadirlock for the parent e2e test. It is not a daemon and never touches
// SQLite, the run-file, or any listener.
func TestDataDirLockHolderHelper(t *testing.T) {
	if os.Getenv("AO_E2E_LOCK_HOLDER") != "1" {
		return
	}
	dataDir := os.Getenv("AO_E2E_LOCK_DATA_DIR")
	lock, err := datadirlock.Acquire(datadirlock.Path(dataDir))
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "acquire lock: %v\n", err)
		os.Exit(1)
	}
	_, _ = fmt.Fprintln(os.Stdout, "locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := lock.Close(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "release lock: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

type e2eLockHolder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr bytes.Buffer
}

func startE2ELockHolder(t *testing.T, dataDir string) *e2eLockHolder {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDataDirLockHolderHelper$")
	cmd.Env = append(os.Environ(),
		"AO_E2E_LOCK_HOLDER=1",
		"AO_E2E_LOCK_DATA_DIR="+dataDir,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	holder := &e2eLockHolder{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	cmd.Stderr = &holder.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if holder.cmd.ProcessState != nil {
			return
		}
		_ = holder.stdin.Close()
		_ = holder.cmd.Process.Kill()
		_ = holder.cmd.Wait()
	})
	line := make(chan string, 1)
	go func() {
		value, _ := holder.stdout.ReadString('\n')
		line <- strings.TrimSpace(value)
	}()
	select {
	case value := <-line:
		if value != "locked" {
			t.Fatalf("lock holder status = %q; stderr=%s", value, holder.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for lock holder; stderr=%s", holder.stderr.String())
	}
	return holder
}

func (h *e2eLockHolder) release(t *testing.T) {
	t.Helper()
	_ = h.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = h.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("lock holder did not exit; stderr=%s", h.stderr.String())
	}
}

func TestRunfileAccessErrorFailsBeforeStore(t *testing.T) {
	dataDir := t.TempDir()
	runDir := filepath.Join(dataDir, "run-directory")
	if err := os.Mkdir(runDir, 0o750); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	failed := runDaemonCommand(t, dataDir, runDir, port)
	if failed.exitCode == 0 {
		t.Fatalf("startup with a directory run-file path unexpectedly succeeded:\n%s", failed.output)
	}
	assertNoAODatabaseOrDiscovery(t, dataDir)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("failed startup opened a listener")
	}
	// The failed startup released the lock; a corrected path can start.
	next := startDaemon(t, dataDir)
	next.stop()
}

func TestDaemonStartupFailureReleasesDataDirLock(t *testing.T) {
	dataDir := t.TempDir()
	badParent := filepath.Join(dataDir, "not-a-directory")
	if err := os.WriteFile(badParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	badRunFile := filepath.Join(badParent, "running.json")
	port := freePort(t)
	failed := runDaemonCommand(t, dataDir, badRunFile, port)
	if failed.exitCode == 0 {
		t.Fatalf("startup with unwritable run-file unexpectedly succeeded:\n%s", failed.output)
	}
	// Access errors must fail before SQLite or any listener/socket is opened.
	assertNoAODatabaseOrDiscovery(t, dataDir)
	next := startDaemon(t, dataDir)
	next.stop()
}

func assertNoAODatabaseOrDiscovery(t *testing.T, dataDir string) {
	t.Helper()
	for _, name := range []string{"ao.db", "ao.db-wal", "ao.db-shm", "running.json", "supervise.sock", "browser.sock"} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
			t.Fatalf("startup failure created %s", filepath.Join(dataDir, name))
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat %s: %v", filepath.Join(dataDir, name), err)
		}
	}
}

type daemonCommandResult struct {
	output   string
	exitCode int
}

func runDaemonCommand(t *testing.T, dataDir, runFile string, port int) daemonCommandResult {
	t.Helper()
	bin := buildDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "daemon")
	cmd.Env = append(os.Environ(),
		"AO_DATA_DIR="+dataDir,
		"AO_RUN_FILE="+runFile,
		"AO_PORT="+strconv.Itoa(port),
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("daemon command did not exit before timeout; output:\n%s", output.String())
	}
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run daemon: %v\n%s", err, output.String())
		}
	}
	return daemonCommandResult{output: output.String(), exitCode: exitCode}
}
