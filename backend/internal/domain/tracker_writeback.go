package domain

import "time"

// TrackerWriteback is one durable, at-most-once tracker comment intent. The
// unique key is project + issue + PR; a claimed row is never retried blindly.
type TrackerWriteback struct {
	ID        string
	ProjectID ProjectID
	SessionID SessionID
	IssueID   IssueID
	PRURL     string
	// HeadSHA is the PR head observed when this readiness intent was enqueued.
	// ProcessOnce refuses to post if the PR has moved since.
	HeadSHA     string
	Status      string
	Attempts    int
	CommentURL  string
	FailureCode string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClaimedAt   *time.Time
}

// Tracker write-back statuses. pending -> claimed -> sent|uncertain|failed.
const (
	TrackerWritebackPending   = "pending"
	TrackerWritebackClaimed   = "claimed"
	TrackerWritebackSent      = "sent"
	TrackerWritebackUncertain = "uncertain"
	TrackerWritebackFailed    = "failed"
)
