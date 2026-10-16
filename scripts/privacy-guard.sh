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
#   2. Static alarms — a reintroduced exporter, an upstream update feed, or a
#      new observability dependency is flagged for review. Static matching is an
#      alarm, never the guarantee; it is here to catch what tests miss.
#   3. The documented policy — see docs/privacy-fork.md — which states what is
#      and is not allowed to leave the machine.
#
# Run locally from the repo root:
#   bash scripts/privacy-guard.sh
# Skip a half when a toolchain is unavailable (document which, and verify it in
# CI instead):
#   PRIVACY_GUARD_SKIP_GO=1 bash scripts/privacy-guard.sh
#
# It never publishes, deploys, or contacts an external service.
set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root" || exit 2

fail=0
note() { printf '%s\n' "$*"; }
bad() { printf 'FAIL: %s\n' "$*" >&2; fail=1; }

# --- 1. Behavioral: backend composition builds no remote exporter ------------

if [[ "${PRIVACY_GUARD_SKIP_GO:-}" != "1" ]]; then
  note "==> go test: telemetry/sentry composition and policy"
  if ! ( cd backend && go test ./internal/daemon/ ./internal/adapters/telemetry/ ./internal/observe/sentryobs/ \
      -run 'TestNewTelemetrySink|TestDaemonSentryDSN|TestNewDaemonSentryConfig' -count=1 ); then
    bad "backend privacy tests failed"
  fi
else
  note "==> go test: SKIPPED (PRIVACY_GUARD_SKIP_GO=1)"
fi

# --- 2. Policy constants stay closed -----------------------------------------

note "==> policy constants are closed"
grep -Eq 'forkExternalTelemetryExportAllowed[[:space:]]*=[[:space:]]*false' backend/internal/daemon/fork_privacy.go \
  || bad "backend/internal/daemon/fork_privacy.go: forkExternalTelemetryExportAllowed is no longer false"
grep -Eq 'forkExternalDiagnosticsExportAllowed[[:space:]]*=[[:space:]]*false' backend/internal/daemon/fork_privacy.go \
  || bad "backend/internal/daemon/fork_privacy.go: forkExternalDiagnosticsExportAllowed is no longer false"
grep -Eq 'EXTERNAL_TELEMETRY_EXPORT_ALLOWED[[:space:]]*=[[:space:]]*false' frontend/src/shared/fork-privacy.ts \
  || bad "frontend/src/shared/fork-privacy.ts: EXTERNAL_TELEMETRY_EXPORT_ALLOWED is no longer false"
grep -Eq 'EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED[[:space:]]*=[[:space:]]*false' frontend/src/shared/fork-privacy.ts \
  || bad "frontend/src/shared/fork-privacy.ts: EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED is no longer false"

# --- 3. Static: no remote exporter construction in production Go ------------

note "==> no PostHog exporter is constructed outside the dormant adapter"
remote_go_hits="$(grep -rn --include='*.go' 'NewPostHogSink(' backend \
  | grep -v '_test\.go' \
  | grep -v '/internal/adapters/telemetry/' || true)"
if [[ -n "$remote_go_hits" ]]; then
  bad "a production Go file constructs the PostHog exporter (fork policy forbids it):
$remote_go_hits"
fi

# --- 4. Static: updater and version floor never target upstream --------------

note "==> update feed and version floor target the fork, not upstream"
for f in \
  frontend/forge.config.ts \
  frontend/src/main/feature-builds.ts \
  frontend/src/main/desktop-version-floor.ts \
  desktop-version-floor.json; do
  # Ignore comment lines: the fork's own comments explain the upstream owner by
  # name. What must never reappear is the upstream owner as a code value.
  upstream_hits="$(grep -n 'Untrivial-ai' "$f" 2>/dev/null \
    | grep -vE '^[0-9]+:[[:space:]]*(//|\*|#)' || true)"
  if [[ -n "$upstream_hits" ]]; then
    bad "$f uses the upstream owner as a value; a fork build could be replaced by an upstream artifact:
$upstream_hits"
  fi
done

# --- 5. Static: no unreviewed observability dependency -----------------------

note "==> observability dependencies match the reviewed allowlist"
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

# --- 6. Alarm: report exporter init sites for human review -------------------
#
# Not a failure on its own — the shipping paths are disabled by the policy
# constants above — but any new/renamed init site here should be eyeballed.

note "==> review alarm: analytics/error init call sites (informational)"
grep -rn --include='*.ts' --include='*.tsx' -E '(posthog|PostHog)\.init\(|new PostHog\(|Sentry\.init\(|sentry\.Init\(' \
  frontend/src packages/mobile/lib 2>/dev/null \
  | grep -v '\.test\.' | sed 's/^/    /' || true

# --- 7. Behavioral: frontend policy tests ------------------------------------

if [[ "${PRIVACY_GUARD_SKIP_FRONTEND:-}" != "1" ]]; then
  note "==> vitest: renderer/main privacy policy"
  if ! ( cd frontend && npx vitest run --config vite.renderer.config.ts \
      src/shared/privacy-policy.test.ts src/shared/telemetry.test.ts src/renderer/lib/telemetry.init.test.ts ); then
    bad "frontend privacy tests failed"
  fi
else
  note "==> vitest: SKIPPED (PRIVACY_GUARD_SKIP_FRONTEND=1)"
fi

if [[ "$fail" -eq 0 ]]; then
  note "privacy guard: OK"
else
  note "privacy guard: FAILED"
fi
exit "$fail"
