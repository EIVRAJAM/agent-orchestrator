package ports

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestSafeProviderEventIDOnlyAcceptsRelayShape(t *testing.T) {
	valid := "acp-host:" + strings.Repeat("a", 64) + ":1"
	if got, ok := SafeProviderEventID(valid); !ok || got != valid {
		t.Fatalf("valid relay id = (%q, %v)", got, ok)
	}
	for _, value := range []string{
		"",
		"provider-supplied-ascii-id",
		"acp-host:abc:1",
		"acp-host:" + strings.Repeat("g", 64) + ":1",
		"acp-host:" + strings.Repeat("a", 64) + ":x",
		"acp-host:" + strings.Repeat("a", 64) + ":1 extra",
		"acp-host:" + strings.Repeat("a", 64) + ":1\n",
		"acp-host:" + strings.Repeat("a", 64) + ":1 é",
		"acp-host:" + strings.Repeat("a", 64) + ":" + strings.Repeat("9", 21),
		strings.Repeat("x", 257),
	} {
		if got, ok := SafeProviderEventID(value); ok || got != "" {
			t.Fatalf("unsafe event id %q accepted as %q", value, got)
		}
	}
}

func TestSanitizeProviderErrorEvidenceClosesTheBoundary(t *testing.T) {
	validEventID := "acp-host:" + strings.Repeat("b", 64) + ":2"
	hostile := domain.ProviderErrorEvidence{
		Class:     "rate_limited",
		Protocol:  "http",
		Harness:   "invented-harness",
		Operation: "session/prompt",
		DataShape: map[string]string{
			"error":                "SECRET_MESSAGE",
			"eventId":              "SECRET_TOKEN",
			"ao.persistentEventId": "string",
			"unknown":              "string",
		},
		EventID: "provider-supplied-ascii-id",
	}
	got := SanitizeProviderErrorEvidence(hostile)
	if got.Class != domain.ProviderErrorUnknown {
		t.Fatalf("class = %q, want unknown", got.Class)
	}
	if got.Protocol != "" || got.Harness != "" || got.Operation != "" || got.EventID != "" {
		t.Fatalf("hostile identity survived: %+v", got)
	}
	if len(got.DataShape) != 1 || got.DataShape["ao.persistentEventId"] != "string" {
		t.Fatalf("hostile shape = %#v", got.DataShape)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET_MESSAGE", "SECRET_TOKEN", "invented-harness", "provider-supplied-ascii-id"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("hostile value %q survived: %s", secret, encoded)
		}
	}

	code := -32603
	allowed := domain.ProviderErrorEvidence{
		Class:     domain.ProviderErrorProviderInternal,
		Protocol:  "acp",
		Harness:   string(domain.HarnessOpenCodeV2),
		Operation: "session/prompt",
		Code:      &code,
		DataShape: map[string]string{
			"error":                "string",
			"eventId":              "array",
			"ao.persistentEventId": "object",
			"unknown":              "string",
		},
		EventID: validEventID,
	}
	got = SanitizeProviderErrorEvidence(allowed)
	if got.Class != domain.ProviderErrorProviderInternal || got.Protocol != "acp" ||
		got.Harness != "opencode-v2" || got.Operation != "session/prompt" ||
		got.Code == nil || *got.Code != -32603 || got.EventID != validEventID {
		t.Fatalf("allowed evidence = %+v", got)
	}
	wantShape := map[string]string{
		"error":                "string",
		"eventId":              "array",
		"ao.persistentEventId": "object",
	}
	if len(got.DataShape) != len(wantShape) {
		t.Fatalf("shape = %#v, want %#v", got.DataShape, wantShape)
	}
	for key, want := range wantShape {
		if got.DataShape[key] != want {
			t.Fatalf("shape[%q] = %q, want %q", key, got.DataShape[key], want)
		}
	}
}
