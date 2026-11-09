package session

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// deriveIssuePublication turns durable branch and attributed-PR facts into the
// explicit publication result for an issue-backed worker. ActivityIdle remains
// only an activity state; this outcome answers the separate question of whether
// the task's work was actually published.
//
// The result is intentionally derived at read time. Branch observation and PR
// attribution remain the durable facts; this value is not persisted as a
// presentation status.
func deriveIssuePublication(rec domain.SessionRecord, prs []domain.PRFacts, now time.Time) *domain.IssuePublication {
	if rec.IssueID == "" || (rec.Kind != "" && rec.Kind != domain.KindWorker) || rec.FirstSignalAt.IsZero() {
		return nil
	}
	var turn domain.IssuePublicationTurn
	switch rec.Activity.State {
	case domain.ActivityIdle:
		turn = domain.IssuePublicationTurnCompleted
	case domain.ActivityExited:
		turn = domain.IssuePublicationTurnInterrupted
	default:
		return nil
	}

	out := &domain.IssuePublication{
		State:      domain.IssuePublicationIncomplete,
		Turn:       turn,
		ObservedAt: now,
	}
	if rec.BranchState != nil {
		out.LocalCommits = rec.BranchState.Commits
		out.HeadSHA = rec.BranchState.HeadSHA
		out.BranchPushed = rec.BranchState.RemoteBranch != "" && rec.BranchState.Unpushed == 0
	}

	mismatched := false
	notReady := false
	for _, pr := range prs {
		if pr.URL == "" || pr.SourceBranch == "" || pr.HeadSHA == "" {
			continue
		}
		if rec.BranchState == nil || rec.BranchState.Branch == "" || rec.BranchState.HeadSHA == "" ||
			pr.SourceBranch != rec.BranchState.Branch || pr.HeadSHA != rec.BranchState.HeadSHA {
			mismatched = true
			continue
		}
		if pr.Draft || pr.Merged || pr.Closed {
			notReady = true
			continue
		}
		out.State = domain.IssuePublicationPRReady
		out.Reason = ""
		out.PRURL = pr.URL
		out.HeadSHA = pr.HeadSHA
		return out
	}

	if mismatched {
		out.Reason = domain.IssuePublicationReasonPRAssociationBad
		return out
	}
	if notReady {
		out.Reason = domain.IssuePublicationReasonPRNotReady
		return out
	}
	if rec.BranchState == nil || rec.BranchState.Commits == 0 {
		out.Reason = domain.IssuePublicationReasonIssuePRMissing
		return out
	}
	if rec.BranchState.RemoteBranch == "" || rec.BranchState.Unpushed > 0 {
		out.Reason = domain.IssuePublicationReasonBranchNotPushed
		return out
	}
	out.Reason = domain.IssuePublicationReasonIssuePRMissing
	return out
}
