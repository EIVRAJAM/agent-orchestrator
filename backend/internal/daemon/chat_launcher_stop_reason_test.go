package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type launcherStopRecord struct {
	reason  domain.ChatStopReason
	episode string
	cutoff  int64
}

type launcherStopStore struct {
	*sqlite.Store
	mu    sync.Mutex
	calls []launcherStopRecord
}

func (s *launcherStopStore) RecordStopIntentAndCancelQueued(
	ctx context.Context,
	conversationID string,
	session domain.SessionID,
	reviewID, generation string,
	reason domain.ChatStopReason,
	episodeID string,
	now time.Time,
) (domain.ChatStopIntent, error) {
	intent, err := s.Store.RecordStopIntentAndCancelQueued(
		ctx, conversationID, session, reviewID, generation, reason, episodeID, now,
	)
	if err == nil {
		s.mu.Lock()
		s.calls = append(s.calls, launcherStopRecord{reason: reason, episode: intent.EpisodeID, cutoff: intent.CutoffRowID})
		s.mu.Unlock()
	}
	return intent, err
}

func (s *launcherStopStore) records() []launcherStopRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]launcherStopRecord(nil), s.calls...)
}

type launcherConversation struct {
	events chan ports.ChatEvent
	once   sync.Once
}

func (*launcherConversation) ProviderConversationID() string { return "launcher-native-conversation" }
func (*launcherConversation) Capabilities() ports.ChatCapabilities {
	return ports.ChatCapabilities{
		ports.ChatCapabilityStreaming: true,
		ports.ChatCapabilityApprovals: true,
		ports.ChatCapabilityInterrupt: true,
		ports.ChatCapabilityResume:    true,
	}
}
func (c *launcherConversation) SendTurn(context.Context, ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	return ports.ChatTurnRef{ProviderTurnID: "launcher-provider-turn"}, nil
}
func (*launcherConversation) Interrupt(context.Context, string) error { return nil }
func (*launcherConversation) ResolveRequest(context.Context, string, ports.ChatDecision) error {
	return nil
}
func (c *launcherConversation) Events() <-chan ports.ChatEvent { return c.events }
func (c *launcherConversation) Close() error {
	c.once.Do(func() { close(c.events) })
	return nil
}

type launcherDriver struct {
	conv *launcherConversation
}

func (launcherDriver) Harness() domain.AgentHarness { return domain.HarnessCodex }
func (launcherDriver) Probe(context.Context) (ports.ChatCapabilities, error) {
	return (&launcherConversation{}).Capabilities(), nil
}
func (d launcherDriver) Start(context.Context, ports.ChatStartConfig) (ports.ChatConversation, error) {
	return d.conv, nil
}
func (d launcherDriver) Resume(context.Context, ports.ChatResumeConfig) (ports.ChatConversation, error) {
	return d.conv, nil
}

type launcherRegistry struct {
	driver launcherDriver
}

func (r launcherRegistry) Driver(harness domain.AgentHarness) (ports.ChatDriver, error) {
	if harness != domain.HarnessCodex {
		return nil, ports.ErrChatUnsupported
	}
	return r.driver, nil
}
func (r launcherRegistry) SupportsChat(harness domain.AgentHarness) bool {
	return harness == domain.HarnessCodex
}

type launcherHarness struct {
	store *launcherStopStore
	svc   *chatsvc.Service
	ctrl  *chatsvc.Controller
	conv  *launcherConversation
	id    domain.SessionID
}

func newLauncherStopHarness(t *testing.T) *launcherHarness {
	t.Helper()
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	recorder := &launcherStopStore{Store: st}
	dir := t.TempDir()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "launcher-project", Path: dir, RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.CreateSession(ctx, domain.SessionRecord{
		ID: "launcher-session", ProjectID: "launcher-project", Kind: domain.KindWorker,
		Harness: domain.HarnessCodex, Mode: domain.SessionModeChat,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	conv := &launcherConversation{events: make(chan ports.ChatEvent)}
	var counter int
	var mu sync.Mutex
	svc := chatsvc.New(chatsvc.Options{
		Store: recorder, Sessions: st, Log: slog.New(slog.DiscardHandler),
		Drivers: launcherRegistry{driver: launcherDriver{conv: conv}},
		NewID: func() string {
			mu.Lock()
			defer mu.Unlock()
			counter++
			return fmt.Sprintf("launcher-id-%d", counter)
		},
		Now: func() time.Time { return time.Now().UTC() },
	})
	ctrl, err := svc.Start(ctx, chatsvc.StartConfig{
		SessionID: rec.ID, ProjectID: rec.ProjectID, Harness: domain.HarnessCodex, WorkspacePath: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.StopAll(context.Background()) })
	return &launcherHarness{store: recorder, svc: svc, ctrl: ctrl, conv: conv, id: rec.ID}
}

func (h *launcherHarness) startRunningTurn(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.svc.Send(ctx, h.id, ports.ChatUserMessage{
		Text: "launcher stop reason", ClientMessageID: "launcher-stop-reason",
	}); err != nil {
		t.Fatal(err)
	}
	h.conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "launcher-provider-turn"}
}

func (h *launcherHarness) terminalTurn(t *testing.T) store.ConversationSnapshot {
	t.Helper()
	snapshot, err := h.store.LoadConversationSnapshot(context.Background(), h.ctrl.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Turns) == 0 || !snapshot.Turns[0].State.Terminal() {
		t.Fatalf("turn did not become terminal: %+v", snapshot.Turns)
	}
	return snapshot
}

func TestChatLauncherPreparesStopReasonAndCleansUpWithoutRewriting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason domain.ChatStopReason
	}{
		{name: "handoff", reason: domain.ChatStopReasonHandoffInterrupt},
		{name: "agent switch", reason: domain.ChatStopReasonAgentSwitch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLauncherStopHarness(t)
			h.startRunningTurn(t)
			launcher := chatLauncher{svc: h.svc}
			if err := launcher.PrepareChatHandoffForReason(
				context.Background(), h.id, domain.SessionInterfaceTransitionInterrupt, tc.reason,
			); err != nil {
				t.Fatal(err)
			}
			records := h.store.records()
			if len(records) != 1 || records[0].reason != tc.reason {
				t.Fatalf("after prepare = %+v, want one %q", records, tc.reason)
			}
			first := records[0]
			if err := launcher.StopChatAfterStopIntent(context.Background(), h.id); err != nil {
				t.Fatal(err)
			}
			records = h.store.records()
			if len(records) != 1 || records[0] != first {
				t.Fatalf("after cleanup = %+v, want the original record %+v", records, first)
			}
			h.terminalTurn(t)
		})
	}
}

func TestChatLauncherExplicitStopWritesSessionTerminated(t *testing.T) {
	h := newLauncherStopHarness(t)
	launcher := chatLauncher{svc: h.svc}
	if err := launcher.StopChat(context.Background(), h.id); err != nil {
		t.Fatal(err)
	}
	records := h.store.records()
	if len(records) != 1 || records[0].reason != domain.ChatStopReasonSessionTerminated {
		t.Fatalf("explicit stop = %+v, want one session_terminated", records)
	}
}
