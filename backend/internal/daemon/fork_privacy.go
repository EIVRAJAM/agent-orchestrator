package daemon

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/sentryobs"
)

// Fork privacy policy: AO's own outbound telemetry and diagnostics.
//
// This fork ships with AO-originated product analytics and error/diagnostic
// reporting to third parties (PostHog, Sentry) permanently disabled as a code
// property, not a runtime setting. The constants below are the single Go-side
// source of truth consulted by every daemon composition point that could
// otherwise build an outbound exporter:
//
//   - forkExternalTelemetryExportAllowed gates remote event export (PostHog).
//   - forkExternalDiagnosticsExportAllowed gates remote error/diagnostic
//     reporting (Sentry, including the staged agent-switch envelope sender).
//
// They are deliberately constants, never configuration: no environment
// variable, on-disk consent authority, user preference, or remote config can
// flip them, and there is no opt-in switch that restores the export. Reverting
// an export therefore requires editing this file, which the privacy regression
// tests (fork_privacy_test.go) and the CI privacy guard
// (scripts/privacy-guard.sh) call out for review. See docs/privacy-fork.md.
//
// Scope: this policy covers AO's own telemetry/diagnostics export only. It does
// not touch functional network traffic — the loopback daemon API, the user's
// own GitHub/GitLab, the cloud control plane, Expo push, SCM/tracker intake,
// and the update feed all keep working. Agent-harness telemetry (Codex,
// Claude Code, Kimi, and the like) is the harness vendors' own code and is out
// of scope here; see docs/privacy-fork.md for that distinction.
const (
	forkExternalTelemetryExportAllowed   = false
	forkExternalDiagnosticsExportAllowed = false
)

// daemonSentryDSN resolves the DSN the daemon may hand to Sentry. This fork
// keeps it permanently blank, so sentryobs.Init never constructs a Sentry
// transport and the staged agent-switch envelope sender stays unbuildable,
// regardless of AO_SENTRY_DSN. The configured value is returned only if the
// diagnostics policy is ever changed.
func daemonSentryDSN(cfg config.TelemetryConfig) string {
	if forkExternalDiagnosticsExportAllowed {
		return cfg.SentryDSN
	}
	return ""
}

// newDaemonSentryConfig builds the daemon's Sentry configuration. It routes the
// DSN through daemonSentryDSN so the fork policy is the only thing that decides
// whether a transport is ever initialized.
func newDaemonSentryConfig(cfg config.TelemetryConfig) sentryobs.Config {
	return sentryobs.Config{
		DSN:         daemonSentryDSN(cfg),
		Release:     cfg.AppVersion,
		Environment: sentryEnvironment(cfg.AppVersion),
	}
}
