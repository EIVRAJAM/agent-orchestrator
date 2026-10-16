package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	telemetryadapter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/telemetry"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	agentswitchobs "github.com/aoagents/agent-orchestrator/backend/internal/observe/agentswitch"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func newAgentSwitchFailureDispatcher(
	store ports.AgentSwitchFailureOutboxStore,
	policy agentswitchobs.PolicyCoordinator,
	observer ports.AgentSwitchFailureObserver,
	log *slog.Logger,
) (*agentswitchobs.Dispatcher, error) {
	if observer == nil {
		backlog, err := store.AgentSwitchFailureBacklog(context.Background(), time.Now().UTC())
		if err != nil {
			return nil, fmt.Errorf("inspect agent switch failure backlog: %w", err)
		}
		if policy.Authorization().Enabled || backlog.Pending != 0 || backlog.Leased != 0 {
			return nil, errors.New("agent switch failure observer unavailable with enabled policy or pending payload")
		}
		return nil, nil
	}
	return agentswitchobs.NewDispatcher(agentswitchobs.DispatcherConfig{
		Store: store, Observer: observer, Policy: policy, Logger: log,
	})
}

// newTelemetrySink builds the daemon's local event sink.
//
// Fork privacy policy: this build never constructs a remote event exporter.
// Whatever the resolved telemetry config says about a remote sink
// (AO_TELEMETRY_REMOTE, a PostHog key/host, or an events-enabled consent
// record), the PostHog chain — the billed PostHogSink plus the aggregation,
// rate-limit, and denylist wrappers that exist only to shape its traffic — is
// not built. Only local, on-disk diagnostics remain: with events enabled every
// event is recorded to SQLite for debugging, and nothing leaves the machine.
// With events disabled the sink discards everything, exactly as before.
//
// This neutralization is unconditional for the fork's builds: no environment
// variable, preference, consent record, or remote config can reach an export
// through this function. See fork_privacy.go and docs/privacy-fork.md.
func newTelemetrySink(cfg config.Config, store *sqlite.Store, log *slog.Logger) ports.EventSink {
	if !cfg.Telemetry.Events {
		return telemetryadapter.NoopSink{}
	}
	return telemetryadapter.NewLocalSQLiteSink(store, log)
}
