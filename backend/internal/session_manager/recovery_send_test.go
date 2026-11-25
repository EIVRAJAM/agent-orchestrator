package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func recoveryManagerFixture(t *testing.T, active bool, mode domain.SessionMode, checkpoint domain.ConversationCheckpointState, launcher *recordingLauncher) (*Manager, *fakeStore) {
	t.Helper()
	m, st, _ := newChatManager(launcher)
	now := time.Now().UTC()
	rec := domain.SessionRecord{
		ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker, InstanceID: "inst-1",
		IssueID: "github:acme/demo#1", Mode: mode, FirstSignalAt: now,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{ConversationCheckpointState: checkpoint},
		CreatedAt: now, UpdatedAt: now,
	}
	if active {
		rec.Activity.State = domain.ActivityActive
	}
	st.sessions["mer-1"] = rec
	return m, st
}

func TestSendRecoveryUsesProtectedIdleChatPath(t *testing.T) {
	launcher := &recordingLauncher{}
	m, _ := recoveryManagerFixture(t, false, domain.SessionModeChat, domain.ConversationCheckpointComplete, launcher)
	if err := m.SendRecovery(context.Background(), "mer-1", "continue", "inst-1", "github:acme/demo#1"); err != nil {
		t.Fatalf("SendRecovery: %v", err)
	}
	if len(launcher.recoveryTexts) != 1 || launcher.recoveryTexts[0] != "continue" {
		t.Fatalf("protected recovery texts = %v", launcher.recoveryTexts)
	}
}

func TestSendRecoveryFailsClosedForActiveIncompleteOrTUI(t *testing.T) {
	tests := []struct {
		name       string
		active     bool
		mode       domain.SessionMode
		checkpoint domain.ConversationCheckpointState
		want       error
	}{
		{name: "active", active: true, mode: domain.SessionModeChat, checkpoint: domain.ConversationCheckpointComplete, want: ErrRecoveryDeliveryNotIdle},
		{name: "unknown checkpoint", mode: domain.SessionModeChat, checkpoint: domain.ConversationCheckpointPrompt, want: ErrRecoveryTurnNotComplete},
		{name: "TUI", mode: domain.SessionModeTUI, checkpoint: domain.ConversationCheckpointComplete, want: ErrRecoveryDeliveryUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			launcher := &recordingLauncher{}
			m, _ := recoveryManagerFixture(t, tt.active, tt.mode, tt.checkpoint, launcher)
			err := m.SendRecovery(context.Background(), "mer-1", "continue", "inst-1", "github:acme/demo#1")
			if !errors.Is(err, tt.want) {
				t.Fatalf("SendRecovery err = %v, want %v", err, tt.want)
			}
			if len(launcher.recoveryTexts) != 0 {
				t.Fatalf("failing closed still called launcher: %v", launcher.recoveryTexts)
			}
		})
	}
}
