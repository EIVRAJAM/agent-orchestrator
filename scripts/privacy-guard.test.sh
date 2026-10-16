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

# --- Multiline imports (specifier on a different line from the keyword) ------

d="$base/multiline-static-import"; make_fixture "$d"
put "$d/frontend/src/main/leak.ts" <<'EOF'
import {
  init
} from
  "@sentry/electron";
EOF
check "multiline static import" fail "$d"

d="$base/multiline-export-from"; make_fixture "$d"
put "$d/frontend/src/main/leak.ts" <<'EOF'
export {
  init as sentryInit,
} from
  "@sentry/electron";
EOF
check "multiline export ... from" fail "$d"

d="$base/multiline-dynamic-import"; make_fixture "$d"
put "$d/frontend/src/main/leak.ts" <<'EOF'
const sentry = await import(
  "@sentry/electron"
);
EOF
check "multiline dynamic import()" fail "$d"

d="$base/multiline-require"; make_fixture "$d"
put "$d/frontend/scripts/leak.cjs" <<'EOF'
const posthog = require(
  "posthog-js"
);
module.exports = posthog;
EOF
check "multiline require()" fail "$d"

d="$base/multiline-alias-indirect"; make_fixture "$d"
put "$d/frontend/src/main/leak.ts" <<'EOF'
import * as ErrorReporter from
  "@sentry/electron";

export const report = (error: unknown) => ErrorReporter.captureMessage(String(error));
EOF
check "multiline aliased import + indirect call" fail "$d"

# --- Allowlist location: allowed exactly there, rejected anywhere else -------

d="$base/allowlisted-location"; make_fixture "$d"
put "$d/packages/mobile/lib/telemetry/runtime.ts" <<'EOF'
import PostHog from "posthog-react-native";
export const MOBILE_TELEMETRY_DISABLED = true;
EOF
check "allowed import at an exact allowlist location" pass "$d"

d="$base/disallowed-location"; make_fixture "$d"
put "$d/frontend/src/main/telemetry-copy.ts" <<'EOF'
import PostHog from "posthog-react-native";
EOF
check "the same import outside the allowlist location" fail "$d"

# --- Package-name boundaries (no false positives from shared prefixes) --------

d="$base/real-sdk-dependency"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "dependencies": { "posthog-node": "4.0.0" } }
EOF
check "real observability SDK dependency is detected" fail "$d"

d="$base/prefix-only-dependency"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "dependencies": { "posthog-analytics": "1.0.0", "segment-analytics": "1.0.0" } }
EOF
check "prefix-only name is not a false positive" pass "$d"

d="$base/scoped-prefix-dependency"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "dependencies": { "@sentryfoo/thing": "1.0.0" } }
EOF
check "scoped name that only shares a prefix is not a false positive" pass "$d"

d="$base/sdk-subpath-import"; make_fixture "$d"
put "$d/frontend/src/renderer/leak.ts" <<'EOF'
import { PostHog } from "posthog-js/dist/module.full.no-external";
EOF
check "legitimate SDK subpath import outside the allowlist" fail "$d"

d="$base/backtick-dynamic-import"; make_fixture "$d"
put "$d/frontend/src/main/leak.ts" <<'EOF'
const sentry = await import(`@sentry/electron`);
EOF
check "backtick dynamic import()" fail "$d"

d="$base/overrides-dependency"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "overrides": { "posthog-node": "4.0.0" } }
EOF
check "observability package pinned via overrides" fail "$d"

d="$base/resolutions-dependency"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "resolutions": { "**/posthog-node": "4.0.0" } }
EOF
check "observability package pinned via a resolutions selector" fail "$d"

# --- npm alias indirection (a name search misses the real package) -----------

d="$base/npm-alias-unscoped"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "dependencies": { "reporter": "npm:posthog-js@1.2.3" } }
EOF
check "npm alias of an unscoped observability package" fail "$d"

d="$base/npm-alias-scoped"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "dependencies": { "reporter": "npm:@sentry/electron@^7.0.0" } }
EOF
check "npm alias of a scoped observability package" fail "$d"

d="$base/npm-alias-dev"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "devDependencies": { "reporter": "npm:@sentry/electron@7.0.0" } }
EOF
check "npm alias inside devDependencies" fail "$d"

d="$base/npm-alias-overrides"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "overrides": { "reporter": "npm:posthog-node@4.0.0" } }
EOF
check "npm alias inside overrides" fail "$d"

d="$base/npm-alias-resolutions"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "resolutions": { "reporter": "npm:@sentry/node@10.0.0" } }
EOF
check "npm alias inside resolutions" fail "$d"

d="$base/npm-alias-pnpm-overrides"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "pnpm": { "overrides": { "reporter": "npm:mixpanel@0.18.0" } } }
EOF
check "npm alias inside pnpm.overrides" fail "$d"

d="$base/imports-map-alias"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "imports": { "#reporter": "@sentry/electron" } }
EOF
check "package.json imports map aliasing an SDK" fail "$d"

d="$base/imports-map-conditional"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "imports": { "#reporter": { "node": "./local.ts", "default": "@sentry/electron" } } }
EOF
check "conditional package.json imports map aliasing an SDK" fail "$d"

d="$base/alias-import-indirect-call"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "imports": { "#reporter": "@sentry/electron" } }
EOF
put "$d/frontend/src/telemetry-via-alias.ts" <<'EOF'
import * as Reporter from "#reporter";
export const report = (error: unknown) => Reporter.init({ dsn: String(error) });
EOF
check "import via an alias followed by an arbitrary-identifier call" fail "$d"

d="$base/tsconfig-path-alias"; make_fixture "$d"
put "$d/frontend/tsconfig.json" <<'EOF'
{ "compilerOptions": { "baseUrl": ".", "paths": { "@reporter": ["@sentry/electron"] } } }
EOF
check "tsconfig paths alias to an SDK" fail "$d"

d="$base/vite-alias"; make_fixture "$d"
put "$d/frontend/vite.renderer.config.ts" <<'EOF'
export default { resolve: { alias: { "#reporter": "@sentry/electron" } } };
EOF
check "Vite resolve.alias to an SDK" fail "$d"

d="$base/normal-dependency-values"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "dependencies": { "react": "^19.0.0", "reporter": "npm:some-lib@1.0.0" }, "resolutions": { "**/left-pad": "1.3.0" } }
EOF
check "normal ranges and a non-SDK npm alias are allowed" pass "$d"

d="$base/self-alias-allowlisted-dep"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "dependencies": { "@sentry/electron": "npm:@sentry/electron@7.0.0" } }
EOF
check "self-alias of an allowlisted dormant dependency is allowed" pass "$d"

# --- Benign literals must not false-positive ---------------------------------

d="$base/benign-string"; make_fixture "$d"
put "$d/frontend/src/benign-string.ts" <<'EOF'
const explanation = 'data from "@sentry/electron" is not an import';
EOF
check "SDK name inside an ordinary string" pass "$d"

d="$base/benign-template"; make_fixture "$d"
put "$d/frontend/src/benign-template.ts" <<'EOF'
const example = `import x from "@sentry/electron";`;
EOF
check "SDK name inside a template literal example" pass "$d"

d="$base/benign-comment"; make_fixture "$d"
put "$d/frontend/src/benign-comment.ts" <<'EOF'
// import y from "@sentry/electron";
/* const p = require("posthog-js"); */
EOF
check "SDK name inside comments" pass "$d"

d="$base/benign-regex"; make_fixture "$d"
put "$d/frontend/src/benign-regex.ts" <<'EOF'
const re = /from "@sentry\/electron"/;
EOF
check "SDK name inside a regex literal" pass "$d"

d="$base/benign-from-word"; make_fixture "$d"
put "$d/frontend/src/benign-from-word.ts" <<'EOF'
const text = "the from source";
const fromFallback = 1;
EOF
check "the word 'from' in ordinary text" pass "$d"

d="$base/benign-mixed-quotes"; make_fixture "$d"
put "$d/frontend/src/benign-mixed-quotes.ts" <<'EOF'
const single = "from '@sentry/electron'";
const double = 'from "posthog-js"';
EOF
check "SDK name with different inner delimiters" pass "$d"

if [[ "$failures" -eq 0 ]]; then
  printf 'privacy-guard self-tests: OK\n'
  exit 0
fi
printf 'privacy-guard self-tests: %s failed\n' "$failures" >&2
exit 1
