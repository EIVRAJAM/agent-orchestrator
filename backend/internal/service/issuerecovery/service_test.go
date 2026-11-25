package issuerecovery

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeStore struct {
	sessions []domain.SessionRecord
	rows     map[domain.SessionID]domain.IssueRecovery
	ensure   int
	claims   int
	finishes int
}

func newFakeStore(rec domain.SessionRecord) *fakeStore {
	return &fakeStore{sessions: []domain.SessionRecord{rec}, rows: map[domain.SessionID]domain.IssueRecovery{}}
}

func (f *fakeStore) ListAllSessions(context.Context) ([]domain.SessionRecord, error) {
	return append([]domain.SessionRecord(nil), f.sessions...), nil
}

func (f *fakeStore) GetIssueRecovery(_ context.Context, id domain.SessionID) (domain.IssueRecovery, bool, error) {
	row, ok := f.rows[id]
	return row, ok, nil
}

func (f *fakeStore) EnsureIssueRecovery(_ context.Context, rec domain.IssueRecovery) (bool, error) {
	f.ensure++
	if _, ok := f.rows[rec.SessionID]; ok {
		return false, nil
	}
	rec.State = domain.IssueRecoveryObserving
	rec.Attempts = 0
	f.rows[rec.SessionID] = rec
	return true, nil
}

func (f *fakeStore) UpdateIssueRecoveryObservation(_ context.Context, rec domain.IssueRecovery) (bool, error) {
	row, ok := f.rows[rec.SessionID]
	if !ok || row.InstanceID != rec.InstanceID || (row.State != domain.IssueRecoveryObserving && row.State != domain.IssueRecoveryContinued) {
		return false, nil
	}
	row.Reason, row.LocalCommits, row.Push, row.Turn = rec.Reason, rec.LocalCommits, rec.Push, rec.Turn
	row.PRURL, row.HeadSHA, row.ObservedAt, row.UpdatedAt = rec.PRURL, rec.HeadSHA, rec.ObservedAt, rec.UpdatedAt
	f.rows[rec.SessionID] = row
	return true, nil
}

func (f *fakeStore) ClaimIssueRecovery(_ context.Context, id domain.SessionID, instanceID string, now time.Time) (bool, error) {
	f.claims++
	row, ok := f.rows[id]
	if !ok || row.InstanceID != instanceID || row.State != domain.IssueRecoveryObserving || row.Attempts != 0 {
		return false, nil
	}
	row.State = domain.IssueRecoveryContinued
	row.Reason = domain.IssueRecoveryReasonContinuationSent
	row.Attempts = 1
	row.AttemptedAt = &now
	f.rows[id] = row
	return true, nil
}

func (f *fakeStore) FinishIssueRecovery(_ context.Context, rec domain.IssueRecovery, now time.Time) (bool, error) {
	f.finishes++
	row, ok := f.rows[rec.SessionID]
	if !ok || row.State == domain.IssueRecoveryComplete || row.InstanceID != rec.InstanceID {
		return false, nil
	}
	row.State, row.Reason = rec.State, rec.Reason
	row.LocalCommits, row.Push, row.Turn = rec.LocalCommits, rec.Push, rec.Turn
	row.PRURL, row.HeadSHA, row.UpdatedAt = rec.PRURL, rec.HeadSHA, now
	f.rows[rec.SessionID] = row
	return true, nil
}

type fakeSessions struct {
	sessions map[domain.SessionID]domain.Session
}

func (f fakeSessions) Get(_ context.Context, id domain.SessionID) (domain.Session, error) {
	sess, ok := f.sessions[id]
	if !ok {
		return domain.Session{}, errors.New("missing session")
	}
	return sess, nil
}

type fakeSender struct {
	ids     []domain.SessionID
	prompts []string
	err     error
}

func (f *fakeSender) Send(_ context.Context, id domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	f.ids = append(f.ids, id)
	f.prompts = append(f.prompts, message)
	return f.err
}

func recoveryFixture(incompleteReason domain.IssuePublicationReason, commits int) (domain.Session, *fakeStore, *fakeSender) {
	rec := domain.SessionRecord{
		ID:            "worker-1",
		IssueID:       "github:acme/demo#1",
		InstanceID:    "inst-1",
		Kind:          domain.KindWorker,
		Activity:      domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Unix(100, 0).UTC()},
		FirstSignalAt: time.Unix(100, 0).UTC(),
	}
	if commits > 0 {
		rec.BranchState = &domain.SessionBranchState{Branch: "ao/worker-1/task", HeadSHA: "head-1", Commits: commits, Unpushed: commits}
	}
	pub := &domain.IssuePublication{
		State:             domain.IssuePublicationIncomplete,
		Reason:            incompleteReason,
		Turn:              domain.IssuePublicationTurnUnknown,
		Push:              domain.IssuePublicationPushUnknown,
		LocalCommits:      commits,
		BranchPushed:      false,
		OwnershipVerified: true,
		ObservedAt:        time.Unix(100, 0).UTC(),
	}
	store := newFakeStore(rec)
	sender := &fakeSender{}
	return domain.Session{SessionRecord: rec, IssuePublication: pub}, store, sender
}

func runScan(t *testing.T, sess domain.Session, store *fakeStore, sender *fakeSender) *Service {
	t.Helper()
	svc := New(store, fakeSessions{sessions: map[domain.SessionID]domain.Session{sess.ID: sess}}, sender, nil, time.Minute)
	svc.clock = func() time.Time { return time.Unix(200, 0).UTC() }
	if err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestIncompleteWorkerGetsAtMostOneContinuation(t *testing.T) {
	sess, store, sender := recoveryFixture(domain.IssuePublicationReasonIssuePRMissing, 0)
	runScan(t, sess, store, sender)
	if len(sender.ids) != 1 || sender.ids[0] != "worker-1" {
		t.Fatalf("continuation sends = %+v, want one for worker-1", sender.ids)
	}
	if !strings.Contains(sender.prompts[0], "single automatic continuation") {
		t.Fatalf("continuation prompt = %q", sender.prompts[0])
	}
	row := store.rows["worker-1"]
	if row.State != domain.IssueRecoveryContinued || row.Attempts != 1 || row.AttemptedAt == nil {
		t.Fatalf("recovery row = %+v, want continued with one attempt", row)
	}
	runScan(t, sess, store, sender)
	if len(sender.ids) != 1 {
		t.Fatalf("second scan sent %d continuations, want no duplicate", len(sender.ids))
	}
	row = store.rows["worker-1"]
	if row.State != domain.IssueRecoveryIncomplete || row.Reason != domain.IssueRecoveryReasonExhausted || row.Attempts != 1 {
		t.Fatalf("row after second scan = %+v, want incomplete exhausted with one attempt", row)
	}
}

func TestExactPRPreventsContinuation(t *testing.T) {
	sess, store, sender := recoveryFixture(domain.IssuePublicationReasonIssuePRMissing, 0)
	sess.IssuePublication.State = domain.IssuePublicationPRReady
	sess.IssuePublication.PRURL = "https://example.test/pull/1"
	sess.IssuePublication.HeadSHA = "head-1"
	sess.IssuePublication.BranchPushed = true
	runScan(t, sess, store, sender)
	if len(sender.ids) != 0 {
		t.Fatalf("sent %d continuations for a ready PR", len(sender.ids))
	}
	if row := store.rows["worker-1"]; row.State != domain.IssueRecoveryComplete || row.Reason != domain.IssueRecoveryReasonPRReady {
		t.Fatalf("recovery row = %+v, want complete/pr_ready", row)
	}
}

func TestExistingCommitPromptPublishesWithoutReimplementation(t *testing.T) {
	sess, store, sender := recoveryFixture(domain.IssuePublicationReasonIssuePRMissing, 1)
	runScan(t, sess, store, sender)
	if len(sender.prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(sender.prompts))
	}
	for _, want := range []string{"Existing local commits", "publish", "Do not discard"} {
		if !strings.Contains(sender.prompts[0], want) {
			t.Fatalf("prompt missing %q: %s", want, sender.prompts[0])
		}
	}
}

func TestDirtyWorkspaceIsRecordedWithoutChangingWorkspace(t *testing.T) {
	sess, store, sender := recoveryFixture(domain.IssuePublicationReasonIssuePRMissing, 0)
	sess.Metadata.WorkspacePath = "/workspace/worker-1"
	svc := New(store, fakeSessions{sessions: map[domain.SessionID]domain.Session{sess.ID: sess}}, sender, nil, time.Minute)
	svc.clock = func() time.Time { return time.Unix(200, 0).UTC() }
	svc.inspect = func(context.Context, string) (bool, bool) { return true, true }
	if err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := store.rows["worker-1"]
	if !row.LocalChanges || !row.LocalChangesKnown {
		t.Fatalf("recovery row = %+v, want known local changes", row)
	}
	if len(sender.ids) != 1 {
		t.Fatalf("continuation sends = %d, want 1", len(sender.ids))
	}
}

func TestNoLocalCommitPromptPreservesChangesAndContinuesOriginalTask(t *testing.T) {
	sess, store, sender := recoveryFixture(domain.IssuePublicationReasonIssuePRMissing, 0)
	runScan(t, sess, store, sender)
	if len(sender.prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(sender.prompts))
	}
	for _, want := range []string{"Preserve all local changes", "remaining implementation", "authorized pull request"} {
		if !strings.Contains(sender.prompts[0], want) {
			t.Fatalf("prompt missing %q: %s", want, sender.prompts[0])
		}
	}
}

func TestNonRecoverablePublicationFailsClosed(t *testing.T) {
	for _, reason := range []domain.IssuePublicationReason{
		domain.IssuePublicationReasonPRNotReady,
		domain.IssuePublicationReasonPushFailed,
		domain.IssuePublicationReasonPRAssociationBad,
		domain.IssuePublicationReasonOwnershipUnavailable,
	} {
		t.Run(string(reason), func(t *testing.T) {
			sess, store, sender := recoveryFixture(reason, 0)
			runScan(t, sess, store, sender)
			if len(sender.ids) != 0 {
				t.Fatalf("sent %d continuations for non-recoverable reason %s", len(sender.ids), reason)
			}
			row := store.rows["worker-1"]
			if row.State != domain.IssueRecoveryIncomplete || row.Reason != domain.IssueRecoveryReason(reason) {
				t.Fatalf("recovery row = %+v, want incomplete/%s", row, reason)
			}
		})
	}
}

func TestSendFailureIsTerminalAndNotRetried(t *testing.T) {
	sess, store, sender := recoveryFixture(domain.IssuePublicationReasonIssuePRMissing, 0)
	sender.err = errors.New("send unavailable")
	runScan(t, sess, store, sender)
	row := store.rows["worker-1"]
	if row.State != domain.IssueRecoveryIncomplete || row.Reason != domain.IssueRecoveryReasonContinuationFailed {
		t.Fatalf("row after send failure = %+v", row)
	}
	sender.err = nil
	runScan(t, sess, store, sender)
	if len(sender.ids) != 1 {
		t.Fatalf("send retried after a consumed attempt: %d", len(sender.ids))
	}
}

func TestRestartDoesNotResetConsumedAttempt(t *testing.T) {
	sess, store, sender := recoveryFixture(domain.IssuePublicationReasonIssuePRMissing, 0)
	runScan(t, sess, store, sender)
	if len(sender.ids) != 1 {
		t.Fatalf("first service sends = %d, want 1", len(sender.ids))
	}
	restarted := &fakeSender{}
	runScan(t, sess, store, restarted)
	if len(restarted.ids) != 0 {
		t.Fatalf("restarted service sent %d continuations, want 0", len(restarted.ids))
	}
	if row := store.rows["worker-1"]; row.Attempts != 1 || row.State != domain.IssueRecoveryIncomplete {
		t.Fatalf("persisted row = %+v, want one consumed attempt", row)
	}
}

func TestActiveWorkerIsNotContinued(t *testing.T) {
	sess, store, sender := recoveryFixture(domain.IssuePublicationReasonIssuePRMissing, 0)
	sess.Activity.State = domain.ActivityActive
	sess.IssuePublication = nil
	runScan(t, sess, store, sender)
	if len(sender.ids) != 0 {
		t.Fatalf("active worker was continued %d times", len(sender.ids))
	}
	if store.claims != 0 {
		t.Fatalf("active worker consumed %d claims", store.claims)
	}
}

func TestActivityIdleRemainsActivityOnly(t *testing.T) {
	sess, store, sender := recoveryFixture(domain.IssuePublicationReasonIssuePRMissing, 0)
	runScan(t, sess, store, sender)
	if sess.Activity.State != domain.ActivityIdle {
		t.Fatalf("activity = %q, want idle unchanged", sess.Activity.State)
	}
}

func TestClaimFailureDoesNotSend(t *testing.T) {
	sess, store, sender := recoveryFixture(domain.IssuePublicationReasonIssuePRMissing, 0)
	store.rows["worker-1"] = domain.IssueRecovery{
		SessionID: "worker-1", IssueID: sess.IssueID, InstanceID: "inst-1",
		State: domain.IssueRecoveryObserving, Attempts: 1,
	}
	runScan(t, sess, store, sender)
	if len(sender.ids) != 0 {
		t.Fatalf("sent after an already consumed attempt: %d", len(sender.ids))
	}
}
