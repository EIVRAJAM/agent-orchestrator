package codexappserver

import (
	"context"
	"errors"
	"io"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/codexappserver/codexproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// codexSendErrorEvidence classifies only facts the app-server transport makes
// structural: an explicit RPC error response, a connection refused before its
// first write, or an ambiguous write/read failure. It never inspects the RPC
// message, so a rate-limit sentence remains a rejected request until the
// provider exposes a stable structured field for it.
func codexSendErrorEvidence(err error) domain.ProviderErrorEvidence {
	evidence := domain.ProviderErrorEvidence{
		Class:     domain.ProviderErrorUnknown,
		Protocol:  "codex",
		Harness:   string(domain.HarnessCodex),
		Operation: "turn/start",
	}
	var rpcErr *rpcError
	if errors.As(err, &rpcErr) {
		evidence.Class = domain.ProviderErrorRejected
		code := rpcErr.Code
		evidence.Code = &code
		return evidence
	}
	if errors.Is(err, errConnClosedBeforeWrite) {
		evidence.Class = domain.ProviderErrorNotDelivered
		return evidence
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrConnClosed) || errors.Is(err, errDecodeResponse) {
		evidence.Class = domain.ProviderErrorUncertain
	}
	return evidence
}

// codexTurnFailureEvidence follows the only provider error metadata AO has
// demonstrated: an explicit unauthorized reason. Every other turn error remains
// unknown rather than being inferred from prose.
func codexTurnFailureEvidence(turnErr codexproto.TurnError) domain.ProviderErrorEvidence {
	evidence := domain.ProviderErrorEvidence{
		Class:     domain.ProviderErrorUnknown,
		Protocol:  "codex",
		Harness:   string(domain.HarnessCodex),
		Operation: "turn/error",
	}
	if turnErr.CodexErrorInfo != nil && string(*turnErr.CodexErrorInfo) == `"unauthorized"` {
		evidence.Class = domain.ProviderErrorAuthRequired
	}
	return evidence
}
