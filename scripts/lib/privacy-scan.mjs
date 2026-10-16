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

// Vite and Vitest both resolve modules through Vite's resolver, so both config
// families are part of the resolution surface. A test file
// (`vite.renderer.config.test.ts`) or `vitest.workspace.*` is not matched.
const VITE_CONFIG_NAME = /^(?:vite|vitest)(\.[^/\\]+)?\.config\.[cm]?[jt]s$/;
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

// --- Strict static resolution ------------------------------------------------
//
// A target is analyzable ONLY if its value is fully determined: a string /
// no-substitution template literal, or a recognized local construction
// (`path.resolve`/`path.join` whose args are all static or local bases, and
// `fileURLToPath(new URL(<static>, ...))`). A single benign literal inside an
// otherwise dynamic expression (a call, an identifier, a concatenation, a
// template with interpolation, a property access, `process.env`, a spread) does
// NOT make it static: such a target is rejected fail-closed.

function unwrap(node) {
  let current = node;
  while (
    current &&
    (ts.isParenthesizedExpression(current) ||
      ts.isAsExpression(current) ||
      ts.isNonNullExpression(current) ||
      (typeof ts.isSatisfiesExpression === "function" && ts.isSatisfiesExpression(current)))
  ) {
    current = current.expression;
  }
  return current;
}

function propertyNameOf(node) {
  if (
    ts.isPropertyAssignment(node) ||
    ts.isShorthandPropertyAssignment(node) ||
    ts.isMethodDeclaration(node) ||
    ts.isPropertyDeclaration(node)
  ) {
    const name = node.name;
    if (name && (ts.isIdentifier(name) || ts.isStringLiteral(name) || ts.isNoSubstitutionTemplateLiteral(name))) {
      return name.text;
    }
  }
  return null;
}

function callTargetName(expression) {
  if (ts.isIdentifier(expression)) return expression.text;
  if (ts.isPropertyAccessExpression(expression) && ts.isIdentifier(expression.expression)) {
    return `${expression.expression.text}.${expression.name.text}`;
  }
  if (
    ts.isPropertyAccessExpression(expression) &&
    ts.isPropertyAccessExpression(expression.expression) &&
    ts.isIdentifier(expression.expression.expression)
  ) {
    return `${expression.expression.expression.text}.${expression.expression.name.text}.${expression.name.text}`;
  }
  return null;
}

// Standard modules whose helpers may be trusted, and the specific member
// functions treated as path/URL construction. Trust is established from the
// file's own imports/requires (see createBindingContext), never from the callee's
// name alone.
const MODULE_PATH = new Set(["node:path", "path"]);
const MODULE_URL = new Set(["node:url", "url"]);
const PATH_MEMBERS = new Set(["resolve", "join", "posix.resolve", "posix.join"]);
const URL_MEMBERS = new Set(["fileURLToPath"]);

// True when a path string names an observability package as a path segment (e.g.
// "node_modules/@sentry/electron" or "node_modules/posthog-js").
function pathReferencesObservability(text) {
  const segments = text.split(/[\\/]+/).filter((segment) => segment && segment !== "." && segment !== "..");
  for (let i = 0; i < segments.length; i += 1) {
    if (OBSERVABILITY_SPECIFIER.test(segments[i])) return true;
    if (i + 1 < segments.length && OBSERVABILITY_SPECIFIER.test(`${segments[i]}/${segments[i + 1]}`)) return true;
  }
  return false;
}

function isObservabilityTarget(text) {
  return OBSERVABILITY_SPECIFIER.test(text) || pathReferencesObservability(text);
}

// Root identifier of a property/element access chain (`path.posix.resolve` →
// `path`).
function rootIdentifierOf(node) {
  let current = unwrap(node);
  while (current) {
    if (ts.isIdentifier(current)) return current;
    if (ts.isPropertyAccessExpression(current) || ts.isElementAccessExpression(current)) {
      current = unwrap(current.expression);
      continue;
    }
    return null;
  }
  return null;
}

// Write targets of an assignment left-hand side, including destructuring
// patterns (`[a.b] = …`, `({ x: a.b } = …)`), defaults (`[a = 1] = …`), spreads
// and omitted elements. Anything else is not treated as a write.
function collectWriteTargets(node, out) {
  const target = unwrap(node);
  if (!target) return out;
  if (ts.isIdentifier(target) || ts.isPropertyAccessExpression(target) || ts.isElementAccessExpression(target)) {
    out.push(target);
    return out;
  }
  if (ts.isArrayLiteralExpression(target)) {
    for (const element of target.elements) {
      if (ts.isOmittedExpression(element)) continue;
      collectWriteTargets(element, out);
    }
    return out;
  }
  if (ts.isSpreadElement(target)) {
    collectWriteTargets(target.expression, out);
    return out;
  }
  if (ts.isObjectLiteralExpression(target)) {
    for (const property of target.properties) {
      if (ts.isPropertyAssignment(property)) collectWriteTargets(property.initializer, out);
      else if (ts.isShorthandPropertyAssignment(property)) out.push(property.name);
      else if (ts.isSpreadAssignment(property)) collectWriteTargets(property.expression, out);
    }
    return out;
  }
  if (ts.isBinaryExpression(target) && ts.isAssignmentOperator(target.operatorToken.kind)) {
    collectWriteTargets(target.left, out);
    return out;
  }
  return out;
}

// Only `++`/`--` are mutating unary operators; `void`, `typeof`, `!`, `+`, `-`
// and `~` are reads.
const isIncrementToken = (kind) => kind === ts.SyntaxKind.PlusPlusToken || kind === ts.SyntaxKind.MinusMinusToken;

function collectPatternNames(pattern, out = []) {
  if (ts.isIdentifier(pattern)) {
    out.push(pattern);
    return out;
  }
  if (ts.isObjectBindingPattern(pattern) || ts.isArrayBindingPattern(pattern)) {
    for (const element of pattern.elements) {
      if (ts.isBindingElement(element)) collectPatternNames(element.name, out);
    }
  }
  return out;
}

// Scope-aware binding model.
//
// Bindings are identified by their declaration's name node, never by text, so two
// variables with the same name — including a loop header's binding — in different
// scopes are distinct. A reference resolves to the nearest declaration in its
// lexical scope. Scopes are the module, blocks, function bodies, `switch` case
// blocks, `catch` clauses and loop headers (`for`/`for…of`/`for…in`); `var` belongs
// to the nearest function, `let`/`const` to the nearest block. An identifier alias
// (`const p = path`) shares its target's object, so any observable object mutation
// — a member write, a destructuring write, `Object.assign(…)`, … — at any depth
// propagates across every alias that shares the object and invalidates the
// original helper. For a statically-resolvable local function (declaration, arrow
// or function expression, plus simple alias chains), a call relates its arguments
// to parameters by identity, default and destructuring: a mutated parameter
// binding invalidates the argument it was bound to — a positional argument, the
// parameter's default initializer when the argument is absent *or statically
// `undefined`* (the global `undefined` or any `void …`, unless shadowed), or the
// matching property/element of an object/array pattern (also falling back to the
// pattern default when the value is statically `undefined`). A duplicate object
// key resolves to its last definition, matching equivalent identifier/string/
// numeric keys; a spread, computed key, method or accessor that could define the
// key makes the resolution ambiguous, and a mutated ambiguous binding fails
// closed by distrusting every helper seed. Mutations are propagated even when
// a function is not proven to run (conservative); an alias that is never written
// invalidates nothing.
function createBindingContext(source) {
  const scopeBindings = new Map(); // scopeNode -> Map(name -> declNameNode)
  const declInitializer = new Map(); // declNameNode -> initializer Expression
  const mutableDecls = new Set(); // let/var/param/function/class/catch/destructured-non-const/duplicate
  const reassignedDecls = new Set();
  const objectMutatedDecls = new Set();
  const helperSeeds = new Map(); // declNameNode -> { module, member }
  let implicitRequireTainted = false;

  const scopeKindOf = (node) => {
    if (ts.isSourceFile(node)) return "source";
    if (ts.isFunctionLike(node)) return "function";
    if (
      ts.isBlock(node) ||
      ts.isModuleBlock(node) ||
      ts.isCaseBlock(node) ||
      ts.isCatchClause(node) ||
      ts.isForStatement(node) ||
      ts.isForOfStatement(node) ||
      ts.isForInStatement(node)
    ) {
      return "block";
    }
    return null;
  };
  const nearestScope = (node, functionOnly) => {
    let current = node.parent;
    while (current) {
      const kind = scopeKindOf(current);
      if (kind === "source" || kind === "function") return current;
      if (kind === "block" && !functionOnly) return current;
      current = current.parent;
    }
    return source;
  };
  const registerName = (scope, nameNode, declName) => {
    if (!scopeBindings.has(scope)) scopeBindings.set(scope, new Map());
    const map = scopeBindings.get(scope);
    if (map.has(nameNode.text) && map.get(nameNode.text) !== declName) {
      // Two declarations of one name in one scope: ambiguous, neither is trusted.
      mutableDecls.add(map.get(nameNode.text));
      mutableDecls.add(declName);
      return;
    }
    map.set(nameNode.text, declName);
  };
  const registerPattern = (scope, pattern) => {
    if (ts.isIdentifier(pattern)) {
      registerName(scope, pattern, pattern);
      return;
    }
    if (ts.isObjectBindingPattern(pattern) || ts.isArrayBindingPattern(pattern)) {
      for (const element of pattern.elements) {
        if (ts.isBindingElement(element)) registerPattern(scope, element.name);
      }
    }
  };
  const visitScopes = (node) => {
    if (ts.isVariableDeclaration(node)) {
      const list = node.parent;
      const isVar = ts.isVariableDeclarationList(list) && (list.flags & ts.NodeFlags.BlockScoped) === 0;
      const isConst = ts.isVariableDeclarationList(list) && (list.flags & ts.NodeFlags.Const) !== 0;
      const scope = nearestScope(node, isVar);
      if (ts.isIdentifier(node.name)) {
        registerName(scope, node.name, node.name);
        if (node.initializer) declInitializer.set(node.name, node.initializer);
        if (!isConst) mutableDecls.add(node.name);
      } else {
        registerPattern(scope, node.name);
        if (!isConst) for (const name of collectPatternNames(node.name)) mutableDecls.add(name);
      }
    } else if (ts.isParameter(node)) {
      const scope = nearestScope(node, true);
      registerPattern(scope, node.name);
      for (const name of collectPatternNames(node.name)) mutableDecls.add(name);
    } else if (ts.isCatchClause(node) && node.variableDeclaration) {
      registerPattern(node, node.variableDeclaration.name);
      for (const name of collectPatternNames(node.variableDeclaration.name)) mutableDecls.add(name);
    } else if ((ts.isFunctionDeclaration(node) || ts.isClassDeclaration(node)) && node.name) {
      registerName(nearestScope(node, false), node.name, node.name);
      mutableDecls.add(node.name);
    } else if (ts.isFunctionExpression(node) && node.name) {
      // A named function expression binds its name inside its own body.
      registerName(node, node.name, node.name);
      mutableDecls.add(node.name);
    } else if (ts.isImportDeclaration(node) && node.importClause) {
      const clause = node.importClause;
      if (clause.name) registerName(source, clause.name, clause.name);
      const named = clause.namedBindings;
      if (named && ts.isNamespaceImport(named)) registerName(source, named.name, named.name);
      if (named && ts.isNamedImports(named)) {
        for (const element of named.elements) registerName(source, element.name, element.name);
      }
    }
    ts.forEachChild(node, visitScopes);
  };
  visitScopes(source);

  // Resolve an identifier reference to the nearest declaration in its lexical
  // scope (declarations are identified by their name node, so same-text bindings
  // in different scopes stay distinct).
  const resolve = (identifier) => {
    let current = identifier.parent;
    while (current) {
      const map = scopeBindings.get(current);
      if (map && map.has(identifier.text)) return map.get(identifier.text);
      current = current.parent;
    }
    return null;
  };

  const markTarget = (targetNode) => {
    const target = unwrap(targetNode);
    if (!target) return;
    if (ts.isIdentifier(target)) {
      const decl = resolve(target);
      if (decl) reassignedDecls.add(decl);
      else if (target.text === "require") implicitRequireTainted = true;
      return;
    }
    if (ts.isPropertyAccessExpression(target) || ts.isElementAccessExpression(target)) {
      const root = rootIdentifierOf(target);
      if (!root) return;
      const decl = resolve(root);
      if (decl) objectMutatedDecls.add(decl);
      else if (root.text === "require") implicitRequireTainted = true;
    }
  };
  const visitWrites = (node) => {
    if (ts.isBinaryExpression(node) && ts.isAssignmentOperator(node.operatorToken.kind)) {
      const targets = [];
      collectWriteTargets(node.left, targets);
      for (const target of targets) markTarget(target);
    } else if (
      (ts.isPrefixUnaryExpression(node) || ts.isPostfixUnaryExpression(node)) &&
      isIncrementToken(node.operator)
    ) {
      markTarget(node.operand);
    } else if (ts.isDeleteExpression(node)) {
      markTarget(node.expression);
    } else if (ts.isCallExpression(node)) {
      const callee = callTargetName(node.expression);
      if (
        (callee === "Object.assign" || callee === "Object.defineProperty" || callee === "Reflect.set") &&
        node.arguments.length >= 1
      ) {
        const first = unwrap(node.arguments[0]);
        if (first && (ts.isIdentifier(first) || ts.isPropertyAccessExpression(first) || ts.isElementAccessExpression(first))) {
          const root = rootIdentifierOf(first);
          if (root) {
            const decl = resolve(root);
            if (decl) objectMutatedDecls.add(decl);
          }
        }
      }
    }
    ts.forEachChild(node, visitWrites);
  };
  visitWrites(source);
  // Helper seeds: bindings imported from node:path/url, or required from them.
  const registerSeed = (idNode, module, member) => {
    if (!module || (!MODULE_PATH.has(module) && !MODULE_URL.has(module))) return;
    helperSeeds.set(idNode, { module, member });
  };
  for (const statement of source.statements) {
    if (ts.isImportDeclaration(statement) && statement.importClause) {
      const module = nodeSpecifier(statement.moduleSpecifier);
      const clause = statement.importClause;
      if (clause.name) registerSeed(clause.name, module, null);
      const named = clause.namedBindings;
      if (named && ts.isNamespaceImport(named)) registerSeed(named.name, module, null);
      if (named && ts.isNamedImports(named)) {
        for (const element of named.elements) {
          const imported = element.propertyName ? element.propertyName.text : element.name.text;
          registerSeed(element.name, module, imported);
        }
      }
    }
    if (ts.isVariableStatement(statement)) {
      for (const declaration of statement.declarationList.declarations) {
        const initializer = unwrap(declaration.initializer);
        if (!initializer || !ts.isCallExpression(initializer)) continue;
        const callee = initializer.expression;
        if (!ts.isIdentifier(callee) || callee.text !== "require") continue;
        if (resolve(callee) !== null || implicitRequireTainted) continue;
        if (initializer.arguments.length < 1) continue;
        const module = nodeSpecifier(initializer.arguments[0]);
        if (ts.isIdentifier(declaration.name)) {
          registerSeed(declaration.name, module, null);
        } else if (ts.isObjectBindingPattern(declaration.name)) {
          for (const element of declaration.name.elements) {
            if (!ts.isBindingElement(element) || !ts.isIdentifier(element.name)) continue;
            const imported = element.propertyName && ts.isIdentifier(element.propertyName) ? element.propertyName.text : element.name.text;
            registerSeed(element.name, module, imported);
          }
        }
      }
    }
  }

  // Identifier aliases share one object; a mutation propagates across the whole
  // component (cycles included). `for…of [a, b]` relates the loop binding to each
  // statically resolvable element of a literal iterable.
  const aliasParent = new Map();
  const findRoot = (node) => {
    let root = node;
    while (aliasParent.has(root) && aliasParent.get(root) !== root) root = aliasParent.get(root);
    let current = node;
    while (aliasParent.has(current) && aliasParent.get(current) !== current) {
      const next = aliasParent.get(current);
      aliasParent.set(current, root);
      current = next;
    }
    return root;
  };
  const linkAliases = (left, right) => {
    if (!left || !right || left === right) return;
    if (!aliasParent.has(left)) aliasParent.set(left, left);
    if (!aliasParent.has(right)) aliasParent.set(right, right);
    const leftRoot = findRoot(left);
    const rightRoot = findRoot(right);
    if (leftRoot !== rightRoot) aliasParent.set(leftRoot, rightRoot);
  };
  for (const [decl, initializer] of declInitializer) {
    const target = unwrap(initializer);
    if (target && ts.isIdentifier(target)) linkAliases(decl, resolve(target));
  }
  const collectForOfLiteralElements = (node) => {
    if (ts.isForOfStatement(node) && ts.isVariableDeclarationList(node.initializer)) {
      const iterable = unwrap(node.expression);
      if (ts.isArrayLiteralExpression(iterable)) {
        for (const declaration of node.initializer.declarations) {
          if (!ts.isIdentifier(declaration.name)) continue;
          for (const element of iterable.elements) {
            if (ts.isOmittedExpression(element) || ts.isSpreadElement(element)) continue;
            const elementNode = unwrap(element);
            if (elementNode && ts.isIdentifier(elementNode)) linkAliases(resolve(declaration.name), resolve(elementNode));
          }
        }
      }
    }
    ts.forEachChild(node, collectForOfLiteralElements);
  };
  collectForOfLiteralElements(source);

  // A declaration (or anything sharing its object) that is object-mutated is
  // invalidated; adding one immediately pulls in its whole alias component.
  const mutated = new Set();
  const addMutated = (decl) => {
    if (!decl || mutated.has(decl)) return false;
    if (aliasParent.has(decl)) {
      const root = findRoot(decl);
      for (const candidate of aliasParent.keys()) if (findRoot(candidate) === root) mutated.add(candidate);
    }
    mutated.add(decl);
    return true;
  };
  for (const decl of objectMutatedDecls) addMutated(decl);
  const isMutated = (decl) => Boolean(decl) && mutated.has(decl);

  // Local functions statically resolved through a binding: function declarations,
  // arrow/function expressions assigned directly, and simple alias chains.
  const functionOf = new Map();
  const functions = [];
  const collectFunctions = (node) => {
    if (ts.isFunctionDeclaration(node) && node.name) {
      functions.push({ decl: node.name, fn: node, from: null });
    } else if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.initializer) {
      const initializer = unwrap(node.initializer);
      if (initializer && (ts.isArrowFunction(initializer) || ts.isFunctionExpression(initializer))) {
        functions.push({ decl: node.name, fn: initializer, from: null });
      } else if (initializer && ts.isIdentifier(initializer)) {
        functions.push({ decl: node.name, fn: null, from: initializer });
      }
    }
    ts.forEachChild(node, collectFunctions);
  };
  collectFunctions(source);
  for (const entry of functions) if (entry.fn) functionOf.set(entry.decl, entry.fn);
  for (let pass = 0; pass <= functions.length; pass += 1) {
    let changed = false;
    for (const entry of functions) {
      if (entry.fn || functionOf.has(entry.decl)) continue;
      const sourceDecl = resolve(entry.from);
      if (sourceDecl && functionOf.has(sourceDecl)) {
        functionOf.set(entry.decl, functionOf.get(sourceDecl));
        changed = true;
      }
    }
    if (!changed) break;
  }

  // Relate each call's arguments to its parameters — identifier, default and
  // destructured patterns alike: a parameter binding that is object-mutated
  // taints the value it was bound to. The fixpoint also covers calls that pass a
  // binding on to another local function.
  const initializerOf = (decl) => (decl ? declInitializer.get(decl) ?? null : null);
  const isTrustworthyBinding = (decl) =>
    Boolean(decl) && !mutableDecls.has(decl) && !reassignedDecls.has(decl) && !mutated.has(decl);
  const taintArgument = (argument) => {
    const node = unwrap(argument);
    if (!node || !ts.isIdentifier(node)) return false;
    return addMutated(resolve(node));
  };
  // Static text of an object/array binding element or property key (identifier,
  // string or numeric); a computed name has none.
  const staticKeyOf = (node) => {
    const target = unwrap(node);
    if (!target) return null;
    if (ts.isIdentifier(target) || ts.isStringLiteral(target) || ts.isNumericLiteral(target)) return target.text;
    return null;
  };
  // A value that is statically `undefined`: any `void …`, or the un-shadowed
  // global `undefined` (a local binding of that name resolves to a declaration).
  const isStaticUndefined = (expression) => {
    const node = unwrap(expression);
    if (!node) return false;
    if (ts.isVoidExpression(node)) return true;
    if (ts.isIdentifier(node) && node.text === "undefined") return resolve(node) === null;
    return false;
  };
  // Effective value of `key` in an object literal, in property order: the last
  // definition wins, equivalent keys (identifier/string/numeric) match, and a
  // spread, computed key, method or accessor that could define `key` makes the
  // resolution ambiguous rather than silently permissive.
  const AMBIGUOUS = Symbol("ambiguous");
  const resolveObjectKey = (literal, key) => {
    let found = false;
    let value = null;
    let ambiguous = false;
    for (const property of literal.properties) {
      if (ts.isSpreadAssignment(property)) {
        ambiguous = true;
        continue;
      }
      const propertyKey = staticKeyOf(property.name);
      if (propertyKey === null) {
        ambiguous = true;
        continue;
      }
      if (propertyKey !== key) continue;
      if (ts.isPropertyAssignment(property)) {
        found = true;
        value = property.initializer;
        ambiguous = false;
      } else if (ts.isShorthandPropertyAssignment(property)) {
        found = true;
        value = property.name;
        ambiguous = false;
      } else {
        found = true;
        value = null;
        ambiguous = true;
      }
    }
    return { found, value, ambiguous };
  };
  const applyCall = (call) => {
    const fn = functionOf.get(resolve(call.expression));
    if (!fn) return false;
    const parameterBindings = new Set();
    for (const parameter of fn.parameters) for (const name of collectPatternNames(parameter.name)) parameterBindings.add(name);
    const bindingSources = new Map(); // binding decl -> source decl | AMBIGUOUS | null
    // Follow one level of a literal initializer (`const o = { h: path }; f(o)`)
    // but never follow a reassigned, mutable or mutated binding.
    const literalOf = (expression) => {
      const node = unwrap(expression);
      if (!node) return null;
      if (ts.isObjectLiteralExpression(node) || ts.isArrayLiteralExpression(node)) return node;
      if (ts.isIdentifier(node)) {
        const decl = resolve(node);
        if (!isTrustworthyBinding(decl)) return null;
        const initializer = unwrap(initializerOf(decl));
        if (initializer && (ts.isObjectLiteralExpression(initializer) || ts.isArrayLiteralExpression(initializer))) return initializer;
      }
      return null;
    };
    // The declaration a value derives from; a reference to another parameter uses
    // that parameter's already-computed value (defaults evaluate left to right).
    const sourceOf = (expression) => {
      const node = unwrap(expression);
      if (!node || !ts.isIdentifier(node)) return null;
      const decl = resolve(node);
      if (decl && parameterBindings.has(decl)) return bindingSources.get(decl) ?? null;
      return decl;
    };
    // An explicit value that is not statically `undefined` wins; otherwise the
    // binding's own default applies.
    const effectiveValue = (explicit, fallback) => {
      if (explicit && !isStaticUndefined(explicit)) return explicit;
      return fallback ?? null;
    };
    const bindPattern = (pattern, value) => {
      if (ts.isIdentifier(pattern)) {
        bindingSources.set(pattern, value ? sourceOf(value) : null);
        return;
      }
      if (ts.isObjectBindingPattern(pattern)) {
        const literal = value ? literalOf(value) : null;
        if (!literal || !ts.isObjectLiteralExpression(literal)) {
          for (const name of collectPatternNames(pattern)) if (!bindingSources.has(name)) bindingSources.set(name, null);
          return;
        }
        for (const element of pattern.elements) {
          if (!ts.isBindingElement(element)) continue;
          if (element.dotDotDotToken) {
            // Object rest is a shallow copy, not an alias of the source.
            for (const name of collectPatternNames(element.name)) bindingSources.set(name, null);
            continue;
          }
          const key = staticKeyOf(element.propertyName ?? element.name);
          if (key === null) {
            for (const name of collectPatternNames(element.name)) if (!bindingSources.has(name)) bindingSources.set(name, null);
            continue;
          }
          const property = resolveObjectKey(literal, key);
          if (property.ambiguous) {
            for (const name of collectPatternNames(element.name)) bindingSources.set(name, AMBIGUOUS);
            continue;
          }
          const explicit = property.found ? property.value : null;
          bindPattern(element.name, effectiveValue(explicit, element.initializer));
        }
        return;
      }
      if (ts.isArrayBindingPattern(pattern)) {
        const literal = value ? literalOf(value) : null;
        if (!literal || !ts.isArrayLiteralExpression(literal) || literal.elements.some((element) => ts.isSpreadElement(element))) {
          for (const name of collectPatternNames(pattern)) if (!bindingSources.has(name)) bindingSources.set(name, null);
          return;
        }
        for (let index = 0; index < pattern.elements.length; index += 1) {
          const element = pattern.elements[index];
          if (!ts.isBindingElement(element)) continue;
          if (element.dotDotDotToken) {
            // Array rest is a new array, not an alias of the source.
            for (const name of collectPatternNames(element.name)) bindingSources.set(name, null);
            continue;
          }
          const source = literal.elements[index];
          const explicit = source && !ts.isOmittedExpression(source) ? source : null;
          bindPattern(element.name, effectiveValue(explicit, element.initializer));
        }
        return;
      }
      for (const name of collectPatternNames(pattern)) if (!bindingSources.has(name)) bindingSources.set(name, null);
    };
    let changed = false;
    if (!call.arguments.some((argument) => ts.isSpreadElement(argument))) {
      for (let index = 0; index < fn.parameters.length; index += 1) {
        const parameter = fn.parameters[index];
        if (parameter.dotDotDotToken) {
          if (collectPatternNames(parameter.name).some(isMutated)) {
            for (let rest = index; rest < call.arguments.length; rest += 1) changed = taintArgument(call.arguments[rest]) || changed;
          }
          break;
        }
        bindPattern(parameter.name, effectiveValue(call.arguments[index], parameter.initializer));
      }
    }
    for (const [binding, source] of bindingSources) {
      if (!isMutated(binding)) continue;
      if (source === AMBIGUOUS) {
        // A mutated binding whose value cannot be resolved could be the helper:
        // fail closed by distrusting every helper seed rather than assuming none.
        for (const decl of helperSeeds.keys()) changed = addMutated(decl) || changed;
      } else if (source) {
        changed = addMutated(source) || changed;
      }
    }
    return changed;
  };
  const calls = [];
  const collectCalls = (node) => {
    if (ts.isCallExpression(node) && ts.isIdentifier(node.expression)) calls.push(node);
    ts.forEachChild(node, collectCalls);
  };
  collectCalls(source);
  for (let pass = 0; pass <= calls.length + 1; pass += 1) {
    let changed = false;
    for (const call of calls) changed = applyCall(call) || changed;
    if (!changed) break;
  }

  const invalidated = new Set(mutableDecls);
  for (const decl of reassignedDecls) invalidated.add(decl);
  for (const decl of mutated) invalidated.add(decl);

  const isInvalidated = (decl) => Boolean(decl) && invalidated.has(decl);
  const seedOf = (decl) => {
    if (!decl || invalidated.has(decl)) return null;
    return helperSeeds.get(decl) ?? null;
  };
  return { resolve, initializerOf, isInvalidated, seedOf };
}

// Identity of a trusted helper binding/expression as { module, member } (member is
// null for a namespace import). Declarations are resolved through the scope-aware
// binding table, so aliases and shadowing are handled by identity, not by text.
function helperIdentity(node, ctx, seen) {
  const target = unwrap(node);
  if (!target) return null;
  if (ts.isIdentifier(target)) {
    const decl = ctx.resolve(target);
    if (!decl || seen.has(decl)) return null;
    const seed = ctx.seedOf(decl);
    if (seed) return seed;
    if (ctx.isInvalidated(decl)) return null;
    const initializer = ctx.initializerOf(decl);
    if (!initializer) return null;
    const next = new Set(seen);
    next.add(decl);
    return helperIdentity(initializer, ctx, next);
  }
  if (ts.isPropertyAccessExpression(target)) {
    const segments = [];
    let current = target;
    while (ts.isPropertyAccessExpression(current)) {
      segments.unshift(current.name.text);
      current = current.expression;
    }
    // Resolve the base through the alias chain (`const p = path; p.resolve`).
    const base = helperIdentity(current, ctx, seen);
    if (!base || base.member !== null) return null;
    return { module: base.module, member: segments.join(".") };
  }
  return null;
}

// Whether `node` is the URL class: the un-shadowed global, or a binding imported
// from node:url/url. A local `URL` from another source is not trusted.
function isTrustedUrlClass(node, ctx) {
  if (ts.isIdentifier(node) && node.text === "URL") {
    const decl = ctx.resolve(node);
    if (!decl) return true; // the un-shadowed global URL
    const seed = ctx.seedOf(decl);
    return Boolean(seed && MODULE_URL.has(seed.module) && seed.member === "URL");
  }
  if (ts.isPropertyAccessExpression(node) && node.name.text === "URL") {
    const base = helperIdentity(node.expression, ctx, new Set());
    return Boolean(base && base.member === null && MODULE_URL.has(base.module));
  }
  return false;
}

function resolveStaticString(node, ctx, seen) {
  const target = unwrap(node);
  if (!target) return null;
  if (ts.isStringLiteral(target) || ts.isNoSubstitutionTemplateLiteral(target)) return target.text;
  if (ts.isIdentifier(target)) {
    const decl = ctx.resolve(target);
    if (!decl || ctx.isInvalidated(decl) || seen.has(decl)) return null;
    const initializer = ctx.initializerOf(decl);
    if (!initializer) return null;
    const next = new Set(seen);
    next.add(decl);
    return resolveStaticString(initializer, ctx, next);
  }
  return null;
}

function resolvePathCallArgument(node, ctx, seen) {
  const target = unwrap(node);
  if (!target) return null;
  if (ts.isStringLiteral(target) || ts.isNoSubstitutionTemplateLiteral(target)) return target.text;
  // import.meta.url is an intrinsic, non-reassignable base.
  if (
    ts.isPropertyAccessExpression(target) &&
    target.expression.kind === ts.SyntaxKind.MetaProperty &&
    target.name.text === "url"
  ) {
    return "";
  }
  if (ts.isIdentifier(target)) {
    if (target.text === "__dirname" || target.text === "__filename") {
      // Implicit only when not declared. A static local const is resolved and
      // analysed; a mutable or non-static one fails closed.
      const decl = ctx.resolve(target);
      if (!decl) return "";
      if (ctx.isInvalidated(decl)) return null;
      if (!ctx.initializerOf(decl)) return null;
      return resolveStaticString(target, ctx, seen);
    }
    return resolveStaticString(target, ctx, seen);
  }
  return null;
}

function resolveNewUrl(node, ctx, seen) {
  if (!isTrustedUrlClass(node.expression, ctx)) return null;
  const first = resolveStaticString(node.arguments && node.arguments[0], ctx, seen);
  if (first === null) return null;
  return { type: "path", value: first };
}

function resolveStatic(node, ctx, seen) {
  const target = unwrap(node);
  if (!target) return { type: "dynamic" };
  if (ts.isStringLiteral(target) || ts.isNoSubstitutionTemplateLiteral(target)) {
    return { type: "string", value: target.text };
  }
  if (ts.isObjectLiteralExpression(target)) return { type: "object", node: target };
  if (ts.isArrayLiteralExpression(target)) return { type: "array", node: target };
  if (ts.isIdentifier(target)) {
    const decl = ctx.resolve(target);
    if (!decl || ctx.isInvalidated(decl) || seen.has(decl)) return { type: "dynamic" };
    const initializer = ctx.initializerOf(decl);
    if (!initializer) return { type: "dynamic" };
    const next = new Set(seen);
    next.add(decl);
    return resolveStatic(initializer, ctx, next);
  }
  if (ts.isNewExpression(target)) {
    return resolveNewUrl(target, ctx, seen) ?? { type: "dynamic" };
  }
  if (ts.isCallExpression(target)) {
    const identity = helperIdentity(target.expression, ctx, new Set());
    if (identity && MODULE_PATH.has(identity.module) && PATH_MEMBERS.has(identity.member)) {
      const parts = [];
      for (const argument of target.arguments) {
        const part = resolvePathCallArgument(argument, ctx, seen);
        if (part === null) return { type: "dynamic" };
        parts.push(part);
      }
      return { type: "path", value: parts.filter(Boolean).join("/") };
    }
    if (identity && MODULE_URL.has(identity.module) && URL_MEMBERS.has(identity.member)) {
      const argument = unwrap(target.arguments && target.arguments[0]);
      if (argument && ts.isNewExpression(argument)) {
        return resolveNewUrl(argument, ctx, seen) ?? { type: "dynamic" };
      }
      const part = resolvePathCallArgument(argument, ctx, seen);
      return part === null ? { type: "dynamic" } : { type: "path", value: part };
    }
    return { type: "dynamic" };
  }
  return { type: "dynamic" };
}

function classifyAliasTarget(rel, lineOf, node, label, ctx) {
  const resolved = resolveStatic(node, ctx, new Set());
  if (resolved.type === "string" || resolved.type === "path") {
    if (isObservabilityTarget(resolved.value)) {
      report(rel, lineOf(node), resolved.value, `${label} target references an observability SDK (not allowlisted)`);
    }
    return;
  }
  // A call/identifier/concatenation/interpolation/spread whose value is not fully
  // determined: reject fail-closed.
  report(rel, lineOf(node), null, `unanalyzable ${label} target: a dynamically built alias cannot be verified (not allowlisted)`);
}

function inspectAliasObject(rel, lineOf, object, label, ctx) {
  for (const property of object.properties) {
    if (ts.isSpreadAssignment(property)) {
      report(rel, lineOf(property), null, `unanalyzable spread inside ${label} (not allowlisted)`);
      continue;
    }
    if (ts.isShorthandPropertyAssignment(property)) {
      classifyAliasTarget(rel, lineOf, property.name, label, ctx);
      continue;
    }
    if (ts.isPropertyAssignment(property)) {
      classifyAliasTarget(rel, lineOf, property.initializer, label, ctx);
      continue;
    }
    report(rel, lineOf(property), null, `unanalyzable ${label} entry (not allowlisted)`);
  }
}

function inspectAliasArrayElement(rel, lineOf, element, label, ctx) {
  const target = unwrap(element);
  if (ts.isSpreadElement(target)) {
    report(rel, lineOf(element), null, `unanalyzable spread inside ${label} (not allowlisted)`);
    return;
  }
  if (ts.isStringLiteral(target) || ts.isNoSubstitutionTemplateLiteral(target)) {
    classifyAliasTarget(rel, lineOf, target, label, ctx);
    return;
  }
  const resolved = resolveStatic(target, ctx, new Set());
  if (resolved.type === "object") {
    let replacement = null;
    for (const property of resolved.node.properties) {
      if (ts.isSpreadAssignment(property)) {
        report(rel, lineOf(property), null, `unanalyzable spread inside ${label} entry (not allowlisted)`);
        continue;
      }
      if (ts.isPropertyAssignment(property) && propertyNameOf(property) === "replacement") replacement = property.initializer;
      else if (ts.isShorthandPropertyAssignment(property) && property.name.text === "replacement") replacement = property.name;
    }
    if (!replacement) {
      report(rel, lineOf(element), null, `unanalyzable ${label} {find, replacement} target (not allowlisted)`);
      return;
    }
    classifyAliasTarget(rel, lineOf, replacement, `${label} {replacement}`, ctx);
    return;
  }
  report(rel, lineOf(element), null, `unanalyzable ${label} array element (not allowlisted)`);
}

function inspectAliasMap(rel, lineOf, node, label, ctx) {
  const resolved = resolveStatic(node, ctx, new Set());
  if (resolved.type === "object") {
    inspectAliasObject(rel, lineOf, resolved.node, label, ctx);
    return;
  }
  if (resolved.type === "array") {
    for (const element of resolved.node.elements) inspectAliasArrayElement(rel, lineOf, element, label, ctx);
    return;
  }
  report(rel, lineOf(node), null, `unanalyzable ${label}: a dynamically built alias map cannot be verified (not allowlisted)`);
}

function inspectResolveObject(rel, lineOf, object, ctx) {
  for (const property of object.properties) {
    if (ts.isSpreadAssignment(property)) {
      report(rel, lineOf(property), null, "unanalyzable spread inside resolve (not allowlisted)");
      continue;
    }
    if (propertyNameOf(property) !== "alias") continue;
    if (ts.isShorthandPropertyAssignment(property)) inspectAliasMap(rel, lineOf, property.name, "resolve.alias", ctx);
    else if (ts.isPropertyAssignment(property)) inspectAliasMap(rel, lineOf, property.initializer, "resolve.alias", ctx);
    else report(rel, lineOf(property), null, "unanalyzable resolve.alias entry (not allowlisted)");
  }
}

function inspectResolveProperty(rel, lineOf, property, ctx) {
  const node = ts.isPropertyAssignment(property) ? property.initializer : property.name;
  const resolved = resolveStatic(node, ctx, new Set());
  if (resolved.type !== "object") {
    report(rel, lineOf(property), null, "unanalyzable resolve: a dynamically built resolve cannot be verified (not allowlisted)");
    return;
  }
  inspectResolveObject(rel, lineOf, resolved.node, ctx);
}

// Resolve an exported config expression to an object literal:
//   export default <obj>
//   export default defineConfig(<obj>) | defineConfig(<binding>) | defineConfig(() => <obj>)
//   module.exports = <obj> | () => <obj>
// Returns { type: "object", node } or { type: "dynamic" }.
function resolveConfigExport(expression, ctx, seen) {
  const node = unwrap(expression);
  if (!node) return { type: "dynamic" };
  if (ts.isCallExpression(node) && callTargetName(node.expression) === "defineConfig") {
    return resolveConfigExport(node.arguments && node.arguments[0], ctx, seen);
  }
  if (ts.isObjectLiteralExpression(node)) return { type: "object", node };
  if (ts.isIdentifier(node)) {
    const decl = ctx.resolve(node);
    if (!decl || ctx.isInvalidated(decl) || seen.has(decl)) return { type: "dynamic" };
    const initializer = ctx.initializerOf(decl);
    if (!initializer) return { type: "dynamic" };
    const next = new Set(seen);
    next.add(decl);
    return resolveConfigExport(initializer, ctx, next);
  }
  if (ts.isArrowFunction(node) || ts.isFunctionExpression(node)) {
    const body = unwrap(node.body);
    if (body && ts.isObjectLiteralExpression(body)) return { type: "object", node: body };
    if (body && ts.isBlock(body)) {
      const returns = body.statements.filter((statement) => ts.isReturnStatement(statement) && statement.expression);
      if (body.statements.length === 1 && returns.length === 1) {
        return resolveConfigExport(returns[0].expression, ctx, seen);
      }
    }
    return { type: "dynamic" };
  }
  return { type: "dynamic" };
}

// Export targets (default export / module.exports) and `default` re-exports.
function configExportTargets(source) {
  const targets = [];
  const reexports = [];
  for (const statement of source.statements) {
    if (ts.isExportAssignment(statement)) {
      targets.push({ node: statement, expression: statement.expression });
    } else if (ts.isExportDeclaration(statement) && statement.moduleSpecifier) {
      const clause = statement.exportClause;
      const reexportsDefault =
        clause &&
        ts.isNamedExports(clause) &&
        clause.elements.some((element) => (element.propertyName ?? element.name).text === "default");
      if (reexportsDefault) reexports.push({ node: statement, spec: nodeSpecifier(statement.moduleSpecifier) });
    } else if (ts.isExpressionStatement(statement) && ts.isBinaryExpression(statement.expression)) {
      const left = statement.expression.left;
      if (
        ts.isPropertyAccessExpression(left) &&
        ts.isIdentifier(left.expression) &&
        left.expression.text === "module" &&
        left.name.text === "exports"
      ) {
        targets.push({ node: statement, expression: statement.expression.right });
      }
    }
  }
  return { targets, reexports };
}

function resolveRelativeModule(fromAbs, spec) {
  if (!spec || !spec.startsWith(".")) return null;
  const base = path.resolve(path.dirname(fromAbs), spec);
  const candidates = [
    base,
    `${base}.ts`,
    `${base}.tsx`,
    `${base}.mts`,
    `${base}.cts`,
    `${base}.js`,
    `${base}.jsx`,
    `${base}.mjs`,
    `${base}.cjs`,
    `${base}.json`,
    path.join(base, "index.ts"),
    path.join(base, "index.tsx"),
    path.join(base, "index.js"),
    path.join(base, "index.mjs"),
    path.join(base, "index.cjs"),
  ];
  for (const candidate of candidates) {
    try {
      if (statSync(candidate).isFile()) return candidate;
    } catch {
      /* try the next candidate */
    }
  }
  return null;
}

// Collect the exported config objects of a resolution config, following relative
// `export { default } from "./…"` re-exports. A target that cannot be resolved to
// an object is returned as dynamic so the caller can fail closed.
function collectConfigObjects(abs, source, visited) {
  const rel = relPath(abs);
  const lineOf = lineOfNode(source);
  const ctx = createBindingContext(source);
  const objects = [];
  const dynamic = [];
  const { targets, reexports } = configExportTargets(source);
  for (const target of targets) {
    const resolved = resolveConfigExport(target.expression, ctx, new Set());
    if (resolved.type === "object") objects.push({ rel, lineOf, node: resolved.node, ctx });
    else dynamic.push({ rel, lineOf, node: target.node });
  }
  for (const reexport of reexports) {
    const target = resolveRelativeModule(abs, reexport.spec);
    if (!target || visited.has(target)) {
      dynamic.push({ rel, lineOf, node: reexport.node });
      continue;
    }
    visited.add(target);
    const targetRel = relPath(target);
    if (IGNORED_FILES.has(targetRel)) continue;
    let text;
    try {
      text = readFileSync(target, "utf8");
    } catch {
      dynamic.push({ rel, lineOf, node: reexport.node });
      continue;
    }
    const kind = SCRIPT_KIND[path.extname(target)] ?? ts.ScriptKind.TS;
    const targetSource = ts.createSourceFile(target, text, ts.ScriptTarget.Latest, true, kind);
    const nested = collectConfigObjects(target, targetSource, visited);
    objects.push(...nested.objects);
    dynamic.push(...nested.dynamic);
  }
  return { objects, dynamic };
}

function inspectConfigObject(rel, lineOf, object, ctx) {
  for (const property of object.properties) {
    if (ts.isSpreadAssignment(property)) {
      report(rel, lineOf(property), null, "unanalyzable spread in the exported config object (not allowlisted)");
      continue;
    }
    if (propertyNameOf(property) === "resolve") inspectResolveProperty(rel, lineOf, property, ctx);
  }
}

function scanViteConfig(abs, source) {
  const visited = new Set([abs]);
  const { objects, dynamic } = collectConfigObjects(abs, source, visited);
  for (const entry of dynamic) {
    report(entry.rel, entry.lineOf(entry.node), null, "unanalyzable exported config (not allowlisted)");
  }
  if (objects.length === 0 && dynamic.length === 0) {
    report(relPath(abs), 0, null, "no analyzable exported config (not allowlisted)");
  }
  for (const entry of objects) inspectConfigObject(entry.rel, entry.lineOf, entry.node, entry.ctx);
}

function isModuleResolverPluginName(name) {
  return typeof name === "string" && /^(?:@[^/]+\/)?(?:babel-plugin-)?module-resolver$/.test(name);
}

function inspectBabelPluginOptions(rel, lineOf, options, ctx) {
  const resolved = resolveStatic(options, ctx, new Set());
  if (resolved.type !== "object") {
    report(rel, lineOf(options), null, "unanalyzable Babel module-resolver options (not allowlisted)");
    return;
  }
  for (const property of resolved.node.properties) {
    if (ts.isSpreadAssignment(property)) {
      report(rel, lineOf(property), null, "unanalyzable spread inside Babel module-resolver options (not allowlisted)");
      continue;
    }
    if (propertyNameOf(property) !== "alias") continue;
    if (ts.isShorthandPropertyAssignment(property)) inspectAliasMap(rel, lineOf, property.name, "Babel module-resolver alias", ctx);
    else if (ts.isPropertyAssignment(property)) inspectAliasMap(rel, lineOf, property.initializer, "Babel module-resolver alias", ctx);
    else report(rel, lineOf(property), null, "unanalyzable Babel module-resolver alias entry (not allowlisted)");
  }
}

function inspectBabelPluginName(rel, lineOf, nameNode, optionsNode, ctx) {
  const name = resolveStaticString(nameNode, ctx, new Set());
  if (name !== null) {
    if (isModuleResolverPluginName(name) && optionsNode) inspectBabelPluginOptions(rel, lineOf, optionsNode, ctx);
    return;
  }
  // A plugin name that is not fully static. A reassigned/shadowed binding cannot
  // be trusted, so fail closed; an otherwise unknown dynamic plugin name is not
  // inspected (a computed plugin name cannot be classified without false-
  // positiving every dynamic plugin — documented limit).
  const identifier = unwrap(nameNode);
  if (identifier && ts.isIdentifier(identifier)) {
    const decl = ctx.resolve(identifier);
    if (decl && ctx.isInvalidated(decl)) {
      report(rel, lineOf(identifier), null, "unanalyzable Babel plugin name (reassigned/shadowed binding; not allowlisted)");
    }
  }
}

function scanBabelConfig(rel, source, ctx) {
  const lineOf = lineOfNode(source);
  const visit = (node) => {
    if (ts.isArrayLiteralExpression(node) && node.elements.length >= 1) {
      inspectBabelPluginName(rel, lineOf, node.elements[0], node.elements.length >= 2 ? node.elements[1] : null, ctx);
    }
    if (ts.isObjectLiteralExpression(node)) {
      let nameNode = null;
      let optionsNode = null;
      for (const property of node.properties) {
        if (!ts.isPropertyAssignment(property)) continue;
        if (propertyNameOf(property) === "name") nameNode = property.initializer;
        if (propertyNameOf(property) === "options") optionsNode = property.initializer;
      }
      if (nameNode && optionsNode) inspectBabelPluginName(rel, lineOf, nameNode, optionsNode, ctx);
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
}

function inspectMetroResolverObject(rel, lineOf, object, ctx) {
  for (const property of object.properties) {
    if (ts.isSpreadAssignment(property)) {
      report(rel, lineOf(property), null, "unanalyzable spread inside Metro resolver (not allowlisted)");
      continue;
    }
    if (propertyNameOf(property) !== "extraNodeModules") continue;
    if (ts.isShorthandPropertyAssignment(property)) inspectAliasMap(rel, lineOf, property.name, "Metro resolver.extraNodeModules", ctx);
    else if (ts.isPropertyAssignment(property)) inspectAliasMap(rel, lineOf, property.initializer, "Metro resolver.extraNodeModules", ctx);
    else report(rel, lineOf(property), null, "unanalyzable Metro resolver.extraNodeModules entry (not allowlisted)");
  }
}

function scanMetroConfig(rel, source, ctx) {
  const lineOf = lineOfNode(source);
  const assignmentTarget = (node) =>
    ts.isBinaryExpression(node) &&
    node.operatorToken.kind === ts.SyntaxKind.EqualsToken &&
    ts.isPropertyAccessExpression(node.left)
      ? node.left.name.text
      : null;
  const visit = (node) => {
    // Object-literal form (`{ resolver: { extraNodeModules } }`).
    if (ts.isObjectLiteralExpression(node)) {
      for (const property of node.properties) {
        if (propertyNameOf(property) !== "resolver") continue;
        const value = ts.isShorthandPropertyAssignment(property) ? property.name : property.initializer;
        const resolved = resolveStatic(value, ctx, new Set());
        if (resolved.type === "object") inspectMetroResolverObject(rel, lineOf, resolved.node, ctx);
        else report(rel, lineOf(property), null, "unanalyzable Metro resolver: a dynamically built resolver cannot be verified (not allowlisted)");
      }
    }
    // Assignment form (`config.resolver.extraNodeModules = ...`,
    // `config.resolver.resolveRequest = fn`).
    const target = assignmentTarget(node);
    if (target === "extraNodeModules") {
      inspectAliasMap(rel, lineOf, node.right, "Metro resolver.extraNodeModules", ctx);
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
    // setParentNodes=true: the resolve.alias walk and binding scoping need parents.
    const source = ts.createSourceFile(abs, text, ts.ScriptTarget.Latest, true, scriptKind);
    if (kind === "vite") {
      scanViteConfig(abs, source);
      continue;
    }
    const ctx = createBindingContext(source);
    if (kind === "babel") scanBabelConfig(rel, source, ctx);
    else if (kind === "metro") scanMetroConfig(rel, source, ctx);
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
