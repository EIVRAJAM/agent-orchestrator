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
	// IssueRecoveryContinued means the one allowed continuation was delivered.
	IssueRecoveryContinued IssueRecoveryState = "continued"
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
	IssueRecoveryReasonPRReady              IssueRecoveryReason = "pr_ready"
	IssueRecoveryReasonIssuePRMissing       IssueRecoveryReason = "issue_pr_missing"
	IssueRecoveryReasonBranchNotPushed      IssueRecoveryReason = "branch_not_pushed"
	IssueRecoveryReasonPRNotReady           IssueRecoveryReason = "pr_not_ready"
	IssueRecoveryReasonPushFailed           IssueRecoveryReason = "push_failed"
	IssueRecoveryReasonAssociationInvalid   IssueRecoveryReason = "pr_association_invalid"
	IssueRecoveryReasonOwnershipUnavailable IssueRecoveryReason = "ownership_unavailable"
	IssueRecoveryReasonWorkerExited         IssueRecoveryReason = "worker_exited"
	IssueRecoveryReasonContinuationSent     IssueRecoveryReason = "continuation_sent"
	IssueRecoveryReasonContinuationFailed   IssueRecoveryReason = "continuation_failed"
	IssueRecoveryReasonExhausted            IssueRecoveryReason = "continuation_exhausted"
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
	UpdatedAt         time.Time
}
