#!/usr/bin/env node
//
// Static privacy scanner for this fork of Agent Orchestrator.
//
// Detects, independent of the local identifier used:
//   - imports / dynamic imports / require / export-from of observability SDKs
//     (posthog-js and subpaths, posthog-react-native, @sentry/*, and the other
//     providers the dependency allowlist knows), including Go imports of
//     github.com/getsentry/sentry-go and friends under any alias;
//   - direct construction/init call sites (defense in depth);
//   - unreviewed observability dependencies in any discovered package.json.
//
// A reintroduction evades a call-name search by aliasing the import; matching
// the module specifier itself does not depend on the identifier.
//
// This is an alarm, not a proof: a hand-written HTTP exporter with no SDK and no
// known marker is not detectable here (see docs/privacy-fork.md).
//
// Usage: node scripts/lib/privacy-scan.mjs [root]
// Exit 0 = clean, 1 = violations, 2 = usage/IO error.

import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";

const root = path.resolve(process.argv[2] ?? ".");

// Observability providers, shared conceptually with the dependency allowlist.
// Applied to a bare module specifier (so "posthog-js/dist/..." matches "posthog-js").
const OBSERVABILITY_SPECIFIER =
  /^(posthog|posthog-js|posthog-react-native|posthog-node|@posthog\/|@sentry\/|@amplitude\/|@datadog\/|@honeycombio\/|@opentelemetry\/|@bugsnag\/|bugsnag|amplitude|mixpanel|segment|@segment\/|datadog|dd-trace|newrelic|rollbar|logrocket|fullstory|heap|@logtail\/|@highlight-run\/)/;

// Go import paths for the same providers.
const GO_OBSERVABILITY_PATH =
  /(github\.com\/getsentry\/|go\.opentelemetry\.io\/|github\.com\/DataDog\/|github\.com\/PostHog\/|github\.com\/rollbar\/|github\.com\/bugsnag\/|github\.com\/segmentio\/)/;

// Direct construction/init call sites. Defense in depth only; the import scan is
// the primary detector. Kept as source strings so this file does not match its
// own patterns.
const CALL_PATTERN_SOURCES = [
  "\\bposthog\\.init\\s*\\(",
  "\\bnew\\s+PostHog\\s*\\(",
  "\\bSentry\\.init\\s*\\(",
  "\\bsentry\\.Init\\s*\\(",
  "\\bsentry\\.init\\s*\\(",
  "\\bmod\\.init\\s*\\(",
  "\\bNewPostHogSink\\s*\\(",
  "\\bsentry\\.NewHTTPSyncTransport\\s*\\(",
];
const CALL_PATTERNS = CALL_PATTERN_SOURCES.map((source) => new RegExp(source));

// Specifier extraction for TS/JS: static import/export-from, bare import,
// dynamic import(...) and require(...).
const TS_SPECIFIER_EXTRACTORS = [
  /\bfrom\s*["']([^"']+)["']/g,
  /\bimport\s*\(\s*["']([^"']+)["']\s*\)/g,
  /\brequire\s*\(\s*["']([^"']+)["']\s*\)/g,
  /(?:^|\s)import\s*["']([^"']+)["']/g,
];

// Go import spec lines: `\talias "path"` inside a block, or `import "path"`.
const GO_IMPORT_LINE = /^\s*(?:import\s+)?(?:[\w.]+\s+)?"([^"]+)"\s*$/;

// --- Allowlists (exact repo-relative paths). Additions require explicit review.

// Files permitted to import an observability SDK: the deliberately dormant
// implementations and their reviewed tests.
const IMPORT_ALLOW = new Set([
  "backend/internal/observe/sentryobs/sentryobs.go",
  "backend/internal/observe/sentryobs/sentryobs_test.go",
  "frontend/src/renderer/lib/telemetry.ts",
  "frontend/src/renderer/lib/telemetry.test.ts",
  "packages/mobile/lib/telemetry/runtime.ts",
]);

// Files permitted to contain a construction/init call site. Test files are not
// scanned for call sites (they legitimately construct fakes).
const CALL_ALLOW = new Set([
  "backend/internal/adapters/telemetry/posthog.go",
  "backend/internal/observe/sentryobs/sentryobs.go",
  "frontend/src/renderer/lib/telemetry.ts",
  "packages/mobile/lib/sentry.ts",
  "packages/mobile/lib/telemetry/runtime.ts",
]);

// Direct observability dependencies allowed to be present (dormant adapters).
const DEPENDENCY_ALLOW = new Set(["posthog-js", "posthog-react-native", "@sentry/electron"]);

// --- Covered tree ------------------------------------------------------------

const CODE_ROOTS = ["backend", "frontend", "packages", "scripts"];
const CODE_EXTENSIONS = new Set([".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"]);
const IGNORED_DIRS = new Set([
  "node_modules",
  ".git",
  "dist",
  "build",
  "out",
  "coverage",
  ".vite",
  ".expo",
  ".turbo",
  "testdata",
  "fixtures",
  "agent-browser",
]);
// The scanner itself contains the provider list and patterns; exclude it so its
// own source is never a false positive.
const IGNORED_FILES = new Set(["scripts/lib/privacy-scan.mjs"]);

const isTestFile = (rel) => /(?:^|\/)(?:[^/]*_test\.go|[^/]*\.(?:test|spec)\.[cm]?[jt]sx?)$/.test(rel);

const violations = [];
const report = (file, line, reason) => violations.push(`${file}:${line}: ${reason}`);

function walk(dir, visit) {
  let entries;
  try {
    entries = readdirSync(dir, { withFileTypes: true });
  } catch {
    return;
  }
  for (const entry of entries) {
    const abs = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (IGNORED_DIRS.has(entry.name)) continue;
      walk(abs, visit);
      continue;
    }
    if (!entry.isFile()) continue;
    visit(abs);
  }
}

// --- Code scan ---------------------------------------------------------------

function scanFile(abs) {
  const rel = path.relative(root, abs).split(path.sep).join("/");
  if (IGNORED_FILES.has(rel)) return;
  const ext = path.extname(abs);
  if (!CODE_EXTENSIONS.has(ext)) return;

  let text;
  try {
    text = readFileSync(abs, "utf8");
  } catch {
    report(rel, 0, "could not read file");
    return;
  }
  const lines = text.split(/\r?\n/);
  const test = isTestFile(rel);

  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index];
    const trimmed = line.trimStart();
    if (trimmed.startsWith("//") || trimmed.startsWith("*") || trimmed.startsWith("/*") || trimmed.startsWith("#")) {
      continue;
    }

    if (ext === ".go") {
      const match = GO_IMPORT_LINE.exec(line);
      if (match && GO_OBSERVABILITY_PATH.test(match[1]) && !IMPORT_ALLOW.has(rel)) {
        report(rel, index + 1, `imports observability module ${JSON.stringify(match[1])} (not allowlisted)`);
      }
    } else {
      for (const extractor of TS_SPECIFIER_EXTRACTORS) {
        extractor.lastIndex = 0;
        let captured;
        while ((captured = extractor.exec(line)) !== null) {
          if (OBSERVABILITY_SPECIFIER.test(captured[1]) && !IMPORT_ALLOW.has(rel)) {
            report(rel, index + 1, `imports/loads observability module ${JSON.stringify(captured[1])} (not allowlisted)`);
            break;
          }
        }
      }
    }

    if (!test) {
      for (const pattern of CALL_PATTERNS) {
        if (pattern.test(line) && !CALL_ALLOW.has(rel)) {
          report(rel, index + 1, `constructs/initializes an observability client (not allowlisted)`);
          break;
        }
      }
    }
  }
}

// --- Dependency discovery ----------------------------------------------------

function discoverPackageJson() {
  const found = new Set();
  const addRoot = path.join(root, "package.json");
  try {
    if (statSync(addRoot).isFile()) found.add(addRoot);
  } catch {
    /* root manifest optional in a fixture */
  }
  for (const dir of CODE_ROOTS) {
    const base = path.join(root, dir);
    walk(base, (abs) => {
      if (path.basename(abs) === "package.json") found.add(abs);
    });
  }
  return [...found].sort();
}

function scanDependencies() {
  for (const manifest of discoverPackageJson()) {
    const rel = path.relative(root, manifest).split(path.sep).join("/");
    let pkg;
    try {
      pkg = JSON.parse(readFileSync(manifest, "utf8"));
    } catch {
      report(rel, 0, "could not parse package.json");
      continue;
    }
    for (const field of ["dependencies", "devDependencies", "optionalDependencies", "peerDependencies"]) {
      for (const name of Object.keys(pkg[field] ?? {})) {
        if (OBSERVABILITY_SPECIFIER.test(name) && !DEPENDENCY_ALLOW.has(name)) {
          report(rel, 0, `unreviewed observability dependency ${JSON.stringify(name)}`);
        }
      }
    }
  }
}

// --- Run ---------------------------------------------------------------------

for (const dir of CODE_ROOTS) {
  const base = path.join(root, dir);
  try {
    if (!statSync(base).isDirectory()) continue;
  } catch {
    continue;
  }
  walk(base, scanFile);
}
scanDependencies();

if (violations.length === 0) {
  console.log("privacy-scan: OK");
  process.exit(0);
}
console.error(`privacy-scan: ${violations.length} violation(s):`);
for (const violation of violations) console.error(`  ${violation}`);
process.exit(1);
