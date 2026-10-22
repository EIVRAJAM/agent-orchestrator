package opencodeacp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/opencode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestOpenCodeACPProviderHelper is the out-of-process ACP agent used by the
// synthetic evidence test. It is not a real OpenCode fixture; it exists only to
// exercise the adapter's shared ACP transport and error projection.
func TestOpenCodeACPProviderHelper(t *testing.T) {
	if os.Getenv("AO_TEST_OPENCODE_V1_ACP_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		switch request.Method {
		case "initialize":
			_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":{}}},"authMethods":[],"agentInfo":{"name":"OpenCode","version":"1.18.33"}}}`+"\n", request.ID)
		case "session/new":
			_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"opencode-v1-provider","configOptions":[]}}`+"\n", request.ID)
		case "session/prompt":
			_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"Internal error","data":{"error":"Rate limit exceeded. Please try again later."}}}`+"\n", request.ID)
		default:
			_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", request.ID)
		}
	}
	os.Exit(0)
}

func TestOpenCodeACPInternalErrorEvidence(t *testing.T) {
	writeOpenCodeV1ACPExecutable(t)
	driver := New(opencode.New(), nil)
	if driver.Harness() != domain.HarnessOpenCode {
		t.Fatalf("harness = %q", driver.Harness())
	}
	conversation, err := driver.Start(context.Background(), ports.ChatStartConfig{
		SessionID: "v1-evidence", DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = conversation.(ports.ChatProviderTerminator).Terminate() }()
	awaitOpenCodeV1ACPReady(t, conversation.Events())

	ref, err := conversation.SendTurn(context.Background(), ports.ChatUserMessage{Text: "hello"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if err := conversation.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	var completed ports.ChatEvent
	for {
		event := nextOpenCodeV1ACPEvent(t, conversation.Events())
		if event.Kind == ports.ChatEventTurnCompleted {
			completed = event
			break
		}
	}
	if completed.TurnState != domain.TurnStateFailed || completed.Err == nil {
		t.Fatalf("completion = %#v, want failed with provider error", completed)
	}
	evidence, ok := ports.ProviderErrorEvidenceOf(completed.Err)
	if !ok || evidence.Class != domain.ProviderErrorProviderInternal ||
		evidence.Protocol != "acp" || evidence.Operation != "session/prompt" ||
		evidence.Code == nil || *evidence.Code != -32603 ||
		evidence.DataShape["error"] != "string" {
		t.Fatalf("evidence = (%+v, %v)", evidence, ok)
	}
	encoded, _ := json.Marshal(evidence)
	if strings.Contains(string(encoded), "Rate limit exceeded") {
		t.Fatalf("sensitive data.error entered v1 evidence: %s", encoded)
	}
	if evidence.Class == "rate_limited" || evidence.Class == "transient" {
		t.Fatalf("v1 prose produced %q", evidence.Class)
	}
}

func writeOpenCodeV1ACPExecutable(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "opencode")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf '%s\n' "1.18.33"
  exit 0
fi
exec "$AO_TEST_OPENCODE_V1_ACP_BINARY" -test.run=TestOpenCodeACPProviderHelper
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("AO_TEST_OPENCODE_V1_ACP_BINARY", os.Args[0])
	t.Setenv("AO_TEST_OPENCODE_V1_ACP_HELPER", "1")
	return binary
}

func awaitOpenCodeV1ACPReady(t *testing.T, events <-chan ports.ChatEvent) {
	t.Helper()
	for {
		event := nextOpenCodeV1ACPEvent(t, events)
		if event.Kind == ports.ChatEventControllerState && event.ControllerState == ports.ChatControllerReady {
			return
		}
	}
}

func nextOpenCodeV1ACPEvent(t *testing.T, events <-chan ports.ChatEvent) ports.ChatEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ACP event")
		return ports.ChatEvent{}
	}
}
