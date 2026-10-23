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

func TestSanitizeProviderErrorEvidenceClosesProtocolHarnessAllowlist(t *testing.T) {
	accepted := []struct {
		name     string
		protocol string
		harness  domain.AgentHarness
		op       string
	}{
		{"codex/codex", "codex", domain.HarnessCodex, "turn/start"},
		{"acp/claude-code", "acp", domain.HarnessClaudeCode, "session/prompt"},
		{"acp/opencode", "acp", domain.HarnessOpenCode, "session/prompt"},
		{"acp/opencode-v2", "acp", domain.HarnessOpenCodeV2, "session/prompt"},
		{"acp/droid", "acp", domain.HarnessDroid, "session/prompt"},
		{"acp/kimi", "acp", domain.HarnessKimi, "session/prompt"},
		{"acp/kimchi", "acp", domain.HarnessKimchi, "session/prompt"},
		{"acp/pi", "acp", domain.HarnessPi, "session/prompt"},
		{"acp/cursor", "acp", domain.HarnessCursor, "session/prompt"},
		{"acp/omp", "acp", domain.HarnessOMP, "session/prompt"},
		{"acp/qwen", "acp", domain.HarnessQwen, "session/prompt"},
		{"acp/deepseek-harness", "acp", domain.HarnessDeepSeek, "session/prompt"},
		{"acp/command-code", "acp", domain.HarnessCommandCode, "session/prompt"},
		{"acp/agy", "acp", domain.HarnessAgy, "session/prompt"},
	}
	for _, test := range accepted {
		t.Run(test.name, func(t *testing.T) {
			got := SanitizeProviderErrorEvidence(domain.ProviderErrorEvidence{
				Class: domain.ProviderErrorProviderInternal, Protocol: test.protocol,
				Harness: string(test.harness), Operation: test.op,
			})
			if got.Protocol != test.protocol || got.Harness != string(test.harness) || got.Operation != test.op {
				t.Fatalf("sanitized evidence = %+v, want protocol=%q harness=%q operation=%q",
					got, test.protocol, test.harness, test.op)
			}
		})
	}

	rejected := []struct {
		name     string
		protocol string
		harness  domain.AgentHarness
		op       string
	}{
		{"acp/codex", "acp", domain.HarnessCodex, "session/prompt"},
		{"acp/unreal", "acp", domain.HarnessUnreal, "session/prompt"},
		{"acp/gemini", "acp", domain.HarnessGemini, "session/prompt"},
		{"acp/aider", "acp", domain.HarnessAider, "session/prompt"},
		{"acp/invented", "acp", "invented-harness", "session/prompt"},
		{"codex/opencode", "codex", domain.HarnessOpenCode, "turn/start"},
		{"codex/opencode-v2", "codex", domain.HarnessOpenCodeV2, "turn/start"},
		{"codex/unreal", "codex", domain.HarnessUnreal, "turn/start"},
		{"unreal/unreal", "unreal", domain.HarnessUnreal, "session/prompt"},
	}
	for _, test := range rejected {
		t.Run(test.name, func(t *testing.T) {
			got := SanitizeProviderErrorEvidence(domain.ProviderErrorEvidence{
				Class: domain.ProviderErrorProviderInternal, Protocol: test.protocol,
				Harness: string(test.harness), Operation: test.op,
			})
			wantProtocol := ""
			if test.protocol == "codex" || test.protocol == "acp" {
				wantProtocol = test.protocol
			}
			if got.Protocol != wantProtocol {
				t.Fatalf("protocol = %q, want %q", got.Protocol, wantProtocol)
			}
			if got.Harness != "" {
				t.Fatalf("rejected harness %q survived", got.Harness)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), string(test.harness)) {
				t.Fatalf("rejected harness %q appeared in JSON: %s", test.harness, encoded)
			}
		})
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
