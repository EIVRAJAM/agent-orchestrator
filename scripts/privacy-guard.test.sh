#!/usr/bin/env bash
#
# Self-tests for scripts/privacy-guard.sh and scripts/lib/privacy-scan.mjs.
#
# Each case builds a fresh fixture from the real policy files, injects exactly
# ONE condition, and runs the guard's static gates against it. A shared fixture
# keeps the surrounding policy files intact, so a failure is attributable to the
# injected condition and one working pattern cannot hide a broken one.
#
# Expected results are recorded per case (expected vs obtained exit code).
#
# Run from the repo root: bash scripts/privacy-guard.test.sh
# It never publishes, deploys, or contacts an external service.
set -uo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
guard="$repo/scripts/privacy-guard.sh"

# The full set of files the static gates read, plus the dormant implementations
# and their reviewed tests so the clean tree exercises the allowlists. Only
# imports/deps/constants change per case.
files=(
  backend/internal/daemon/fork_privacy.go
  backend/internal/daemon/daemon.go
  backend/internal/adapters/telemetry/posthog.go
  backend/internal/observe/sentryobs/sentryobs.go
  backend/internal/observe/sentryobs/sentryobs_test.go
  frontend/forge.config.ts
  frontend/package.json
  frontend/src/main/feature-builds.ts
  frontend/src/main/desktop-version-floor.ts
  frontend/src/main/sentry-main.ts
  frontend/src/renderer/lib/telemetry.ts
  frontend/src/renderer/lib/telemetry.test.ts
  frontend/src/renderer/lib/sentry.ts
  frontend/src/shared/fork-privacy.ts
  frontend/src/shared/release-target.ts
  packages/mobile/lib/fork-privacy.ts
  packages/mobile/lib/sentry.ts
  packages/mobile/lib/telemetry/config.ts
  packages/mobile/lib/telemetry/runtime.ts
  packages/mobile/package.json
  package.json
  desktop-version-floor.json
  scripts/lib/privacy-scan.mjs
)

make_fixture() {
  local dir="$1"
  mkdir -p "$dir"
  ( cd "$repo" && cp --parents "${files[@]}" "$dir/" )
}

put() { # dir-relative path, then stdin content
  local rel="$1"
  mkdir -p "$(dirname "$rel")"
  cat >"$rel"
}

run_guard() { # dir -> exit code
  PRIVACY_GUARD_ROOT="$1" PRIVACY_GUARD_STATIC_ONLY=1 bash "$guard" >/dev/null 2>&1
  printf '%s' "$?"
}

failures=0
check() { # name expected(pass|fail) dir
  local name="$1" expected="$2" dir="$3" rc expected_code
  rc="$(run_guard "$dir")"
  if [[ "$expected" == "pass" ]]; then expected_code=0; else expected_code="non-zero"; fi
  if { [[ "$expected" == "pass" && "$rc" == "0" ]] || [[ "$expected" == "fail" && "$rc" != "0" ]]; }; then
    printf 'ok   [expect %-8s obtained %-8s] %s\n' "$expected_code" "$rc" "$name"
  else
    printf 'FAIL [expect %-8s obtained %-8s] %s\n' "$expected_code" "$rc" "$name" >&2
    failures=$((failures + 1))
  fi
}

base="$(mktemp -d)"
trap 'rm -rf "$base"' EXIT

d="$base/clean"; make_fixture "$d"
check "clean dormant tree (implementations + reviewed tests allowed)" pass "$d"

d="$base/sentry-direct"; make_fixture "$d"
put "$d/frontend/src/main/leak.ts" <<'EOF'
Sentry.init({ dsn: "https://abc@o1.ingest.sentry.io/1" });
EOF
check "direct Sentry.init" fail "$d"

d="$base/sentry-alias"; make_fixture "$d"
put "$d/frontend/src/main/leak.ts" <<'EOF'
import * as ErrorReporter from "@sentry/electron";
ErrorReporter.init({ dsn: "https://abc@o1.ingest.sentry.io/1" });
EOF
check "Sentry imported under a different alias" fail "$d"

d="$base/go-sentry-go-alias"; make_fixture "$d"
put "$d/backend/internal/daemon/leak.go" <<'EOF'
package daemon

import sdk "github.com/getsentry/sentry-go"

var _ = sdk.Init
EOF
check "sentry-go imported under a different alias" fail "$d"

d="$base/posthog-init-only"; make_fixture "$d"
put "$d/frontend/src/renderer/leak.ts" <<'EOF'
posthog.init("phc_should_not_be_used", {});
EOF
check "posthog.init without new PostHog in the same fixture" fail "$d"

d="$base/posthog-new-only"; make_fixture "$d"
put "$d/frontend/src/renderer/leak.ts" <<'EOF'
new PostHog("phc_should_not_be_used", {});
EOF
check "new PostHog without posthog.init in the same fixture" fail "$d"

d="$base/dynamic-import"; make_fixture "$d"
put "$d/frontend/src/main/leak.ts" <<'EOF'
export const load = () => import("@sentry/electron");
EOF
check "dynamic import() of Sentry" fail "$d"

d="$base/require"; make_fixture "$d"
put "$d/frontend/scripts/leak.cjs" <<'EOF'
const posthog = require("posthog-js");
module.exports = posthog;
EOF
check "require() of PostHog" fail "$d"

d="$base/mobile-app"; make_fixture "$d"
put "$d/packages/mobile/app/leak.ts" <<'EOF'
import * as Sentry from "@sentry/react-native";
Sentry.init({ dsn: "https://abc@o1.ingest.sentry.io/1" });
EOF
check "initialization inside packages/mobile/app/**" fail "$d"

d="$base/product-ui-import"; make_fixture "$d"
put "$d/packages/product-ui/src/leak.ts" <<'EOF'
import posthog from "posthog-js";
posthog.init("phc_should_not_be_used", {});
EOF
check "import inside packages/product-ui/**" fail "$d"

d="$base/cloud-client-import"; make_fixture "$d"
put "$d/packages/cloud-client/src/leak.ts" <<'EOF'
import * as Sentry from "@sentry/browser";
EOF
check "import inside packages/cloud-client/**" fail "$d"

d="$base/product-ui-dep"; make_fixture "$d"
put "$d/packages/product-ui/package.json" <<'EOF'
{ "name": "@aoagents/product-ui", "version": "0.0.0", "dependencies": { "@datadog/browser-rum": "1.0.0" } }
EOF
check "observability dependency in packages/product-ui/package.json" fail "$d"

d="$base/discovered-package-dep"; make_fixture "$d"
put "$d/packages/cloud-client/package.json" <<'EOF'
{ "name": "@aoagents/cloud-client", "version": "0.0.0", "dependencies": { "mixpanel": "1.0.0" } }
EOF
check "observability dependency in another discovered package" fail "$d"

d="$base/mobile-config-reactivated"; make_fixture "$d"
put "$d/packages/mobile/lib/telemetry/config.ts" <<'EOF'
export const MOBILE_TELEMETRY_DISABLED = false;
EOF
check "mobile telemetry reactivation (policy guard removed)" fail "$d"

d="$base/mobile-policy-flipped"; make_fixture "$d"
sed -i 's/MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED = false/MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED = true/' \
  "$d/packages/mobile/lib/fork-privacy.ts"
check "mobile policy constant flipped" fail "$d"

d="$base/release-upstream"; make_fixture "$d"
sed -i 's#AUTHORIZED_RELEASE_REPOS = \["EIVRAJAM/agent-orchestrator"\]#AUTHORIZED_RELEASE_REPOS = ["Untrivial-ai/agent-orchestrator"]#' \
  "$d/frontend/src/shared/release-target.ts"
check "release target pointed at upstream" fail "$d"

d="$base/release-unauthorized"; make_fixture "$d"
sed -i 's#AUTHORIZED_RELEASE_REPOS = \["EIVRAJAM/agent-orchestrator"\]#AUTHORIZED_RELEASE_REPOS = ["attacker/fork"]#' \
  "$d/frontend/src/shared/release-target.ts"
check "release target pointed at an unauthorized repo" fail "$d"

if [[ "$failures" -eq 0 ]]; then
  printf 'privacy-guard self-tests: OK\n'
  exit 0
fi
printf 'privacy-guard self-tests: %s failed\n' "$failures" >&2
exit 1
