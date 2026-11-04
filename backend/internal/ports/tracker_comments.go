package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrTrackerCommentUnsupported means the configured tracker adapter has no
// issue-comment mutation capability. Callers must treat it as a permanent
// write-back failure rather than retrying indefinitely.
var ErrTrackerCommentUnsupported = errors.New("tracker: issue comment write unsupported")

// TrackerComment is the provider receipt for one created issue comment.
type TrackerComment struct {
	ID  string
	URL string
}

// TrackerCommenter is the optional write-side tracker capability. It is kept
// separate from Tracker so read-only adapters and fakes do not acquire mutation
// methods implicitly.
type TrackerCommenter interface {
	CreateIssueComment(ctx context.Context, id domain.TrackerID, body string) (TrackerComment, error)
}
