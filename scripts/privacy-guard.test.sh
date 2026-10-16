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

# --- Resolution configs: Vite, Babel, Metro ----------------------------------

d="$base/vite-product-ui-alias"; make_fixture "$d"
put "$d/packages/product-ui/vite.build.config.ts" <<'EOF'
export default { resolve: { alias: { reporter: "@sentry/electron" } } };
EOF
check "Vite resolve.alias in packages/product-ui (object form)" fail "$d"

d="$base/vite-conventional-defineConfig"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import { defineConfig } from "vite";
export default defineConfig({ resolve: { alias: { reporter: "@sentry/electron" } } });
EOF
check "conventional vite.config.ts with defineConfig resolve.alias" fail "$d"

d="$base/vite-outside-frontend-mjs"; make_fixture "$d"
put "$d/packages/product-ui/vite.config.mjs" <<'EOF'
export default { resolve: { alias: { reporter: "@sentry/electron" } } };
EOF
check "Vite config outside frontend (packages/product-ui, .mjs)" fail "$d"

d="$base/vite-array-find-replacement"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
export default { resolve: { alias: [{ find: "reporter", replacement: "@sentry/electron" }] } };
EOF
check "Vite resolve.alias { find, replacement } array form" fail "$d"

d="$base/vite-non-alias-sdk-string"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const banner = "generated without @sentry/electron";
export default { resolve: { alias: { "@ui": "./src/ui" } }, build: { banner } };
EOF
check "an SDK string in a Vite config that is not an alias" pass "$d"

d="$base/vite-benign-alias"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
export default { resolve: { alias: { "@ui": "./src/ui", "@utils": "./src/utils" } } };
EOF
check "a benign Vite resolve.alias" pass "$d"

d="$base/vite-dynamic-alias-unsupported"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const aliases = buildAliasMap();
export default { resolve: { alias: aliases } };
EOF
check "a dynamic (non-literal) Vite resolve.alias is rejected fail-closed" fail "$d"

d="$base/babel-module-resolver-alias"; make_fixture "$d"
put "$d/packages/mobile/babel.config.js" <<'EOF'
module.exports = { plugins: [["module-resolver", { alias: { reporter: "@sentry/electron" } }]] };
EOF
check "Babel module-resolver alias to an SDK" fail "$d"

d="$base/metro-extra-node-modules"; make_fixture "$d"
put "$d/packages/mobile/metro.config.js" <<'EOF'
const config = {};
config.resolver.extraNodeModules = { reporter: "@sentry/electron" };
module.exports = config;
EOF
check "Metro resolver.extraNodeModules alias to an SDK" fail "$d"

d="$base/metro-resolve-request-literal"; make_fixture "$d"
put "$d/packages/mobile/metro.config.js" <<'EOF'
const config = {};
config.resolver.resolveRequest = (ctx, name, platform) => (name === "@sentry/electron" ? ctx.resolveRequest(ctx, name, platform) : null);
module.exports = config;
EOF
check "Metro resolver.resolveRequest naming an SDK literally" fail "$d"

d="$base/metro-resolve-request-computed"; make_fixture "$d"
put "$d/packages/mobile/metro.config.js" <<'EOF'
const sdk = ["post", "hog-js"].join("");
const config = {};
config.resolver.resolveRequest = (ctx, name, platform) => (name === sdk ? ctx.resolveRequest(ctx, name, platform) : null);
module.exports = config;
EOF
check "Metro resolver with a computed module name (documented gap: not detected)" pass "$d"

# --- Manifest-scoped dependency allowlist ------------------------------------

d="$base/dep-allowed-frontend"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "dependencies": { "@sentry/electron": "7.17.0", "posthog-js": "^1.0.0" } }
EOF
check "frontend's dormant dependencies pass in frontend/package.json" pass "$d"

d="$base/dep-allowed-mobile"; make_fixture "$d"
put "$d/packages/mobile/package.json" <<'EOF'
{ "name": "@agent-orchestrator/mobile", "version": "0.0.0", "dependencies": { "posthog-react-native": "^4.62.0" } }
EOF
check "mobile's dormant dependency passes in packages/mobile/package.json" pass "$d"

d="$base/dep-foreign-direct"; make_fixture "$d"
put "$d/packages/product-ui/package.json" <<'EOF'
{ "name": "@aoagents/product-ui", "version": "0.0.0", "dependencies": { "@sentry/electron": "7.17.0" } }
EOF
check "a dormant dependency in a foreign manifest (dependencies)" fail "$d"

d="$base/dep-foreign-dev"; make_fixture "$d"
put "$d/packages/product-ui/package.json" <<'EOF'
{ "name": "@aoagents/product-ui", "version": "0.0.0", "devDependencies": { "@sentry/electron": "7.17.0" } }
EOF
check "a dormant dependency in a foreign manifest (devDependencies)" fail "$d"

d="$base/dep-foreign-optional"; make_fixture "$d"
put "$d/packages/product-ui/package.json" <<'EOF'
{ "name": "@aoagents/product-ui", "version": "0.0.0", "optionalDependencies": { "posthog-js": "^1.0.0" } }
EOF
check "a dormant dependency in a foreign manifest (optionalDependencies)" fail "$d"

d="$base/dep-foreign-peer"; make_fixture "$d"
put "$d/packages/product-ui/package.json" <<'EOF'
{ "name": "@aoagents/product-ui", "version": "0.0.0", "peerDependencies": { "@sentry/electron": "7.17.0" } }
EOF
check "a dormant dependency in a foreign manifest (peerDependencies)" fail "$d"

d="$base/self-alias-foreign-manifest"; make_fixture "$d"
put "$d/packages/product-ui/package.json" <<'EOF'
{ "name": "@aoagents/product-ui", "version": "0.0.0", "dependencies": { "@sentry/electron": "npm:@sentry/electron@7.0.0" } }
EOF
check "a self-alias outside its authorized manifest" fail "$d"

d="$base/override-foreign-manifest"; make_fixture "$d"
put "$d/packages/product-ui/package.json" <<'EOF'
{ "name": "@aoagents/product-ui", "version": "0.0.0", "overrides": { "reporter": "npm:@sentry/electron@7.0.0" } }
EOF
check "an override alias in a foreign manifest" fail "$d"

d="$base/new-obs-package-authorized-manifest"; make_fixture "$d"
put "$d/frontend/package.json" <<'EOF'
{ "name": "agent-orchestrator", "version": "0.0.0", "dependencies": { "@datadog/browser-rum": "5.0.0" } }
EOF
check "a new observability package fails in an authorized manifest" fail "$d"

# --- Strict static classification of alias targets ---------------------------

d="$base/alias-call-partial-literal"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const target = "@sentry/electron";
export default { resolve: { alias: { reporter: resolveAlias("production", target) } } };
EOF
check "a call with a benign literal and a dynamic identifier (fail)" fail "$d"

d="$base/alias-template-interpolation"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const dir = "./src";
export default { resolve: { alias: { reporter: `${dir}/ui` } } };
EOF
check "a template literal with interpolation (fail)" fail "$d"

d="$base/alias-concatenation"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const name = "ui";
export default { resolve: { alias: { reporter: "./src/" + name } } };
EOF
check "a partially dynamic concatenation (fail)" fail "$d"

d="$base/alias-path-resolve-local"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
export default { resolve: { alias: { "@ui": path.resolve(__dirname, "src/ui") } } };
EOF
check "a legitimate local path.resolve() target (pass)" pass "$d"

d="$base/alias-path-resolve-node-modules-sdk"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
export default { resolve: { alias: { reporter: path.resolve(__dirname, "node_modules/@sentry/electron") } } };
EOF
check "path.resolve() into node_modules/@sentry/electron (fail)" fail "$d"

d="$base/alias-file-url-local"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import { fileURLToPath } from "node:url";
export default { resolve: { alias: { "@": fileURLToPath(new URL("./src/renderer", import.meta.url)) } } };
EOF
check "the repository's fileURLToPath(new URL(...)) local alias (pass)" pass "$d"

d="$base/alias-file-url-node-modules-sdk"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import { fileURLToPath } from "node:url";
export default { resolve: { alias: { reporter: fileURLToPath(new URL("./node_modules/@sentry/electron", import.meta.url)) } } };
EOF
check "fileURLToPath(new URL(...)) into a node_modules SDK (fail)" fail "$d"

d="$base/alias-literal-benign"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
export default { resolve: { alias: { "@ui": "./src/ui" } } };
EOF
check "a literal benign alias target (pass)" pass "$d"

d="$base/alias-literal-observability"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
export default { resolve: { alias: { reporter: "@sentry/electron" } } };
EOF
check "a literal observability alias target (fail)" fail "$d"

# --- Shorthand, spreads and indirection --------------------------------------

d="$base/vite-shorthand-resolvable"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const alias = { reporter: "./src/local-reporter" };
export default { resolve: { alias } };
EOF
check "a resolvable Vite shorthand alias (pass)" pass "$d"

d="$base/vite-shorthand-dynamic"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const alias = buildAliases();
export default { resolve: { alias } };
EOF
check "a dynamic Vite shorthand alias (fail)" fail "$d"

d="$base/babel-shorthand-dynamic"; make_fixture "$d"
put "$d/packages/mobile/babel.config.js" <<'EOF'
const alias = buildAliases();
module.exports = { plugins: [["module-resolver", { alias }]] };
EOF
check "a dynamic Babel module-resolver shorthand (fail)" fail "$d"

d="$base/metro-shorthand-dynamic"; make_fixture "$d"
put "$d/packages/mobile/metro.config.js" <<'EOF'
const extraNodeModules = buildModules();
module.exports = { resolver: { extraNodeModules } };
EOF
check "a dynamic Metro extraNodeModules shorthand (fail)" fail "$d"

d="$base/vite-spread-in-resolve"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const base = {};
export default { resolve: { ...base, alias: { "@ui": "./src/ui" } } };
EOF
check "a dynamic spread inside resolve (fail)" fail "$d"

d="$base/vite-spread-in-alias"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const base = {};
export default { resolve: { alias: { ...base } } };
EOF
check "a dynamic spread inside alias (fail)" fail "$d"

d="$base/vite-resolve-indirect-resolvable"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const resolve = { alias: { "@ui": "./src/ui" } };
export default { resolve };
EOF
check "an indirect but resolvable resolve object (pass)" pass "$d"

d="$base/vite-resolve-indirect-unresolvable"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const resolve = buildResolve();
export default { resolve };
EOF
check "an indirect unresolvable resolve object (fail)" fail "$d"

d="$base/babel-full-plugin-name"; make_fixture "$d"
put "$d/packages/mobile/babel.config.js" <<'EOF'
module.exports = { plugins: [["babel-plugin-module-resolver", { alias: { reporter: "@sentry/electron" } }]] };
EOF
check "the full babel-plugin-module-resolver name (fail)" fail "$d"

# --- Indirect Vite exports ---------------------------------------------------

d="$base/vite-export-default-binding"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const config = { resolve: { alias: { reporter: "@sentry/electron" } } };
export default config;
EOF
check "export default <const binding> with an observability alias (fail)" fail "$d"

d="$base/vite-export-defineconfig-binding"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import { defineConfig } from "vite";
const config = { resolve: { alias: { reporter: "@sentry/electron" } } };
export default defineConfig(config);
EOF
check "export default defineConfig(<const binding>) (fail)" fail "$d"

d="$base/vite-module-exports-binding"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const config = { resolve: { alias: { reporter: "@sentry/electron" } } };
module.exports = config;
EOF
check "module.exports = <const binding> (fail)" fail "$d"

d="$base/vite-defineconfig-arrow"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import { defineConfig } from "vite";
export default defineConfig(() => ({ resolve: { alias: { reporter: "@sentry/electron" } } }));
EOF
check "defineConfig(() => ({ observability alias })) (fail)" fail "$d"

d="$base/vite-module-exports-arrow"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
module.exports = () => ({ resolve: { alias: { reporter: "@sentry/electron" } } });
EOF
check "module.exports = () => ({ observability alias }) (fail)" fail "$d"

d="$base/vite-export-arrow-benign"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
export default () => ({ resolve: { alias: { "@ui": "./src/ui" } } });
EOF
check "an arrow function with a fully static benign return (pass)" pass "$d"

d="$base/vite-export-dynamic-unresolvable"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
export default buildConfig();
EOF
check "a dynamic/unresolvable exported config (fail)" fail "$d"

d="$base/vite-export-indirect-benign"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const config = { resolve: { alias: { "@ui": "./src/ui" } } };
export default config;
EOF
check "an indirect static benign exported config (pass)" pass "$d"

# --- Helper provenance (path/url) --------------------------------------------

d="$base/vite-helper-fake-path-object"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const path = { resolve() { return "@sentry/electron"; } };
export default { resolve: { alias: { reporter: path.resolve("x") } } };
EOF
check "a local object named path is not a trusted helper (fail)" fail "$d"

d="$base/vite-helper-fake-fileurltopath-function"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
function fileURLToPath() { return "@sentry/electron"; }
export default { resolve: { alias: { reporter: fileURLToPath("x") } } };
EOF
check "a local function named fileURLToPath is not trusted (fail)" fail "$d"

d="$base/vite-helper-alias-dynamic-arg"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
const resolve = path.resolve;
export default { resolve: { alias: { reporter: resolve(dynamicValue) } } };
EOF
check "an alias of path.resolve with a dynamic argument (fail)" fail "$d"

d="$base/vite-helper-reassigned"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
path = fake;
export default { resolve: { alias: { reporter: path.resolve("x") } } };
EOF
check "a reassigned trusted helper binding (fail)" fail "$d"

d="$base/vite-helper-shadowed"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
function build(path) { return path.resolve("x"); }
export default { resolve: { alias: { reporter: build(0) } } };
EOF
check "a locally shadowed helper name (fail)" fail "$d"

d="$base/vite-helper-parameter"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
function build(pathResolve) { return pathResolve(__dirname, "x"); }
export default { resolve: { alias: { reporter: build(0) } } };
EOF
check "a helper received as a parameter (fail)" fail "$d"

d="$base/vite-helper-other-package"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "lodash";
export default { resolve: { alias: { reporter: path.resolve("x") } } };
EOF
check "a same-named import from another package (fail)" fail "$d"

d="$base/vite-helper-namespace-import"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as path from "node:path";
export default { resolve: { alias: { "@ui": path.join(__dirname, "src/ui") } } };
EOF
check "a trusted import * as path from node:path (pass)" pass "$d"

d="$base/vite-helper-require-namespace"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const path = require("node:path");
export default { resolve: { alias: { "@ui": path.resolve(__dirname, "src/ui") } } };
EOF
check "a trusted const path = require(\"node:path\") (pass)" pass "$d"

d="$base/vite-helper-require-destructured"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const { resolve: pathResolve } = require("node:path");
export default { resolve: { alias: { "@ui": pathResolve(__dirname, "src/ui") } } };
EOF
check "a trusted destructured require of node:path (pass)" pass "$d"

# --- Babel plugin name via bindings ------------------------------------------

d="$base/babel-plugin-short-via-binding"; make_fixture "$d"
put "$d/packages/mobile/babel.config.js" <<'EOF'
const resolverPlugin = "module-resolver";
module.exports = { plugins: [[resolverPlugin, { alias: { reporter: "@sentry/electron" } }]] };
EOF
check "module-resolver short name via a binding (fail)" fail "$d"

d="$base/babel-plugin-full-via-binding"; make_fixture "$d"
put "$d/packages/mobile/babel.config.js" <<'EOF'
const resolverPlugin = "babel-plugin-module-resolver";
module.exports = { plugins: [[resolverPlugin, { alias: { reporter: "@sentry/electron" } }]] };
EOF
check "babel-plugin-module-resolver full name via a binding (fail)" fail "$d"

d="$base/babel-plugin-scoped-via-binding"; make_fixture "$d"
put "$d/packages/mobile/babel.config.js" <<'EOF'
const resolverPlugin = "@scope/babel-plugin-module-resolver";
module.exports = { plugins: [[resolverPlugin, { alias: { reporter: "@sentry/electron" } }]] };
EOF
check "scoped module-resolver name via a binding (fail)" fail "$d"

d="$base/babel-plugin-binding-reassigned"; make_fixture "$d"
put "$d/packages/mobile/babel.config.js" <<'EOF'
let resolverPlugin = "babel-plugin-module-resolver";
resolverPlugin = "something-else";
module.exports = { plugins: [[resolverPlugin, { alias: { reporter: "@sentry/electron" } }]] };
EOF
check "a reassigned Babel plugin-name binding (fail)" fail "$d"

d="$base/babel-plugin-binding-benign"; make_fixture "$d"
put "$d/packages/mobile/babel.config.js" <<'EOF'
const benignPlugin = "eslint-plugin-foo";
module.exports = { plugins: [[benignPlugin, { options: {} }]] };
EOF
check "a benign Babel plugin name via a binding (pass)" pass "$d"

# --- require provenance -------------------------------------------------------

d="$base/require-local-const"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const require = () => ({ resolve() { return "@sentry/electron"; } });
const path = require("node:path");
export default { resolve: { alias: { reporter: path.resolve(__dirname, "src") } } };
EOF
check "a local const named require makes require untrusted (fail)" fail "$d"

d="$base/require-local-function"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
function require() { return { resolve() { return "@sentry/electron"; } }; }
const path = require("node:path");
export default { resolve: { alias: { reporter: path.resolve(__dirname, "src") } } };
EOF
check "a local function named require makes require untrusted (fail)" fail "$d"

d="$base/require-parameter"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
function build(require) { const path = require("node:path"); return path.resolve(__dirname, "src"); }
export default { resolve: { alias: { reporter: build(0) } } };
EOF
check "require received as a parameter (fail)" fail "$d"

d="$base/require-reassigned"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
require = fake;
const path = require("node:path");
export default { resolve: { alias: { reporter: path.resolve(__dirname, "src") } } };
EOF
check "a reassigned require binding (fail)" fail "$d"

# --- helper member mutations -------------------------------------------------

d="$base/helper-mutate-resolve"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
const path = require("node:path");
path.resolve = () => "@sentry/electron";
export default { resolve: { alias: { reporter: path.resolve(__dirname, "src") } } };
EOF
check "an assignment to path.resolve invalidates the helper (fail)" fail "$d"

d="$base/helper-mutate-join"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as path from "node:path";
path.join = fake;
export default { resolve: { alias: { reporter: path.join(__dirname, "src") } } };
EOF
check "an assignment to path.join invalidates the helper (fail)" fail "$d"

d="$base/helper-mutate-posix-resolve"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as path from "node:path";
path.posix.resolve = fake;
export default { resolve: { alias: { reporter: path.posix.resolve(__dirname, "src") } } };
EOF
check "an assignment to path.posix.resolve invalidates the helper (fail)" fail "$d"

d="$base/helper-mutate-posix-join"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as path from "node:path";
path.posix.join = fake;
export default { resolve: { alias: { reporter: path.posix.join(__dirname, "src") } } };
EOF
check "an assignment to path.posix.join invalidates the helper (fail)" fail "$d"

d="$base/helper-mutate-fileurltopath"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as url from "node:url";
url.fileURLToPath = fake;
export default { resolve: { alias: { reporter: url.fileURLToPath(__dirname) } } };
EOF
check "an assignment to url.fileURLToPath invalidates the helper (fail)" fail "$d"

d="$base/helper-mutate-url-class"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as url from "node:url";
url.URL = FakeURL;
export default { resolve: { alias: { reporter: url.URL.name } } };
EOF
check "an assignment to url.URL invalidates the helper (fail)" fail "$d"

d="$base/helper-mutate-bracket"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as path from "node:path";
path["resolve"] = fake;
export default { resolve: { alias: { reporter: path.resolve(__dirname, "src") } } };
EOF
check "an element-access assignment to path[\"resolve\"] invalidates the helper (fail)" fail "$d"

d="$base/helper-mutate-compound"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as path from "node:path";
path.resolve += fake;
export default { resolve: { alias: { reporter: path.resolve(__dirname, "src") } } };
EOF
check "a compound assignment to path.resolve invalidates the helper (fail)" fail "$d"

d="$base/helper-mutate-increment"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as path from "node:path";
path.resolve++;
export default { resolve: { alias: { reporter: path.resolve(__dirname, "src") } } };
EOF
check "an increment of path.resolve invalidates the helper (fail)" fail "$d"

d="$base/helper-mutate-object-assign"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as path from "node:path";
Object.assign(path, { resolve: fake });
export default { resolve: { alias: { reporter: path.resolve(__dirname, "src") } } };
EOF
check "Object.assign(path, …) invalidates the helper (fail)" fail "$d"

d="$base/helper-unmodified-resolve-join"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
export default { resolve: { alias: { "@ui": path.resolve(__dirname, "src/ui"), "@u2": path.join(__dirname, "src") } } };
EOF
check "unmodified path.resolve and path.join (pass)" pass "$d"

d="$base/helper-unmodified-posix"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as path from "node:path";
export default { resolve: { alias: { "@ui": path.posix.resolve(__dirname, "src"), "@u2": path.posix.join(__dirname, "src") } } };
EOF
check "unmodified path.posix.resolve and path.posix.join (pass)" pass "$d"

d="$base/helper-unmodified-fileurltopath"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import * as url from "node:url";
export default { resolve: { alias: { "@ui": url.fileURLToPath("./src/ui") } } };
EOF
check "unmodified url.fileURLToPath (pass)" pass "$d"

d="$base/helper-unmodified-url-class"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import { fileURLToPath, URL } from "node:url";
export default { resolve: { alias: { "@ui": fileURLToPath(new URL("./src/ui", import.meta.url)) } } };
EOF
check "unmodified URL class (pass)" pass "$d"

# --- __dirname / __filename / import.meta.url --------------------------------

d="$base/dirname-implicit"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
export default { resolve: { alias: { "@ui": path.resolve(__dirname, "src/ui") } } };
EOF
check "the implicit __dirname is a valid base (pass)" pass "$d"

d="$base/filename-implicit"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
export default { resolve: { alias: { "@ui": path.resolve(__filename, "src/ui") } } };
EOF
check "the implicit __filename is a valid base (pass)" pass "$d"

d="$base/dirname-local-sdk"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
const __dirname = "node_modules/@sentry";
export default { resolve: { alias: { reporter: path.resolve(__dirname, "electron") } } };
EOF
check "a local const __dirname pointing at an SDK (fail)" fail "$d"

d="$base/dirname-local-benign"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
const __dirname = "src";
export default { resolve: { alias: { "@ui": path.resolve(__dirname, "ui") } } };
EOF
check "a local const __dirname benign and static (pass after resolving)" pass "$d"

d="$base/dirname-let"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
let __dirname = "src";
export default { resolve: { alias: { "@ui": path.resolve(__dirname, "ui") } } };
EOF
check "a mutable let __dirname (fail)" fail "$d"

d="$base/dirname-parameter"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import path from "node:path";
function build(__dirname) { return path.resolve(__dirname, "ui"); }
export default { resolve: { alias: { "@ui": build(0) } } };
EOF
check "a parameter named __dirname (fail)" fail "$d"

d="$base/import-meta-url"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
import { fileURLToPath } from "node:url";
export default { resolve: { alias: { "@ui": fileURLToPath(new URL("./src/ui", import.meta.url)) } } };
EOF
check "import.meta.url is a valid intrinsic base (pass)" pass "$d"

# --- Relative re-exports and Vitest discovery --------------------------------

d="$base/vitest-reexport-observability"; make_fixture "$d"
put "$d/frontend/vitest.config.ts" <<'EOF'
export { default } from "./base";
EOF
put "$d/frontend/base.ts" <<'EOF'
export default { resolve: { alias: { reporter: "@sentry/electron" } } };
EOF
check "a Vitest config re-exporting a config with an observability alias (fail)" fail "$d"

d="$base/vite-reexport-benign"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
export { default } from "./base";
EOF
put "$d/frontend/base.ts" <<'EOF'
export default { resolve: { alias: { "@ui": "./src/ui" } } };
EOF
check "a relative re-export of a benign config (pass)" pass "$d"

d="$base/reexport-missing"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
export { default } from "./does-not-exist";
EOF
check "a relative re-export of a missing file (fail)" fail "$d"

d="$base/reexport-cycle"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
export { default } from "./other";
EOF
put "$d/frontend/other.ts" <<'EOF'
export { default } from "./vite.config";
EOF
check "a cycle of relative re-exports (fail)" fail "$d"

d="$base/reexport-bare-package"; make_fixture "$d"
put "$d/frontend/vite.config.ts" <<'EOF'
export { default } from "some-package";
EOF
check "a default re-export from a non-relative package (fail)" fail "$d"

d="$base/vitest-config-direct-alias"; make_fixture "$d"
put "$d/packages/product-ui/vitest.config.ts" <<'EOF'
export default { resolve: { alias: { reporter: "@sentry/electron" } } };
EOF
check "an observability alias in a discovered vitest.config.ts (fail)" fail "$d"

if [[ "$failures" -eq 0 ]]; then
  printf 'privacy-guard self-tests: OK\n'
  exit 0
fi
printf 'privacy-guard self-tests: %s failed\n' "$failures" >&2
exit 1
