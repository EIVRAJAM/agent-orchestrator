package domain

import "time"

// IssueRecoveryState is the durable internal state of the bounded continuation
// path for an issue-backed worker. It is deliberately not a presentation
// status; IssuePublication remains the public publication result.
type IssueRecoveryState string

const (
	// IssueRecoveryObserving means the worker was seen without a valid PR and
	// has not yet been continued.
	IssueRecoveryObserving IssueRecoveryState = "observing"
	// IssueRecoveryClaimed means the one attempt was atomically reserved, but
	// delivery has not yet been demonstrated.
	IssueRecoveryClaimed IssueRecoveryState = "claimed"
	// IssueRecoverySent means the protected send returned successfully and the
	// sent fact was durably recorded.
	IssueRecoverySent IssueRecoveryState = "sent"
	// IssueRecoveryUncertain means the process could have sent the continuation
	// but did not settle the result. It is never retried.
	IssueRecoveryUncertain IssueRecoveryState = "uncertain"
	// IssueRecoveryComplete means an exact valid PR exists; no continuation is
	// needed.
	IssueRecoveryComplete IssueRecoveryState = "complete"
	// IssueRecoveryIncomplete is the terminal fail-closed outcome. It covers an
	// exhausted attempt, a failed continuation delivery, or evidence that cannot
	// be safely continued.
	IssueRecoveryIncomplete IssueRecoveryState = "incomplete"
)

// IssueRecoveryReason is a closed, sanitized reason for the last recovery
// decision.
type IssueRecoveryReason string

// Recovery reasons are closed and sanitized; they never include prompts or
// arbitrary provider output.
const (
	IssueRecoveryReasonPRReady               IssueRecoveryReason = "pr_ready"
	IssueRecoveryReasonIssuePRMissing        IssueRecoveryReason = "issue_pr_missing"
	IssueRecoveryReasonBranchNotPushed       IssueRecoveryReason = "branch_not_pushed"
	IssueRecoveryReasonPRNotReady            IssueRecoveryReason = "pr_not_ready"
	IssueRecoveryReasonPushFailed            IssueRecoveryReason = "push_failed"
	IssueRecoveryReasonAssociationInvalid    IssueRecoveryReason = "pr_association_invalid"
	IssueRecoveryReasonOwnershipUnavailable  IssueRecoveryReason = "ownership_unavailable"
	IssueRecoveryReasonWorkerExited          IssueRecoveryReason = "worker_exited"
	IssueRecoveryReasonContinuationClaimed   IssueRecoveryReason = "continuation_claimed"
	IssueRecoveryReasonContinuationSent      IssueRecoveryReason = "continuation_sent"
	IssueRecoveryReasonContinuationUncertain IssueRecoveryReason = "continuation_uncertain"
	IssueRecoveryReasonContinuationFailed    IssueRecoveryReason = "continuation_failed"
	IssueRecoveryReasonExhausted             IssueRecoveryReason = "continuation_exhausted"
	IssueRecoveryReasonDeliveryUnsupported   IssueRecoveryReason = "delivery_unsupported"
	IssueRecoveryReasonDeliveryNotIdle       IssueRecoveryReason = "delivery_not_idle"
	IssueRecoveryReasonTurnNotCompleted      IssueRecoveryReason = "turn_not_completed"
	IssueRecoveryReasonFinalValidation       IssueRecoveryReason = "final_validation_failed"
)

// IssueRecovery is the durable, at-most-once continuation record for one
// issue-backed worker session. It stores only evidence needed to decide whether
// the same session may be continued once; it never stores prompts or provider
// output.
type IssueRecovery struct {
	SessionID         SessionID
	IssueID           IssueID
	InstanceID        string
	State             IssueRecoveryState
	Reason            IssueRecoveryReason
	Attempts          int
	LocalCommits      int
	LocalChanges      bool
	LocalChangesKnown bool
	Push              IssuePublicationPush
	Turn              IssuePublicationTurn
	PRURL             string
	HeadSHA           string
	ObservedAt        time.Time
	AttemptedAt       *time.Time
	SentAt            *time.Time
	UpdatedAt         time.Time
}
