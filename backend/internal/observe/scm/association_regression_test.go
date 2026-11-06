package scm

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func ownershipObserver(store *fakeStore, provider *fakeProvider, now time.Time) *Observer {
	provider.identity = ports.SCMIdentity{Login: "alice", Human: true}
	for repo, prs := range provider.openPRs {
		for i := range prs {
			if prs[i].Author == "" {
				prs[i].Author = "alice"
			}
		}
		provider.openPRs[repo] = prs
	}
	return New(provider, store, &fakeLifecycle{}, Config{
		Clock: func() time.Time { return now }, Tick: time.Hour,
		Logger: quietSlog(), ScopedIdentityResolver: provider,
	})
}

func TestPoll_RequiresDurableInstanceBranchAndHead(t *testing.T) {
	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "ao/praxas-mini-1/root"
	store.sessions[0].InstanceID = "inst-current"
	store.sessions[0].CreatedAt = time.Unix(1, 0).UTC()
	store.addBranchTip("inst-current", "ao/praxas-mini-1/root", "root-sha")
	store.addBranchTip("inst-current", "ao/praxas-mini-1/stack", "stack-sha")

	future := time.Unix(9_999_999_999, 0).UTC()
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs: map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {
			// Historical child under the reused prefix. Its timestamp is AFTER
			// the session; time is deliberately not ownership evidence.
			{URL: "https://github.com/o/r/pull/4", Number: 4, SourceBranch: "ao/praxas-mini-1/old-task", HeadRepo: "o/r", HeadSHA: "old-sha", CreatedAtProvider: future},
			// Exact branch, but a different head that this instance never
			// observed in its workspace.
			{URL: "https://github.com/o/r/pull/6", Number: 6, SourceBranch: "ao/praxas-mini-1/root", HeadRepo: "o/r", HeadSHA: "foreign-sha", CreatedAtProvider: future},
			// Explicitly registered stacked branch.
			{URL: "https://github.com/o/r/pull/7", Number: 7, SourceBranch: "ao/praxas-mini-1/stack", HeadRepo: "o/r", HeadSHA: "stack-sha", CreatedAtProvider: future},
			// Exact root branch + registered tip.
			{URL: "https://github.com/o/r/pull/8", Number: 8, SourceBranch: "ao/praxas-mini-1/root", HeadRepo: "o/r", HeadSHA: "root-sha", CreatedAtProvider: future},
		}},
	}
	obs := ownershipObserver(store, provider, time.Unix(2, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := map[int]bool{}
	for _, write := range store.writes {
		got[write.pr.Number] = true
	}
	if got[4] || got[6] {
		t.Fatalf("unowned PRs were associated: writes=%v ownership=%+v", got, store.prOwnerships)
	}
	if !got[7] || !got[8] {
		t.Fatalf("registered branch tips were not associated: writes=%v", got)
	}
	if store.prOwnerships["https://github.com/o/r/pull/4"].PRURL != "" ||
		store.prOwnerships["https://github.com/o/r/pull/6"].PRURL != "" {
		t.Fatal("ownership row was created for an unowned PR")
	}
}

func TestPoll_StackedPRAssociatesOnlyAfterExplicitRegistration(t *testing.T) {
	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "ao/p-1/root"
	store.sessions[0].InstanceID = "inst-p-1"
	store.addBranchTip("inst-p-1", "ao/p-1/root", "root-sha")
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs: map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {
			{URL: "https://github.com/o/r/pull/2", Number: 2, SourceBranch: "ao/p-1/stack", HeadRepo: "o/r", HeadSHA: "stack-sha"},
		}},
	}
	obs := ownershipObserver(store, provider, time.Unix(2, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 0 {
		t.Fatalf("unregistered stacked PR was associated: %+v", store.writes)
	}

	store.addBranchTip("inst-p-1", "ao/p-1/stack", "stack-sha")
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, write := range store.writes {
		if write.pr.Number == 2 {
			found = true
		}
	}
	if !found || store.prOwnerships["https://github.com/o/r/pull/2"].PRURL == "" {
		t.Fatalf("registered stacked PR was not associated: writes=%+v ownership=%+v", store.writes, store.prOwnerships)
	}
}

func TestPoll_NoBranchOwnershipFailsClosed(t *testing.T) {
	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "feat"
	store.sessions[0].InstanceID = "inst-p-1"
	// Deliberately no branch tip: a branch name alone is not ownership.
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs: map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {
			{URL: "https://github.com/o/r/pull/1", Number: 1, SourceBranch: "feat", HeadRepo: "o/r", HeadSHA: "sha1"},
		}},
	}
	obs := ownershipObserver(store, provider, time.Unix(2, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 0 || len(store.prOwnerships) != 0 {
		t.Fatalf("unowned branch was associated: writes=%+v ownership=%+v", store.writes, store.prOwnerships)
	}
}

func TestPoll_AmbiguousOwnershipFailsClosed(t *testing.T) {
	store := testStoreWithSession()
	store.sessions = append(store.sessions, domain.SessionRecord{
		ID: "p-2", InstanceID: "inst-p-2", ProjectID: "p",
		Metadata: domain.SessionMetadata{Branch: "ao/shared"},
	})
	store.sessions[0].Metadata.Branch = "ao/shared"
	store.sessions[0].InstanceID = "inst-p-1"
	store.addBranchTip("inst-p-1", "ao/shared", "sha1")
	store.addBranchTip("inst-p-2", "ao/shared", "sha1")
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs: map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {
			{URL: "https://github.com/o/r/pull/9", Number: 9, SourceBranch: "ao/shared", HeadRepo: "o/r", HeadSHA: "sha1"},
		}},
	}
	obs := ownershipObserver(store, provider, time.Unix(2, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 0 || len(store.prOwnerships) != 0 {
		t.Fatalf("ambiguous ownership was associated: writes=%+v ownership=%+v", store.writes, store.prOwnerships)
	}
}
