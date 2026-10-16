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
#      An explicit allowlist pins the deliberately-dormant files, and those files
#      must still carry their fork-policy guard.
#   3. The documented policy — see docs/privacy-fork.md — which states what is
#      and is not allowed to leave the machine.
#
# Static matching is an alarm, never the guarantee; the behavioral tests are the
# backstop for a reintroduction that avoids the grepped shapes.
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

require_absent_file() {
  local file="$1"
  [[ -e "$file" ]] && bad "$file must not exist"
}

# --- Static gate 1: policy constants are closed ------------------------------

note "==> static: fork policy constants are closed"
require_false backend/internal/daemon/fork_privacy.go 'forkExternalTelemetryExportAllowed[[:space:]]*=[[:space:]]*false'
require_false backend/internal/daemon/fork_privacy.go 'forkExternalDiagnosticsExportAllowed[[:space:]]*=[[:space:]]*false'
require_false frontend/src/shared/fork-privacy.ts 'EXTERNAL_TELEMETRY_EXPORT_ALLOWED[[:space:]]*=[[:space:]]*false'
require_false frontend/src/shared/fork-privacy.ts 'EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED[[:space:]]*=[[:space:]]*false'
require_false packages/mobile/lib/fork-privacy.ts 'MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED[[:space:]]*=[[:space:]]*false'
require_false packages/mobile/lib/fork-privacy.ts 'MOBILE_EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED[[:space:]]*=[[:space:]]*false'

# --- Static gate 2: authorized observability init/construction sites ---------
#
# Every exporter construction or client init in Go, Electron main/renderer and
# mobile must be in the reviewed allowlist below. Anything else fails.

note "==> static: observability init/construction sites are allowlisted"
allowed=" backend/internal/adapters/telemetry/posthog.go backend/internal/observe/sentryobs/sentryobs.go frontend/src/renderer/lib/telemetry.ts packages/mobile/lib/telemetry/runtime.ts packages/mobile/lib/sentry.ts "
init_pattern='posthog\.init\(|new PostHog\(|Sentry\.init\(|sentry\.Init\(|sentry\.init\(|mod\.init\(|NewPostHogSink\(|sentry\.NewHTTPSyncTransport\('
scan_dirs=(backend frontend/src packages/mobile/lib)
mapfile -t init_hits < <(
  grep -rnE --include='*.go' --include='*.ts' --include='*.tsx' "$init_pattern" "${scan_dirs[@]}" 2>/dev/null || true
)
for hit in "${init_hits[@]:-}"; do
  [[ -z "$hit" ]] && continue
  file="${hit%%:*}"
  file="${file#./}"
  base="$(basename "$file")"
  case "$base" in
    *_test.go|*.test.ts|*.test.tsx) continue ;;
  esac
  content="${hit#*:}"; content="${content#*:}"
  trimmed="${content#"${content%%[![:space:]]*}"}"
  case "$trimmed" in
    //*|'*'*|'/*'*|'#'*) continue ;; # comment line
  esac
  if [[ " $allowed " != *" $file "* ]]; then
    bad "unauthorized observability init/construction in $file: $trimmed"
  fi
done

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

# --- Static gate 5: no unreviewed observability dependency -------------------

note "==> static: observability dependencies match the reviewed allowlist"
node - <<'NODE' || bad "unreviewed observability dependency found"
const fs = require("fs");
const files = ["package.json", "frontend/package.json", "packages/mobile/package.json"];
// Direct dependencies that ARE allowed to be present: they back dormant code
// paths (see docs/privacy-fork.md) and are neutralized at the composition
// points. Adding any other analytics/error SDK must be a deliberate review.
const reviewed = new Set([
  "posthog-js",
  "posthog-react-native",
  "@sentry/electron",
]);
const observability = /^(posthog|@posthog\/|@sentry\/|@amplitude\/|@datadog\/|@honeycombio\/|@opentelemetry\/|@bugsnag\/|bugsnag|amplitude|mixpanel|segment|@segment\/|datadog|dd-trace|newrelic|rollbar|logrocket|fullstory|heap|@logtail\/|@highlight-run\/)/;
const flagged = [];
for (const file of files) {
  if (!fs.existsSync(file)) continue;
  const pkg = JSON.parse(fs.readFileSync(file, "utf8"));
  for (const field of ["dependencies", "devDependencies", "optionalDependencies"]) {
    for (const name of Object.keys(pkg[field] ?? {})) {
      if (observability.test(name) && !reviewed.has(name)) flagged.push(`${file}: ${name}`);
    }
  }
}
if (flagged.length) {
  console.error("unreviewed observability dependency present:\n" + flagged.join("\n"));
  process.exit(1);
}
console.log("ok: no unreviewed observability dependency");
NODE

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
