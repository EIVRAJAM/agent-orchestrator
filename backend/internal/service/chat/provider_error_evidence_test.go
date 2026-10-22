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
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// A classified provider failure changes only the diagnostic payload. The
// visible turn message and the #4861 retention policy are exactly the same as
// before the evidence was attached.
func TestProviderErrorEvidencePreservesFailureRetention(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "root", ClientMessageID: "evidence-root", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("send root: %v", err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateRunning
	})

	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "automation", ClientMessageID: "evidence-automation", Origin: domain.MessageOriginAutomation,
	}); err != nil {
		t.Fatalf("queue automation: %v", err)
	}

	const visible = "ACP session/prompt: Internal error: Rate limit exceeded. Please try again later."
	code := -32603
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		ProviderEventID: "terminal-event-1", TurnState: domain.TurnStateFailed,
		Err: ports.WithProviderError(errors.New(visible), domain.ProviderErrorEvidence{
			Class: domain.ProviderErrorProviderInternal, Protocol: "acp", Operation: "session/prompt",
			Code: &code,
			DataShape: map[string]string{
				"error":   "string",
				"eventId": "string",
			},
			EventID: "terminal-event-1",
		}),
	})

	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateFailed
	})
	if got := snapshot.Turns[0].ErrorMessage; got != visible {
		t.Fatalf("visible turn error = %q, want unchanged %q", got, visible)
	}
	assertTurnStaysQueued(t, h, "automation")
	if got := h.conv.sentTexts(); len(got) != 1 || got[0] != "root" {
		t.Fatalf("provider received %v, want only root", got)
	}

	events, err := h.st.ProviderEventsSince(ctx, h.ctrl.ConversationID(), 0, 100)
	if err != nil {
		t.Fatalf("read provider archive: %v", err)
	}
	var archived map[string]any
	var archivedPayload string
	for _, event := range events {
		if event.ProviderEventID != "terminal-event-1" {
			continue
		}
		archivedPayload = event.PayloadJson
		if err := json.Unmarshal([]byte(event.PayloadJson), &archived); err != nil {
			t.Fatalf("decode archived event: %v", err)
		}
		break
	}
	if archived == nil {
		t.Fatal("terminal provider event was not archived")
	}
	providerError, ok := archived["providerError"].(map[string]any)
	if !ok {
		t.Fatalf("archived providerError = %#v", archived)
	}
	if providerError["class"] != string(domain.ProviderErrorProviderInternal) ||
		providerError["protocol"] != "acp" || providerError["operation"] != "session/prompt" ||
		providerError["eventId"] != "terminal-event-1" {
		t.Fatalf("archived providerError = %#v", providerError)
	}
	if providerError["code"] != float64(-32603) {
		t.Fatalf("archived code = %#v", providerError["code"])
	}
	shape, ok := providerError["dataShape"].(map[string]any)
	if !ok || shape["error"] != "string" || shape["eventId"] != "string" {
		t.Fatalf("archived shape = %#v", providerError["dataShape"])
	}
	if archived["error"] != string(domain.ProviderErrorProviderInternal) {
		t.Fatalf("raw archive error = %#v, want class only", archived["error"])
	}
	if strings.Contains(strings.ToLower(archivedPayload), "rate limit exceeded") {
		t.Fatal("visible provider prose leaked into the provider-event archive")
	}
	if providerError["message"] != nil || providerError["data"] != nil {
		t.Fatalf("providerError carried a message/data value: %#v", providerError)
	}
}

// SendTurn failures have no provider event, so they use the same archive with a
// no-op projection. The durable turn remains failed and the queue is untouched.
func TestDispatchErrorEvidenceIsDurableWithoutChangingTurn(t *testing.T) {
	h := newHarness(t)
	cause := fmt.Errorf("%w: Rate limit exceeded. Please try again later.", ports.ErrTurnNotDelivered)
	visible := cause.Error()
	h.conv.sendErr = ports.WithProviderError(
		cause,
		domain.ProviderErrorEvidence{
			Class: domain.ProviderErrorNotDelivered, Protocol: "codex", Operation: "turn/start",
		},
	)

	_, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "root", ClientMessageID: "dispatch-evidence", Origin: domain.MessageOriginHuman,
	})
	if err == nil {
		t.Fatal("SendTurn failure was reported as success")
	}
	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateFailed
	})
	if got := snapshot.Turns[0].ErrorMessage; got != visible {
		t.Fatalf("visible turn error = %q, want unchanged %q", got, visible)
	}

	events, readErr := h.st.ProviderEventsSince(context.Background(), h.ctrl.ConversationID(), 0, 100)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var archived map[string]any
	for _, event := range events {
		if event.Method != "provider.error" {
			continue
		}
		if err := json.Unmarshal([]byte(event.PayloadJson), &archived); err != nil {
			t.Fatal(err)
		}
		break
	}
	if archived == nil {
		t.Fatal("dispatch error was not archived")
	}
	providerError, ok := archived["providerError"].(map[string]any)
	if !ok || providerError["class"] != string(domain.ProviderErrorNotDelivered) ||
		providerError["protocol"] != "codex" || providerError["operation"] != "turn/start" {
		t.Fatalf("dispatch evidence = %#v", archived)
	}
	if archived["error"] != string(domain.ProviderErrorNotDelivered) {
		t.Fatalf("dispatch archive error = %#v, want class only", archived["error"])
	}
	encoded, _ := json.Marshal(archived)
	if strings.Contains(strings.ToLower(string(encoded)), "rate limit exceeded") {
		t.Fatalf("dispatch evidence leaked provider prose: %s", encoded)
	}
}

// An ambiguous SendTurn error keeps the existing running/unbound outcome even
// when it carries diagnostic evidence. This is the controller-side guarantee
// that classification never became a retry decision.
func TestDispatchUncertainEvidenceKeepsRunningOutcome(t *testing.T) {
	h := newHarness(t)
	h.conv.sendErr = ports.WithProviderError(errors.New("ambiguous transport"), domain.ProviderErrorEvidence{
		Class: domain.ProviderErrorUncertain, Protocol: "codex", Operation: "turn/start",
	})

	_, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "uncertain", ClientMessageID: "dispatch-uncertain", Origin: domain.MessageOriginHuman,
	})
	if err == nil {
		t.Fatal("ambiguous SendTurn failure was reported as success")
	}
	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateRunning &&
			s.Turns[0].ProviderTurnID == ""
	})
	if snapshot.Turns[0].State != domain.TurnStateRunning {
		t.Fatalf("uncertain turn = %+v, want running", snapshot.Turns[0])
	}

	events, readErr := h.st.ProviderEventsSince(context.Background(), h.ctrl.ConversationID(), 0, 100)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var class string
	for _, event := range events {
		if event.Method != "provider.error" {
			continue
		}
		var archived map[string]any
		if err := json.Unmarshal([]byte(event.PayloadJson), &archived); err != nil {
			t.Fatal(err)
		}
		if providerError, ok := archived["providerError"].(map[string]any); ok {
			class, _ = providerError["class"].(string)
		}
	}
	if class != string(domain.ProviderErrorUncertain) {
		t.Fatalf("archived uncertain class = %q", class)
	}
}
