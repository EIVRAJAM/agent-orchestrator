#!/usr/bin/env node
//
// Static privacy scanner for this fork of Agent Orchestrator.
//
// Detects, independent of the local identifier used and independent of line
// breaks:
//   - imports / export-from / dynamic import() / require() of observability SDKs
//     (posthog-js and subpaths, posthog-react-native, @sentry/*, and the other
//     providers the dependency allowlist knows), including Go imports of
//     github.com/getsentry/sentry-go and friends under any alias;
//   - direct construction/init call sites (defense in depth only);
//   - unreviewed observability dependencies in any discovered package.json.
//
// A reintroduction evades a call-name search by aliasing the import, and evades a
// line-oriented search by splitting the import across lines:
//
//     import {
//       init
//     } from
//       "@sentry/electron";
//
// So imports are matched against the whole file content (whitespace, including
// newlines, is allowed between the keyword and the string literal), after
// comments are blanked out. Matching the module specifier itself does not depend
// on the identifier.
//
// This is an alarm, not a proof: a hand-written HTTP exporter with no SDK and no
// known marker is not detectable here (see docs/privacy-fork.md).
//
// Usage: node scripts/lib/privacy-scan.mjs [root]
// Exit 0 = clean, 1 = violations, 2 = usage/IO error.

import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";

const root = path.resolve(process.argv[2] ?? ".");

const escapeRe = (value) => value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// Unscoped providers: match the exact package or a "/" subpath, so an unrelated
// name that merely shares a prefix ("posthog-analytics", "segment-analytics")
// is NOT treated as the SDK.
const UNSCOPED_PROVIDERS = [
  "posthog",
  "posthog-js",
  "posthog-react-native",
  "posthog-node",
  "bugsnag",
  "amplitude",
  "mixpanel",
  "segment",
  "datadog",
  "dd-trace",
  "newrelic",
  "rollbar",
  "logrocket",
  "fullstory",
  "heap",
];

// Scoped providers: match the scope, so "@sentry/electron", "@sentry/browser"
// and "@sentry/node/..." all match while "@sentryfoo/..." does not.
const SCOPED_PROVIDERS = [
  "@posthog/",
  "@sentry/",
  "@amplitude/",
  "@datadog/",
  "@honeycombio/",
  "@opentelemetry/",
  "@bugsnag/",
  "@segment/",
  "@logtail/",
  "@highlight-run/",
];

// Matches a bare module specifier (no subpath resolution).
const OBSERVABILITY_SPECIFIER = new RegExp(
  `^(?:${UNSCOPED_PROVIDERS.map(escapeRe).join("|")})(?:$|/)|^(?:${SCOPED_PROVIDERS.map(escapeRe).join("|")})`,
);

// Go import paths for the same providers.
const GO_OBSERVABILITY_PATH =
  /(github\.com\/getsentry\/|go\.opentelemetry\.io\/|github\.com\/DataDog\/|github\.com\/PostHog\/|github\.com\/rollbar\/|github\.com\/bugsnag\/|github\.com\/segmentio\/)/;

// Direct construction/init call sites. Defense in depth only; the import scan is
// the primary detector, so an aliased import is caught even when no call-name
// pattern matches. Kept as source strings so this file does not match its own
// patterns. `\s*` allows the call to be split across lines.
const CALL_PATTERN_SOURCES = [
  "\\bposthog\\s*\\.\\s*init\\s*\\(",
  "\\bnew\\s+PostHog\\s*\\(",
  "\\bSentry\\s*\\.\\s*init\\s*\\(",
  "\\bsentry\\s*\\.\\s*Init\\s*\\(",
  "\\bsentry\\s*\\.\\s*init\\s*\\(",
  "\\bmod\\s*\\.\\s*init\\s*\\(",
  "\\bNewPostHogSink\\s*\\(",
  "\\bsentry\\s*\\.\\s*NewHTTPSyncTransport\\s*\\(",
];

// Specifier extractors for TS/JS, run over the whole (comment-blanked) content so
// a specifier on its own line after the keyword is still matched. A specifier
// cannot contain a newline, so the capture excludes one. Backtick-delimited
// specifiers are matched too, so `import(`@sentry/electron`)` (a template with no
// interpolation) is not a bypass.
const QUOTE = "[\"'`]";
const NOT_QUOTE = "[^\"'`\\n]+";
const TS_SPECIFIER_EXTRACTORS = [
  { reason: "import/export from", re: new RegExp(`\\bfrom\\s*${QUOTE}(${NOT_QUOTE})${QUOTE}`, "g") },
  { reason: "dynamic import", re: new RegExp(`\\bimport\\s*\\(\\s*${QUOTE}(${NOT_QUOTE})${QUOTE}`, "g") },
  { reason: "require", re: new RegExp(`\\brequire\\s*\\(\\s*${QUOTE}(${NOT_QUOTE})${QUOTE}`, "g") },
  { reason: "bare import", re: new RegExp(`(?:^|[\\s;{}()])import\\s*${QUOTE}(${NOT_QUOTE})${QUOTE}`, "gm") },
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

const violations = new Set();
const report = (file, line, moduleName, reason) =>
  violations.add(`${file}:${line}: ${moduleName ? `observability module ${JSON.stringify(moduleName)}: ` : ""}${reason}`);

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

// --- Comment blanking --------------------------------------------------------

// Replaces comments with spaces while preserving newlines and string literals,
// so a commented-out import is not a false positive and a specifier inside a
// string (e.g. a URL) is not mistaken for a comment.
function blankComments(text) {
  const out = text.split("");
  const n = text.length;
  let state = "code";
  let quote = "";
  let i = 0;
  while (i < n) {
    const ch = text[i];
    const next = text[i + 1];
    if (state === "code") {
      if (ch === "/" && next === "/") {
        out[i] = " ";
        out[i + 1] = " ";
        state = "line";
        i += 2;
        continue;
      }
      if (ch === "/" && next === "*") {
        out[i] = " ";
        out[i + 1] = " ";
        state = "block";
        i += 2;
        continue;
      }
      if (ch === "'" || ch === '"' || ch === "`") {
        state = "string";
        quote = ch;
        i += 1;
        continue;
      }
      i += 1;
      continue;
    }
    if (state === "line") {
      if (ch === "\n") {
        state = "code";
      } else {
        out[i] = " ";
      }
      i += 1;
      continue;
    }
    if (state === "block") {
      if (ch === "*" && next === "/") {
        out[i] = " ";
        out[i + 1] = " ";
        state = "code";
        i += 2;
        continue;
      }
      if (ch !== "\n") out[i] = " ";
      i += 1;
      continue;
    }
    // Inside a string/template: keep verbatim.
    if (ch === "\\") {
      i += 2;
      continue;
    }
    if (ch === quote) {
      state = "code";
    }
    i += 1;
  }
  return out.join("");
}

function lineLookup(text) {
  const starts = [0];
  for (let i = 0; i < text.length; i += 1) if (text[i] === "\n") starts.push(i + 1);
  return (index) => {
    let lo = 0;
    let hi = starts.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (starts[mid] <= index) lo = mid;
      else hi = mid - 1;
    }
    return lo + 1;
  };
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
    report(rel, 0, null, "could not read file");
    return;
  }
  const blanked = blankComments(text);
  const lineOf = lineLookup(blanked);
  const test = isTestFile(rel);

  if (ext === ".go") {
    const lines = blanked.split("\n");
    for (let index = 0; index < lines.length; index += 1) {
      const match = GO_IMPORT_LINE.exec(lines[index]);
      if (match && GO_OBSERVABILITY_PATH.test(match[1]) && !IMPORT_ALLOW.has(rel)) {
        report(rel, index + 1, match[1], "imported (not allowlisted)");
      }
    }
  } else {
    for (const { re, reason } of TS_SPECIFIER_EXTRACTORS) {
      re.lastIndex = 0;
      let captured;
      while ((captured = re.exec(blanked)) !== null) {
        if (OBSERVABILITY_SPECIFIER.test(captured[1]) && !IMPORT_ALLOW.has(rel)) {
          report(rel, lineOf(captured.index), captured[1], `${reason} (not allowlisted)`);
        }
      }
    }
  }

  if (!test) {
    for (const source of CALL_PATTERN_SOURCES) {
      const re = new RegExp(source, "g");
      let captured;
      while ((captured = re.exec(blanked)) !== null) {
        if (!CALL_ALLOW.has(rel)) report(rel, lineOf(captured.index), null, "constructs/initializes an observability client (not allowlisted)");
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
      report(rel, 0, null, "could not parse package.json");
      continue;
    }
    for (const name of declaredDependencyNames(pkg)) {
      if (OBSERVABILITY_SPECIFIER.test(name) && !DEPENDENCY_ALLOW.has(name)) {
        report(rel, 0, name, "unreviewed observability dependency");
      }
    }
  }
}

// Names declared anywhere a package.json can introduce a dependency, including
// resolutions/overrides (which can pull a package in without a direct
// dependency entry). Selector keys like "**/posthog-js" are reduced to the
// package name.
function declaredDependencyNames(pkg) {
  const names = new Set();
  for (const field of ["dependencies", "devDependencies", "optionalDependencies", "peerDependencies"]) {
    for (const name of Object.keys(pkg[field] ?? {})) names.add(name);
  }
  const collect = (value) => {
    if (!value || typeof value !== "object" || Array.isArray(value)) return;
    for (const [key, nested] of Object.entries(value)) {
      if (key && key !== ".") names.add(key.startsWith("**/") ? key.slice(3) : key);
      collect(nested);
    }
  };
  collect(pkg.resolutions);
  collect(pkg.overrides);
  collect(pkg.pnpm?.overrides);
  return names;
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

if (violations.size === 0) {
  console.log("privacy-scan: OK");
  process.exit(0);
}
console.error(`privacy-scan: ${violations.size} violation(s):`);
for (const violation of [...violations].sort()) console.error(`  ${violation}`);
process.exit(1);
