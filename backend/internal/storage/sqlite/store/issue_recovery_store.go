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

// GetIssueRecovery returns the durable bounded-continuation record for one
// session. Missing rows are a normal observation, not an error.
func (s *Store) GetIssueRecovery(ctx context.Context, id domain.SessionID) (domain.IssueRecovery, bool, error) {
	row, err := s.qr.GetIssueRecovery(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.IssueRecovery{}, false, nil
	}
	if err != nil {
		return domain.IssueRecovery{}, false, fmt.Errorf("get issue recovery %s: %w", id, err)
	}
	return issueRecoveryFromRow(row), true, nil
}

// EnsureIssueRecovery creates the observing row if it does not exist.
func (s *Store) EnsureIssueRecovery(ctx context.Context, rec domain.IssueRecovery) (bool, error) {
	if rec.SessionID == "" || rec.IssueID == "" || rec.InstanceID == "" {
		return false, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.EnsureIssueRecovery(ctx, gen.EnsureIssueRecoveryParams{
		SessionID:         string(rec.SessionID),
		IssueID:           string(rec.IssueID),
		InstanceID:        rec.InstanceID,
		State:             string(domain.IssueRecoveryObserving),
		Reason:            string(rec.Reason),
		LocalCommits:      int64(rec.LocalCommits),
		LocalChanges:      issueRecoveryBoolInt(rec.LocalChanges),
		LocalChangesKnown: issueRecoveryBoolInt(rec.LocalChangesKnown),
		Push:              string(rec.Push),
		Turn:              string(rec.Turn),
		PRURL:             rec.PRURL,
		HeadSha:           rec.HeadSHA,
		ObservedAt:        rec.ObservedAt,
		UpdatedAt:         rec.UpdatedAt,
	})
	if err != nil {
		return false, fmt.Errorf("ensure issue recovery %s: %w", rec.SessionID, err)
	}
	return rows > 0, nil
}

// ClaimIssueRecovery atomically consumes the one continuation attempt. It
// returns false when the row is missing, the instance changed, or an attempt
// was already consumed.
func (s *Store) ClaimIssueRecovery(ctx context.Context, id domain.SessionID, instanceID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ClaimIssueRecovery(ctx, gen.ClaimIssueRecoveryParams{
		AttemptedAt: sql.NullTime{Time: now, Valid: true},
		UpdatedAt:   now,
		SessionID:   string(id),
		InstanceID:  instanceID,
	})
	if err != nil {
		return false, fmt.Errorf("claim issue recovery %s: %w", id, err)
	}
	return rows > 0, nil
}

// MarkIssueRecoverySent records that the protected send returned successfully.
// It only transitions a row still in claimed, so a crash or a competing process
// cannot turn an unproven delivery into sent.
func (s *Store) MarkIssueRecoverySent(ctx context.Context, id domain.SessionID, instanceID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.MarkIssueRecoverySent(ctx, gen.MarkIssueRecoverySentParams{
		SentAt:     sql.NullTime{Time: now, Valid: true},
		UpdatedAt:  now,
		SessionID:  string(id),
		InstanceID: instanceID,
	})
	if err != nil {
		return false, fmt.Errorf("mark issue recovery sent %s: %w", id, err)
	}
	return rows > 0, nil
}

// MarkIssueRecoveryUncertain makes a claimed attempt terminal without claiming
// delivery. It is used when the process cannot prove the send result.
func (s *Store) MarkIssueRecoveryUncertain(ctx context.Context, id domain.SessionID, instanceID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.MarkIssueRecoveryUncertain(ctx, gen.MarkIssueRecoveryUncertainParams{
		UpdatedAt:  now,
		SessionID:  string(id),
		InstanceID: instanceID,
	})
	if err != nil {
		return false, fmt.Errorf("mark issue recovery uncertain %s: %w", id, err)
	}
	return rows > 0, nil
}

// UpdateIssueRecoveryObservation refreshes sanitized publication evidence on a
// non-terminal recovery row without changing its attempt count.
func (s *Store) UpdateIssueRecoveryObservation(ctx context.Context, rec domain.IssueRecovery) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.UpdateIssueRecoveryObservation(ctx, gen.UpdateIssueRecoveryObservationParams{
		Reason:            string(rec.Reason),
		LocalCommits:      int64(rec.LocalCommits),
		LocalChanges:      issueRecoveryBoolInt(rec.LocalChanges),
		LocalChangesKnown: issueRecoveryBoolInt(rec.LocalChangesKnown),
		Push:              string(rec.Push),
		Turn:              string(rec.Turn),
		PRURL:             rec.PRURL,
		HeadSha:           rec.HeadSHA,
		ObservedAt:        rec.ObservedAt,
		UpdatedAt:         rec.UpdatedAt,
		SessionID:         string(rec.SessionID),
		InstanceID:        rec.InstanceID,
	})
	if err != nil {
		return false, fmt.Errorf("update issue recovery observation %s: %w", rec.SessionID, err)
	}
	return rows > 0, nil
}

// FinishIssueRecovery records a terminal complete/incomplete decision and its
// sanitized evidence. A complete row is never reopened.
func (s *Store) FinishIssueRecovery(ctx context.Context, rec domain.IssueRecovery, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.FinishIssueRecovery(ctx, gen.FinishIssueRecoveryParams{
		State:             string(rec.State),
		Reason:            string(rec.Reason),
		LocalCommits:      int64(rec.LocalCommits),
		LocalChanges:      issueRecoveryBoolInt(rec.LocalChanges),
		LocalChangesKnown: issueRecoveryBoolInt(rec.LocalChangesKnown),
		Push:              string(rec.Push),
		Turn:              string(rec.Turn),
		PRURL:             rec.PRURL,
		HeadSha:           rec.HeadSHA,
		UpdatedAt:         now,
		SessionID:         string(rec.SessionID),
		InstanceID:        rec.InstanceID,
	})
	if err != nil {
		return false, fmt.Errorf("finish issue recovery %s: %w", rec.SessionID, err)
	}
	return rows > 0, nil
}

func issueRecoveryFromRow(row gen.GetIssueRecoveryRow) domain.IssueRecovery {
	var attemptedAt *time.Time
	if row.AttemptedAt.Valid {
		t := row.AttemptedAt.Time
		attemptedAt = &t
	}
	var sentAt *time.Time
	if row.SentAt.Valid {
		t := row.SentAt.Time
		sentAt = &t
	}
	return domain.IssueRecovery{
		SessionID:         domain.SessionID(row.SessionID),
		IssueID:           domain.IssueID(row.IssueID),
		InstanceID:        row.InstanceID,
		State:             domain.IssueRecoveryState(row.State),
		Reason:            domain.IssueRecoveryReason(row.Reason),
		Attempts:          int(row.Attempts),
		LocalCommits:      int(row.LocalCommits),
		LocalChanges:      row.LocalChanges != 0,
		LocalChangesKnown: row.LocalChangesKnown != 0,
		Push:              domain.IssuePublicationPush(row.Push),
		Turn:              domain.IssuePublicationTurn(row.Turn),
		PRURL:             row.PRURL,
		HeadSHA:           row.HeadSha,
		ObservedAt:        row.ObservedAt,
		AttemptedAt:       attemptedAt,
		SentAt:            sentAt,
		UpdatedAt:         row.UpdatedAt,
	}
}

func issueRecoveryBoolInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
