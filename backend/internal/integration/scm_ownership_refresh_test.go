package integration

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestSCMOwnershipTracksBranchHeadMovement(t *testing.T) {
	ctx := context.Background()
	f := newSCMFixture(t, "feat/root")
	const prURL = "https://github.com/octocat/hello/pull/501"
	f.provider.detected["feat/root"] = detectedPR(prURL, 501, "feat/root", "main", "sha-one")
	f.provider.observations[501] = openSCMObservation(prURL, 501, "sha-one", "feat/root", "main", domain.MergeMergeable)
	if err := f.poll(ctx); err != nil {
		t.Fatal(err)
	}
	first, ok, err := f.store.GetSessionPROwnership(ctx, prURL)
	if err != nil || !ok || first.HeadSHA != "sha-one" {
		t.Fatalf("initial ownership = %+v ok=%v err=%v, want sha-one", first, ok, err)
	}

	// The branch/PR moves. f.poll records the new exact branch tip before the
	// observer refresh, so this head is durable workspace evidence.
	f.provider.detected["feat/root"] = detectedPR(prURL, 501, "feat/root", "main", "sha-two")
	f.provider.observations[501] = openSCMObservation(prURL, 501, "sha-two", "feat/root", "main", domain.MergeMergeable)
	if err := f.poll(ctx); err != nil {
		t.Fatal(err)
	}
	pr, ok, err := f.store.GetPR(ctx, prURL)
	if err != nil || !ok || pr.HeadSHA != "sha-two" {
		t.Fatalf("refreshed PR = %+v ok=%v err=%v, want sha-two", pr, ok, err)
	}
	second, ok, err := f.store.GetSessionPROwnership(ctx, prURL)
	if err != nil || !ok {
		t.Fatalf("ownership after refresh = %+v ok=%v err=%v", second, ok, err)
	}
	if second.HeadSHA != "sha-two" {
		t.Fatalf("ownership head = %q, want sha-two after the durable branch tip moved", second.HeadSHA)
	}
}

func TestSCMOldPRStaysForeignAcrossWorktreeRestore(t *testing.T) {
	ctx := context.Background()
	f := newSCMFixture(t, "feat/root")
	const oldURL = "https://github.com/octocat/hello/pull/502"
	// The new session restores its own worktree at a new base. The historical
	// PR head belongs to a different instance and is never registered.
	for i := 0; i < 2; i++ {
		if err := f.store.UpsertSessionWorktree(ctx, domain.SessionWorktreeRecord{
			SessionID: f.session.ID, RepoName: domain.RootWorkspaceRepoName,
			Branch: "feat/root", CreationSHA: "new-base", WorktreePath: "/ws/octo", State: "active",
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.provider.detected["feat/root"] = detectedPR(oldURL, 502, "feat/root", "main", "old-head")
	f.provider.observations[502] = openSCMObservation(oldURL, 502, "old-head", "feat/root", "main", domain.MergeMergeable)
	if err := f.observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.store.GetPR(ctx, oldURL); err != nil || ok {
		t.Fatalf("historical PR after worktree restore = ok=%v err=%v; want unassociated", ok, err)
	}
	if own, ok, err := f.store.GetSessionPROwnership(ctx, oldURL); err != nil || ok {
		t.Fatalf("historical PR ownership after restore = %+v ok=%v err=%v; want none", own, ok, err)
	}
}
