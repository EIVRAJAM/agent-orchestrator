package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// EnqueueTrackerWriteback inserts one durable intent. The unique
// (project, issue, PR) key makes repeated lifecycle observations harmless.
// It returns inserted=false when an identical intent already exists.
func (s *Store) EnqueueTrackerWriteback(ctx context.Context, w domain.TrackerWriteback) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if w.Status == "" {
		w.Status = domain.TrackerWritebackPending
	}
	var claimedAt sql.NullTime
	if w.ClaimedAt != nil {
		claimedAt = sql.NullTime{Time: *w.ClaimedAt, Valid: true}
	}
	rows, err := s.qw.EnqueueTrackerWriteback(ctx, gen.EnqueueTrackerWritebackParams{
		ID:          w.ID,
		ProjectID:   string(w.ProjectID),
		SessionID:   string(w.SessionID),
		IssueID:     string(w.IssueID),
		PRURL:       w.PRURL,
		Status:      w.Status,
		Attempts:    int64(w.Attempts),
		CommentURL:  w.CommentURL,
		FailureCode: w.FailureCode,
		CreatedAt:   w.CreatedAt,
		UpdatedAt:   w.UpdatedAt,
		ClaimedAt:   claimedAt,
	})
	if err != nil {
		return false, fmt.Errorf("enqueue tracker writeback: %w", err)
	}
	return rows > 0, nil
}

// ClaimTrackerWriteback atomically claims the oldest pending row. A claimed row
// is deliberately never returned again; startup recovery turns stale claims
// into uncertain outcomes so no comment is retried blindly.
func (s *Store) ClaimTrackerWriteback(ctx context.Context, now time.Time) (domain.TrackerWriteback, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	row, err := s.qw.ClaimTrackerWriteback(ctx, gen.ClaimTrackerWritebackParams{
		ClaimedAt: sql.NullTime{Time: now, Valid: true},
		UpdatedAt: now,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TrackerWriteback{}, false, nil
	}
	if err != nil {
		return domain.TrackerWriteback{}, false, fmt.Errorf("claim tracker writeback: %w", err)
	}
	return trackerWritebackFromRow(row), true, nil
}

// FinishTrackerWriteback settles a claimed row. The status guard makes a
// repeated or stale worker unable to overwrite a terminal outcome.
func (s *Store) FinishTrackerWriteback(ctx context.Context, id, status, commentURL, failureCode string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.FinishTrackerWriteback(ctx, gen.FinishTrackerWritebackParams{
		Status:      status,
		CommentURL:  commentURL,
		FailureCode: failureCode,
		UpdatedAt:   now,
		ID:          id,
	})
	if err != nil {
		return false, fmt.Errorf("finish tracker writeback: %w", err)
	}
	return rows > 0, nil
}

// RecoverStaleTrackerWritebacks converts claims left by a previous process into
// uncertain outcomes. This is the conservative restart rule: a comment may have
// been posted before the crash, so AO does not retry it.
func (s *Store) RecoverStaleTrackerWritebacks(ctx context.Context, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.RecoverStaleTrackerWritebacks(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("recover stale tracker writebacks: %w", err)
	}
	return rows, nil
}

// ListTrackerWritebacks returns a bounded page ordered newest-first. The
// caller requests one extra row to determine hasMore.
func (s *Store) ListTrackerWritebacks(ctx context.Context, status string, limit, offset int) ([]domain.TrackerWriteback, error) {
	rows, err := s.qr.ListTrackerWritebacks(ctx, gen.ListTrackerWritebacksParams{
		Status:    status,
		RowLimit:  int64(limit),
		RowOffset: int64(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list tracker writebacks: %w", err)
	}
	out := make([]domain.TrackerWriteback, len(rows))
	for i, row := range rows {
		out[i] = trackerWritebackFromRow(row)
	}
	return out, nil
}

func trackerWritebackFromRow(row gen.TrackerWriteback) domain.TrackerWriteback {
	out := domain.TrackerWriteback{
		ID:          row.ID,
		ProjectID:   domain.ProjectID(row.ProjectID),
		SessionID:   domain.SessionID(row.SessionID),
		IssueID:     domain.IssueID(row.IssueID),
		PRURL:       row.PRURL,
		Status:      row.Status,
		Attempts:    int(row.Attempts),
		CommentURL:  row.CommentURL,
		FailureCode: row.FailureCode,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
	if row.ClaimedAt.Valid {
		out.ClaimedAt = &row.ClaimedAt.Time
	}
	return out
}
