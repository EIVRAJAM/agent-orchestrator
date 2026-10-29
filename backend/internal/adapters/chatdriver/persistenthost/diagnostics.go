package persistenthost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DiagnosticsDirEnv opts a single test into persistent-host startup diagnostics.
// It is intentionally unset by default: production hosts keep their normal
// stdout/stderr behavior and create no diagnostic files.
//
// Only the detached host's stdout/stderr and sanitized lifecycle events are
// captured. Provider stderr is deliberately discarded: it is arbitrary provider
// output and may contain credentials or prompt material.
const DiagnosticsDirEnv = "AO_CHAT_HOST_DIAGNOSTICS_DIR"

type hostDiagnostics struct {
	events *os.File
}

func diagnosticsDirFromEnv() (string, bool, error) {
	raw := strings.TrimSpace(os.Getenv(DiagnosticsDirEnv))
	if raw == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(raw) {
		return "", false, errors.New("chat host diagnostics directory must be absolute")
	}
	return filepath.Clean(raw), true, nil
}

func openDiagnosticFile(dir, name string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create chat host diagnostics directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open chat host diagnostic %s: %w", name, err)
	}
	return file, nil
}

func openHostStreams() (stdout, stderr *os.File, closeFiles func(), err error) {
	dir, enabled, err := diagnosticsDirFromEnv()
	if err != nil || !enabled {
		return nil, nil, func() {}, err
	}
	stdout, err = openDiagnosticFile(dir, "host.stdout.log")
	if err != nil {
		return nil, nil, func() {}, err
	}
	stderr, err = openDiagnosticFile(dir, "host.stderr.log")
	if err != nil {
		_ = stdout.Close()
		return nil, nil, func() {}, err
	}
	return stdout, stderr, func() {
		_ = stdout.Close()
		_ = stderr.Close()
	}, nil
}

func openHostDiagnostics() (*hostDiagnostics, error) {
	dir, enabled, err := diagnosticsDirFromEnv()
	if err != nil || !enabled {
		return nil, err
	}
	events, err := openDiagnosticFile(dir, "host.events.log")
	if err != nil {
		return nil, err
	}
	return &hostDiagnostics{events: events}, nil
}

func (d *hostDiagnostics) Event(format string, args ...any) {
	if d == nil || d.events == nil {
		return
	}
	_, _ = fmt.Fprintf(d.events, format+"\n", args...)
}

func (d *hostDiagnostics) Close() error {
	if d == nil {
		return nil
	}
	return d.events.Close()
}
