#!/usr/bin/env bash
#
# Self-tests for scripts/privacy-guard.sh.
#
# Builds throwaway fixture trees from the real policy files and asserts the
# static gates PASS on a clean tree and FAIL on each reintroduction:
#   - a new Sentry.init
#   - a new posthog.init
#   - a mobile telemetry reactivation (policy guard removed / constant flipped)
#   - a new observability dependency
#   - the release target pointed at upstream
#
# Run from the repo root: bash scripts/privacy-guard.test.sh
# It never publishes, deploys, or contacts an external service.
set -uo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
guard="$repo/scripts/privacy-guard.sh"

files=(
  backend/internal/daemon/fork_privacy.go
  frontend/src/shared/fork-privacy.ts
  frontend/src/shared/release-target.ts
  frontend/forge.config.ts
  frontend/src/main/feature-builds.ts
  frontend/src/main/desktop-version-floor.ts
  frontend/src/main/sentry-main.ts
  frontend/src/renderer/lib/telemetry.ts
  frontend/src/renderer/lib/sentry.ts
  packages/mobile/lib/fork-privacy.ts
  packages/mobile/lib/sentry.ts
  packages/mobile/lib/telemetry/config.ts
  packages/mobile/lib/telemetry/runtime.ts
  package.json
  frontend/package.json
  packages/mobile/package.json
  desktop-version-floor.json
)

make_fixture() {
  local dir="$1"
  mkdir -p "$dir"
  ( cd "$repo" && cp --parents "${files[@]}" "$dir/" )
}

# run_guard echoes the guard's exit code for a fixture root.
run_guard() {
  PRIVACY_GUARD_ROOT="$1" PRIVACY_GUARD_STATIC_ONLY=1 bash "$guard" >/dev/null 2>&1
  printf '%s' "$?"
}

failures=0
expect_pass() {
  local dir="$1" name="$2" rc
  rc="$(run_guard "$dir")"
  if [[ "$rc" == "0" ]]; then
    printf 'ok (pass): %s\n' "$name"
  else
    printf 'SELF-TEST FAIL: %s expected to pass, got exit %s\n' "$name" "$rc" >&2
    failures=$((failures + 1))
  fi
}
expect_fail() {
  local dir="$1" name="$2" rc
  rc="$(run_guard "$dir")"
  if [[ "$rc" != "0" ]]; then
    printf 'ok (fail): %s\n' "$name"
  else
    printf 'SELF-TEST FAIL: %s expected to fail, but the guard passed\n' "$name" >&2
    failures=$((failures + 1))
  fi
}

base="$(mktemp -d)"
trap 'rm -rf "$base"' EXIT

# 1. A clean fixture must pass the static gates.
make_fixture "$base/clean"
expect_pass "$base/clean" "clean policy tree"

# 2. A new Sentry.init must fail.
make_fixture "$base/sentry"
printf 'import * as Sentry from "@sentry/electron";\nSentry.init({ dsn: "https://abc@o1.ingest.sentry.io/1" });\n' \
  >"$base/sentry/frontend/src/main/new-sentry.ts"
expect_fail "$base/sentry" "new Sentry.init"

# 3. A new posthog.init (and new PostHog) must fail.
make_fixture "$base/posthog"
printf 'import posthog from "posthog-js";\nposthog.init("phc_test", {});\nnew PostHog("phc_test", {});\n' \
  >"$base/posthog/frontend/src/renderer/new-analytics.ts"
expect_fail "$base/posthog" "new posthog.init / new PostHog"

# 4. Reactivating mobile telemetry by dropping the policy guard must fail.
make_fixture "$base/mobile-config"
printf 'export const MOBILE_TELEMETRY_DISABLED = false;\n' \
  >"$base/mobile-config/packages/mobile/lib/telemetry/config.ts"
expect_fail "$base/mobile-config" "mobile telemetry reactivation (guard removed)"

# 5. Flipping the mobile policy constant must fail.
make_fixture "$base/mobile-policy"
sed -i 's/MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED = false/MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED = true/' \
  "$base/mobile-policy/packages/mobile/lib/fork-privacy.ts"
expect_fail "$base/mobile-policy" "mobile policy constant flipped"

# 6. A new observability dependency must fail.
make_fixture "$base/dep"
node -e '
const fs = require("fs");
const p = process.argv[1];
const j = JSON.parse(fs.readFileSync(p, "utf8"));
j.dependencies = j.dependencies ?? {};
j.dependencies["@datadog/browser-rum"] = "1.0.0";
fs.writeFileSync(p, JSON.stringify(j, null, 2));
' "$base/dep/frontend/package.json"
expect_fail "$base/dep" "new observability dependency"

# 7. Pointing the release target at upstream must fail.
make_fixture "$base/upstream"
sed -i 's#AUTHORIZED_RELEASE_REPOS = \["EIVRAJAM/agent-orchestrator"\]#AUTHORIZED_RELEASE_REPOS = ["Untrivial-ai/agent-orchestrator"]#' \
  "$base/upstream/frontend/src/shared/release-target.ts"
expect_fail "$base/upstream" "release target pointed at upstream"

if [[ "$failures" -eq 0 ]]; then
  printf 'privacy-guard self-tests: OK\n'
  exit 0
fi
printf 'privacy-guard self-tests: %s failed\n' "$failures" >&2
exit 1
