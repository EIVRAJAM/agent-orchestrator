// Package trackerwriteback owns the single outbound tracker mutation AO
// permits: one deterministic issue comment when a session's PR is ready for
// review and CI is green. The durable outbox is deliberately at-most-once; a
// lost response is recorded as uncertain and is never retried blindly.
package trackerwriteback

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const defaultTick = 30 * time.Second

// Store is the durable surface needed to enqueue and deliver a write-back.
type Store interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
	GetPR(ctx context.Context, url string) (domain.PullRequest, bool, error)
	GetSessionPROwnership(ctx context.Context, prURL string) (domain.SessionPROwnership, bool, error)
	EnqueueTrackerWriteback(ctx context.Context, w domain.TrackerWriteback) (bool, error)
	ClaimTrackerWriteback(ctx context.Context, now time.Time) (domain.TrackerWriteback, bool, error)
	FinishTrackerWriteback(ctx context.Context, id, status, commentURL, failureCode string, now time.Time) (bool, error)
	RecoverStaleTrackerWritebacks(ctx context.Context, now time.Time) (int64, error)
	ListTrackerWritebacks(ctx context.Context, status string, limit, offset int) ([]domain.TrackerWriteback, error)
}

// Service enqueues lifecycle intents and delivers them through one tracker
// comment capability. A nil commenter is a permanent, conservative failure.
type Service struct {
	store     Store
	commenter ports.TrackerCommenter
	logger    *slog.Logger
	clock     func() time.Time
	newID     func() string
	tick      time.Duration
}

// New constructs a write-back service. The caller supplies the same tracker
// adapter already configured for tracker intake; no new credential path is
// introduced.
func New(store Store, commenter ports.TrackerCommenter, logger *slog.Logger, tick time.Duration) *Service {
	if tick <= 0 {
		tick = defaultTick
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:     store,
		commenter: commenter,
		logger:    logger,
		clock:     func() time.Time { return time.Now().UTC() },
		newID:     uuid.NewString,
		tick:      tick,
	}
}

// Enqueue records one deduplicated intent. It is intentionally cheap and is
// called from the lifecycle reducer; it never performs provider I/O. Missing
// sessions, non-worker sessions, missing issue identities, disabled projects,
// and malformed PR URLs are ignored conservatively.
func (s *Service) Enqueue(ctx context.Context, sessionID domain.SessionID, prURL string) error {
	if s == nil || s.store == nil || s.commenter == nil || sessionID == "" {
		return nil
	}
	prURL = strings.TrimSpace(prURL)
	if !validPRURL(prURL) {
		return nil
	}
	rec, ok, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session for tracker writeback: %w", err)
	}
	if !ok || rec.ProjectID == "" || rec.IssueID == "" {
		return nil
	}
	if rec.Kind != "" && rec.Kind != domain.KindWorker {
		return nil
	}
	if rec.IsTerminated {
		return nil
	}
	if _, ok := domain.ParseCanonicalIssueID(string(rec.IssueID)); !ok {
		return nil
	}
	project, ok, err := s.store.GetProject(ctx, string(rec.ProjectID))
	if err != nil {
		return fmt.Errorf("load project for tracker writeback: %w", err)
	}
	if !ok || !project.Config.TrackerWriteback.Enabled {
		return nil
	}
	pr, ok, err := s.store.GetPR(ctx, prURL)
	if err != nil {
		return fmt.Errorf("load PR for tracker writeback: %w", err)
	}
	ownership, owned, err := s.store.GetSessionPROwnership(ctx, prURL)
	if err != nil {
		return fmt.Errorf("load PR ownership for tracker writeback: %w", err)
	}
	if !ok || !owned || !exactPROwnership(pr, rec, ownership) {
		s.logger.Warn("tracker writeback: rejected PR association", "session", rec.ID, "reason", "pr_ownership_missing_or_mismatched")
		return nil
	}
	now := s.now()
	_, err = s.store.EnqueueTrackerWriteback(ctx, domain.TrackerWriteback{
		ID:        s.newID(),
		ProjectID: rec.ProjectID,
		SessionID: rec.ID,
		IssueID:   rec.IssueID,
		PRURL:     prURL,
		HeadSHA:   pr.HeadSHA,
		Status:    domain.TrackerWritebackPending,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		return fmt.Errorf("enqueue tracker writeback: %w", err)
	}
	return nil
}

// List returns a bounded, read-only page of durable outbox states. It exposes
// no issue/comment body, PR URL, or credential material.
func (s *Service) List(ctx context.Context, status string, limit, offset int) ([]domain.TrackerWriteback, bool, error) {
	if s == nil || s.store == nil {
		return nil, false, nil
	}
	status = strings.TrimSpace(status)
	switch status {
	case "", domain.TrackerWritebackPending, domain.TrackerWritebackClaimed,
		domain.TrackerWritebackSent, domain.TrackerWritebackUncertain, domain.TrackerWritebackFailed:
	default:
		return nil, false, fmt.Errorf("tracker writeback: unsupported status %q", status)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.store.ListTrackerWritebacks(ctx, status, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	return rows, hasMore, nil
}

// ProcessOnce claims at most one pending row and attempts its single delivery.
// Provider errors settle the row as uncertain; they never leave it pending for
// a blind retry.
func (s *Service) ProcessOnce(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	now := s.now()
	row, ok, err := s.store.ClaimTrackerWriteback(ctx, now)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	settle := func(status, commentURL, code string) {
		updated, err := s.store.FinishTrackerWriteback(context.WithoutCancel(ctx), row.ID, status, commentURL, code, s.now())
		if err != nil || !updated {
			s.logger.Warn("tracker writeback: settlement unconfirmed",
				"writeback_id", row.ID, "desired_status", status, "code", "settlement_unconfirmed")
			return
		}
		attrs := []any{"writeback_id", row.ID, "status", status}
		if code != "" {
			attrs = append(attrs, "code", code)
		}
		if status == domain.TrackerWritebackSent {
			s.logger.Info("tracker writeback: settled", attrs...)
		} else {
			s.logger.Warn("tracker writeback: settled", attrs...)
		}
	}
	rec, ok, err := s.store.GetSession(ctx, row.SessionID)
	if err != nil {
		settle(domain.TrackerWritebackFailed, "", "session_read")
		return err
	}
	if !ok {
		settle(domain.TrackerWritebackFailed, "", "session_missing")
		return nil
	}
	project, ok, err := s.store.GetProject(ctx, string(rec.ProjectID))
	if err != nil {
		settle(domain.TrackerWritebackFailed, "", "project_read")
		return err
	}
	if !ok || !project.Config.TrackerWriteback.Enabled {
		settle(domain.TrackerWritebackFailed, "", "writeback_disabled")
		return nil
	}
	pr, ok, err := s.store.GetPR(ctx, row.PRURL)
	if err != nil {
		settle(domain.TrackerWritebackFailed, "", "pr_read")
		return err
	}
	ownership, owned, err := s.store.GetSessionPROwnership(ctx, row.PRURL)
	if err != nil {
		settle(domain.TrackerWritebackFailed, "", "pr_ownership_read")
		return err
	}
	if !ok || !owned || !exactPROwnership(pr, rec, ownership) {
		settle(domain.TrackerWritebackFailed, "", "pr_association_invalid")
		return nil
	}
	if row.HeadSHA == "" || row.HeadSHA != pr.HeadSHA {
		settle(domain.TrackerWritebackFailed, "", "pr_head_changed")
		return nil
	}
	issue, ok := domain.ParseCanonicalIssueID(string(row.IssueID))
	if !ok {
		settle(domain.TrackerWritebackFailed, "", "issue_invalid")
		return nil
	}
	if s.commenter == nil {
		settle(domain.TrackerWritebackFailed, "", "tracker_unsupported")
		return nil
	}
	if !validPRURL(row.PRURL) {
		settle(domain.TrackerWritebackFailed, "", "pr_url_invalid")
		return nil
	}
	comment, err := s.commenter.CreateIssueComment(ctx, issue, commentBody(row.PRURL))
	if err != nil {
		code := "remote_uncertain"
		if errors.Is(err, ports.ErrTrackerCommentReceipt) {
			code = "missing_comment_receipt"
		}
		// The request may have reached the provider before the response was
		// lost. At-most-once wins over speculative retries.
		settle(domain.TrackerWritebackUncertain, "", code)
		return nil //nolint:nilerr // remote failure is settled as uncertain, never blindly retried
	}
	if !ports.ValidTrackerCommentURL(comment.URL) {
		settle(domain.TrackerWritebackUncertain, "", "missing_comment_receipt")
		return nil
	}
	settle(domain.TrackerWritebackSent, comment.URL, "")
	return nil
}

// Start runs the durable delivery loop. Stale claims left by a previous process
// are converted to uncertain before the first claim.
func (s *Service) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if s == nil || s.store == nil {
			return
		}
		if n, err := s.store.RecoverStaleTrackerWritebacks(ctx, s.now()); err != nil {
			s.logger.Warn("tracker writeback: recovery failed")
		} else if n > 0 {
			s.logger.Info("tracker writeback: stale claims marked uncertain", "count", n)
		}
		ticker := time.NewTicker(s.tick)
		defer ticker.Stop()
		for {
			if err := s.ProcessOnce(ctx); err != nil && ctx.Err() == nil {
				s.logger.Warn("tracker writeback: pass failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// exactPROwnership requires the durable instance/branch/head chain recorded by
// the observer or an explicit claim. Timestamps and branch prefixes are never
// ownership evidence.
func exactPROwnership(pr domain.PullRequest, rec domain.SessionRecord, ownership domain.SessionPROwnership) bool {
	return rec.InstanceID != "" &&
		pr.SessionID == rec.ID &&
		ownership.PRURL == pr.URL &&
		ownership.SessionInstanceID == rec.InstanceID &&
		ownership.SourceBranch == pr.SourceBranch &&
		ownership.HeadSHA == pr.HeadSHA
}

func (s *Service) now() time.Time {
	if s.clock == nil {
		return time.Now().UTC()
	}
	return s.clock().UTC()
}

func commentBody(prURL string) string {
	return "AO: PR ready for review: " + strings.TrimSpace(prURL)
}

func validPRURL(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}
