package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// queueResumeCapture records the resume request body the CLI sent, guarded so the
// httptest handler goroutine and the test do not race.
type queueResumeCapture struct {
	mu   sync.Mutex
	body map[string]string
}

func (c *queueResumeCapture) set(body map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = body
}

func (c *queueResumeCapture) get() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

// queueResumeServer routes the observable snapshot read and the resume POST, so a
// test can assert the CLI derives its compare-and-set preconditions from the
// state it read first.
func queueResumeServer(t *testing.T, queueJSON string, resumeStatus int, resumeBody string) (*httptest.Server, *queueResumeCapture) {
	t.Helper()
	capture := &queueResumeCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/demo-1/conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, queueJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/demo-1/conversation/queue/resume":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			capture.set(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resumeStatus)
			_, _ = io.WriteString(w, resumeBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, capture
}

const heldQueueJSON = `{"queue":{"queueState":"held_after_terminal_failure","pendingCount":1,"headTurnId":"turn-9","controllerGeneration":"gen-1","controllerAvailable":true,"resumeSafety":"resumed"}}`

// TestSessionResumeQueueDerivesHeadAndGeneration proves the CLI reads the
// observable state first and echoes that state's head and generation as the
// compare-and-set preconditions.
func TestSessionResumeQueueDerivesHeadAndGeneration(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := queueResumeServer(t, heldQueueJSON, http.StatusAccepted, `{"outcome":"resumed","turnId":"turn-9"}`)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "resume-queue", "demo-1")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	body := capture.get()
	if body["expectedTurnId"] != "turn-9" || body["expectedControllerGeneration"] != "gen-1" {
		t.Fatalf("body = %v, want the observed head and generation", body)
	}
	if !strings.Contains(out, "turn-9") {
		t.Fatalf("output = %q, want the released turn id", out)
	}
}

// TestSessionResumeQueueNoPendingWork proves the CLI reports the observable state
// instead of sending a request the server would reject for a missing turn id.
func TestSessionResumeQueueNoPendingWork(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := queueResumeServer(t, `{"queue":{"queueState":"idle","pendingCount":0,"controllerAvailable":true,"resumeSafety":"no_pending_work"}}`, http.StatusOK, `{}`)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "resume-queue", "demo-1")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if capture.get() != nil {
		t.Fatalf("body = %v, want no resume request", capture.get())
	}
	if !strings.Contains(out, "no queued message to resume") {
		t.Fatalf("output = %q, want the no-work message", out)
	}
}

// TestSessionResumeQueueSurfacesStaleCAS proves a stale compare-and-set is
// surfaced as a daemon error (exit 1), not swallowed.
func TestSessionResumeQueueSurfacesStaleCAS(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, _ := queueResumeServer(t, heldQueueJSON, http.StatusConflict,
		`{"error":"conflict","code":"CHAT_QUEUE_STALE_STATE","message":"that turn is no longer the queue head; re-read the conversation and retry","requestId":"req-1"}`)
	writeRunFileFor(t, cfg, srv)

	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "resume-queue", "demo-1")
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("err = %v, want a daemon failure (exit 1)", err)
	}
	if !strings.Contains(err.Error(), "CHAT_QUEUE_STALE_STATE") || !strings.Contains(err.Error(), "req-1") {
		t.Fatalf("err = %v, want the stable code and request id preserved", err)
	}
}

// TestSessionResumeQueueSurfacesDaemonError proves a server fault is a daemon
// failure (exit 1).
func TestSessionResumeQueueSurfacesDaemonError(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, _ := queueResumeServer(t, heldQueueJSON, http.StatusInternalServerError,
		`{"error":"internal_error","code":"INTERNAL","message":"boom","requestId":"req-2"}`)
	writeRunFileFor(t, cfg, srv)

	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "resume-queue", "demo-1")
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("err = %v, want a daemon failure (exit 1)", err)
	}
}

// TestSessionResumeQueueRequiresSessionID proves misuse is a usage error (exit 2).
func TestSessionResumeQueueRequiresSessionID(t *testing.T) {
	setConfigEnv(t)
	_, _, err := executeCLI(t, Deps{}, "session", "resume-queue")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("err = %v, want a usage error (exit 2)", err)
	}
}

// TestSessionResumeQueueRequiresGeneration proves a snapshot without a
// controller generation and no --generation flag is a usage error (exit 2),
// never an implicit "use the current generation".
func TestSessionResumeQueueRequiresGeneration(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := queueResumeServer(t,
		`{"queue":{"queueState":"held_after_terminal_failure","pendingCount":1,"headTurnId":"turn-9","controllerAvailable":true,"resumeSafety":"resumed"}}`,
		http.StatusAccepted, `{"outcome":"resumed","turnId":"turn-9"}`)
	writeRunFileFor(t, cfg, srv)

	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "resume-queue", "demo-1")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("err = %v, want a usage error (exit 2)", err)
	}
	if capture.get() != nil {
		t.Fatalf("body = %v, want no resume request", capture.get())
	}
}

// TestSessionResumeQueueFlagsOverrideObserved proves the manual flags respect the
// same mandatory contract and are sent explicitly.
func TestSessionResumeQueueFlagsOverrideObserved(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := queueResumeServer(t, heldQueueJSON, http.StatusAccepted, `{"outcome":"resumed","turnId":"turn-x"}`)
	writeRunFileFor(t, cfg, srv)

	_, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "resume-queue", "demo-1", "--turn", "turn-x", "--generation", "gen-x")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	body := capture.get()
	if body["expectedTurnId"] != "turn-x" || body["expectedControllerGeneration"] != "gen-x" {
		t.Fatalf("body = %v, want the explicit overrides", body)
	}
}

// TestSessionResumeQueueEmptyGenerationFlagIsUsageError proves an explicitly
// empty generation is refused before the request is sent.
func TestSessionResumeQueueEmptyGenerationFlagIsUsageError(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := queueResumeServer(t, heldQueueJSON, http.StatusAccepted, `{"outcome":"resumed","turnId":"turn-9"}`)
	writeRunFileFor(t, cfg, srv)

	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "resume-queue", "demo-1", "--generation", "")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("err = %v, want a usage error (exit 2)", err)
	}
	if capture.get() != nil {
		t.Fatalf("body = %v, want no resume request", capture.get())
	}
}

func TestSessionQueuePrintsDerivedState(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/v1/sessions/demo-1/conversation" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"queue":{"queueState":"held_after_terminal_failure","pendingCount":2,"headTurnId":"turn-9","controllerGeneration":"gen-1","controllerAvailable":true,"resumeSafety":"resumed"}}`)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "queue", "demo-1")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if !strings.Contains(out, "held_after_terminal_failure") || !strings.Contains(out, "turn-9") {
		t.Fatalf("output = %q, want the derived state and head", out)
	}
	if !strings.Contains(out, "ao session resume-queue demo-1 --turn turn-9 --generation gen-1") {
		t.Fatalf("output missing the resume hint: %q", out)
	}
}

// TestSessionQueuePrintsHeldAfterManualResume proves the CLI surfaces the
// one-shot retention distinctly and offers the explicit resume.
func TestSessionQueuePrintsHeldAfterManualResume(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/v1/sessions/demo-1/conversation" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"queue":{"queueState":"held_after_manual_resume","pendingCount":2,"headTurnId":"turn-10","controllerGeneration":"gen-2","controllerAvailable":true,"resumeSafety":"resumed"}}`)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"session", "queue", "demo-1")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if !strings.Contains(out, "held_after_manual_resume") || !strings.Contains(out, "turn-10") {
		t.Fatalf("output = %q, want the manual-resume state and head", out)
	}
	if !strings.Contains(out, "ao session resume-queue demo-1 --turn turn-10 --generation gen-2") {
		t.Fatalf("output missing the resume hint: %q", out)
	}
}
