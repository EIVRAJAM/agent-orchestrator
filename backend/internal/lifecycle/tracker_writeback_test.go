package lifecycle

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeTrackerWriteback struct {
	ids   []domain.SessionID
	prs   []string
	calls int
	err   error
}

func (f *fakeTrackerWriteback) Enqueue(_ context.Context, id domain.SessionID, prURL string) error {
	f.calls++
	f.ids = append(f.ids, id)
	f.prs = append(f.prs, prURL)
	return f.err
}

func TestSCMObservationEnqueuesTrackerWritebackOnlyWhenReady(t *testing.T) {
	m, st, _ := newManager()
	sink := &fakeTrackerWriteback{}
	m.SetTrackerWritebackSink(sink)
	st.sessions["mer-1"] = working("mer-1")

	ready := ports.SCMObservation{
		Fetched:      true,
		PR:           ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1},
		CI:           ports.SCMCIObservation{Summary: string(domain.CIPassing)},
		Review:       ports.SCMReviewObservation{Decision: string(domain.ReviewApproved)},
		Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)},
	}
	if err := m.ApplySCMObservation(context.Background(), "mer-1", ready); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 || sink.ids[0] != "mer-1" || sink.prs[0] != ready.PR.URL {
		t.Fatalf("writeback calls = %+v", sink)
	}

	blocked := ready
	blocked.CI.Summary = string(domain.CIPending)
	if err := m.ApplySCMObservation(context.Background(), "mer-1", blocked); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 {
		t.Fatalf("writeback calls after blocked observation = %d, want 1", sink.calls)
	}
}
