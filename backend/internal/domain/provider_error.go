package domain

// ProviderErrorClass is the conservative, evidence-backed classification of a
// provider failure. It is diagnostic vocabulary, not a recovery policy: a class
// never decides whether a turn is retried, settled, or drained.
//
// Only classes demonstrated by a structured provider signal belong here. In
// particular, there is deliberately no rate-limit or transient class until a
// stable structured field proves one; provider prose is never classified.
type ProviderErrorClass string

const (
	// ProviderErrorAuthRequired means the provider explicitly refused the
	// credential. The existing reauthentication path remains authoritative.
	ProviderErrorAuthRequired ProviderErrorClass = "auth_required"
	// ProviderErrorRejected means the provider returned an explicit RPC error
	// response for the request.
	ProviderErrorRejected ProviderErrorClass = "rejected"
	// ProviderErrorNotDelivered means the request provably never reached the
	// provider. It says nothing about temporary provider availability.
	ProviderErrorNotDelivered ProviderErrorClass = "not_delivered"
	// ProviderErrorUncertain means the request may have reached the provider, but
	// the outcome cannot be established from the observed failure.
	ProviderErrorUncertain ProviderErrorClass = "uncertain"
	// ProviderErrorInterrupted means the turn was deliberately interrupted.
	ProviderErrorInterrupted ProviderErrorClass = "interrupted"
	// ProviderErrorProviderInternal means the provider returned a structured
	// protocol error describing its own internal failure.
	ProviderErrorProviderInternal ProviderErrorClass = "provider_internal"
	// ProviderErrorUnknown covers a provider failure that is demonstrated but
	// does not match a narrower class. It is intentionally not a retry signal.
	ProviderErrorUnknown ProviderErrorClass = "unknown"
)

// ProviderErrorEvidence is the sanitized, durable diagnostic attached to a
// provider failure. It contains no message, prompt, response, path, credential,
// arbitrary data value, or raw provider error string.
//
// DataShape holds only allowlisted field names mapped to their JSON type. The
// provider's actual data values must never be copied here.
type ProviderErrorEvidence struct {
	Class     ProviderErrorClass `json:"class"`
	Protocol  string             `json:"protocol,omitempty"`
	Operation string             `json:"operation,omitempty"`
	Code      *int               `json:"code,omitempty"`
	DataShape map[string]string  `json:"dataShape,omitempty"`
	EventID   string             `json:"eventId,omitempty"`
}
