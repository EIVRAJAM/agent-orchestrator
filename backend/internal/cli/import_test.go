package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemon/datadirlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/legacyimport"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

func writeLegacyProject(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".agent-orchestrator")
	if err := os.MkdirAll(filepath.Join(root, "projects", "alpha", "sessions"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"),
		[]byte("projects:\n  alpha:\n    path: /repos/alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestImportCommand_NoLegacyData(t *testing.T) {
	setConfigEnv(t)
	empty := filepath.Join(t.TempDir(), "nope")
	out, _, err := executeCLI(t, Deps{}, "import", "--from", empty, "--yes")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(out, "Nothing to import") {
		t.Fatalf("out = %q, want 'Nothing to import'", out)
	}
}

func TestImportCommand_ImportsProjectJSON(t *testing.T) {
	setConfigEnv(t)
	root := writeLegacyProject(t)

	out, _, err := executeCLI(t, Deps{}, "import", "--from", root, "--yes", "--json")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var rep legacyimport.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse report %q: %v", out, err)
	}
	if rep.ProjectsImported != 1 {
		t.Fatalf("projectsImported = %d, want 1", rep.ProjectsImported)
	}
}

func TestImportCommand_SurfacesParseErrorOnce(t *testing.T) {
	setConfigEnv(t)
	// A tab-indented line is a YAML syntax error (not a *yaml.TypeError), so
	// loadLegacyConfig surfaces it exactly like issue #2186 describes.
	root := filepath.Join(t.TempDir(), ".agent-orchestrator")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"),
		[]byte("projects:\n\talpha:\n  path: /repos/alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := executeCLI(t, Deps{}, "import", "--from", root, "--yes")
	if err == nil {
		t.Fatal("import: want non-nil error for a config.yaml that fails to parse")
	}
	if got := ExitCode(err); got != 1 {
		t.Fatalf("ExitCode = %d, want 1 (runtime failure)", got)
	}
	// The parse error must reach the user exactly once. main.go prints the
	// returned error, so the command itself must not also Fprintf it; that
	// doubled the output before this fix.
	count := strings.Count(stderr, "parse legacy config.yaml") + strings.Count(err.Error(), "parse legacy config.yaml")
	if count != 1 {
		t.Fatalf("parse error appeared %d time(s) (stderr=%q, err=%q), want exactly 1", count, stderr, err)
	}
}

func TestImportCommand_RefusesWhenDaemonRunning(t *testing.T) {
	cfg := setConfigEnv(t)
	root := writeLegacyProject(t)

	// A run-file owned by this (alive) process makes the daemon look live.
	if err := runfile.Write(cfg.runFile, runfile.Info{PID: os.Getpid(), Port: 3001, StartedAt: time.Now()}); err != nil {
		t.Fatalf("write run-file: %v", err)
	}

	_, _, err := executeCLI(t, Deps{}, "import", "--from", root, "--yes")
	if err == nil || !strings.Contains(err.Error(), "daemon is running") {
		t.Fatalf("err = %v, want refusal because daemon is running", err)
	}
}

func TestImportCommand_DataDirLockIsAuthoritative(t *testing.T) {
	cfg := setConfigEnv(t)
	root := writeLegacyProject(t)
	lock, err := datadirlock.Acquire(datadirlock.Path(cfg.dataDir))
	if err != nil {
		t.Fatalf("hold data-dir lock: %v", err)
	}
	defer func() { _ = lock.Close() }()

	// Neither a missing, malformed, nor stale run-file can bypass the lock.
	for _, test := range []struct {
		name    string
		prepare func() error
	}{
		{name: "missing run-file", prepare: func() error { return nil }},
		{name: "malformed run-file", prepare: func() error {
			return os.WriteFile(cfg.runFile, []byte("{not-json"), 0o600)
		}},
		{name: "stale run-file", prepare: func() error {
			return runfile.Write(cfg.runFile, runfile.Info{PID: 0x7fffffff, Port: 3001})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.Remove(cfg.runFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err := test.prepare(); err != nil {
				t.Fatal(err)
			}
			_, _, err := executeCLI(t, Deps{}, "import", "--from", root, "--yes")
			if err == nil {
				t.Fatal("import succeeded while the data-directory lock was held")
			}
			if got := ExitCode(err); got != 2 {
				t.Fatalf("ExitCode = %d, want 2 for a contended data directory: %v", got, err)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "in use") {
				t.Fatalf("lock error is not clear: %v", err)
			}
			assertNoMainStoreFiles(t, cfg.dataDir)
		})
	}

	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	// Releasing the holder lets import acquire the lock, and import releases it
	// when finished.
	if _, _, err := executeCLI(t, Deps{}, "import", "--from", root, "--yes"); err != nil {
		t.Fatalf("import after lock release: %v", err)
	}
	reacquired, err := datadirlock.Acquire(datadirlock.Path(cfg.dataDir))
	if err != nil {
		t.Fatalf("lock after successful import: %v", err)
	}
	_ = reacquired.Close()
}

func TestImportCommand_HoldsDataDirLockUntilFinished(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		name := "write"
		if dryRun {
			name = "dry-run"
		}
		t.Run(name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			root := writeLegacyProject(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			importAfterLock = func() {
				close(entered)
				<-release
			}
			t.Cleanup(func() { importAfterLock = nil })

			args := []string{"import", "--from", root, "--yes"}
			if dryRun {
				args = append(args, "--dry-run")
			}
			type result struct {
				err error
			}
			done := make(chan result, 1)
			go func() {
				_, _, err := executeCLI(t, Deps{}, args...)
				done <- result{err: err}
			}()
			<-entered
			if _, err := datadirlock.Acquire(datadirlock.Path(cfg.dataDir)); !errors.Is(err, datadirlock.ErrLocked) {
				t.Fatalf("daemon-path lock during import = %v, want ErrLocked", err)
			}
			close(release)
			if got := <-done; got.err != nil {
				t.Fatalf("import failed: %v", got.err)
			}
			lock, err := datadirlock.Acquire(datadirlock.Path(cfg.dataDir))
			if err != nil {
				t.Fatalf("lock after import: %v", err)
			}
			_ = lock.Close()
		})
	}
}

func TestImportCommand_ParseFailureReleasesDataDirLock(t *testing.T) {
	cfg := setConfigEnv(t)
	root := filepath.Join(t.TempDir(), ".agent-orchestrator")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"),
		[]byte("projects:\n\talpha:\n  path: /repos/alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := executeCLI(t, Deps{}, "import", "--from", root, "--yes")
	if err == nil {
		t.Fatal("import with malformed legacy config succeeded")
	}
	if got := ExitCode(err); got != 1 {
		t.Fatalf("ExitCode = %d, want 1 for parse failure", got)
	}
	assertNoMainStoreFiles(t, cfg.dataDir)
	lock, lockErr := datadirlock.Acquire(datadirlock.Path(cfg.dataDir))
	if lockErr != nil {
		t.Fatalf("lock was not released after parse failure: %v", lockErr)
	}
	_ = lock.Close()
}

func assertNoMainStoreFiles(t *testing.T, dataDir string) {
	t.Helper()
	for _, name := range []string{"ao.db", "ao.db-wal", "ao.db-shm", "running.json", "supervise.sock", "browser.sock"} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
			t.Fatalf("loser created %s", filepath.Join(dataDir, name))
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat %s: %v", filepath.Join(dataDir, name), err)
		}
	}
}
