package daemon

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/sentryobs"
)

// A representative AO_SENTRY_DSN value. The daemon receives it from the desktop
// supervisor's environment, so it is exactly what the fork policy must ignore.
const testSentryDSN = "https://664339d231569b18bcf50d69a0ce37bf@o4511955966361600.ingest.us.sentry.io/4511955986677760"

// TestForkPrivacyPolicyIsClosed asserts the compiled-in policy, which the daemon
// checks at startup. This fork keeps it false; a flip must be deliberate and is
// caught here and by the CI privacy guard.
func TestForkPrivacyPolicyIsClosed(t *testing.T) {
	if forkExternalTelemetryExportAllowed {
		t.Fatal("forkExternalTelemetryExportAllowed = true; this fork must never allow external telemetry export")
	}
	if forkExternalDiagnosticsExportAllowed {
		t.Fatal("forkExternalDiagnosticsExportAllowed = true; this fork must never allow external diagnostics export")
	}
	if forkPrivacyPolicyViolated() {
		t.Fatal("forkPrivacyPolicyViolated() = true; daemon.Run would refuse to start")
	}
}

// TestNewDaemonSentryConfigNeverEnablesSentry wires the composed config through
// the real sentryobs.Init and asserts no transport is initialized. Before the
// fork policy the daemon passed AO_SENTRY_DSN straight through, so this is the
// regression that keeps the Sentry surface a no-op even when a DSN is present.
func TestNewDaemonSentryConfigNeverEnablesSentry(t *testing.T) {
	cfg := newDaemonSentryConfig(config.TelemetryConfig{SentryDSN: testSentryDSN, AppVersion: "0.11.2"})
	if cfg.DSN != "" {
		t.Fatalf("config.DSN = %q, want \"\" (a configured DSN must never reach sentryobs)", cfg.DSN)
	}
	if err := sentryobs.Init(cfg); err != nil {
		t.Fatalf("sentryobs.Init: %v", err)
	}
	if sentryobs.Enabled() {
		t.Fatal("sentryobs.Enabled() = true; a fork build must never initialize a Sentry transport")
	}
}
