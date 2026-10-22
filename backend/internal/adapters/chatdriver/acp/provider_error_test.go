package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestNormalizeACPErrorEvidence(t *testing.T) {
	t.Run("auth code", func(t *testing.T) {
		evidence := acpEvidence(t, normalizeACPError("ACP session/prompt", acpsdk.NewAuthRequired(nil)))
		if evidence.Class != domain.ProviderErrorAuthRequired ||
			evidence.Protocol != "acp" || evidence.Operation != "session/prompt" ||
			evidence.Code == nil || *evidence.Code != -32000 {
			t.Fatalf("auth evidence = %+v", evidence)
		}
	})

	t.Run("internal request error keeps only shape", func(t *testing.T) {
		err := normalizeACPError("ACP session/prompt", &acpsdk.RequestError{
			Code:    -32603,
			Message: "Internal error",
			Data: map[string]any{
				"error":                          "Rate limit exceeded. Please try again later.",
				"eventId":                        "not-the-real-event-key",
				persistenthost.ACPEventIDMetaKey: "terminal-event-1",
				"nested":                         map[string]any{"secret": "nested-secret"},
				"array":                          []any{"array-secret"},
			},
		})
		evidence := acpEvidence(t, err)
		if evidence.Class != domain.ProviderErrorProviderInternal ||
			evidence.Protocol != "acp" || evidence.Operation != "session/prompt" ||
			evidence.Code == nil || *evidence.Code != -32603 {
			t.Fatalf("internal evidence = %+v", evidence)
		}
		if evidence.DataShape["error"] != "string" ||
			evidence.DataShape[persistenthost.ACPEventIDMetaKey] != "string" ||
			evidence.DataShape["eventId"] != "string" {
			t.Fatalf("allowlisted shape = %#v", evidence.DataShape)
		}
		if _, ok := evidence.DataShape["nested"]; ok {
			t.Fatalf("unknown nested field entered shape: %#v", evidence.DataShape)
		}
		if _, ok := evidence.DataShape["array"]; ok {
			t.Fatalf("unknown array field entered shape: %#v", evidence.DataShape)
		}
		if evidence.EventID != "terminal-event-1" {
			t.Fatalf("event ID = %q, want the relay's safe event ID", evidence.EventID)
		}
		encoded, marshalErr := json.Marshal(evidence)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		for _, secret := range []string{"Rate limit exceeded", "nested-secret", "array-secret", "not-the-real-event-key"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("evidence persisted %q: %s", secret, encoded)
			}
		}
		if evidence.Class == "rate_limited" || evidence.Class == "transient" {
			t.Fatalf("prose produced undemonstrated class %q", evidence.Class)
		}
		if !strings.Contains(err.Error(), "Rate limit exceeded") {
			t.Fatalf("visible error lost existing provider prose: %v", err)
		}
	})

	t.Run("nested, array, and opaque values are types only", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			value any
			shape string
			leak  string
		}{
			{"nested object", map[string]any{"message": "nested-secret"}, "object", "nested-secret"},
			{"array", []any{"array-secret"}, "array", "array-secret"},
			{"opaque blob", json.RawMessage(`{"secret":"blob-secret"}`), "unknown", "blob-secret"},
		} {
			t.Run(test.name, func(t *testing.T) {
				evidence := acpEvidence(t, normalizeACPError("ACP session/prompt", &acpsdk.RequestError{
					Code: -32603,
					Data: map[string]any{"error": test.value},
				}))
				if evidence.DataShape["error"] != test.shape {
					t.Fatalf("shape = %#v, want %s", evidence.DataShape, test.shape)
				}
				encoded, _ := json.Marshal(evidence)
				if strings.Contains(string(encoded), test.leak) {
					t.Fatalf("opaque value persisted: %s", encoded)
				}
			})
		}
	})

	t.Run("invalid event ID is omitted", func(t *testing.T) {
		for _, eventID := range []string{"line\nbreak", "contains space", strings.Repeat("x", 257)} {
			evidence := acpEvidence(t, normalizeACPError("ACP session/prompt", &acpsdk.RequestError{
				Code: -32603,
				Data: map[string]any{persistenthost.ACPEventIDMetaKey: eventID},
			}))
			if evidence.EventID != "" {
				t.Fatalf("unsafe event ID %q survived as %q", eventID, evidence.EventID)
			}
			if evidence.DataShape[persistenthost.ACPEventIDMetaKey] != "string" {
				t.Fatalf("event ID type shape = %#v", evidence.DataShape)
			}
		}
	})

	t.Run("plain error is unknown", func(t *testing.T) {
		evidence := acpEvidence(t, normalizeACPError("ACP session/prompt", errors.New("connection closed")))
		if evidence.Class != domain.ProviderErrorUnknown || evidence.Code != nil || len(evidence.DataShape) != 0 {
			t.Fatalf("plain error evidence = %+v", evidence)
		}
	})
}

func TestSyntheticOpenCodeACPInternalRateLimitIsNotAClass(t *testing.T) {
	// Synthetic representative case for the reported ACP -32603 frame. It is not
	// forensic evidence of the original incident: the original frame is not in the
	// repository. The assertion is deliberately only about structure, never text.
	for _, test := range []struct {
		name    string
		harness domain.AgentHarness
	}{
		{name: "OpenCode", harness: domain.HarnessOpenCode},
		{name: "OpenCode v2", harness: domain.HarnessOpenCodeV2},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := &fakeAgent{promptErr: &acpsdk.RequestError{
				Code:    -32603,
				Message: "Internal error",
				Data: map[string]any{
					"error": "Rate limit exceeded. Please try again later.",
				},
			}}
			driver := New(Config{
				Harness:      test.harness,
				Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true},
				Launch:       func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			driver.useTestProcess(fakeSpawn(agent))

			opened, err := driver.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer opened.Close()
			_ = nextEvent(t, opened.Events()) // controller.ready

			ref, err := opened.SendTurn(context.Background(), ports.ChatUserMessage{Text: "hello"})
			if err != nil {
				t.Fatalf("SendTurn: %v", err)
			}
			if err := opened.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
				t.Fatalf("StartDeferredTurn: %v", err)
			}

			var completed ports.ChatEvent
			for {
				event := nextEvent(t, opened.Events())
				if event.Kind == ports.ChatEventTurnCompleted {
					completed = event
					break
				}
			}
			if completed.TurnState != domain.TurnStateFailed || completed.Err == nil {
				t.Fatalf("completion = %#v, want failed with provider error", completed)
			}
			evidence, ok := ports.ProviderErrorEvidenceOf(completed.Err)
			if !ok {
				t.Fatalf("completion has no structured evidence: %v", completed.Err)
			}
			if evidence.Class != domain.ProviderErrorProviderInternal ||
				evidence.Protocol != "acp" || evidence.Operation != "session/prompt" ||
				evidence.Code == nil || *evidence.Code != -32603 {
				t.Fatalf("evidence = %+v", evidence)
			}
			if evidence.DataShape["error"] != "string" {
				t.Fatalf("data shape = %#v, want error:string", evidence.DataShape)
			}
			if evidence.Class == "rate_limited" || evidence.Class == "transient" {
				t.Fatalf("synthetic rate-limit prose produced %q", evidence.Class)
			}
			if errors.Is(completed.Err, ports.ErrChatAuthRequired) {
				t.Fatalf("internal error was classified as auth: %v", completed.Err)
			}
			encoded, _ := json.Marshal(evidence)
			if strings.Contains(string(encoded), "Rate limit exceeded") {
				t.Fatalf("sensitive data.error text entered evidence: %s", encoded)
			}
			if !strings.Contains(completed.Err.Error(), "Rate limit exceeded") {
				t.Fatalf("visible error lost existing prose: %v", completed.Err)
			}
		})
	}
}

func TestACPStartDeferredNotStartedIsNotDelivered(t *testing.T) {
	conv := &conversation{closed: true}
	err := conv.StartDeferredTurn("missing")
	if !errors.Is(err, ports.ErrDeferredTurnNotStarted) {
		t.Fatalf("err = %v, want ErrDeferredTurnNotStarted", err)
	}
	evidence, ok := ports.ProviderErrorEvidenceOf(err)
	if !ok || evidence.Class != domain.ProviderErrorNotDelivered ||
		evidence.Protocol != "acp" || evidence.Operation != "session/prompt" {
		t.Fatalf("evidence = (%+v, %v), want acp not_delivered", evidence, ok)
	}
}

func acpEvidence(t *testing.T, err error) domain.ProviderErrorEvidence {
	t.Helper()
	evidence, ok := ports.ProviderErrorEvidenceOf(err)
	if !ok {
		t.Fatalf("error has no provider evidence: %v", err)
	}
	return evidence
}
