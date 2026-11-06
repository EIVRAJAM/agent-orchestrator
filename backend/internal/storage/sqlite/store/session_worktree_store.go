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

// UpsertSessionWorktree records or updates one repo worktree for a session.
func (s *Store) UpsertSessionWorktree(ctx context.Context, row domain.SessionWorktreeRecord) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// ponytail: session_worktrees.state is unused multi-repo scaffolding; no
	// live code path sets domain.SessionWorktreeRecord.State, so it arrives
	// here as "". The generated upsert includes state in the INSERT column list
	// and the CHECK constraint rejects "". Default to 'active' (the column
	// default) so the row stays valid without touching the schema or gen code.
	// Wire a real value when multi-repo worktree lifecycle states ship.
	state := row.State
	if state == "" {
		state = "active"
	}
	repoKey := strings.TrimSpace(row.RepoKey)
	if repoKey == "" {
		var keyErr error
		repoKey, keyErr = s.sessionWorktreeRepoKeyLocked(ctx, row.SessionID, row.RepoName)
		if keyErr != nil {
			return keyErr
		}
	}
	if err := s.qw.UpsertSessionWorktree(ctx, gen.UpsertSessionWorktreeParams{
		SessionID:    row.SessionID,
		RepoName:     row.RepoName,
		RepoKey:      repoKey,
		Branch:       row.Branch,
		BaseSha:      row.BaseSHA,
		BaseRef:      row.BaseRef,
		CreationSha:  row.CreationSHA,
		WorktreePath: row.WorktreePath,
		PreservedRef: row.PreservedRef,
		State:        state,
	}); err != nil {
		return err
	}
	headSHA := row.CreationSHA
	if headSHA == "" {
		headSHA = row.BaseSHA
	}
	_, err := s.recordSessionBranchTipLocked(ctx, row.SessionID, repoKey, row.Branch, headSHA, row.WorktreePath, time.Now().UTC())
	return err
}

// GetSessionWorktree returns one session worktree row.
func (s *Store) GetSessionWorktree(ctx context.Context, sessionID domain.SessionID, repoName string) (domain.SessionWorktreeRecord, bool, error) {
	row, err := s.qr.GetSessionWorktree(ctx, gen.GetSessionWorktreeParams{SessionID: sessionID, RepoName: repoName})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SessionWorktreeRecord{}, false, nil
	}
	if err != nil {
		return domain.SessionWorktreeRecord{}, false, fmt.Errorf("get session worktree %s/%s: %w", sessionID, repoName, err)
	}
	return sessionWorktreeFromGetRow(row), true, nil
}

// ListSessionWorktrees returns every repo worktree for a session, root first.
func (s *Store) ListSessionWorktrees(ctx context.Context, sessionID domain.SessionID) ([]domain.SessionWorktreeRecord, error) {
	rows, err := s.qr.ListSessionWorktrees(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list session worktrees for %s: %w", sessionID, err)
	}
	out := make([]domain.SessionWorktreeRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionWorktreeFromListRow(row))
	}
	return out, nil
}

// DeleteSessionWorktrees deletes the per-repo worktree rows for a session.
func (s *Store) DeleteSessionWorktrees(ctx context.Context, sessionID domain.SessionID) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.DeleteSessionWorktrees(ctx, sessionID)
}

// sessionWorktreeRepoKeyLocked derives the canonical repository identity from
// the registered project/workspace origin. A missing or invalid origin yields
// an empty key, which disables ownership for that worktree rather than using a
// symbolic AO repo name as a substitute.
func (s *Store) sessionWorktreeRepoKeyLocked(ctx context.Context, sessionID domain.SessionID, repoName string) (string, error) {
	origin, err := s.qw.GetSessionProjectOrigin(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read project origin for %s: %w", sessionID, err)
	}
	if repoName != domain.RootWorkspaceRepoName {
		origin, err = s.qw.GetWorkspaceRepoOrigin(ctx, gen.GetWorkspaceRepoOriginParams{
			ID: sessionID, Name: repoName,
		})
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("read workspace repo origin for %s/%s: %w", sessionID, repoName, err)
		}
	}
	identity, err := domain.ParseRepositoryIdentity(origin)
	if err != nil || identity.Provider == "" || identity.Host == "" {
		// A missing/invalid origin only disables ownership; it must not block
		// the worktree lifecycle itself.
		return "", nil //nolint:nilerr // invalid origin disables ownership only
	}
	return identity.Key(), nil
}

func sessionWorktreeFromGetRow(row gen.GetSessionWorktreeRow) domain.SessionWorktreeRecord {
	return sessionWorktreeFromValues(row.SessionID, row.RepoName, row.RepoKey, row.Branch, row.BaseSha, row.BaseRef, row.CreationSha, row.WorktreePath, row.PreservedRef, row.State)
}

func sessionWorktreeFromListRow(row gen.ListSessionWorktreesRow) domain.SessionWorktreeRecord {
	return sessionWorktreeFromValues(row.SessionID, row.RepoName, row.RepoKey, row.Branch, row.BaseSha, row.BaseRef, row.CreationSha, row.WorktreePath, row.PreservedRef, row.State)
}

func sessionWorktreeFromValues(
	sessionID domain.SessionID, repoName, repoKey, branch, baseSHA, baseRef, creationSHA, worktreePath, preservedRef, state string,
) domain.SessionWorktreeRecord {
	return domain.SessionWorktreeRecord{
		SessionID: sessionID, RepoName: repoName, RepoKey: repoKey, Branch: branch,
		BaseSHA: baseSHA, BaseRef: baseRef, CreationSHA: creationSHA,
		WorktreePath: worktreePath, PreservedRef: preservedRef,
		// ponytail: state is read back from the DB but no caller uses it;
		// it is unused multi-repo scaffolding (see UpsertSessionWorktree above).
		State: state,
	}
}
