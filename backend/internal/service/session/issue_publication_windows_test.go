//go:build windows

package session

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/pushobservation"
)

func TestWindowsInheritedPushMarkerDoesNotClassifyPublication(t *testing.T) {
	rec := publicationRecord()
	rec.Metadata.RuntimeLaunchID = "gen-1"
	dataDir := t.TempDir()
	writePushAttempt(t, dataDir, rec, rec.InstanceID, "gen-1", rec.BranchState.HeadSHA, rec.BranchState.Branch)

	if ok, err := pushobservation.Attempted(dataDir, rec.ID, rec.InstanceID, "gen-1", rec.BranchState.Branch, rec.BranchState.HeadSHA); err != nil || ok {
		t.Fatalf("Windows Attempted = %v, %v; want inherited marker ignored", ok, err)
	}
	if got := pushFor(rec, dataDir); got != domain.IssuePublicationPushUnknown {
		t.Fatalf("Windows inherited marker classified push as %s, want unknown", got)
	}
}
