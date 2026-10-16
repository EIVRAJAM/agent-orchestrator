package daemon

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/sentryobs"
)

// A representative AO_SENTRY_DSN value. The daemon receives it from the desktop
// supervisor's environment, so it is exactly what the fork policy must drop.
const testSentryDSN = "https://664339d231569b18bcf50d69a0ce37bf@o4511955966361600.ingest.us.sentry.io/4511955986677760"

func TestDaemonSentryDSNDropsConfiguredDSN(t *testing.T) {
	got := daemonSentryDSN(config.TelemetryConfig{SentryDSN: testSentryDSN})
	if got != "" {
		t.Fatalf("daemonSentryDSN = %q, want \"\" (fork policy must never forward a configured DSN)", got)
	}
}

// TestNewDaemonSentryConfigNeverEnablesSentry wires the composed config through
// the real sentryobs.Init and asserts no transport is initialized. Before the
// fork policy the daemon passed AO_SENTRY_DSN straight through, so this is the
// regression that keeps the Sentry surface a no-op even when a DSN is present.
func TestNewDaemonSentryConfigNeverEnablesSentry(t *testing.T) {
	cfg := newDaemonSentryConfig(config.TelemetryConfig{SentryDSN: testSentryDSN, AppVersion: "0.11.2"})
	if cfg.DSN != "" {
		t.Fatalf("config.DSN = %q, want \"\"", cfg.DSN)
	}
	if err := sentryobs.Init(cfg); err != nil {
		t.Fatalf("sentryobs.Init: %v", err)
	}
	if sentryobs.Enabled() {
		t.Fatal("sentryobs.Enabled() = true; a fork build must never initialize a Sentry transport")
	}
}
