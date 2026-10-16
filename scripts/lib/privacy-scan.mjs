#!/usr/bin/env node
//
// Static privacy scanner for this fork of Agent Orchestrator.
//
// Detects, independent of the local identifier used and independent of line
// breaks:
//   - imports / export-from / dynamic import() / require() of observability SDKs,
//     parsed with the repository's own TypeScript compiler so ordinary strings,
//     template literals, comments and regex literals are NOT mistaken for
//     imports;
//   - Go imports of github.com/getsentry/sentry-go and friends under any alias;
//   - unreviewed observability dependencies, including npm aliases
//     ("reporter": "npm:@sentry/electron@^7.0.0") and package.json "imports"
//     maps ("#reporter": "@sentry/electron"), in every discovered manifest;
//   - observability targets hidden behind the repository's own module aliases
//     (tsconfig "paths" and Vite "resolve.alias").
//
// A reintroduction can hide an SDK in three ways that a name search misses: an
// aliased import (the identifier differs), an import split across lines, and an
// indirection alias (`reporter` -> npm:@sentry/electron). This scanner covers all
// three.
//
// This is an alarm, not a proof: a hand-written HTTP exporter with no SDK and no
// known marker, a specifier computed at runtime, or a Go raw-string/URL import is
// not detectable here (see docs/privacy-fork.md).
//
// Usage: node scripts/lib/privacy-scan.mjs [root]
// Exit 0 = clean, 1 = violations, 2 = usage/IO/parser error.

import { createRequire } from "node:module";
import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..", "..");
const root = path.resolve(process.argv[2] ?? ".");

const escapeRe = (value) => value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// --- Observability providers -------------------------------------------------

// Unscoped providers: match the exact package or a "/" subpath, so an unrelated
// name that merely shares a prefix ("posthog-analytics") is NOT the SDK.
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

// Scoped providers: match the scope, so "@sentry/electron" and
// "@sentry/node/..." match while "@sentryfoo/..." does not.
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

const OBSERVABILITY_SPECIFIER = new RegExp(
  `^(?:${UNSCOPED_PROVIDERS.map(escapeRe).join("|")})(?:$|/)|^(?:${SCOPED_PROVIDERS.map(escapeRe).join("|")})`,
);

// Cheap pre-filter: a real observability specifier always contains one of these
// stems, so a file without any of them cannot contain one. Keeps the AST parse
// off the vast majority of files.
const PROVIDER_HINTS = [
  "posthog",
  "sentry",
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
  "opentelemetry",
  "honeycomb",
  "logtail",
  "highlight",
];

const GO_OBSERVABILITY_PATH =
  /(github\.com\/getsentry\/|go\.opentelemetry\.io\/|github\.com\/DataDog\/|github\.com\/PostHog\/|github\.com\/rollbar\/|github\.com\/bugsnag\/|github\.com\/segmentio\/)/;

// Go construction/init call sites (secondary; the Go import path is primary).
const GO_CALL_PATTERNS = [
  "\\bNewPostHogSink\\s*\\(",
  "\\bsentry\\s*\\.\\s*Init\\s*\\(",
  "\\bsentry\\s*\\.\\s*NewHTTPSyncTransport\\s*\\(",
].map((source) => new RegExp(source));

// TS/JS construction/init call sites (secondary; the import specifier is
// primary). Matched on the parsed callee, so a string never false-positives.
const JS_CALL_CALLEES = new Set([
  "posthog.init",
  "Sentry.init",
  "sentry.Init",
  "sentry.init",
  "mod.init",
  "sentry.NewHTTPSyncTransport",
]);
const JS_NEW_CALLEES = new Set(["PostHog"]);

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
const IGNORED_FILES = new Set(["scripts/lib/privacy-scan.mjs"]);

const isTestFile = (rel) => /(?:^|\/)(?:[^/]*_test\.go|[^/]*\.(?:test|spec)\.[cm]?[jt]sx?)$/.test(rel);
const relPath = (abs) => path.relative(root, abs).split(path.sep).join("/");

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

// --- Shared lexical helpers --------------------------------------------------

// Replaces comments with spaces while preserving newlines and string/template
// literals, so a commented-out reference is not a false positive and a "/" inside
// a string (e.g. a URL) is not mistaken for a comment.
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
      if (ch === "\n") state = "code";
      else out[i] = " ";
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
    if (ch === "\\") {
      i += 2;
      continue;
    }
    if (ch === quote) state = "code";
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

function containsProviderHint(text) {
  const lower = text.toLowerCase();
  return PROVIDER_HINTS.some((hint) => lower.includes(hint));
}

// --- TypeScript parser (the repository's own compiler) -----------------------

function loadTypeScript() {
  const candidates = [
    path.join(repoRoot, "frontend", "node_modules", "typescript"),
    path.join(root, "frontend", "node_modules", "typescript"),
  ];
  for (const candidate of candidates) {
    try {
      return require(candidate);
    } catch {
      /* try the next candidate */
    }
  }
  try {
    return require("typescript");
  } catch {
    return null;
  }
}

const ts = loadTypeScript();
if (!ts) {
  console.error(
    "privacy-scan: the repository's TypeScript parser was not found (run `npm ci` in frontend/); refusing to skip the import analysis",
  );
  process.exit(2);
}

const SCRIPT_KIND = {
  ".ts": ts.ScriptKind.TS,
  ".tsx": ts.ScriptKind.TSX,
  ".js": ts.ScriptKind.JS,
  ".jsx": ts.ScriptKind.JSX,
  ".mjs": ts.ScriptKind.JS,
  ".cjs": ts.ScriptKind.JS,
};

function nodeSpecifier(node) {
  if (!node) return null;
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) return node.text;
  return null;
}

function jsCalleeName(expression) {
  if (ts.isIdentifier(expression)) return expression.text;
  if (ts.isPropertyAccessExpression(expression) && ts.isIdentifier(expression.expression)) {
    return `${expression.expression.text}.${expression.name.text}`;
  }
  return null;
}

// --- JS/TS scan (AST) --------------------------------------------------------

function scanJsFile(abs, rel, text) {
  if (!containsProviderHint(text)) return;
  const kind = SCRIPT_KIND[path.extname(abs)] ?? ts.ScriptKind.TS;
  const source = ts.createSourceFile(abs, text, ts.ScriptTarget.Latest, false, kind);
  const test = isTestFile(rel);
  const lineOf = (node) => source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;

  const visit = (node) => {
    if (ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) {
      const spec = nodeSpecifier(node.moduleSpecifier);
      if (spec && OBSERVABILITY_SPECIFIER.test(spec) && !IMPORT_ALLOW.has(rel)) {
        report(rel, lineOf(node), spec, "import/export from (not allowlisted)");
      }
    } else if (ts.isImportEqualsDeclaration(node) && ts.isExternalModuleReference(node.moduleReference)) {
      const spec = nodeSpecifier(node.moduleReference.expression);
      if (spec && OBSERVABILITY_SPECIFIER.test(spec) && !IMPORT_ALLOW.has(rel)) {
        report(rel, lineOf(node), spec, "import = require (not allowlisted)");
      }
    } else if (ts.isCallExpression(node)) {
      const isDynamicImport = node.expression.kind === ts.SyntaxKind.ImportKeyword;
      const isRequire = ts.isIdentifier(node.expression) && node.expression.text === "require";
      if (isDynamicImport || isRequire) {
        const spec = nodeSpecifier(node.arguments[0]);
        if (spec && OBSERVABILITY_SPECIFIER.test(spec) && !IMPORT_ALLOW.has(rel)) {
          report(rel, lineOf(node), spec, `${isDynamicImport ? "dynamic import" : "require"} (not allowlisted)`);
        }
      } else if (!test && !CALL_ALLOW.has(rel)) {
        const callee = jsCalleeName(node.expression);
        if (callee && JS_CALL_CALLEES.has(callee)) {
          report(rel, lineOf(node), null, "constructs/initializes an observability client (not allowlisted)");
        }
      }
    } else if (ts.isNewExpression(node) && !test && !CALL_ALLOW.has(rel)) {
      const callee = ts.isIdentifier(node.expression) ? node.expression.text : null;
      if (callee && JS_NEW_CALLEES.has(callee)) {
        report(rel, lineOf(node), null, "constructs/initializes an observability client (not allowlisted)");
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
}

// --- Go scan -----------------------------------------------------------------

const GO_IMPORT_LINE = /^\s*(?:import\s+)?(?:[\w.]+\s+)?"([^"]+)"\s*$/;

function scanGoFile(rel, text) {
  const blanked = blankComments(text);
  const lineOf = lineLookup(blanked);
  const lines = blanked.split("\n");
  for (let index = 0; index < lines.length; index += 1) {
    const match = GO_IMPORT_LINE.exec(lines[index]);
    if (match && GO_OBSERVABILITY_PATH.test(match[1]) && !IMPORT_ALLOW.has(rel)) {
      report(rel, index + 1, match[1], "imported (not allowlisted)");
    }
  }
  if (isTestFile(rel) || CALL_ALLOW.has(rel)) return;
  for (const pattern of GO_CALL_PATTERNS) {
    const re = new RegExp(pattern.source, "g");
    let captured;
    while ((captured = re.exec(blanked)) !== null) {
      report(rel, lineOf(captured.index), null, "constructs/initializes an observability client (not allowlisted)");
    }
  }
}

// --- Module-alias configuration (tsconfig paths, Vite resolve.alias) ---------

function discoverAliasConfigs() {
  const found = new Set();
  for (const baseRoot of ["frontend", "packages"]) {
    const base = path.join(root, baseRoot);
    walk(base, (abs) => {
      const name = path.basename(abs);
      if (/^tsconfig[^/\\]*\.json$/.test(name)) {
        found.add(abs);
        return;
      }
      // Only the bundler configs at the frontend root (e.g. vite.renderer.config.ts).
      if (/^vite\..*\.config\.[cm]?ts$/.test(name) && path.dirname(abs) === path.join(root, "frontend")) {
        found.add(abs);
      }
    });
  }
  return [...found];
}

function scanAliasConfig(abs) {
  const rel = relPath(abs);
  let text;
  try {
    text = readFileSync(abs, "utf8");
  } catch {
    report(rel, 0, null, "could not read config file");
    return;
  }
  const blanked = blankComments(text);
  const lineOf = lineLookup(blanked);
  const re = /["'`]([^"'`\n]+)["'`]/g;
  let captured;
  while ((captured = re.exec(blanked)) !== null) {
    const target = captured[1].replace(/^@\//, "@");
    if (OBSERVABILITY_SPECIFIER.test(captured[1]) || OBSERVABILITY_SPECIFIER.test(target)) {
      report(rel, lineOf(captured.index), captured[1], "module-alias/config target references an observability SDK (not allowlisted)");
    }
  }
}

// --- Dependency and alias analysis -------------------------------------------

function packageNameOf(spec) {
  const value = spec.trim().replace(/^[*]+\//, "");
  if (!value) return null;
  if (value.startsWith("@")) {
    const scoped = /^(@[^/@]+\/[^/@]+)/.exec(value);
    return scoped ? scoped[1] : null;
  }
  const plain = /^([^/@]+)/.exec(value);
  return plain ? plain[1] : null;
}

// A dependency value is a rename when it is an npm: alias, e.g.
// "reporter": "npm:@sentry/electron@^7.0.0". Plain ranges, file:, link:, git+,
// and workspace: values name no other package.
function aliasedTargetOf(value) {
  if (typeof value !== "string") return null;
  const match = /^npm:(.+)$/.exec(value.trim());
  return match ? packageNameOf(match[1]) : null;
}

function* declarationEntries(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return;
  for (const [key, nested] of Object.entries(value)) {
    yield [key, nested];
    yield* declarationEntries(nested);
  }
}

function* importMapValues(value) {
  if (typeof value === "string") {
    yield value;
    return;
  }
  if (value && typeof value === "object" && !Array.isArray(value)) {
    for (const nested of Object.values(value)) yield* importMapValues(nested);
  }
}

function scanManifest(manifest) {
  const rel = relPath(manifest);
  let pkg;
  try {
    pkg = JSON.parse(readFileSync(manifest, "utf8"));
  } catch {
    report(rel, 0, null, "could not parse package.json");
    return;
  }

  const directNames = new Set();
  const aliasTargets = [];
  for (const field of ["dependencies", "devDependencies", "optionalDependencies", "peerDependencies"]) {
    for (const [key, value] of Object.entries(pkg[field] ?? {})) {
      const name = packageNameOf(key);
      if (name) directNames.add(name);
      const target = aliasedTargetOf(value);
      if (target) aliasTargets.push([key, target]);
    }
  }
  for (const source of [pkg.resolutions, pkg.overrides, pkg.pnpm?.overrides]) {
    for (const [key, value] of declarationEntries(source)) {
      const name = packageNameOf(key);
      if (name) directNames.add(name);
      const target = aliasedTargetOf(value);
      if (target) aliasTargets.push([key, target]);
    }
  }

  for (const name of directNames) {
    if (OBSERVABILITY_SPECIFIER.test(name) && !DEPENDENCY_ALLOW.has(name)) {
      report(rel, 0, name, "unreviewed observability dependency");
    }
  }
  // An alias that renames an observability package is an indirection that hides
  // the import site, so it is flagged even when the target is an allowlisted
  // dormant dependency. A self-alias ("posthog-js": "npm:posthog-js@1") is not a
  // rename and is not flagged here.
  for (const [key, target] of aliasTargets) {
    if (OBSERVABILITY_SPECIFIER.test(target) && target !== packageNameOf(key)) {
      report(rel, 0, target, `aliased as ${JSON.stringify(key)} (npm alias, not allowlisted)`);
    }
  }
  for (const target of importMapValues(pkg.imports)) {
    if (OBSERVABILITY_SPECIFIER.test(target)) {
      report(rel, 0, target, "exposed via the package.json \"imports\" map (not allowlisted)");
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
  walk(base, (abs) => {
    const rel = relPath(abs);
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
    if (ext === ".go") scanGoFile(rel, text);
    else scanJsFile(abs, rel, text);
  });
}

for (const abs of discoverAliasConfigs()) scanAliasConfig(abs);

const manifests = new Set();
const addRootManifest = path.join(root, "package.json");
try {
  if (statSync(addRootManifest).isFile()) manifests.add(addRootManifest);
} catch {
  /* root manifest optional in a fixture */
}
for (const dir of CODE_ROOTS) {
  walk(path.join(root, dir), (abs) => {
    if (path.basename(abs) === "package.json") manifests.add(abs);
  });
}
for (const manifest of [...manifests].sort()) scanManifest(manifest);

if (violations.size === 0) {
  console.log("privacy-scan: OK");
  process.exit(0);
}
console.error(`privacy-scan: ${violations.size} violation(s):`);
for (const violation of [...violations].sort()) console.error(`  ${violation}`);
process.exit(1);
