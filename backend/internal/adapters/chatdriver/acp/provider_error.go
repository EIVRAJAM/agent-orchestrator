package acp

import (
	"encoding/json"
	"errors"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The ACP layer records only what the protocol makes structural: JSON-RPC code,
// allowlisted data field names with their types, and an already-used host event
// id. Provider prose is never inspected, so a data.error string such as
// "Rate limit exceeded" cannot become a new error class.
var acpProviderErrorShapeKeys = []string{
	"error",
	"eventId",
	persistenthost.ACPEventIDMetaKey,
}

func acpProviderErrorEvidence(operation string, err error) domain.ProviderErrorEvidence {
	evidence := domain.ProviderErrorEvidence{
		Class:     domain.ProviderErrorUnknown,
		Protocol:  "acp",
		Operation: strings.TrimPrefix(operation, "ACP "),
	}
	var requestErr *acpsdk.RequestError
	if !errors.As(err, &requestErr) {
		return evidence
	}
	evidence.Class = domain.ProviderErrorProviderInternal
	if requestErr.Code == -32000 {
		evidence.Class = domain.ProviderErrorAuthRequired
	}
	code := requestErr.Code
	evidence.Code = &code
	evidence.DataShape = acpProviderErrorDataShape(requestErr.Data)
	if eventID, ok := ports.SafeProviderEventID(acpProviderEventID(requestErr.Data)); ok {
		evidence.EventID = eventID
	}
	return evidence
}

// attachACPPromptEvidence preserves the adapter's classification and adds the
// terminal prompt's already-used host event id when it is safe. It is also the
// fallback for structured prompt failures that carry no RequestError.
func attachACPPromptEvidence(err error, eventID string) error {
	if err == nil {
		return nil
	}
	evidence, ok := ports.ProviderErrorEvidenceOf(err)
	if !ok {
		evidence = domain.ProviderErrorEvidence{
			Class:     domain.ProviderErrorUnknown,
			Protocol:  "acp",
			Operation: "session/prompt",
		}
		if errors.Is(err, ports.ErrChatAuthRequired) {
			evidence.Class = domain.ProviderErrorAuthRequired
		}
	}
	if safeID, safe := ports.SafeProviderEventID(eventID); safe && evidence.EventID == "" {
		evidence.EventID = safeID
	}
	return ports.WithProviderError(err, evidence)
}

func acpProviderErrorDataShape(data any) map[string]string {
	values, ok := data.(map[string]any)
	if !ok || len(values) == 0 {
		return nil
	}
	shape := make(map[string]string, len(acpProviderErrorShapeKeys))
	addACPProviderErrorShape(shape, values)
	// The persistent relay wraps arbitrary provider error data exactly one level
	// under this AO-owned key when it adds the host event id. Flatten that known
	// wrapper by name/type only; no nested value is read or retained.
	if providerData, ok := values["providerData"].(map[string]any); ok {
		addACPProviderErrorShape(shape, providerData)
	}
	if len(shape) == 0 {
		return nil
	}
	return shape
}

func addACPProviderErrorShape(shape map[string]string, values map[string]any) {
	for _, key := range acpProviderErrorShapeKeys {
		value, present := values[key]
		if !present {
			continue
		}
		shape[key] = acpProviderErrorDataType(value)
	}
}

func acpProviderErrorDataType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float64, float32,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		// A type AO has not proven safe to describe is still only a type; no
		// value is copied.
		return "unknown"
	}
}

func acpProviderEventID(data any) string {
	values, ok := data.(map[string]any)
	if !ok {
		return ""
	}
	eventID, _ := values[persistenthost.ACPEventIDMetaKey].(string)
	return eventID
}
