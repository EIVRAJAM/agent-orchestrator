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

const PATH_CALLS = new Set(["path.resolve", "path.join", "path.posix.resolve", "path.posix.join"]);
const FILE_URL_CALLS = new Set(["fileURLToPath", "url.fileURLToPath"]);

// A filesystem base that contributes no package name (the module's own location).
function isLocalBase(node) {
  if (ts.isIdentifier(node) && (node.text === "__dirname" || node.text === "__filename")) return true;
  if (ts.isPropertyAccessExpression(node) && node.expression.kind === ts.SyntaxKind.MetaProperty && node.name.text === "url") {
    return true;
  }
  return false;
}

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

// Top-level `const` identifier bindings, so a simple local alias can be resolved.
// Anything declared with let/var, destructured, redeclared, reassigned or
// declared in a nested scope is marked unsafe and treated as dynamic.
function createBindingContext(source) {
  const bindings = new Map();
  const unsafe = new Set();
  const isTopLevel = (node) => {
    let parent = node.parent;
    while (parent) {
      if (parent === source) return true;
      if (ts.isFunctionLike(parent) || ts.isBlock(parent) || ts.isModuleBlock(parent)) return false;
      parent = parent.parent;
    }
    return false;
  };
  const markPattern = (pattern) => {
    if (ts.isIdentifier(pattern)) unsafe.add(pattern.text);
    else ts.forEachChild(pattern, markPattern);
  };
  const visit = (node) => {
    if (ts.isVariableDeclaration(node) && node.initializer && isTopLevel(node)) {
      if (ts.isIdentifier(node.name)) {
        const isConst = ts.isVariableDeclarationList(node.parent) && (node.parent.flags & ts.NodeFlags.Const) !== 0;
        if (!isConst || bindings.has(node.name.text)) unsafe.add(node.name.text);
        else bindings.set(node.name.text, node.initializer);
      } else {
        markPattern(node.name);
      }
    }
    if (ts.isBinaryExpression(node) && ts.isIdentifier(node.left)) unsafe.add(node.left.text);
    if ((ts.isPrefixUnaryExpression(node) || ts.isPostfixUnaryExpression(node)) && ts.isIdentifier(node.operand)) {
      unsafe.add(node.operand.text);
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return { bindings, unsafe };
}

function resolveStaticString(node, ctx, seen) {
  const target = unwrap(node);
  if (!target) return null;
  if (ts.isStringLiteral(target) || ts.isNoSubstitutionTemplateLiteral(target)) return target.text;
  if (ts.isIdentifier(target)) {
    if (ctx.unsafe.has(target.text) || seen.has(target.text)) return null;
    const binding = ctx.bindings.get(target.text);
    if (!binding) return null;
    const next = new Set(seen);
    next.add(target.text);
    return resolveStaticString(binding, ctx, next);
  }
  return null;
}

function resolvePathCallArgument(node, ctx, seen) {
  const target = unwrap(node);
  if (!target) return null;
  if (ts.isStringLiteral(target) || ts.isNoSubstitutionTemplateLiteral(target)) return target.text;
  if (isLocalBase(target)) return "";
  if (ts.isIdentifier(target)) return resolveStaticString(target, ctx, seen);
  return null;
}

function resolveNewUrl(node, ctx, seen) {
  const name = callTargetName(node.expression);
  if (name !== "URL" && name !== "url.URL") return null;
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
    if (ctx.unsafe.has(target.text) || seen.has(target.text)) return { type: "dynamic" };
    const binding = ctx.bindings.get(target.text);
    if (!binding) return { type: "dynamic" };
    const next = new Set(seen);
    next.add(target.text);
    return resolveStatic(binding, ctx, next);
  }
  if (ts.isNewExpression(target)) {
    return resolveNewUrl(target, ctx, seen) ?? { type: "dynamic" };
  }
  if (ts.isCallExpression(target)) {
    const name = callTargetName(target.expression);
    if (name && PATH_CALLS.has(name)) {
      const parts = [];
      for (const argument of target.arguments) {
        const part = resolvePathCallArgument(argument, ctx, seen);
        if (part === null) return { type: "dynamic" };
        parts.push(part);
      }
      return { type: "path", value: parts.filter(Boolean).join("/") };
    }
    if (name && FILE_URL_CALLS.has(name)) {
      const argument = unwrap(target.arguments && target.arguments[0]);
      if (argument && ts.isNewExpression(argument)) {
        return resolveNewUrl(argument, ctx, seen) ?? { type: "dynamic" };
      }
      const text = resolveStaticString(argument, ctx, seen);
      return text === null ? { type: "dynamic" } : { type: "path", value: text };
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

// The exported config object: `export default <obj>`, `export default
// defineConfig(<obj>)`, or `module.exports = <obj>`. Only these are inspected, so
// a plugin's own `resolve` method is never mistaken for `resolve.alias`.
function exportedConfigObjects(source) {
  const objects = [];
  const consider = (expression) => {
    const node = unwrap(expression);
    if (!node) return;
    if (ts.isCallExpression(node) && callTargetName(node.expression) === "defineConfig") {
      consider(node.arguments && node.arguments[0]);
      return;
    }
    if (ts.isObjectLiteralExpression(node)) objects.push(node);
  };
  for (const statement of source.statements) {
    if (ts.isExportAssignment(statement)) consider(statement.expression);
    if (ts.isExpressionStatement(statement) && ts.isBinaryExpression(statement.expression)) {
      const left = statement.expression.left;
      if (
        ts.isPropertyAccessExpression(left) &&
        ts.isIdentifier(left.expression) &&
        left.expression.text === "module" &&
        left.name.text === "exports"
      ) {
        consider(statement.expression.right);
      }
    }
  }
  return objects;
}

function scanViteConfig(rel, source, ctx) {
  const lineOf = lineOfNode(source);
  for (const object of exportedConfigObjects(source)) {
    for (const property of object.properties) {
      if (ts.isSpreadAssignment(property)) {
        report(rel, lineOf(property), null, "unanalyzable spread in the exported config object (not allowlisted)");
        continue;
      }
      if (propertyNameOf(property) === "resolve") inspectResolveProperty(rel, lineOf, property, ctx);
    }
  }
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

function scanBabelConfig(rel, source, ctx) {
  const lineOf = lineOfNode(source);
  const visit = (node) => {
    if (ts.isArrayLiteralExpression(node) && node.elements.length >= 1) {
      const first = unwrap(node.elements[0]);
      if (first && ts.isStringLiteral(first) && isModuleResolverPluginName(first.text) && node.elements.length >= 2) {
        inspectBabelPluginOptions(rel, lineOf, node.elements[1], ctx);
      }
    }
    if (ts.isObjectLiteralExpression(node)) {
      let name = null;
      let options = null;
      for (const property of node.properties) {
        if (!ts.isPropertyAssignment(property)) continue;
        const initializer = unwrap(property.initializer);
        if (propertyNameOf(property) === "name" && ts.isStringLiteral(initializer)) name = initializer.text;
        if (propertyNameOf(property) === "options") options = property.initializer;
      }
      if (isModuleResolverPluginName(name) && options) inspectBabelPluginOptions(rel, lineOf, options, ctx);
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
    const ctx = createBindingContext(source);
    if (kind === "vite") scanViteConfig(rel, source, ctx);
    else if (kind === "babel") scanBabelConfig(rel, source, ctx);
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
