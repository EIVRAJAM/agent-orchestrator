package persistenthost

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
)

const (
	// maxDiagnosticLogBytes caps every diagnostic file opened by the test.
	maxDiagnosticLogBytes int64 = 64 << 10
	hostEventsFDEnv             = "AO_CHAT_HOST_EVENTS_FD"
)

// HostDiagnostics is an explicit, test-owned set of already-open files. The
// test opens these under its own t.TempDir(); the detached host receives the
// event descriptor through ExtraFiles and never receives a writable path.
type HostDiagnostics struct {
	Stdout *os.File
	Stderr *os.File
	Events *os.File
}

type hostDiagnostics struct {
	events    *os.File
	remaining int64
}

// validateHostDiagnostics only checks that handles are usable. Confinement does
// not come from this check; it comes from the test owning the handles and
// opening them under t.TempDir(). A closed handle simply disables diagnostics.
func validateHostDiagnostics(diagnostics *HostDiagnostics) error {
	if diagnostics == nil {
		return nil
	}
	for name, file := range map[string]*os.File{
		"host stdout": diagnostics.Stdout,
		"host stderr": diagnostics.Stderr,
		"host events": diagnostics.Events,
	} {
		if file == nil {
			continue
		}
		if _, err := file.Stat(); err != nil {
			return fmt.Errorf("%s diagnostic handle: %w", name, err)
		}
	}
	return nil
}

func openHostDiagnosticsFromFD() (*hostDiagnostics, error) {
	raw := os.Getenv(hostEventsFDEnv)
	if raw == "" {
		return nil, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd != 3 {
		return nil, fmt.Errorf("invalid chat host events fd %q", raw)
	}
	events := os.NewFile(uintptr(fd), "host.events.log")
	if events == nil {
		return nil, errors.New("invalid chat host events descriptor")
	}
	remaining := maxDiagnosticLogBytes
	if info, err := events.Stat(); err == nil && info.Size() < remaining {
		remaining -= info.Size()
	} else if err == nil {
		remaining = 0
	}
	return &hostDiagnostics{events: events, remaining: remaining}, nil
}

// Event writes a closed, caller-selected lifecycle name. It never accepts an
// error value, path, prompt, argument vector, or environment.
func (d *hostDiagnostics) Event(name string) {
	if d == nil || d.events == nil || d.remaining <= 0 {
		return
	}
	line := name + "\n"
	if int64(len(line)) > d.remaining {
		line = line[:d.remaining]
	}
	n, _ := io.WriteString(d.events, line)
	d.remaining -= int64(n)
}

func (d *hostDiagnostics) Close() error {
	if d == nil || d.events == nil {
		return nil
	}
	return d.events.Close()
}

// limitedWriter keeps the host's stdout/stderr bounded without changing the
// provider's ACP stdout pipe. Extra bytes are drained and discarded.
type limitedWriter struct {
	mu        sync.Mutex
	dst       io.Writer
	remaining int64
}

func newLimitedWriter(dst io.Writer) io.Writer {
	if dst == nil {
		return nil
	}
	remaining := maxDiagnosticLogBytes
	if file, ok := dst.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Size() < remaining {
			remaining -= info.Size()
		} else if err == nil {
			remaining = 0
		}
	}
	return &limitedWriter{dst: dst, remaining: remaining}
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(p)
	if w.remaining <= 0 {
		return original, nil
	}
	if int64(len(p)) > w.remaining {
		p = p[:w.remaining]
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	if err != nil {
		return n, err
	}
	return original, nil
}
