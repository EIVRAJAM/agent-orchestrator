package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/pushobservation"
)

const publicationRepoKey = "github|github.com|acme/demo"

func publicationRecord() domain.SessionRecord {
	return domain.SessionRecord{
		ID:            "worker-1",
		ProjectID:     "p",
		InstanceID:    "inst-1",
		Kind:          domain.KindWorker,
		IssueID:       "github:acme/demo#29",
		Activity:      domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Unix(100, 0).UTC()},
		FirstSignalAt: time.Unix(100, 0).UTC(),
		Metadata: domain.SessionMetadata{
			ConversationCheckpointState: domain.ConversationCheckpointComplete,
		},
		BranchState: &domain.SessionBranchState{
			Branch: "ao/worker-1/task", HeadSHA: "head-1", Commits: 1, Unpushed: 1,
		},
	}
}

func publicationEvidenceFor(prs []domain.PRFacts, push domain.IssuePublicationPush) issuePublicationEvidence {
	evidence := issuePublicationEvidence{
		RepoKey:    publicationRepoKey,
		Ownership:  map[string]domain.SessionPROwnership{},
		Tips:       map[string]struct{}{},
		SessionPRs: map[string]struct{}{},
		Push:       push,
		Available:  len(prs) > 0,
	}
	for _, pr := range prs {
		if pr.URL == "" {
			continue
		}
		evidence.SessionPRs[pr.URL] = struct{}{}
		evidence.Ownership[pr.URL] = domain.SessionPROwnership{
			PRURL:             pr.URL,
			SessionInstanceID: "inst-1",
			RepoKey:           publicationRepoKey,
			SourceBranch:      pr.SourceBranch,
			HeadSHA:           pr.HeadSHA,
		}
		evidence.Tips[branchTipKey(publicationRepoKey, pr.SourceBranch, pr.HeadSHA)] = struct{}{}
	}
	return evidence
}

func TestDeriveIssuePublicationOutcomes(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	validPR := domain.PRFacts{
		URL: "https://github.com/acme/demo/pull/29", SourceBranch: "ao/worker-1/task", HeadSHA: "head-1",
	}
	tests := []struct {
		name       string
		mutate     func(*domain.SessionRecord, *[]domain.PRFacts)
		push       domain.IssuePublicationPush
		wantState  domain.IssuePublicationState
		wantReason domain.IssuePublicationReason
		wantTurn   domain.IssuePublicationTurn
		wantPush   domain.IssuePublicationPush
	}{
		{
			name: "idle issue with no commits or pr is missing pr",
			mutate: func(rec *domain.SessionRecord, prs *[]domain.PRFacts) {
				rec.BranchState = &domain.SessionBranchState{Branch: "ao/worker-1/task", Commits: 0}
			},
			push:       domain.IssuePublicationPushUnknown,
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonIssuePRMissing,
			wantTurn:   domain.IssuePublicationTurnCompleted,
			wantPush:   domain.IssuePublicationPushUnknown,
		},
		{
			name:       "local commit without a remote branch is not pushed",
			push:       domain.IssuePublicationPushUnknown,
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonBranchNotPushed,
			wantTurn:   domain.IssuePublicationTurnCompleted,
			wantPush:   domain.IssuePublicationPushUnknown,
		},
		{
			name: "durable failed push is distinguished from an unknown push",
			mutate: func(rec *domain.SessionRecord, _ *[]domain.PRFacts) {
				rec.BranchState.RemoteBranch = "origin/ao/worker-1/task"
				rec.BranchState.Unpushed = 1
			},
			push:       domain.IssuePublicationPushFailed,
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonPushFailed,
			wantTurn:   domain.IssuePublicationTurnCompleted,
			wantPush:   domain.IssuePublicationPushFailed,
		},
		{
			name: "pushed branch without a pr is still missing a pr",
			mutate: func(rec *domain.SessionRecord, _ *[]domain.PRFacts) {
				rec.BranchState.RemoteBranch = "origin/ao/worker-1/task"
				rec.BranchState.Unpushed = 0
			},
			push:       domain.IssuePublicationPushSucceeded,
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonIssuePRMissing,
			wantTurn:   domain.IssuePublicationTurnCompleted,
			wantPush:   domain.IssuePublicationPushSucceeded,
		},
		{
			name: "matching open pr with exact ownership and tip is ready",
			mutate: func(rec *domain.SessionRecord, prs *[]domain.PRFacts) {
				rec.BranchState.RemoteBranch = "origin/ao/worker-1/task"
				rec.BranchState.Unpushed = 0
				*prs = []domain.PRFacts{validPR}
			},
			push:      domain.IssuePublicationPushSucceeded,
			wantState: domain.IssuePublicationPRReady,
			wantTurn:  domain.IssuePublicationTurnCompleted,
			wantPush:  domain.IssuePublicationPushSucceeded,
		},
		{
			name: "pr for another branch is rejected",
			mutate: func(_ *domain.SessionRecord, prs *[]domain.PRFacts) {
				pr := validPR
				pr.SourceBranch = "ao/worker-1/other"
				*prs = []domain.PRFacts{pr}
			},
			push:       domain.IssuePublicationPushSucceeded,
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonPRAssociationBad,
			wantTurn:   domain.IssuePublicationTurnCompleted,
			wantPush:   domain.IssuePublicationPushSucceeded,
		},
		{
			name: "pr with another head is rejected",
			mutate: func(_ *domain.SessionRecord, prs *[]domain.PRFacts) {
				pr := validPR
				pr.HeadSHA = "head-2"
				*prs = []domain.PRFacts{pr}
			},
			push:       domain.IssuePublicationPushSucceeded,
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonPRAssociationBad,
			wantTurn:   domain.IssuePublicationTurnCompleted,
			wantPush:   domain.IssuePublicationPushSucceeded,
		},
		{
			name: "matching draft pr is not ready",
			mutate: func(_ *domain.SessionRecord, prs *[]domain.PRFacts) {
				pr := validPR
				pr.Draft = true
				*prs = []domain.PRFacts{pr}
			},
			push:       domain.IssuePublicationPushSucceeded,
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonPRNotReady,
			wantTurn:   domain.IssuePublicationTurnCompleted,
			wantPush:   domain.IssuePublicationPushSucceeded,
		},
		{
			name: "exited worker after a durable completion keeps the completed turn",
			mutate: func(rec *domain.SessionRecord, _ *[]domain.PRFacts) {
				rec.Activity.State = domain.ActivityExited
			},
			push:       domain.IssuePublicationPushUnknown,
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonBranchNotPushed,
			wantTurn:   domain.IssuePublicationTurnCompleted,
			wantPush:   domain.IssuePublicationPushUnknown,
		},
		{
			name: "exited worker before a durable completion is interrupted",
			mutate: func(rec *domain.SessionRecord, _ *[]domain.PRFacts) {
				rec.Activity.State = domain.ActivityExited
				rec.Metadata.ConversationCheckpointState = domain.ConversationCheckpointPrompt
			},
			push:       domain.IssuePublicationPushUnknown,
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonBranchNotPushed,
			wantTurn:   domain.IssuePublicationTurnInterrupted,
			wantPush:   domain.IssuePublicationPushUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := publicationRecord()
			prs := []domain.PRFacts(nil)
			if tt.mutate != nil {
				tt.mutate(&rec, &prs)
			}
			got := deriveIssuePublication(rec, prs, publicationEvidenceFor(prs, tt.push), now)
			if got == nil {
				t.Fatal("deriveIssuePublication returned nil")
			}
			if got.State != tt.wantState || got.Reason != tt.wantReason || got.Turn != tt.wantTurn || got.Push != tt.wantPush {
				t.Fatalf("outcome = %+v, want state=%s reason=%s turn=%s push=%s", got, tt.wantState, tt.wantReason, tt.wantTurn, tt.wantPush)
			}
			if got.LocalCommits != rec.BranchState.Commits || got.HeadSHA != rec.BranchState.HeadSHA {
				t.Fatalf("evidence = %+v, want commits/head from branch state", got)
			}
		})
	}
}

func TestDeriveIssuePublicationRequiresDurablePROwnership(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	validPR := domain.PRFacts{
		URL: "https://github.com/acme/demo/pull/29", SourceBranch: "ao/worker-1/task", HeadSHA: "head-1",
	}
	rec := publicationRecord()
	rec.BranchState.RemoteBranch = "origin/ao/worker-1/task"
	rec.BranchState.Unpushed = 0

	t.Run("missing ownership is not ready", func(t *testing.T) {
		evidence := issuePublicationEvidence{
			RepoKey: publicationRepoKey, Available: true,
			Ownership: map[string]domain.SessionPROwnership{}, Tips: map[string]struct{}{},
			Push: domain.IssuePublicationPushSucceeded,
		}
		got := deriveIssuePublication(rec, []domain.PRFacts{validPR}, evidence, now)
		if got == nil || got.State != domain.IssuePublicationIncomplete || got.Reason != domain.IssuePublicationReasonPRAssociationBad {
			t.Fatalf("outcome = %+v, want incomplete association invalid", got)
		}
	})

	for _, tt := range []struct {
		name   string
		mutate func(*issuePublicationEvidence)
	}{
		{
			name: "foreign instance",
			mutate: func(e *issuePublicationEvidence) {
				own := e.Ownership[validPR.URL]
				own.SessionInstanceID = "inst-foreign"
				e.Ownership[validPR.URL] = own
			},
		},
		{
			name: "foreign repository",
			mutate: func(e *issuePublicationEvidence) {
				own := e.Ownership[validPR.URL]
				own.RepoKey = "github|github.com|other/repo"
				e.Ownership[validPR.URL] = own
			},
		},
		{
			name: "tips for another head",
			mutate: func(e *issuePublicationEvidence) {
				e.Tips = map[string]struct{}{branchTipKey(publicationRepoKey, validPR.SourceBranch, "head-2"): {}}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			evidence := publicationEvidenceFor([]domain.PRFacts{validPR}, domain.IssuePublicationPushSucceeded)
			tt.mutate(&evidence)
			got := deriveIssuePublication(rec, []domain.PRFacts{validPR}, evidence, now)
			if got == nil || got.State != domain.IssuePublicationIncomplete || got.Reason != domain.IssuePublicationReasonPRAssociationBad {
				t.Fatalf("outcome = %+v, want incomplete association invalid", got)
			}
		})
	}

	t.Run("ownership read failure fails closed", func(t *testing.T) {
		evidence := publicationEvidenceFor([]domain.PRFacts{validPR}, domain.IssuePublicationPushSucceeded)
		evidence.ReadFailed = true
		evidence.Available = false
		got := deriveIssuePublication(rec, []domain.PRFacts{validPR}, evidence, now)
		if got == nil || got.State != domain.IssuePublicationIncomplete || got.Reason != domain.IssuePublicationReasonOwnershipUnavailable {
			t.Fatalf("outcome = %+v, want incomplete ownership unavailable", got)
		}
	})
}

func TestDeriveIssuePublicationDoesNotInferTurnCompletionFromIdle(t *testing.T) {
	rec := publicationRecord()
	rec.Metadata.ConversationCheckpointState = domain.ConversationCheckpointPrompt
	got := deriveIssuePublication(rec, nil, publicationEvidenceFor(nil, domain.IssuePublicationPushUnknown), time.Unix(200, 0))
	if got == nil || got.Turn != domain.IssuePublicationTurnUnknown {
		t.Fatalf("outcome = %+v, want turn unknown without a completed checkpoint", got)
	}
	if got.State != domain.IssuePublicationIncomplete {
		t.Fatalf("state = %q, want incomplete", got.State)
	}
	if rec.Activity.State != domain.ActivityIdle {
		t.Fatalf("ActivityState = %q, changed by publication derivation", rec.Activity.State)
	}
}

func TestDeriveIssuePublicationOnlyAppliesToIssueWorkers(t *testing.T) {
	rec := publicationRecord()
	rec.IssueID = ""
	if got := deriveIssuePublication(rec, nil, issuePublicationEvidence{}, time.Unix(200, 0)); got != nil {
		t.Fatalf("non-issue outcome = %+v, want nil", got)
	}

	rec = publicationRecord()
	rec.Kind = domain.KindOrchestrator
	if got := deriveIssuePublication(rec, nil, issuePublicationEvidence{}, time.Unix(200, 0)); got != nil {
		t.Fatalf("orchestrator outcome = %+v, want nil", got)
	}

	rec = publicationRecord()
	rec.Activity.State = domain.ActivityActive
	if got := deriveIssuePublication(rec, nil, issuePublicationEvidence{}, time.Unix(200, 0)); got != nil {
		t.Fatalf("active issue outcome = %+v, want nil", got)
	}

	rec = publicationRecord()
	rec.FirstSignalAt = time.Time{}
	if got := deriveIssuePublication(rec, nil, issuePublicationEvidence{}, time.Unix(200, 0)); got != nil {
		t.Fatalf("pre-signal issue outcome = %+v, want nil", got)
	}
}

func TestIssuePublicationReadUsesDurableOwnershipEvidence(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	const prURL = "https://github.com/acme/demo/pull/29"
	rec := publicationRecord()
	rec.BranchState.RemoteBranch = "origin/ao/worker-1/task"
	rec.BranchState.Unpushed = 0
	pr := domain.PRFacts{URL: prURL, SourceBranch: "ao/worker-1/task", HeadSHA: "head-1"}

	st := newFakeStore()
	st.projects["p"] = domain.ProjectRecord{ID: "p", RepoOriginURL: "https://github.com/acme/demo.git"}
	st.sessions[rec.ID] = rec
	st.prs[rec.ID] = []domain.PullRequest{{URL: prURL, SessionID: rec.ID}}
	svc := &Service{store: st, dataDir: t.TempDir()}

	read := func() *domain.IssuePublication {
		t.Helper()
		sess, err := svc.toSessionWithFacts(context.Background(), rec, []domain.PRFacts{pr}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sess.IssuePublication == nil {
			t.Fatal("issue publication is nil")
		}
		return sess.IssuePublication
	}

	got := read()
	if got.State != domain.IssuePublicationIncomplete || got.Reason != domain.IssuePublicationReasonPRAssociationBad {
		t.Fatalf("outcome without ownership = %+v, want incomplete association invalid", got)
	}

	st.prOwnerships[prURL] = domain.SessionPROwnership{
		PRURL: prURL, SessionInstanceID: rec.InstanceID, RepoKey: publicationRepoKey,
		SourceBranch: pr.SourceBranch, HeadSHA: pr.HeadSHA, RegisteredAt: now,
	}
	st.branchTips[rec.InstanceID] = []domain.SessionBranchTip{{
		SessionInstanceID: rec.InstanceID, RepoKey: publicationRepoKey,
		Branch: pr.SourceBranch, HeadSHA: pr.HeadSHA,
	}}
	got = read()
	if got.State != domain.IssuePublicationPRReady || !got.OwnershipVerified || got.Push != domain.IssuePublicationPushSucceeded {
		t.Fatalf("outcome with exact ownership = %+v, want pr_ready with verified ownership and successful push", got)
	}

	st.prs[rec.ID] = nil
	got = read()
	if got.State != domain.IssuePublicationIncomplete || got.Reason != domain.IssuePublicationReasonPRAssociationBad {
		t.Fatalf("outcome with a PR from another session = %+v, want incomplete association invalid", got)
	}
	st.prs[rec.ID] = []domain.PullRequest{{URL: prURL, SessionID: rec.ID}}

	st.prOwnerships[prURL] = domain.SessionPROwnership{
		PRURL: prURL, SessionInstanceID: "foreign-instance", RepoKey: publicationRepoKey,
		SourceBranch: pr.SourceBranch, HeadSHA: pr.HeadSHA, RegisteredAt: now,
	}
	got = read()
	if got.State != domain.IssuePublicationIncomplete || got.Reason != domain.IssuePublicationReasonPRAssociationBad {
		t.Fatalf("outcome with foreign instance = %+v, want incomplete association invalid", got)
	}
}

func TestIssuePublicationReadDistinguishesRecordedPushFailure(t *testing.T) {
	rec := publicationRecord()
	dir := pushobservation.Dir(t.TempDir(), rec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "attempts"), []byte("head-1\trefs/heads/ao/worker-1/task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec.BranchState.RemoteBranch = "origin/ao/worker-1/task"
	rec.BranchState.Unpushed = 1
	st := newFakeStore()
	st.sessions[rec.ID] = rec
	svc := &Service{store: st, dataDir: filepath.Dir(filepath.Dir(dir))}
	sess, err := svc.toSessionWithFacts(context.Background(), rec, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.IssuePublication == nil || sess.IssuePublication.Push != domain.IssuePublicationPushFailed || sess.IssuePublication.Reason != domain.IssuePublicationReasonPushFailed {
		t.Fatalf("outcome = %+v, want durable push failure", sess.IssuePublication)
	}
}

func TestIssuePublicationDoesNotChangeActivityIdleStatus(t *testing.T) {
	rec := publicationRecord()
	got := deriveIssuePublication(rec, nil, publicationEvidenceFor(nil, domain.IssuePublicationPushUnknown), time.Unix(200, 0))
	if got == nil || got.State != domain.IssuePublicationIncomplete {
		t.Fatalf("outcome = %+v, want incomplete", got)
	}
	if rec.Activity.State != domain.ActivityIdle {
		t.Fatalf("ActivityState = %q, changed by publication derivation", rec.Activity.State)
	}
	if status := deriveStatus(rec, nil, time.Unix(201, 0), true); status != domain.StatusIdle {
		t.Fatalf("derived status = %q, want idle; publication outcome must not redefine activity", status)
	}
}
