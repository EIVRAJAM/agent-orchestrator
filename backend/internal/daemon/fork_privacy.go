package daemon

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/sentryobs"
)

// Fork privacy policy: AO's own outbound telemetry and diagnostics.
//
// This fork ships with AO-originated product analytics and error/diagnostic
// reporting to third parties (PostHog, Sentry) permanently disabled as a code
// property, not a runtime setting. The daemon's composition points are
// local-only *by construction*: newTelemetrySink builds only a local SQLite or
// no-op sink, and newDaemonSentryConfig always yields a blank DSN, so there is
// no branch that turns configuration into an exporter.
//
// The constants below are therefore an explicit, greppable policy *assertion*,
// not a switch that enables anything:
//
//   - forkExternalTelemetryExportAllowed would, if true, mean remote event
//     export is permitted. No code path reads it to build a PostHog sink.
//   - forkExternalDiagnosticsExportAllowed would, if true, mean remote error
//     reporting is permitted. No code path reads it to build a Sentry
//     transport.
//
// daemon.Run refuses to start if either is ever flipped (`forkPrivacyPolicyViolated`),
// so a change to this policy fails closed and loudly instead of exporting
// silently. They are deliberately constants: no environment variable, on-disk
// consent authority, user preference, or remote config can flip them. Reverting
// the policy requires editing this file, which the privacy regression tests
// (fork_privacy_test.go) and the CI privacy guard (scripts/privacy-guard.sh)
// call out for review. See docs/privacy-fork.md.
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

// forkPrivacyPolicyViolated reports whether the compiled-in policy has been
// changed to allow external export. It is asserted at daemon startup: this fork
// keeps it false, and a true result aborts the boot rather than exporting.
func forkPrivacyPolicyViolated() bool {
	return forkExternalTelemetryExportAllowed || forkExternalDiagnosticsExportAllowed
}

// newDaemonSentryConfig builds the daemon's Sentry configuration. The DSN is
// always blank and is never read from configuration, so sentryobs.Init stays a
// permanent no-op: no transport, no upload, no SDK queue, and the staged
// agent-switch envelope sender stays unbuildable (see daemon.Run). Release and
// Environment are carried for a future approved build; they do not enable
// anything on their own.
func newDaemonSentryConfig(cfg config.TelemetryConfig) sentryobs.Config {
	return sentryobs.Config{
		DSN:         "",
		Release:     cfg.AppVersion,
		Environment: sentryEnvironment(cfg.AppVersion),
	}
}
