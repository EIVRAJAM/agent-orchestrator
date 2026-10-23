package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func hostileProviderEvidence() domain.ProviderErrorEvidence {
	code := -32603
	return domain.ProviderErrorEvidence{
		Class:     "rate_limited",
		Protocol:  "http",
		Harness:   "invented-harness",
		Operation: "session/steal",
		Code:      &code,
		DataShape: map[string]string{
			"error":                "SECRET_MESSAGE",
			"eventId":              "SECRET_TOKEN",
			"ao.persistentEventId": "string",
			"unknown":              "string",
		},
		EventID: "provider-supplied-ascii-id",
	}
}

func findProviderEvent(t *testing.T, events []gen.ConversationProviderEvent, method string) map[string]any {
	t.Helper()
	for _, event := range events {
		if event.Method != method {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(event.PayloadJson), &payload); err != nil {
			t.Fatalf("decode %s archive: %v", method, err)
		}
		return payload
	}
	t.Fatalf("%s event not found in provider archive", method)
	return nil
}

func assertNoHostileEvidence(t *testing.T, payload map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET_MESSAGE", "SECRET_TOKEN", "SECRET_VISIBLE_MESSAGE", "invented-harness", "provider-supplied-ascii-id", "session/steal"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("hostile value %q reached payload: %s", secret, encoded)
		}
	}
}

func TestTerminalProviderEvidenceIsSanitizedAtTheArchiveBoundary(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "root", ClientMessageID: "hostile-terminal", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("send root: %v", err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateRunning
	})
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "automation", ClientMessageID: "hostile-queue", Origin: domain.MessageOriginAutomation,
	}); err != nil {
		t.Fatalf("queue automation: %v", err)
	}

	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		ProviderEventID: "hostile-terminal-event", TurnState: domain.TurnStateFailed,
		Err: ports.WithProviderError(errors.New("SECRET_VISIBLE_MESSAGE"), hostileProviderEvidence()),
	})
	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateFailed
	})
	if snapshot.Turns[0].ErrorMessage != "SECRET_VISIBLE_MESSAGE" {
		t.Fatalf("visible turn error changed: %q", snapshot.Turns[0].ErrorMessage)
	}
	assertTurnStaysQueued(t, h, "automation")

	events, err := h.st.ProviderEventsSince(ctx, h.ctrl.ConversationID(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	payload := findProviderEvent(t, events, "turn.completed")
	providerError, ok := payload["providerError"].(map[string]any)
	if !ok {
		t.Fatalf("providerError = %#v", payload)
	}
	if providerError["class"] != "unknown" {
		t.Fatalf("class = %#v, want unknown", providerError["class"])
	}
	for _, field := range []string{"protocol", "harness", "operation", "eventId"} {
		if _, present := providerError[field]; present {
			t.Fatalf("hostile %s survived: %#v", field, providerError)
		}
	}
	shape, ok := providerError["dataShape"].(map[string]any)
	if !ok || len(shape) != 1 || shape["ao.persistentEventId"] != "string" {
		t.Fatalf("shape = %#v, want only the safe key", providerError["dataShape"])
	}
	if payload["error"] != "unknown" {
		t.Fatalf("raw archive error = %#v, want class only", payload["error"])
	}
	assertNoHostileEvidence(t, payload)
}

func TestArchiveDropsKnownHarnessWithoutAnACPAdapter(t *testing.T) {
	h := newHarness(t)
	h.conv.sendErr = ports.WithProviderError(
		fmt.Errorf("%w: known non-ACP harness", ports.ErrTurnNotDelivered),
		domain.ProviderErrorEvidence{
			Class: domain.ProviderErrorProviderInternal, Protocol: "acp",
			Harness: string(domain.HarnessUnreal), Operation: "session/prompt",
		},
	)
	if _, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "root", ClientMessageID: "known-non-acp-harness", Origin: domain.MessageOriginHuman,
	}); err == nil {
		t.Fatal("definitive SendTurn failure was reported as success")
	}
	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateFailed
	})
	if snapshot.Turns[0].State != domain.TurnStateFailed {
		t.Fatalf("turn state = %q, want failed", snapshot.Turns[0].State)
	}

	events, err := h.st.ProviderEventsSince(context.Background(), h.ctrl.ConversationID(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	payload := findProviderEvent(t, events, "provider.error")
	providerError, ok := payload["providerError"].(map[string]any)
	if !ok || providerError["class"] != "provider_internal" || providerError["protocol"] != "acp" {
		t.Fatalf("providerError = %#v", payload)
	}
	if _, present := providerError["harness"]; present {
		t.Fatalf("non-ACP harness survived: %#v", providerError)
	}
	encoded, _ := json.Marshal(payload)
	if strings.Contains(string(encoded), string(domain.HarnessUnreal)) {
		t.Fatalf("unreal-agent appeared in payload: %s", encoded)
	}
}

func TestDispatchProviderEvidenceIsSanitizedAtTheArchiveBoundary(t *testing.T) {
	h := newHarness(t)
	h.conv.sendErr = ports.WithProviderError(
		fmt.Errorf("%w: SECRET_VISIBLE_MESSAGE", ports.ErrTurnNotDelivered),
		hostileProviderEvidence(),
	)
	if _, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "root", ClientMessageID: "hostile-dispatch", Origin: domain.MessageOriginHuman,
	}); err == nil {
		t.Fatal("definitive SendTurn failure was reported as success")
	}
	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateFailed
	})
	turnID := snapshot.Turns[0].ID

	events, err := h.st.ProviderEventsSince(context.Background(), h.ctrl.ConversationID(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	payload := findProviderEvent(t, events, "provider.error")
	if payload["turnId"] != turnID {
		t.Fatalf("turnId = %#v, want %q", payload["turnId"], turnID)
	}
	if _, present := payload["providerTurnId"]; present {
		t.Fatalf("diagnostic row contains providerTurnId: %#v", payload)
	}
	providerError, ok := payload["providerError"].(map[string]any)
	if !ok || providerError["class"] != "unknown" {
		t.Fatalf("providerError = %#v", payload)
	}
	for _, field := range []string{"protocol", "harness", "operation", "eventId"} {
		if _, present := providerError[field]; present {
			t.Fatalf("hostile %s survived: %#v", field, providerError)
		}
	}
	if providerError["code"] != float64(-32603) {
		t.Fatalf("structurally safe code = %#v", providerError["code"])
	}
	shape, ok := providerError["dataShape"].(map[string]any)
	if !ok || len(shape) != 1 || shape["ao.persistentEventId"] != "string" {
		t.Fatalf("shape = %#v, want only the safe key", providerError["dataShape"])
	}
	assertNoHostileEvidence(t, payload)
}
