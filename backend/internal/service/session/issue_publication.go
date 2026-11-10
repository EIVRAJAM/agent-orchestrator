package session

import (
	"context"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/pushobservation"
)

// issuePublicationEvidence is the durable ownership and push evidence read
// once per session read. Ownership is deliberately kept separate from PRFacts:
// a PR fact says what a provider observed, while this evidence says that the
// exact PR belongs to this concrete session instance, repository, branch and
// tip.
type issuePublicationEvidence struct {
	RepoKey    string
	Ownership  map[string]domain.SessionPROwnership
	Tips       map[string]struct{}
	SessionPRs map[string]struct{}
	Push       domain.IssuePublicationPush
	Available  bool
	ReadFailed bool
}

type issuePublicationOwnershipStore interface {
	GetSessionPROwnership(ctx context.Context, prURL string) (domain.SessionPROwnership, bool, error)
	ListSessionBranchTips(ctx context.Context, instanceID string) ([]domain.SessionBranchTip, error)
}

// deriveIssuePublication turns durable branch, push, ownership, and
// attributed-PR facts into the explicit publication result for an issue-backed
// worker. ActivityIdle remains only an activity state; completion requires the
// durable conversation checkpoint written by the lifecycle manager.
//
// The result is intentionally derived at read time. Branch observation, push
// attempts, ownership, and PR attribution remain the durable facts; this value
// is not persisted as a presentation status.
func deriveIssuePublication(rec domain.SessionRecord, prs []domain.PRFacts, evidence issuePublicationEvidence, now time.Time) *domain.IssuePublication {
	if rec.IssueID == "" || (rec.Kind != "" && rec.Kind != domain.KindWorker) || rec.FirstSignalAt.IsZero() {
		return nil
	}
	var turn domain.IssuePublicationTurn
	switch {
	case rec.Metadata.ConversationCheckpointState == domain.ConversationCheckpointComplete &&
		(rec.Activity.State == domain.ActivityIdle || rec.Activity.State == domain.ActivityExited):
		turn = domain.IssuePublicationTurnCompleted
	case rec.Activity.State == domain.ActivityExited:
		turn = domain.IssuePublicationTurnInterrupted
	case rec.Activity.State == domain.ActivityIdle:
		turn = domain.IssuePublicationTurnUnknown
	default:
		return nil
	}

	out := &domain.IssuePublication{
		State:      domain.IssuePublicationIncomplete,
		Turn:       turn,
		Push:       evidence.Push,
		ObservedAt: now,
	}
	if out.Push == "" {
		out.Push = domain.IssuePublicationPushUnknown
	}
	if rec.BranchState != nil {
		out.LocalCommits = rec.BranchState.Commits
		out.HeadSHA = rec.BranchState.HeadSHA
		out.BranchPushed = out.Push == domain.IssuePublicationPushSucceeded
	}

	mismatched := false
	notReady := false
	ownershipUnavailable := evidence.ReadFailed
	for _, pr := range prs {
		if pr.URL == "" || pr.SourceBranch == "" || pr.HeadSHA == "" {
			continue
		}
		if rec.BranchState == nil || rec.BranchState.Branch == "" || rec.BranchState.HeadSHA == "" ||
			pr.SourceBranch != rec.BranchState.Branch || pr.HeadSHA != rec.BranchState.HeadSHA {
			mismatched = true
			continue
		}
		if !evidence.Available {
			ownershipUnavailable = true
			continue
		}
		if !evidenceOwnsExactPR(rec, pr, evidence) {
			mismatched = true
			continue
		}
		out.OwnershipVerified = true
		if out.Push != domain.IssuePublicationPushSucceeded {
			if out.Push == domain.IssuePublicationPushFailed {
				out.Reason = domain.IssuePublicationReasonPushFailed
			} else {
				out.Reason = domain.IssuePublicationReasonBranchNotPushed
			}
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
	if ownershipUnavailable {
		out.Reason = domain.IssuePublicationReasonOwnershipUnavailable
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
	if out.Push != domain.IssuePublicationPushSucceeded {
		if out.Push == domain.IssuePublicationPushFailed {
			out.Reason = domain.IssuePublicationReasonPushFailed
		} else {
			out.Reason = domain.IssuePublicationReasonBranchNotPushed
		}
		return out
	}
	out.Reason = domain.IssuePublicationReasonIssuePRMissing
	return out
}

func evidenceOwnsExactPR(rec domain.SessionRecord, pr domain.PRFacts, evidence issuePublicationEvidence) bool {
	if rec.InstanceID == "" || evidence.RepoKey == "" {
		return false
	}
	ownership, ok := evidence.Ownership[pr.URL]
	if !ok {
		return false
	}
	if _, ok := evidence.SessionPRs[pr.URL]; !ok {
		return false
	}
	if ownership.PRURL != pr.URL ||
		ownership.SessionInstanceID != rec.InstanceID ||
		ownership.RepoKey != evidence.RepoKey ||
		ownership.SourceBranch != pr.SourceBranch ||
		ownership.HeadSHA != pr.HeadSHA {
		return false
	}
	if _, ok := evidence.Tips[branchTipKey(evidence.RepoKey, pr.SourceBranch, pr.HeadSHA)]; !ok {
		return false
	}
	return true
}

func branchTipKey(repoKey, branch, headSHA string) string {
	return repoKey + "\x00" + branch + "\x00" + headSHA
}

// loadIssuePublicationEvidence reads the durable ownership chain and push
// attempt marker for an issue worker. Missing ownership, a foreign instance or
// repository, an unregistered tip, or a read error all fail closed later.
func (s *Service) loadIssuePublicationEvidence(ctx context.Context, rec domain.SessionRecord, prs []domain.PRFacts) issuePublicationEvidence {
	evidence := issuePublicationEvidence{
		Ownership:  map[string]domain.SessionPROwnership{},
		Tips:       map[string]struct{}{},
		SessionPRs: map[string]struct{}{},
	}
	if rec.IssueID == "" || (rec.Kind != "" && rec.Kind != domain.KindWorker) || rec.InstanceID == "" {
		return evidence
	}
	if rec.Activity.State != domain.ActivityIdle && rec.Activity.State != domain.ActivityExited {
		return evidence
	}
	if rec.BranchState != nil {
		evidence.Push = pushFor(rec, s.dataDir)
	}

	hasPR := false
	for _, pr := range prs {
		if pr.URL != "" && pr.SourceBranch != "" && pr.HeadSHA != "" {
			hasPR = true
			break
		}
	}
	if !hasPR {
		return evidence
	}

	project, ok, err := s.sessionProject(ctx, rec)
	if err != nil || !ok {
		evidence.ReadFailed = true
		return evidence
	}
	origin := project.RepoOriginURL
	if strings.TrimSpace(project.Config.CanonicalRepoURL) != "" {
		origin = project.Config.CanonicalRepoURL
	}
	identity, err := domain.ParseRepositoryIdentity(origin)
	if err != nil || identity.Provider == "" || identity.Host == "" {
		evidence.ReadFailed = true
		return evidence
	}
	evidence.RepoKey = identity.Key()

	store, ok := s.store.(issuePublicationOwnershipStore)
	if !ok {
		evidence.ReadFailed = true
		return evidence
	}
	sessionPRs, err := s.store.ListPRsBySession(ctx, rec.ID)
	if err != nil {
		evidence.ReadFailed = true
		return evidence
	}
	for _, pr := range sessionPRs {
		if pr.URL != "" {
			evidence.SessionPRs[pr.URL] = struct{}{}
		}
	}
	tips, err := store.ListSessionBranchTips(ctx, rec.InstanceID)
	if err != nil {
		evidence.ReadFailed = true
		return evidence
	}
	for _, tip := range tips {
		if tip.SessionInstanceID != rec.InstanceID {
			continue
		}
		evidence.Tips[branchTipKey(tip.RepoKey, tip.Branch, tip.HeadSHA)] = struct{}{}
	}
	for _, pr := range prs {
		if pr.URL == "" {
			continue
		}
		ownership, found, err := store.GetSessionPROwnership(ctx, pr.URL)
		if err != nil {
			evidence.ReadFailed = true
			return evidence
		}
		if found {
			evidence.Ownership[pr.URL] = ownership
		}
	}
	evidence.Available = true
	return evidence
}

func pushFor(rec domain.SessionRecord, dataDir string) domain.IssuePublicationPush {
	if rec.BranchState == nil || rec.BranchState.Branch == "" || rec.BranchState.HeadSHA == "" {
		return domain.IssuePublicationPushUnknown
	}
	if rec.BranchState.RemoteBranch != "" && rec.BranchState.Unpushed == 0 &&
		rec.BranchState.RemoteHeadSHA == rec.BranchState.HeadSHA {
		return domain.IssuePublicationPushSucceeded
	}
	// A pre-push hook proves only that an attempt started. The observer may not
	// have persisted the resulting remote ref yet, so an attempt without a
	// confirmed remote head is not a failure. The current flow has no safe point
	// to capture git's exit code; a direct failure would need its own durable
	// result fact before this can ever return PushFailed.
	attempted, err := pushobservation.Attempted(
		dataDir,
		rec.ID,
		rec.InstanceID,
		strings.TrimSpace(rec.Metadata.RuntimeLaunchID),
		rec.BranchState.Branch,
		rec.BranchState.HeadSHA,
	)
	if err != nil {
		return domain.IssuePublicationPushUnknown
	}
	if attempted {
		return domain.IssuePublicationPushAttemptedUnconfirmed
	}
	return domain.IssuePublicationPushUnknown
}
