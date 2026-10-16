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
//     maps ("#reporter": "@sentry/electron"), in every discovered manifest, with a
//     manifest-scoped allowlist (a dormant dep is only allowed in the manifest
//     that declares it);
//   - observability targets hidden behind the repository's resolution configs:
//     tsconfig "paths", Vite "resolve.alias" (object and {find, replacement}
//     array forms), Babel "module-resolver" aliases and Metro
//     "resolver.extraNodeModules".
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

// Manifest-scoped allowlist of deliberately dormant observability dependencies.
// Exact repo-relative manifest -> the package names allowed IN THAT MANIFEST.
// A dormant dependency in any other manifest, a self-alias outside its
// authorized manifest, or a new observability package anywhere is flagged.
const DEPENDENCY_ALLOW = new Map([
  ["frontend/package.json", new Set(["@sentry/electron", "posthog-js"])],
  ["packages/mobile/package.json", new Set(["posthog-react-native"])],
]);
const EMPTY_ALLOW = new Set();

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

// --- Module resolution configuration -----------------------------------------
//
// Resolution configs can hide an SDK behind an alias. Vite `resolve.alias`,
// Babel `module-resolver` and Metro `resolver.extraNodeModules` are inspected
// through the AST, so only real alias targets are read — not every string in the
// file. A dynamic (non-literal) alias map or target is rejected fail-closed: it
// cannot be proven safe. Metro's `resolver.resolveRequest` is a function; only
// observability literals inside it are flagged, and a fully computed module name
// is a documented gap (see docs/privacy-fork.md).

const VITE_CONFIG_NAME = /^vite(\.[^/\\]+)?\.config\.[cm]?[jt]s$/;
const BABEL_CONFIG_NAME = /^babel\.config\.[cm]?[jt]s$/;
const METRO_CONFIG_NAME = /^metro\.config\.[cm]?[jt]s$/;

function discoverResolutionConfigs() {
  const found = [];
  const add = (abs, kind) => found.push({ abs, kind });
  for (const baseRoot of ["frontend", "packages"]) {
    walk(path.join(root, baseRoot), (abs) => {
      const name = path.basename(abs);
      if (/^tsconfig[^/\\]*\.json$/.test(name)) return add(abs, "tsconfig");
      if (VITE_CONFIG_NAME.test(name)) return add(abs, "vite");
      if (BABEL_CONFIG_NAME.test(name) || name === "babel.config.json") return add(abs, "babel");
      if (METRO_CONFIG_NAME.test(name)) return add(abs, "metro");
    });
  }
  return found;
}

function nameOfProperty(node) {
  if (!node || !ts.isPropertyAssignment(node)) return null;
  const name = node.name;
  if (ts.isIdentifier(name) || ts.isStringLiteral(name) || ts.isNoSubstitutionTemplateLiteral(name)) return name.text;
  return null;
}

function collectLiteralStrings(node, out) {
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) {
    out.push(node.text);
    return out;
  }
  ts.forEachChild(node, (child) => {
    collectLiteralStrings(child, out);
  });
  return out;
}

// tsconfig is JSON: every string is a value (no executable code), so a
// string-literal sweep of the file is targeted here rather than indiscriminate.
function scanTsconfigConfig(rel, text) {
  const blanked = blankComments(text);
  const lineOf = lineLookup(blanked);
  const re = /["'`]([^"'`\n]+)["'`]/g;
  let captured;
  while ((captured = re.exec(blanked)) !== null) {
    if (OBSERVABILITY_SPECIFIER.test(captured[1])) {
      report(rel, lineOf(captured.index), captured[1], "tsconfig value references an observability SDK (not allowlisted)");
    }
  }
}

const lineOfNode = (source) => (node) => source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;

function inspectAliasMap(rel, lineOf, value, label) {
  if (ts.isObjectLiteralExpression(value)) {
    for (const property of value.properties) {
      if (!ts.isPropertyAssignment(property)) {
        report(rel, lineOf(property), null, `unanalyzable ${label} entry (not allowlisted)`);
        continue;
      }
      const literals = collectLiteralStrings(property.initializer, []);
      if (literals.length === 0) {
        report(rel, lineOf(property), null, `unanalyzable ${label} target: a dynamically built alias cannot be verified (not allowlisted)`);
        continue;
      }
      for (const literal of literals) {
        if (OBSERVABILITY_SPECIFIER.test(literal)) {
          report(rel, lineOf(property), literal, `${label} target references an observability SDK (not allowlisted)`);
        }
      }
    }
    return;
  }
  if (ts.isArrayLiteralExpression(value)) {
    for (const element of value.elements) inspectAliasArrayElement(rel, lineOf, element, label);
    return;
  }
  report(rel, lineOf(value), null, `unanalyzable ${label}: a dynamically built alias map cannot be verified (not allowlisted)`);
}

function inspectAliasArrayElement(rel, lineOf, element, label) {
  if (ts.isStringLiteral(element) || ts.isNoSubstitutionTemplateLiteral(element)) {
    if (OBSERVABILITY_SPECIFIER.test(element.text)) {
      report(rel, lineOf(element), element.text, `${label} entry references an observability SDK (not allowlisted)`);
    }
    return;
  }
  if (ts.isObjectLiteralExpression(element)) {
    let target = null;
    for (const property of element.properties) {
      if (ts.isPropertyAssignment(property) && nameOfProperty(property) === "replacement") target = property.initializer;
    }
    const literals = collectLiteralStrings(target ?? element, []);
    if (literals.length === 0) {
      report(rel, lineOf(element), null, `unanalyzable ${label} {find, replacement} target (not allowlisted)`);
      return;
    }
    for (const literal of literals) {
      if (OBSERVABILITY_SPECIFIER.test(literal)) {
        report(rel, lineOf(element), literal, `${label} {replacement} references an observability SDK (not allowlisted)`);
      }
    }
    return;
  }
  report(rel, lineOf(element), null, `unanalyzable ${label} array element (not allowlisted)`);
}

function scanViteConfig(rel, source) {
  const lineOf = lineOfNode(source);
  const visit = (node) => {
    if (
      ts.isPropertyAssignment(node) &&
      nameOfProperty(node) === "alias" &&
      ts.isObjectLiteralExpression(node.parent) &&
      ts.isPropertyAssignment(node.parent.parent) &&
      nameOfProperty(node.parent.parent) === "resolve"
    ) {
      inspectAliasMap(rel, lineOf, node.initializer, "resolve.alias");
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
}

function scanBabelConfig(rel, source) {
  const lineOf = lineOfNode(source);
  const inspectPlugin = (name, options) => {
    if (!name || !/(^|\/)module-resolver$/.test(name)) return;
    if (!options) return;
    if (!ts.isObjectLiteralExpression(options)) {
      report(rel, lineOf(options), null, "unanalyzable Babel module-resolver options (not allowlisted)");
      return;
    }
    for (const property of options.properties) {
      if (ts.isPropertyAssignment(property) && nameOfProperty(property) === "alias") {
        inspectAliasMap(rel, lineOf, property.initializer, "Babel module-resolver alias");
      }
    }
  };
  const visit = (node) => {
    if (ts.isArrayLiteralExpression(node) && node.elements.length >= 1 && ts.isStringLiteral(node.elements[0])) {
      inspectPlugin(node.elements[0].text, node.elements[1]);
    }
    if (ts.isObjectLiteralExpression(node)) {
      let name = null;
      let options = null;
      for (const property of node.properties) {
        if (!ts.isPropertyAssignment(property)) continue;
        if (nameOfProperty(property) === "name" && ts.isStringLiteral(property.initializer)) name = property.initializer.text;
        if (nameOfProperty(property) === "options") options = property.initializer;
      }
      inspectPlugin(name, options);
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
}

function scanMetroConfig(rel, source) {
  const lineOf = lineOfNode(source);
  const assignmentTarget = (node) =>
    ts.isBinaryExpression(node) &&
    node.operatorToken.kind === ts.SyntaxKind.EqualsToken &&
    ts.isPropertyAccessExpression(node.left)
      ? node.left.name.text
      : null;
  const visit = (node) => {
    // Object-literal form and assignment form are both supported because Metro
    // configs use either (`{ resolver: { extraNodeModules } }` or
    // `config.resolver.extraNodeModules = ...`).
    if (ts.isPropertyAssignment(node) && nameOfProperty(node) === "extraNodeModules") {
      inspectAliasMap(rel, lineOf, node.initializer, "Metro resolver.extraNodeModules");
    }
    const target = assignmentTarget(node);
    if (target === "extraNodeModules") {
      inspectAliasMap(rel, lineOf, node.right, "Metro resolver.extraNodeModules");
    }
    if (target === "resolveRequest") {
      // Dynamic resolver: only literal module names are visible; a computed name
      // is a documented gap (docs/privacy-fork.md).
      for (const literal of collectLiteralStrings(node.right, [])) {
        if (OBSERVABILITY_SPECIFIER.test(literal)) {
          report(rel, lineOf(node), literal, "Metro resolver.resolveRequest references an observability SDK (not allowlisted)");
        }
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
}

function scanResolutionConfigs() {
  for (const { abs, kind } of discoverResolutionConfigs()) {
    const rel = relPath(abs);
    let text;
    try {
      text = readFileSync(abs, "utf8");
    } catch {
      report(rel, 0, null, "could not read config file");
      continue;
    }
    if (kind === "tsconfig" || path.extname(abs) === ".json") {
      scanTsconfigConfig(rel, text);
      continue;
    }
    const scriptKind = SCRIPT_KIND[path.extname(abs)] ?? ts.ScriptKind.JS;
    // setParentNodes=true: the resolve.alias walk needs node.parent.
    const source = ts.createSourceFile(abs, text, ts.ScriptTarget.Latest, true, scriptKind);
    if (kind === "vite") scanViteConfig(rel, source);
    else if (kind === "babel") scanBabelConfig(rel, source);
    else if (kind === "metro") scanMetroConfig(rel, source);
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

  const allowed = DEPENDENCY_ALLOW.get(rel) ?? EMPTY_ALLOW;

  for (const name of directNames) {
    if (OBSERVABILITY_SPECIFIER.test(name) && !allowed.has(name)) {
      report(rel, 0, name, "unreviewed observability dependency for this manifest");
    }
  }
  // An alias that renames an observability package hides the import site, so it
  // fails even in an authorized manifest. A self-alias ("posthog-js":
  // "npm:posthog-js@1") is only acceptable when the target is allowed in THIS
  // manifest; a self-alias outside its authorized manifest fails.
  for (const [key, target] of aliasTargets) {
    if (!OBSERVABILITY_SPECIFIER.test(target)) continue;
    if (target !== packageNameOf(key)) {
      report(rel, 0, target, `aliased as ${JSON.stringify(key)} (npm alias, not allowlisted)`);
    } else if (!allowed.has(target)) {
      report(rel, 0, target, `self-alias in an unauthorized manifest (not allowlisted here)`);
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

scanResolutionConfigs();

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
