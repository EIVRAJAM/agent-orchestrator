#!/usr/bin/env bash
#
# Privacy guard for this fork of Agent Orchestrator.
#
# This fork disables AO's own outbound telemetry and diagnostics export
# (PostHog, Sentry) as a code property. The guard combines three things, because
# no single one is sufficient:
#
#   1. Behavioral tests — the composition points must not build an exporter and
#      must not initialize a client/transport, no matter the config or env.
#   2. Static gates — a reintroduced exporter, an upstream/unauthorized update
#      feed, or a new observability dependency FAILS the guard (not just warns).
#      scripts/lib/privacy-scan.mjs matches observability SDK *module specifiers*
#      (so an aliased import is caught regardless of the local identifier) across
#      backend/**, frontend/**, packages/** and scripts/**, and discovers every
#      package.json under those roots. An explicit allowlist pins the
#      deliberately-dormant files, and those files must still carry their
#      fork-policy guard.
#   3. The documented policy — see docs/privacy-fork.md — which states what is
#      and is not allowed to leave the machine.
#
# Static matching is an alarm, never the guarantee: it cannot detect a
# hand-written HTTP exporter with no known SDK or marker, and it does not prove
# absence. The behavioral tests cover the known composition points; upstream
# merges still need code review and, where warranted, traffic inspection.
#
# Run locally from the repo root:
#   bash scripts/privacy-guard.sh
#
# Local-only diagnostics (NOT allowed in CI; the guard rejects them when CI is
# set, so a workflow can never skip a mandatory part):
#   PRIVACY_GUARD_SKIP_GO=1        skip the Go tests
#   PRIVACY_GUARD_SKIP_FRONTEND=1  skip the frontend tests
#   PRIVACY_GUARD_SKIP_MOBILE=1    skip the mobile tests
#   PRIVACY_GUARD_STATIC_ONLY=1    run only the static gates (guard self-tests)
#   PRIVACY_GUARD_ROOT=<dir>       run against a fixture tree (guard self-tests)
#
# It never publishes, deploys, or contacts an external service.
set -uo pipefail

root="${PRIVACY_GUARD_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$root" || exit 2

static_only="${PRIVACY_GUARD_STATIC_ONLY:-0}"

fail=0
note() { printf '%s\n' "$*"; }
bad() { printf 'FAIL: %s\n' "$*" >&2; fail=1; }

# --- CI must run every mandatory part ----------------------------------------

if [[ -n "${CI:-}" ]]; then
  for var in PRIVACY_GUARD_SKIP_GO PRIVACY_GUARD_SKIP_FRONTEND PRIVACY_GUARD_SKIP_MOBILE PRIVACY_GUARD_STATIC_ONLY; do
    if [[ -n "${!var:-}" ]]; then
      printf 'privacy-guard: refusing to run in CI with %s set (CI=%s)\n' "$var" "$CI" >&2
      exit 2
    fi
  done
fi

# --- Static helpers ----------------------------------------------------------

# require_false <file> <regex that must appear with "false">
require_false() {
  local file="$1" pattern="$2"
  if [[ ! -f "$file" ]]; then
    bad "$file is missing (expected fork policy file)"
    return
  fi
  grep -Eq "$pattern" "$file" || bad "$file: expected a closed policy value matching /$pattern/"
}

require_contains() {
  local file="$1" text="$2"
  if [[ ! -f "$file" ]]; then
    bad "$file is missing"
    return
  fi
  grep -Fq "$text" "$file" || bad "$file: missing required policy guard \"$text\""
}

# --- Static gate 1: policy constants are closed ------------------------------

note "==> static: fork policy constants are closed"
require_false backend/internal/daemon/fork_privacy.go 'forkExternalTelemetryExportAllowed[[:space:]]*=[[:space:]]*false'
require_false backend/internal/daemon/fork_privacy.go 'forkExternalDiagnosticsExportAllowed[[:space:]]*=[[:space:]]*false'
require_false frontend/src/shared/fork-privacy.ts 'EXTERNAL_TELEMETRY_EXPORT_ALLOWED[[:space:]]*=[[:space:]]*false'
require_false frontend/src/shared/fork-privacy.ts 'EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED[[:space:]]*=[[:space:]]*false'
require_false packages/mobile/lib/fork-privacy.ts 'MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED[[:space:]]*=[[:space:]]*false'
require_false packages/mobile/lib/fork-privacy.ts 'MOBILE_EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED[[:space:]]*=[[:space:]]*false'

# --- Static gate 2: imports, call sites, and dependencies --------------------
#
# scripts/lib/privacy-scan.mjs matches the observability module *specifier*, so an
# aliased import is caught regardless of the local identifier, and covers Go,
# Electron main, renderer and all of packages/**. It also discovers every
# package.json under the covered roots, so an observability dependency added to
# any package (product-ui, cloud-client, …) fails unless reviewed.

note "==> static: observability imports, call sites, and dependencies"
if ! node scripts/lib/privacy-scan.mjs "$root"; then
  bad "privacy-scan found unauthorized observability imports/constructions or an unreviewed dependency"
fi

# --- Static gate 3: allowed files stay protected by the policy ---------------

note "==> static: dormant files still carry their fork-policy guard"
require_contains frontend/src/renderer/lib/telemetry.ts EXTERNAL_TELEMETRY_EXPORT_ALLOWED
require_contains frontend/src/main/sentry-main.ts EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED
require_contains frontend/src/renderer/lib/sentry.ts EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED
require_contains packages/mobile/lib/sentry.ts MOBILE_EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED
require_contains packages/mobile/lib/telemetry/runtime.ts MOBILE_TELEMETRY_DISABLED
require_contains packages/mobile/lib/telemetry/config.ts MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED
# The daemon Sentry DSN must never be read from configuration.
if grep -Eq 'daemonSentryDSN|cfg\.Telemetry\.SentryDSN' backend/internal/daemon/daemon.go; then
  bad "backend/internal/daemon/daemon.go reads a Sentry DSN from configuration; the fork keeps it blank"
fi

# --- Static gate 4: release target authority ---------------------------------

note "==> static: release target is the authorized fork repo"
require_contains frontend/src/shared/release-target.ts 'AUTHORIZED_RELEASE_REPOS'
require_contains frontend/src/shared/release-target.ts 'UPSTREAM_RELEASE_REPOS'
require_contains frontend/src/shared/release-target.ts 'EIVRAJAM/agent-orchestrator'
for consumer in frontend/forge.config.ts frontend/src/main/feature-builds.ts frontend/src/main/desktop-version-floor.ts; do
  require_contains "$consumer" 'release-target'
done
if grep -n '"downloadUrl"' desktop-version-floor.json 2>/dev/null | grep -q 'Untrivial-ai'; then
  bad "desktop-version-floor.json points its downloadUrl at upstream"
fi

# --- Behavioral gates below (skipped only outside CI) ------------------------

if [[ "$static_only" == "1" ]]; then
  if [[ "$fail" -eq 0 ]]; then
    note "privacy guard (static only): OK"
  else
    note "privacy guard (static only): FAILED"
  fi
  exit "$fail"
fi

# --- Behavioral gate 1: Go daemon composition --------------------------------

if [[ "${PRIVACY_GUARD_SKIP_GO:-}" != "1" ]]; then
  note "==> go test: telemetry/sentry composition and policy"
  if ! ( cd backend && go test ./internal/daemon/ ./internal/adapters/telemetry/ ./internal/observe/sentryobs/ \
      -run 'TestNewTelemetrySink|TestForkPrivacyPolicyIsClosed|TestNewDaemonSentryConfig' -count=1 ); then
    bad "backend privacy tests failed"
  fi
else
  note "==> go test: SKIPPED (PRIVACY_GUARD_SKIP_GO=1)"
fi

# --- Behavioral gate 2: frontend composition ---------------------------------

if [[ "${PRIVACY_GUARD_SKIP_FRONTEND:-}" != "1" ]]; then
  note "==> vitest: renderer/main/release-target privacy policy"
  if ! ( cd frontend && npx vitest run --config vite.renderer.config.ts \
      src/shared/privacy-policy.test.ts \
      src/shared/telemetry.test.ts \
      src/shared/release-target.test.ts \
      src/renderer/lib/telemetry.init.test.ts \
      src/main/sentry-main.test.ts \
      src/main/desktop-version-floor.test.ts ); then
    bad "frontend privacy tests failed"
  fi
else
  note "==> vitest: SKIPPED (PRIVACY_GUARD_SKIP_FRONTEND=1)"
fi

# --- Behavioral gate 3: mobile composition -----------------------------------

if [[ "${PRIVACY_GUARD_SKIP_MOBILE:-}" != "1" ]]; then
  note "==> vitest: mobile privacy policy"
  if [[ ! -d packages/mobile/node_modules ]]; then
    bad "packages/mobile/node_modules is missing; run \`npm ci\` in packages/mobile (or use PRIVACY_GUARD_SKIP_MOBILE=1 outside CI)"
  elif ! ( cd packages/mobile && npx vitest run lib/sentry.test.ts lib/telemetry/fork-privacy.test.ts ); then
    bad "mobile privacy tests failed"
  fi
else
  note "==> vitest: SKIPPED (PRIVACY_GUARD_SKIP_MOBILE=1)"
fi

if [[ "$fail" -eq 0 ]]; then
  note "privacy guard: OK"
else
  note "privacy guard: FAILED"
fi
exit "$fail"
