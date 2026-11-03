package chat_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type backgroundTestConversation struct {
	*terminatingConversation
	interaction ports.ChatEventKind
}

func (c *backgroundTestConversation) StartDeferredTurn(providerTurnID string) error {
	if c.interaction != "" {
		c.emit(ports.ChatEvent{Kind: c.interaction, ProviderTurnID: providerTurnID})
		return nil
	}
	c.emit(
		ports.ChatEvent{Kind: ports.ChatEventMessageCompleted, ProviderTurnID: providerTurnID, Text: "Fix renderer"},
		ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: providerTurnID, TurnState: domain.TurnStateCompleted},
	)
	return nil
}
func (*backgroundTestConversation) DiscardDeferredTurn(string) {}

type unsupportedBackgroundDriver struct {
	startCalls int
}

func (*unsupportedBackgroundDriver) Harness() domain.AgentHarness {
	return domain.HarnessClaudeCode
}
func (*unsupportedBackgroundDriver) Probe(context.Context) (ports.ChatCapabilities, error) {
	return nil, nil
}
func (*unsupportedBackgroundDriver) BackgroundRestrictionReason() string {
	return "ACP provider-native approval modes are not an OS or network sandbox"
}
func (d *unsupportedBackgroundDriver) Start(context.Context, ports.ChatStartConfig) (ports.ChatConversation, error) {
	d.startCalls++
	return nil, errors.New("background start must not be called")
}
func (*unsupportedBackgroundDriver) Resume(context.Context, ports.ChatResumeConfig) (ports.ChatConversation, error) {
	return nil, errors.New("background resume must not be called")
}

func TestRunBackgroundTaskRejectsUnsupportedRestrictionsBeforeStart(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	before, err := st.ListSessions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	driver := &unsupportedBackgroundDriver{}
	idCalls := 0
	service := chatsvc.New(chatsvc.Options{
		Store:   st,
		Drivers: fakeRegistry{driver: driver},
		NewID: func() string {
			idCalls++
			return "must-not-be-used"
		},
	})

	_, err = service.RunBackgroundTask(ctx, domain.HarnessClaudeCode, ports.ChatStartConfig{}, "Write a short title")
	if !errors.Is(err, ports.ErrChatBackgroundRestrictions) {
		t.Fatalf("error = %v, want ErrChatBackgroundRestrictions", err)
	}
	var rejection *ports.ChatBackgroundRestrictionsError
	if !errors.As(err, &rejection) {
		t.Fatalf("error = %T, want ChatBackgroundRestrictionsError", err)
	}
	if rejection.Harness != domain.HarnessClaudeCode {
		t.Fatalf("rejected harness = %q, want %q", rejection.Harness, domain.HarnessClaudeCode)
	}
	if driver.startCalls != 0 {
		t.Fatalf("provider Start calls = %d, want 0", driver.startCalls)
	}
	if idCalls != 0 {
		t.Fatalf("background session ID calls = %d, want 0", idCalls)
	}
	after, listErr := st.ListSessions(ctx, "")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(after) != len(before) {
		t.Fatalf("durable sessions changed after rejected background task: before=%d after=%d", len(before), len(after))
	}
	for _, want := range []string{"ACP", "read-only", "network isolation", "task was not started"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestPromptSuggestionTaskRejectsUnsupportedRestrictionsBeforeFork(t *testing.T) {
	driver := &unsupportedBackgroundDriver{}
	service := chatsvc.New(chatsvc.Options{Drivers: fakeRegistry{driver: driver}})
	_, err := service.RunPromptSuggestionTask(context.Background(), "session-1", "turn-1", domain.HarnessClaudeCode, ports.ChatStartConfig{}, "Suggest a prompt")
	if !errors.Is(err, ports.ErrChatBackgroundRestrictions) {
		t.Fatalf("error = %v, want ErrChatBackgroundRestrictions", err)
	}
	if driver.startCalls != 0 {
		t.Fatalf("provider Start calls = %d, want 0", driver.startCalls)
	}
}

func TestRunBackgroundTaskUsesNativeDriverAndTerminatesIt(t *testing.T) {
	conversation := &backgroundTestConversation{terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()}}
	var started ports.ChatStartConfig
	driver := fakeDriver{conv: conversation, startCfg: &started}
	ids := []string{"task-id", "scope-id"}
	service := chatsvc.New(chatsvc.Options{
		Drivers: fakeRegistry{driver: driver},
		NewID: func() string {
			id := ids[0]
			ids = ids[1:]
			return id
		},
	})

	title, err := service.RunBackgroundTask(context.Background(), domain.HarnessCodex, ports.ChatStartConfig{
		DataDir: "/data", WorkspacePath: "/workspace",
		Env: map[string]string{"CODEX_HOME": "/account"}, Model: "small", Effort: "low",
		Permissions: ports.PermissionModeAuto, SystemPrompt: "title only",
	}, "Fix the renderer")
	if err != nil || title != "Fix renderer" {
		t.Fatalf("RunBackgroundTask = %q, %v", title, err)
	}
	if started.SessionID != "background-task-id" || started.ProviderScopeID != "scope-id" || !started.ProviderIDsScoped || !started.Ephemeral || !started.ReadOnly || !started.DisableNetwork {
		t.Fatalf("start identity = %#v", started)
	}
	if started.Model != "small" || started.Effort != "" || started.Env["CODEX_HOME"] != "/account" {
		t.Fatalf("start config = %#v", started)
	}
	messages := conversation.sentMessages()
	if len(messages) != 1 || messages[0].Text != "Fix the renderer" || messages[0].Origin != domain.MessageOriginAutomation {
		t.Fatalf("messages = %#v", messages)
	}
	if !conversation.terminated.Load() {
		t.Fatal("background provider was not terminated")
	}
}

type backgroundModelConversation struct {
	*backgroundTestConversation
	models []ports.ChatModel
}

func (c *backgroundModelConversation) ListModels(context.Context) ([]ports.ChatModel, error) {
	return c.models, nil
}

func TestRunBackgroundTaskUsesLowestEffortOfferedByActiveModel(t *testing.T) {
	for _, tt := range []struct {
		name    string
		efforts []string
		want    string
	}{
		{"low offered", []string{"xhigh", "low", "high"}, "low"},
		{"low unsupported", []string{"high", "medium"}, "medium"},
		{"no reasoning", nil, ""},
		{"unknown vocabulary", []string{"adaptive"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conversation := &backgroundModelConversation{
				backgroundTestConversation: &backgroundTestConversation{terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()}},
				models: []ports.ChatModel{
					{ID: "other-model", Efforts: []string{"none"}},
					{ID: "selected-model", Default: true, Efforts: tt.efforts},
				},
			}
			var started ports.ChatStartConfig
			service := chatsvc.New(chatsvc.Options{Drivers: fakeRegistry{driver: fakeDriver{conv: conversation, startCfg: &started}}, NewID: func() string { return "id" }})
			if _, err := service.RunBackgroundTask(context.Background(), domain.HarnessCodex, ports.ChatStartConfig{Model: "selected-model", Effort: "xhigh"}, "Write a short title"); err != nil {
				t.Fatal(err)
			}
			message := conversation.sentMessages()[0]
			if started.Model != "selected-model" || started.Effort != "" || message.Settings.Model != "" || message.Settings.Effort != tt.want {
				t.Fatalf("start model/effort = %q/%q, turn settings = %#v, want effort %q", started.Model, started.Effort, message.Settings, tt.want)
			}
		})
	}
}

type backgroundConfigConversation struct {
	*backgroundTestConversation
	options  []ports.ChatConfigOption
	setID    string
	setValue ports.ChatConfigOptionValue
	setErr   error
}

func (c *backgroundConfigConversation) ListConfigOptions(context.Context) ([]ports.ChatConfigOption, error) {
	return c.options, nil
}

func (c *backgroundConfigConversation) SetConfigOption(_ context.Context, id string, value ports.ChatConfigOptionValue) ([]ports.ChatConfigOption, error) {
	c.setID, c.setValue = id, value
	return c.options, c.setErr
}

func TestRunBackgroundTaskUsesAdvertisedNativeEffort(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
		want  string
	}{
		{"native value preserved", "OFF", "OFF"},
		{"unknown value unchanged", "adaptive", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conversation := &backgroundConfigConversation{
				backgroundTestConversation: &backgroundTestConversation{terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()}},
				options: []ports.ChatConfigOption{
					{ID: "provider-model", Category: "model", Type: ports.ChatConfigOptionSelect, Choices: []ports.ChatConfigOptionChoice{{Value: "low"}}},
					{ID: "native-reasoning", Category: "thought_level", Type: ports.ChatConfigOptionSelect, Current: ports.ChatConfigOptionValue{Select: "high"}, Choices: []ports.ChatConfigOptionChoice{{Value: tt.value}}},
				},
			}
			service := chatsvc.New(chatsvc.Options{Drivers: fakeRegistry{driver: fakeDriver{conv: conversation}}, NewID: func() string { return "id" }})
			if _, err := service.RunBackgroundTask(context.Background(), domain.HarnessDeepSeek, ports.ChatStartConfig{}, "Write a short title"); err != nil {
				t.Fatal(err)
			}
			if conversation.setValue.Select != tt.want || (tt.want != "" && conversation.setID != "native-reasoning") {
				t.Fatalf("native option = %q/%#v, want effort %q", conversation.setID, conversation.setValue, tt.want)
			}
			if conversation.sentMessages()[0].Settings.Effort != "" {
				t.Fatal("native effort was remapped through a different turn setting")
			}
		})
	}
}

func TestRunBackgroundTaskStopsBeforeGenerationIfEffortCannotBeSet(t *testing.T) {
	wantErr := errors.New("provider rejected its advertised effort")
	conversation := &backgroundConfigConversation{
		backgroundTestConversation: &backgroundTestConversation{terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()}},
		options:                    []ports.ChatConfigOption{{ID: "effort", Category: "thought_level", Type: ports.ChatConfigOptionSelect, Choices: []ports.ChatConfigOptionChoice{{Value: "low"}}}},
		setErr:                     wantErr,
	}
	service := chatsvc.New(chatsvc.Options{Drivers: fakeRegistry{driver: fakeDriver{conv: conversation}}, NewID: func() string { return "id" }})
	if _, err := service.RunBackgroundTask(context.Background(), domain.HarnessClaudeCode, ports.ChatStartConfig{}, "Write a short title"); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if len(conversation.sentMessages()) != 0 || !conversation.terminated.Load() {
		t.Fatal("background generation started or provider was left running")
	}
}

func TestRunBackgroundTaskRejectsInteractionAndTerminatesProvider(t *testing.T) {
	for _, kind := range []ports.ChatEventKind{ports.ChatEventApprovalRequested, ports.ChatEventInputRequested} {
		t.Run(string(kind), func(t *testing.T) {
			conversation := &backgroundTestConversation{terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()}, interaction: kind}
			service := chatsvc.New(chatsvc.Options{Drivers: fakeRegistry{driver: fakeDriver{conv: conversation}}, NewID: func() string { return "id" }})
			if _, err := service.RunBackgroundTask(context.Background(), domain.HarnessCodex, ports.ChatStartConfig{}, "Write a short title"); err == nil || !strings.Contains(err.Error(), "requested user interaction") {
				t.Fatalf("error = %v, want rejected interaction", err)
			}
			if !conversation.terminated.Load() || len(conversation.resolved) != 0 {
				t.Fatal("background task granted interaction or left provider running")
			}
		})
	}
}
