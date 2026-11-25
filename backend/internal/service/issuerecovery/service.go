// Package issuerecovery provides one bounded, durable continuation for
// issue-backed workers that become idle without a valid published PR.
package issuerecovery

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

const defaultTick = 30 * time.Second

// Store is the durable state surface for bounded continuation decisions.
type Store interface {
	ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error)
	GetIssueRecovery(ctx context.Context, id domain.SessionID) (domain.IssueRecovery, bool, error)
	EnsureIssueRecovery(ctx context.Context, rec domain.IssueRecovery) (bool, error)
	UpdateIssueRecoveryObservation(ctx context.Context, rec domain.IssueRecovery) (bool, error)
	ClaimIssueRecovery(ctx context.Context, id domain.SessionID, instanceID string, now time.Time) (bool, error)
	FinishIssueRecovery(ctx context.Context, rec domain.IssueRecovery, now time.Time) (bool, error)
}

// Sessions supplies the already-derived issue publication result. The service
// never re-derives or weakens ownership/publication rules.
type Sessions interface {
	Get(ctx context.Context, id domain.SessionID) (domain.Session, error)
}

// Sender delivers a recovery prompt through the normal session path. It must
// target the same session; this package never creates a session or invokes a
// provider directly.
type Sender interface {
	Send(ctx context.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) error
}

// Service is a bounded issue-worker continuation loop.
type Service struct {
	store    Store
	sessions Sessions
	sender   Sender
	logger   *slog.Logger
	clock    func() time.Time
	tick     time.Duration
	inspect  func(context.Context, string) (bool, bool)
}

// New constructs a recovery service. A nil logger uses the package default.
func New(store Store, sessions Sessions, sender Sender, logger *slog.Logger, tick time.Duration) *Service {
	if tick <= 0 {
		tick = defaultTick
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:    store,
		sessions: sessions,
		sender:   sender,
		logger:   logger,
		clock:    func() time.Time { return time.Now().UTC() },
		tick:     tick,
		inspect:  inspectWorkspace,
	}
}

// Start runs the observer loop until ctx is cancelled.
func (s *Service) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.tick)
		defer ticker.Stop()
		for {
			if err := s.ScanOnce(ctx); err != nil && ctx.Err() == nil {
				s.logger.Warn("issue recovery: scan failed")
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

// ScanOnce evaluates all issue workers once. It is synchronous so tests can
// exercise exactly one observation without polling or timers.
func (s *Service) ScanOnce(ctx context.Context) error {
	if s == nil || s.store == nil || s.sessions == nil || s.sender == nil {
		return nil
	}
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	now := s.clock()
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if rec.IssueID == "" || rec.Kind != domain.KindWorker || rec.IsTerminated || rec.FirstSignalAt.IsZero() {
			continue
		}
		if rec.Activity.State != domain.ActivityIdle && rec.Activity.State != domain.ActivityExited {
			continue
		}
		sess, err := s.sessions.Get(ctx, rec.ID)
		if err != nil {
			s.logger.Warn("issue recovery: session read failed", "session", rec.ID)
			continue
		}
		s.evaluate(ctx, sess, now)
	}
	return nil
}

func (s *Service) evaluate(ctx context.Context, sess domain.Session, now time.Time) {
	rec := sess.SessionRecord
	pub := sess.IssuePublication
	if pub == nil {
		return
	}
	prior, found, err := s.store.GetIssueRecovery(ctx, rec.ID)
	if err != nil {
		s.logger.Warn("issue recovery: state read failed", "session", rec.ID)
		return
	}
	if found && prior.InstanceID != rec.InstanceID {
		s.logger.Warn("issue recovery: instance changed; failing closed", "session", rec.ID)
		return
	}
	localChanges, localChangesKnown := false, false
	if s.inspect != nil {
		localChanges, localChangesKnown = s.inspect(ctx, rec.Metadata.WorkspacePath)
	}
	observed := recoveryObservation(rec, pub, prior, localChanges, localChangesKnown, now)
	if !found {
		if _, err := s.store.EnsureIssueRecovery(ctx, observed); err != nil {
			s.logger.Warn("issue recovery: state create failed", "session", rec.ID)
			return
		}
		prior, found, err = s.store.GetIssueRecovery(ctx, rec.ID)
		if err != nil || !found {
			s.logger.Warn("issue recovery: state reload failed", "session", rec.ID)
			return
		}
		observed = recoveryObservation(rec, pub, prior, localChanges, localChangesKnown, now)
	}
	if pub.State == domain.IssuePublicationPRReady {
		s.finish(ctx, observed, domain.IssueRecoveryComplete, domain.IssueRecoveryReasonPRReady, now)
		return
	}
	if prior.State == domain.IssueRecoveryComplete {
		return
	}
	if !recoverable(pub) {
		s.finish(ctx, observed, domain.IssueRecoveryIncomplete, recoveryReason(pub), now)
		return
	}
	if rec.Activity.State == domain.ActivityExited {
		s.finish(ctx, observed, domain.IssueRecoveryIncomplete, domain.IssueRecoveryReasonWorkerExited, now)
		return
	}
	if prior.State == domain.IssueRecoveryIncomplete || prior.Attempts >= 1 {
		// A prior continuation either failed or was already consumed. Idle again
		// without a valid PR is terminal; never start another execution.
		s.finish(ctx, observed, domain.IssueRecoveryIncomplete, domain.IssueRecoveryReasonExhausted, now)
		return
	}
	if prior.State != domain.IssueRecoveryObserving {
		return
	}
	observed.State = domain.IssueRecoveryObserving
	if _, err := s.store.UpdateIssueRecoveryObservation(ctx, observed); err != nil {
		s.logger.Warn("issue recovery: observation update failed", "session", rec.ID)
		return
	}
	claimed, err := s.store.ClaimIssueRecovery(ctx, rec.ID, rec.InstanceID, now)
	if err != nil || !claimed {
		if err != nil {
			s.logger.Warn("issue recovery: claim failed", "session", rec.ID)
		}
		return
	}
	if err := s.sender.Send(ctx, rec.ID, continuationPrompt(rec, pub), nil); err != nil {
		observed.State = domain.IssueRecoveryIncomplete
		observed.Reason = domain.IssueRecoveryReasonContinuationFailed
		s.finish(ctx, observed, domain.IssueRecoveryIncomplete, domain.IssueRecoveryReasonContinuationFailed, now)
		s.logger.Warn("issue recovery: continuation delivery failed", "session", rec.ID)
		return
	}
	s.logger.Info("issue recovery: continuation delivered", "session", rec.ID, "attempts", 1)
}

func (s *Service) finish(ctx context.Context, observed domain.IssueRecovery, state domain.IssueRecoveryState, reason domain.IssueRecoveryReason, now time.Time) {
	observed.State = state
	observed.Reason = reason
	if _, err := s.store.FinishIssueRecovery(ctx, observed, now); err != nil {
		s.logger.Warn("issue recovery: terminal write failed", "session", observed.SessionID)
	}
}

func recoveryObservation(rec domain.SessionRecord, pub *domain.IssuePublication, prior domain.IssueRecovery, localChanges, localChangesKnown bool, now time.Time) domain.IssueRecovery {
	out := prior
	out.SessionID = rec.ID
	out.IssueID = rec.IssueID
	out.InstanceID = rec.InstanceID
	out.LocalCommits = pub.LocalCommits
	out.LocalChanges = localChanges
	out.LocalChangesKnown = localChangesKnown
	out.Push = pub.Push
	out.Turn = pub.Turn
	out.PRURL = pub.PRURL
	out.HeadSHA = pub.HeadSHA
	out.ObservedAt = now
	out.UpdatedAt = now
	return out
}

func recoverable(pub *domain.IssuePublication) bool {
	switch string(pub.Reason) {
	case string(domain.IssueRecoveryReasonIssuePRMissing), string(domain.IssueRecoveryReasonBranchNotPushed):
		return true
	default:
		return false
	}
}

func recoveryReason(pub *domain.IssuePublication) domain.IssueRecoveryReason {
	if pub.Reason == "" {
		return domain.IssueRecoveryReasonPRNotReady
	}
	return domain.IssueRecoveryReason(pub.Reason)
}

func continuationPrompt(rec domain.SessionRecord, pub *domain.IssuePublication) string {
	if pub.LocalCommits > 0 || (rec.BranchState != nil && rec.BranchState.Commits > 0) {
		return "AO issue-task recovery: continue this same task in the existing workspace. Existing local commits were found. Do not discard, reset, or duplicate work. Verify the tests, publish the existing branch, and create or update the authorized pull request for the original issue. Do not merge. This is the single automatic continuation."
	}
	return "AO issue-task recovery: continue this same task in the existing workspace. Preserve all local changes, including uncommitted files. If work already exists, verify it and publish it; otherwise complete the remaining implementation and tests. Commit only the intended work, push the branch, and create or update the authorized pull request for the original issue. Do not merge. This is the single automatic continuation."
}

func inspectWorkspace(ctx context.Context, path string) (bool, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, false
	}
	command := aoprocess.CommandContext(ctx, "git", "-C", path, "status", "--porcelain", "--untracked-files=normal")
	output, err := command.Output()
	if err != nil {
		return false, false
	}
	return len(bytes.TrimSpace(output)) > 0, true
}
