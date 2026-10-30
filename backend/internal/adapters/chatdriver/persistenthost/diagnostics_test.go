package persistenthost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostDiagnosticsDisabledByDefault(t *testing.T) {
	t.Setenv(hostEventsFDEnv, "")
	diagnostics, err := openHostDiagnosticsFromFD()
	if err != nil || diagnostics != nil {
		t.Fatalf("disabled diagnostics = %v, %v; want nil, nil", diagnostics, err)
	}
}

func TestHostDiagnosticsRejectsNonExtraFileDescriptor(t *testing.T) {
	t.Setenv(hostEventsFDEnv, "4")
	if _, err := openHostDiagnosticsFromFD(); err == nil {
		t.Fatal("diagnostics accepted an arbitrary inherited descriptor")
	}
}

func TestHostDiagnosticsWritesOnlyProvidedHandles(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "host.events.log")
	events, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := &hostDiagnostics{events: events, remaining: maxDiagnosticLogBytes}
	diagnostics.Event("host lock acquired")
	diagnostics.Event("provider started")
	diagnostics.Event("descriptor published")
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}

	names := []string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "host.events.log" {
		t.Fatalf("diagnostic files = %v, want only host.events.log", names)
	}
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "host lock acquired\nprovider started\ndescriptor published\n" {
		t.Fatalf("events = %q", got)
	}
	for _, sensitive := range []string{"AO_TEST_CHAT_ACP_REPLAY", "prompt text", "credential", "token=", "SECRET"} {
		if strings.Contains(string(data), sensitive) {
			t.Fatalf("host.events.log contains sensitive data %q: %q", sensitive, data)
		}
	}
}

func TestHostDiagnosticsEventSizeIsCapped(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "host.events.log")
	events, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := &hostDiagnostics{events: events, remaining: maxDiagnosticLogBytes}
	diagnostics.Event(strings.Repeat("x", int(maxDiagnosticLogBytes)+100))
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxDiagnosticLogBytes {
		t.Fatalf("events file grew to %d bytes, max %d", info.Size(), maxDiagnosticLogBytes)
	}
}

func TestLimitedWriterCapsWrites(t *testing.T) {
	var dst bytes.Buffer
	writer := newLimitedWriter(&dst)
	input := []byte(strings.Repeat("x", int(maxDiagnosticLogBytes)+100))
	n, err := writer.Write(input)
	if err != nil || n != len(input) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(input))
	}
	if int64(dst.Len()) != maxDiagnosticLogBytes {
		t.Fatalf("limited writer stored %d bytes, want %d", dst.Len(), maxDiagnosticLogBytes)
	}
}

func TestHostDiagnosticsNeverCreatesProviderStderrFile(t *testing.T) {
	dir := t.TempDir()
	events, err := os.OpenFile(filepath.Join(dir, "host.events.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := &hostDiagnostics{events: events, remaining: maxDiagnosticLogBytes}
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
	events, err := os.OpenFile(filepath.Join(dir, "host.events.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	const sentinel = "SECRET_PROVIDER_STDERR_SENTINEL"
	cfg := Config{
		SessionID: "diagnostic-provider", DataDir: t.TempDir(), Workdir: t.TempDir(),
		Env: append(os.Environ(),
			"AO_CHAT_HOST_PROVIDER_HELPER=1",
			"AO_CHAT_HOST_DIAGNOSTIC_STDERR_SENTINEL="+sentinel),
		Argv:        []string{os.Args[0], "-test.run=TestProviderHelper"},
		Diagnostics: &HostDiagnostics{Events: events},
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
	if err := events.Close(); err != nil {
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

func TestSpawnDetachedInvalidDiagnosticsFallsBack(t *testing.T) {
	dir := t.TempDir()
	closed, err := os.Create(filepath.Join(dir, "closed.events.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		SessionID: "diagnostic-fallback", DataDir: t.TempDir(), Workdir: t.TempDir(),
		Env:         append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1"),
		Argv:        []string{os.Args[0], "-test.run=TestProviderHelper"},
		Diagnostics: &HostDiagnostics{Events: closed},
	}
	transport, err := ConnectOrStart(context.Background(), cfg)
	if err != nil {
		t.Fatalf("invalid diagnostics blocked host startup: %v", err)
	}
	t.Cleanup(func() { _ = Shutdown(context.Background(), cfg.DataDir, cfg.SessionID) })
	pid := requestProviderPID(t, transport, 1, "pid")
	if pid <= 0 {
		t.Fatalf("provider PID = %d, want a running fake provider", pid)
	}
	if err := Shutdown(context.Background(), cfg.DataDir, cfg.SessionID); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil {
		t.Fatal(readErr)
	} else if len(entries) != 1 {
		t.Fatalf("invalid diagnostics created extra files: %v", entries)
	}
}

func TestRunReportsProviderErrorWhenDiagnosticsInvalid(t *testing.T) {
	t.Setenv(hostEventsFDEnv, "999999")
	err := Run(context.Background(), Config{
		SessionID: "diagnostic-provider-error", DataDir: t.TempDir(), Workdir: t.TempDir(),
		Argv: []string{"/definitely/missing-provider-binary"},
	})
	if err == nil {
		t.Fatal("Run accepted a missing provider binary")
	}
	if strings.Contains(err.Error(), "diagnostics") {
		t.Fatalf("diagnostic failure replaced the provider error: %v", err)
	}
}
