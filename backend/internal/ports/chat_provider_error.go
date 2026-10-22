package ports

import (
	"errors"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// providerError carries sanitized diagnostic evidence without changing the
// wrapped error's message, identity, or errors.Is/errors.As behavior. The
// controller projects the evidence into the existing provider-event archive;
// policy continues to use the original typed errors.
type providerError struct {
	evidence domain.ProviderErrorEvidence
	err      error
}

func (e *providerError) Error() string { return e.err.Error() }
func (e *providerError) Unwrap() error { return e.err }

// WithProviderError attaches sanitized evidence to an existing error. If the
// error already carries evidence, non-empty fields are merged so a later layer
// (for example a validated ACP event id) can refine the classification without
// losing the adapter's result.
func WithProviderError(err error, evidence domain.ProviderErrorEvidence) error {
	if err == nil {
		return nil
	}
	if existing, ok := ProviderErrorEvidenceOf(err); ok {
		evidence = mergeProviderErrorEvidence(existing, evidence)
	}
	if evidence.Class == "" {
		evidence.Class = domain.ProviderErrorUnknown
	}
	return &providerError{evidence: evidence, err: err}
}

// ProviderErrorEvidenceOf returns the sanitized evidence attached to err, if
// any. It follows the normal error chain and never interprets prose.
func ProviderErrorEvidenceOf(err error) (domain.ProviderErrorEvidence, bool) {
	var providerErr *providerError
	if !errors.As(err, &providerErr) {
		return domain.ProviderErrorEvidence{}, false
	}
	return providerErr.evidence, true
}

func mergeProviderErrorEvidence(base, next domain.ProviderErrorEvidence) domain.ProviderErrorEvidence {
	merged := base
	if next.Class != "" {
		merged.Class = next.Class
	}
	if next.Protocol != "" {
		merged.Protocol = next.Protocol
	}
	if next.Harness != "" {
		merged.Harness = next.Harness
	}
	if next.Operation != "" {
		merged.Operation = next.Operation
	}
	if next.Code != nil {
		merged.Code = next.Code
	}
	if next.EventID != "" {
		merged.EventID = next.EventID
	}
	if len(next.DataShape) > 0 {
		shape := make(map[string]string, len(base.DataShape)+len(next.DataShape))
		for key, value := range base.DataShape {
			shape[key] = value
		}
		for key, value := range next.DataShape {
			shape[key] = value
		}
		merged.DataShape = shape
	}
	return merged
}

// SafeProviderEventID reports whether value is the exact event-id shape AO's
// persistent ACP relay owns: "acp-host:<64 lowercase hex>:<decimal sequence>".
// ASCII alone is not enough: an arbitrary provider identifier must not become
// trusted evidence merely because it contains printable characters.
func SafeProviderEventID(value string) (string, bool) {
	const prefix = "acp-host:"
	if value == "" || len(value) > 256 || !strings.HasPrefix(value, prefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(value, prefix), ":")
	if len(parts) != 2 || len(parts[0]) != 64 || len(parts[1]) == 0 || len(parts[1]) > 20 {
		return "", false
	}
	for i := 0; i < len(parts[0]); i++ {
		c := parts[0][i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", false
		}
	}
	for i := 0; i < len(parts[1]); i++ {
		if parts[1][i] < '0' || parts[1][i] > '9' {
			return "", false
		}
	}
	return value, true
}

var providerErrorClasses = map[domain.ProviderErrorClass]struct{}{
	domain.ProviderErrorAuthRequired:     {},
	domain.ProviderErrorRejected:         {},
	domain.ProviderErrorNotDelivered:     {},
	domain.ProviderErrorUncertain:        {},
	domain.ProviderErrorInterrupted:      {},
	domain.ProviderErrorProviderInternal: {},
	domain.ProviderErrorUnknown:          {},
}

var providerErrorDataShapeKeys = []string{
	"error",
	"eventId",
	"ao.persistentEventId",
}

var providerErrorDataShapeTypes = map[string]struct{}{
	"null": {}, "string": {}, "boolean": {}, "number": {},
	"object": {}, "array": {}, "unknown": {},
}

var providerErrorOperations = map[string]map[string]struct{}{
	"codex": {"turn/start": {}, "turn/error": {}},
	"acp":   {"session/prompt": {}},
}

const providerErrorMaxDataShapeFields = 3

// SanitizeProviderErrorEvidence is the closed boundary applied before writing
// provider evidence to conversation_provider_events. Adapters are not trusted
// to hand over safe values: unknown classes fall back to unknown, unknown
// protocols/harnesses/operations are omitted, and DataShape is rebuilt from a
// fixed key/type allowlist with no values. It returns a copy and never mutates
// the caller's evidence.
func SanitizeProviderErrorEvidence(evidence domain.ProviderErrorEvidence) domain.ProviderErrorEvidence {
	sanitized := domain.ProviderErrorEvidence{Class: domain.ProviderErrorUnknown}
	if _, ok := providerErrorClasses[evidence.Class]; ok {
		sanitized.Class = evidence.Class
	}

	protocol := ""
	if _, ok := providerErrorOperations[evidence.Protocol]; ok {
		protocol = evidence.Protocol
		sanitized.Protocol = protocol
	}

	if protocol != "" && evidence.Harness != "" {
		harness := domain.AgentHarness(evidence.Harness)
		pertinent := harness.IsKnown() &&
			((protocol == "codex" && harness == domain.HarnessCodex) ||
				(protocol == "acp" && harness != domain.HarnessCodex))
		if pertinent {
			sanitized.Harness = evidence.Harness
		}
	}

	if operations, ok := providerErrorOperations[protocol]; ok {
		if _, ok := operations[evidence.Operation]; ok {
			sanitized.Operation = evidence.Operation
		}
	}

	if evidence.Code != nil {
		code := *evidence.Code
		sanitized.Code = &code
	}

	if len(evidence.DataShape) > 0 {
		shape := make(map[string]string, providerErrorMaxDataShapeFields)
		for _, key := range providerErrorDataShapeKeys {
			if len(shape) >= providerErrorMaxDataShapeFields {
				break
			}
			valueType, present := evidence.DataShape[key]
			if !present {
				continue
			}
			if _, ok := providerErrorDataShapeTypes[valueType]; !ok {
				continue
			}
			shape[key] = valueType
		}
		if len(shape) > 0 {
			sanitized.DataShape = shape
		}
	}

	if protocol == "acp" {
		if eventID, ok := SafeProviderEventID(evidence.EventID); ok {
			sanitized.EventID = eventID
		}
	}
	return sanitized
}
