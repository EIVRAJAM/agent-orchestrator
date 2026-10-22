package codexappserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/codexappserver/codexproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestClassifySendTurnError proves the adapter only claims a definitive outcome
// when it has one. A provider error response is a rejection; a connection
// refused before any write is not delivered. Everything else stays ambiguous so
// the controller leaves the turn running/uncertain.
func TestClassifySendTurnError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		delivered bool
		rejected  bool
		class     domain.ProviderErrorClass
	}{
		{"rpc rejection", fmt.Errorf("turn/start: %w", &rpcError{Code: -1, Message: "bad request"}), false, true, domain.ProviderErrorRejected},
		{"rpc rejection with rate-limit prose", fmt.Errorf("turn/start: %w", &rpcError{Code: -32603, Message: "Rate limit exceeded. Please try again later."}), false, true, domain.ProviderErrorRejected},
		{"closed before write", fmt.Errorf("turn/start: %w: %w", ErrConnClosed, errConnClosedBeforeWrite), true, false, domain.ProviderErrorNotDelivered},
		{"eof", io.EOF, false, false, domain.ProviderErrorUncertain},
		{"closed after write", fmt.Errorf("turn/start: %w", ErrConnClosed), false, false, domain.ProviderErrorUncertain},
		{"timeout", context.DeadlineExceeded, false, false, domain.ProviderErrorUncertain},
		{"cancelled", context.Canceled, false, false, domain.ProviderErrorUncertain},
		{"undecodable response", fmt.Errorf("turn/start: decode result: %w", errors.New("invalid character")), false, false, domain.ProviderErrorUncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySendTurnError(tc.err)
			if errors.Is(got, ports.ErrTurnNotDelivered) != tc.delivered {
				t.Fatalf("ErrTurnNotDelivered = %v, want %v (err %v)", errors.Is(got, ports.ErrTurnNotDelivered), tc.delivered, got)
			}
			if errors.Is(got, ports.ErrTurnRejected) != tc.rejected {
				t.Fatalf("ErrTurnRejected = %v, want %v (err %v)", errors.Is(got, ports.ErrTurnRejected), tc.rejected, got)
			}
			if !tc.delivered && !tc.rejected && !errors.Is(got, tc.err) {
				t.Fatalf("ambiguous error lost its chain: %v", got)
			}
			evidence, ok := ports.ProviderErrorEvidenceOf(got)
			if !ok || evidence.Class != tc.class {
				t.Fatalf("evidence = (%+v, %v), want class %q", evidence, ok, tc.class)
			}
			if evidence.Class == "rate_limited" || evidence.Class == "transient" {
				t.Fatalf("prose produced an undemonstrated class %q", evidence.Class)
			}
			if evidence.Protocol != "codex" || evidence.Operation != "turn/start" {
				t.Fatalf("evidence identity = %+v, want codex turn/start", evidence)
			}
		})
	}
}

func TestCodexProviderFailureEvidence(t *testing.T) {
	unauthorized := codexproto.CodexErrorInfo(`"unauthorized"`)
	err := codexProviderFailure(codexproto.TurnError{
		Message:        "sign in again",
		CodexErrorInfo: &unauthorized,
	})
	evidence, ok := ports.ProviderErrorEvidenceOf(err)
	if !ok || evidence.Class != domain.ProviderErrorAuthRequired {
		t.Fatalf("unauthorized evidence = (%+v, %v), want auth_required", evidence, ok)
	}
	if !errors.Is(err, ports.ErrChatAuthRequired) {
		t.Fatalf("unauthorized error = %v, want ErrChatAuthRequired", err)
	}

	rate := codexProviderFailure(codexproto.TurnError{
		Message: "Rate limit exceeded. Please try again later.",
	})
	evidence, ok = ports.ProviderErrorEvidenceOf(rate)
	if !ok || evidence.Class != domain.ProviderErrorUnknown {
		t.Fatalf("rate-limit prose evidence = (%+v, %v), want unknown", evidence, ok)
	}
	if evidence.Class == "rate_limited" || evidence.Class == "transient" {
		t.Fatalf("rate-limit prose produced %q", evidence.Class)
	}
	if rate.Error() != "Rate limit exceeded. Please try again later." {
		t.Fatalf("visible error changed: %q", rate.Error())
	}
}

// TestSendTurnEmptyTextIsDefinitive proves a local validation failure is
// classified before touching the wire.
func TestSendTurnEmptyTextIsDefinitive(t *testing.T) {
	c := &conversation{}
	_, err := c.SendTurn(context.Background(), ports.ChatUserMessage{Text: "  "})
	if !errors.Is(err, ports.ErrTurnNotDelivered) {
		t.Fatalf("err = %v, want ErrTurnNotDelivered", err)
	}
	if errors.Is(err, ports.ErrTurnRejected) {
		t.Fatalf("local validation error = %v, must not be a provider rejection", err)
	}
	evidence, ok := ports.ProviderErrorEvidenceOf(err)
	if !ok || evidence.Class != domain.ProviderErrorNotDelivered ||
		evidence.Protocol != "codex" || evidence.Operation != "turn/start" {
		t.Fatalf("local validation evidence = (%+v, %v), want codex not_delivered", evidence, ok)
	}
}
