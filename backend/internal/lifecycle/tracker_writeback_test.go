package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeTrackerWriteback struct {
	ids   []domain.SessionID
	prs   []string
	heads []string
	calls int
	err   error
}

func (f *fakeTrackerWriteback) Enqueue(_ context.Context, id domain.SessionID, prURL, headSHA string) error {
	f.calls++
	f.ids = append(f.ids, id)
	f.prs = append(f.prs, prURL)
	f.heads = append(f.heads, headSHA)
	return f.err
}

func writebackObservation() ports.SCMObservation {
	return ports.SCMObservation{
		Fetched:      true,
		PR:           ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1, HeadSHA: "sha-expected"},
		CI:           ports.SCMCIObservation{Summary: string(domain.CIPassing)},
		Review:       ports.SCMReviewObservation{Decision: string(domain.ReviewApproved)},
		Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)},
	}
}

func TestTrackerWritebackUsesIndependentReadinessContract(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*ports.SCMObservation)
		wantEnqueue bool
	}{
		{
			name: "open non-draft passing CI ignores unknown mergeability",
			mutate: func(o *ports.SCMObservation) {
				o.Mergeability.State = string(domain.MergeUnknown)
			},
			wantEnqueue: true,
		},
		{
			name: "open non-draft passing CI ignores review blockers",
			mutate: func(o *ports.SCMObservation) {
				o.Review.Decision = string(domain.ReviewChangesRequest)
				o.Review.Threads = []ports.SCMReviewThreadObservation{{
					ID: "thread-1", Path: "src/app.go", Line: 4,
					Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Body: "fix"}},
				}}
			},
			wantEnqueue: true,
		},
		{
			name:        "draft",
			mutate:      func(o *ports.SCMObservation) { o.PR.Draft = true },
			wantEnqueue: false,
		},
		{
			name:        "merged",
			mutate:      func(o *ports.SCMObservation) { o.PR.Merged = true },
			wantEnqueue: false,
		},
		{
			name:        "closed",
			mutate:      func(o *ports.SCMObservation) { o.PR.Closed = true },
			wantEnqueue: false,
		},
		{
			name:        "unknown CI",
			mutate:      func(o *ports.SCMObservation) { o.CI.Summary = "" },
			wantEnqueue: false,
		},
		{
			name:        "pending CI",
			mutate:      func(o *ports.SCMObservation) { o.CI.Summary = string(domain.CIPending) },
			wantEnqueue: false,
		},
		{
			name:        "failing CI",
			mutate:      func(o *ports.SCMObservation) { o.CI.Summary = string(domain.CIFailing) },
			wantEnqueue: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, st, _ := newManager()
			sink := &fakeTrackerWriteback{}
			m.SetTrackerWritebackSink(sink)
			st.sessions["mer-1"] = working("mer-1")
			o := writebackObservation()
			if tt.mutate != nil {
				tt.mutate(&o)
			}
			if err := m.ApplySCMObservation(context.Background(), "mer-1", o); err != nil {
				t.Fatal(err)
			}
			if got := sink.calls > 0; got != tt.wantEnqueue {
				t.Fatalf("enqueue = %v, want %v (calls=%d)", got, tt.wantEnqueue, sink.calls)
			}
			if tt.wantEnqueue && (len(sink.heads) != 1 || sink.heads[0] != "sha-expected") {
				t.Fatalf("enqueued heads = %v, want [sha-expected]", sink.heads)
			}
		})
	}
}

func TestTrackerWritebackMissingHeadDoesNotEnqueue(t *testing.T) {
	m, st, _ := newManager()
	sink := &fakeTrackerWriteback{}
	m.SetTrackerWritebackSink(sink)
	st.sessions["mer-1"] = working("mer-1")
	o := writebackObservation()
	o.PR.HeadSHA = ""

	if err := m.ApplySCMObservation(context.Background(), "mer-1", o); err == nil {
		t.Fatal("ApplySCMObservation returned nil for a ready observation without a head")
	}
	if sink.calls != 0 {
		t.Fatalf("enqueue calls = %d, want 0 without a head", sink.calls)
	}
}

func TestSCMObservationEnqueueFailureReturnsErrorAndRetries(t *testing.T) {
	notifications := &fakeNotificationSink{}
	m, st, _ := newManager(WithNotificationSink(notifications))
	sink := &fakeTrackerWriteback{err: errors.New("outbox unavailable")}
	m.SetTrackerWritebackSink(sink)
	st.sessions["mer-1"] = working("mer-1")
	o := writebackObservation()

	if err := m.ApplySCMObservation(context.Background(), "mer-1", o); err == nil {
		t.Fatal("ApplySCMObservation returned nil after enqueue failure")
	}
	if sink.calls != 1 {
		t.Fatalf("enqueue attempts = %d, want 1", sink.calls)
	}
	if len(notifications.intents) != 0 {
		t.Fatalf("notification emitted before durable enqueue: %+v", notifications.intents)
	}

	sink.err = nil
	if err := m.ApplySCMObservation(context.Background(), "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 2 {
		t.Fatalf("enqueue attempts after retry = %d, want 2", sink.calls)
	}
	if len(notifications.intents) != 1 || notifications.intents[0].Type != domain.NotificationReadyToMerge {
		t.Fatalf("retry notifications = %+v, want one ready-to-merge", notifications.intents)
	}
}
