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

// SafeProviderEventID reports whether an ACP host event id is safe to copy into
// structured evidence. The relay already uses this value for acknowledgement;
// the conservative format check keeps control characters, whitespace, and
// unbounded host data out of the diagnostic record.
func SafeProviderEventID(value string) (string, bool) {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return "", false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return "", false
		}
	}
	return value, true
}
