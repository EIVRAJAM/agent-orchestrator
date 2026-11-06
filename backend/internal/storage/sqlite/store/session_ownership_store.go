package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// RecordSessionBranchTip records one exact branch/worktree relation and one
// observed commit tip for the session instance. Registration is idempotent and
// ignores empty branch/SHA inputs.
func (s *Store) RecordSessionBranchTip(
	ctx context.Context,
	sessionID domain.SessionID,
	branch, headSHA, worktreePath string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.recordSessionBranchTipLocked(ctx, sessionID, branch, headSHA, worktreePath, time.Now().UTC())
}

func (s *Store) recordSessionBranchTipLocked(
	ctx context.Context,
	sessionID domain.SessionID,
	branch, headSHA, worktreePath string,
	now time.Time,
) (bool, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return false, nil
	}
	if _, err := s.qw.RegisterSessionBranch(ctx, gen.RegisterSessionBranchParams{
		Branch:       branch,
		WorktreePath: strings.TrimSpace(worktreePath),
		RegisteredAt: now,
		SessionID:    sessionID,
	}); err != nil {
		return false, fmt.Errorf("register session branch %s for %s: %w", branch, sessionID, err)
	}
	instanceID, err := s.qw.GetSessionInstanceID(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read session instance for %s: %w", sessionID, err)
	}
	if strings.TrimSpace(instanceID) == "" {
		return false, nil
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return true, nil
	}
	if _, err := s.qw.RegisterSessionBranchTip(ctx, gen.RegisterSessionBranchTipParams{
		HeadSha:           headSHA,
		ObservedAt:        now,
		SessionInstanceID: instanceID,
		Branch:            branch,
	}); err != nil {
		return false, fmt.Errorf("register session branch tip %s/%s: %w", branch, headSHA, err)
	}
	return true, nil
}

// ListSessionBranchTips returns every exact branch tip registered for one
// concrete session instance. The observer requires a matching branch AND HEAD
// SHA before it will adopt a PR.
func (s *Store) ListSessionBranchTips(ctx context.Context, instanceID string) ([]domain.SessionBranchTip, error) {
	if strings.TrimSpace(instanceID) == "" {
		return nil, nil
	}
	rows, err := s.qr.ListSessionBranchTips(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list session branch tips for %s: %w", instanceID, err)
	}
	out := make([]domain.SessionBranchTip, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.SessionBranchTip{
			SessionInstanceID: row.SessionInstanceID,
			Branch:            row.Branch,
			HeadSHA:           row.HeadSha,
		})
	}
	return out, nil
}

// RegisterSessionPROwnership binds one PR to the exact instance, branch, and
// observed head SHA. It returns false unless the branch tip is already owned by
// that instance and no other instance owns the PR URL.
func (s *Store) RegisterSessionPROwnership(
	ctx context.Context,
	instanceID, prURL, sourceBranch, headSHA string,
	now time.Time,
) (bool, error) {
	if strings.TrimSpace(instanceID) == "" || strings.TrimSpace(prURL) == "" || strings.TrimSpace(sourceBranch) == "" || strings.TrimSpace(headSHA) == "" {
		return false, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.RegisterSessionPROwnership(ctx, gen.RegisterSessionPROwnershipParams{
		PRURL:             prURL,
		HeadSha:           headSHA,
		RegisteredAt:      now,
		SessionInstanceID: instanceID,
		SourceBranch:      sourceBranch,
	})
	if err != nil {
		return false, fmt.Errorf("register PR ownership %s: %w", prURL, err)
	}
	if rows > 0 {
		return true, nil
	}
	existing, findErr := s.qw.GetSessionPROwnership(ctx, prURL)
	if errors.Is(findErr, sql.ErrNoRows) {
		return false, nil
	}
	if findErr != nil {
		return false, fmt.Errorf("read PR ownership %s: %w", prURL, findErr)
	}
	return existing.SessionInstanceID == instanceID &&
		existing.SourceBranch == sourceBranch &&
		existing.HeadSha == headSHA, nil
}

// RefreshSessionPROwnership advances an already-owned PR to a new head only
// when that head is a registered tip of the same instance/branch. It returns
// false for missing ownership, a different branch, or an unregistered head.
func (s *Store) RefreshSessionPROwnership(
	ctx context.Context,
	instanceID, prURL, sourceBranch, headSHA string,
	now time.Time,
) (bool, error) {
	if strings.TrimSpace(instanceID) == "" || strings.TrimSpace(prURL) == "" ||
		strings.TrimSpace(sourceBranch) == "" || strings.TrimSpace(headSHA) == "" {
		return false, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.RefreshSessionPROwnership(ctx, gen.RefreshSessionPROwnershipParams{
		NewHeadSha:           headSHA,
		NewRegisteredAt:      now,
		TargetPRURL:          prURL,
		ExpectedInstanceID:   instanceID,
		ExpectedSourceBranch: sourceBranch,
	})
	if err != nil {
		return false, fmt.Errorf("refresh PR ownership %s: %w", prURL, err)
	}
	return rows > 0, nil
}

// GetSessionPROwnership reads the live ownership chain for a PR. A missing
// branch ownership row or tip makes the record unavailable (fail closed).
func (s *Store) GetSessionPROwnership(ctx context.Context, prURL string) (domain.SessionPROwnership, bool, error) {
	row, err := s.qr.GetSessionPROwnership(ctx, prURL)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SessionPROwnership{}, false, nil
	}
	if err != nil {
		return domain.SessionPROwnership{}, false, fmt.Errorf("get PR ownership %s: %w", prURL, err)
	}
	return domain.SessionPROwnership{
		PRURL:             row.PRURL,
		SessionInstanceID: row.SessionInstanceID,
		SourceBranch:      row.SourceBranch,
		HeadSHA:           row.HeadSha,
		RegisteredAt:      row.RegisteredAt,
	}, true, nil
}
