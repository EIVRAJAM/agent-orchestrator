package persistenthost

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostDiagnosticsDisabledByDefault(t *testing.T) {
	t.Setenv(DiagnosticsDirEnv, "")
	dir := t.TempDir()
	diagnostics, err := openHostDiagnostics()
	if err != nil || diagnostics != nil {
		t.Fatalf("disabled diagnostics = %v, %v; want nil, nil", diagnostics, err)
	}
	stdout, stderr, closeFiles, err := openHostStreams()
	if err != nil || stdout != nil || stderr != nil {
		t.Fatalf("disabled host streams = %v, %v, %v; want nil, nil, nil", stdout, stderr, err)
	}
	closeFiles()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("disabled diagnostics created files: %v", entries)
	}
}

func TestHostDiagnosticsWritesOnlyConfiguredFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DiagnosticsDirEnv, dir)
	diagnostics, err := openHostDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	diagnostics.Event("host lock acquired")
	diagnostics.Event("provider started")
	diagnostics.Event("descriptor published")
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, closeFiles, err := openHostStreams()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(stdout, "host stdout\n")
	_, _ = io.WriteString(stderr, "host stderr\n")
	closeFiles()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{"host.events.log", "host.stderr.log", "host.stdout.log"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("diagnostic files = %v, want exactly %v", names, want)
	}

	events, err := os.ReadFile(filepath.Join(dir, "host.events.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(events); got != "host lock acquired\nprovider started\ndescriptor published\n" {
		t.Fatalf("events = %q", got)
	}
	for _, name := range names {
		data, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, sensitive := range []string{"AO_TEST_CHAT_ACP_REPLAY", "prompt text", "credential", "token=", "SECRET"} {
			if strings.Contains(string(data), sensitive) {
				t.Fatalf("%s contains sensitive data %q: %q", name, sensitive, data)
			}
		}
	}
}

func TestHostDiagnosticsNeverCreatesProviderStderrFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DiagnosticsDirEnv, dir)
	diagnostics, err := openHostDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	diagnostics.Event("provider started")
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "host.events.log" {
		t.Fatalf("provider diagnostics unexpectedly persisted files: %v", entries)
	}
}

func TestHostDiagnosticsDiscardArbitraryProviderStderr(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DiagnosticsDirEnv, dir)
	const sentinel = "SECRET_PROVIDER_STDERR_SENTINEL"
	cfg := Config{
		SessionID: "diagnostic-provider", DataDir: t.TempDir(), Workdir: t.TempDir(),
		Env: append(os.Environ(),
			"AO_CHAT_HOST_PROVIDER_HELPER=1",
			"AO_CHAT_HOST_DIAGNOSTIC_STDERR_SENTINEL="+sentinel),
		Argv: []string{os.Args[0], "-test.run=TestProviderHelper"},
	}
	transport, err := ConnectOrStart(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Shutdown(context.Background(), cfg.DataDir, cfg.SessionID) })
	_ = transport.Stdin.Close()
	if err := Shutdown(context.Background(), cfg.DataDir, cfg.SessionID); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "provider.stderr.log" {
			t.Fatal("arbitrary provider stderr was persisted")
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(data), sentinel) {
			t.Fatalf("sentinel leaked into %s", entry.Name())
		}
	}
}

func TestHostDiagnosticsOpenFailureIsReported(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DiagnosticsDirEnv, blocker)
	if _, err := openHostDiagnostics(); err == nil {
		t.Fatal("diagnostics accepted a regular file as the diagnostic directory")
	}
	if _, _, _, err := openHostStreams(); err == nil {
		t.Fatal("host streams accepted a regular file as the diagnostic directory")
	}
}
