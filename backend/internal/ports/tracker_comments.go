package ports

import (
	"context"
	"errors"
	"net/url"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrTrackerCommentUnsupported means the configured tracker adapter has no
// issue-comment mutation capability. Callers must treat it as a permanent
// write-back failure rather than retrying indefinitely.
var ErrTrackerCommentUnsupported = errors.New("tracker: issue comment write unsupported")

// ErrTrackerCommentReceipt means the provider accepted a comment request but
// did not return a usable receipt URL. The comment may exist, so callers must
// settle it as uncertain rather than retrying.
var ErrTrackerCommentReceipt = errors.New("tracker: issue comment receipt invalid")

// ValidTrackerCommentURL reports whether a provider comment receipt names an
// http(s) resource. It deliberately does not log or echo the value.
func ValidTrackerCommentURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

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
