package session

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func publicationRecord() domain.SessionRecord {
	return domain.SessionRecord{
		ID:            "worker-1",
		Kind:          domain.KindWorker,
		IssueID:       "github:acme/demo#29",
		Activity:      domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Unix(100, 0).UTC()},
		FirstSignalAt: time.Unix(100, 0).UTC(),
		BranchState: &domain.SessionBranchState{
			Branch: "ao/worker-1/task", HeadSHA: "head-1", Commits: 1, Unpushed: 1,
		},
	}
}

func TestDeriveIssuePublicationOutcomes(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	validPR := domain.PRFacts{
		URL: "https://github.com/acme/demo/pull/29", SourceBranch: "ao/worker-1/task", HeadSHA: "head-1",
	}
	tests := []struct {
		name       string
		mutate     func(*domain.SessionRecord, *[]domain.PRFacts)
		wantState  domain.IssuePublicationState
		wantReason domain.IssuePublicationReason
		wantTurn   domain.IssuePublicationTurn
	}{
		{
			name: "idle issue with no commits or pr is missing pr",
			mutate: func(rec *domain.SessionRecord, prs *[]domain.PRFacts) {
				rec.BranchState = &domain.SessionBranchState{Branch: "ao/worker-1/task", Commits: 0}
			},
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonIssuePRMissing,
			wantTurn:   domain.IssuePublicationTurnCompleted,
		},
		{
			name:       "local commit without a remote branch is not pushed",
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonBranchNotPushed,
			wantTurn:   domain.IssuePublicationTurnCompleted,
		},
		{
			name: "push failed or partial leaves unpushed commits",
			mutate: func(rec *domain.SessionRecord, _ *[]domain.PRFacts) {
				rec.BranchState.RemoteBranch = "origin/ao/worker-1/task"
				rec.BranchState.Unpushed = 1
			},
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonBranchNotPushed,
			wantTurn:   domain.IssuePublicationTurnCompleted,
		},
		{
			name: "pushed branch without a pr is still missing a pr",
			mutate: func(rec *domain.SessionRecord, _ *[]domain.PRFacts) {
				rec.BranchState.RemoteBranch = "origin/ao/worker-1/task"
				rec.BranchState.Unpushed = 0
			},
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonIssuePRMissing,
			wantTurn:   domain.IssuePublicationTurnCompleted,
		},
		{
			name: "matching open pr is ready",
			mutate: func(rec *domain.SessionRecord, prs *[]domain.PRFacts) {
				rec.BranchState.RemoteBranch = "origin/ao/worker-1/task"
				rec.BranchState.Unpushed = 0
				*prs = []domain.PRFacts{validPR}
			},
			wantState: domain.IssuePublicationPRReady,
			wantTurn:  domain.IssuePublicationTurnCompleted,
		},
		{
			name: "pr for another branch is rejected",
			mutate: func(_ *domain.SessionRecord, prs *[]domain.PRFacts) {
				pr := validPR
				pr.SourceBranch = "ao/worker-1/other"
				*prs = []domain.PRFacts{pr}
			},
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonPRAssociationBad,
			wantTurn:   domain.IssuePublicationTurnCompleted,
		},
		{
			name: "pr with another head is rejected",
			mutate: func(_ *domain.SessionRecord, prs *[]domain.PRFacts) {
				pr := validPR
				pr.HeadSHA = "head-2"
				*prs = []domain.PRFacts{pr}
			},
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonPRAssociationBad,
			wantTurn:   domain.IssuePublicationTurnCompleted,
		},
		{
			name: "matching draft pr is not ready",
			mutate: func(_ *domain.SessionRecord, prs *[]domain.PRFacts) {
				pr := validPR
				pr.Draft = true
				*prs = []domain.PRFacts{pr}
			},
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonPRNotReady,
			wantTurn:   domain.IssuePublicationTurnCompleted,
		},
		{
			name: "exited worker reports an interrupted turn",
			mutate: func(rec *domain.SessionRecord, _ *[]domain.PRFacts) {
				rec.Activity.State = domain.ActivityExited
			},
			wantState:  domain.IssuePublicationIncomplete,
			wantReason: domain.IssuePublicationReasonBranchNotPushed,
			wantTurn:   domain.IssuePublicationTurnInterrupted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := publicationRecord()
			prs := []domain.PRFacts(nil)
			if tt.mutate != nil {
				tt.mutate(&rec, &prs)
			}
			got := deriveIssuePublication(rec, prs, now)
			if got == nil {
				t.Fatal("deriveIssuePublication returned nil")
			}
			if got.State != tt.wantState || got.Reason != tt.wantReason || got.Turn != tt.wantTurn {
				t.Fatalf("outcome = %+v, want state=%s reason=%s turn=%s", got, tt.wantState, tt.wantReason, tt.wantTurn)
			}
			if got.LocalCommits != rec.BranchState.Commits || got.HeadSHA != rec.BranchState.HeadSHA {
				t.Fatalf("evidence = %+v, want commits/head from branch state", got)
			}
		})
	}
}

func TestDeriveIssuePublicationOnlyAppliesToIssueWorkers(t *testing.T) {
	rec := publicationRecord()
	rec.IssueID = ""
	if got := deriveIssuePublication(rec, nil, time.Unix(200, 0)); got != nil {
		t.Fatalf("non-issue outcome = %+v, want nil", got)
	}

	rec = publicationRecord()
	rec.Kind = domain.KindOrchestrator
	if got := deriveIssuePublication(rec, nil, time.Unix(200, 0)); got != nil {
		t.Fatalf("orchestrator outcome = %+v, want nil", got)
	}

	rec = publicationRecord()
	rec.Activity.State = domain.ActivityActive
	if got := deriveIssuePublication(rec, nil, time.Unix(200, 0)); got != nil {
		t.Fatalf("active issue outcome = %+v, want nil", got)
	}

	rec = publicationRecord()
	rec.FirstSignalAt = time.Time{}
	if got := deriveIssuePublication(rec, nil, time.Unix(200, 0)); got != nil {
		t.Fatalf("pre-signal issue outcome = %+v, want nil", got)
	}
}

func TestIssuePublicationDoesNotChangeActivityIdleStatus(t *testing.T) {
	rec := publicationRecord()
	got := deriveIssuePublication(rec, nil, time.Unix(200, 0))
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
