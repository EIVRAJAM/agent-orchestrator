# Privacy fork: external telemetry and diagnostics disabled by construction

This document is the policy and maintenance record for this fork of Agent
Orchestrator (AO). It states what AO is allowed to send off the machine, how the
policy is enforced in code, how it is verified, and how to keep the fork in sync
with upstream without silently losing the policy.

> Scope note: this policy governs **AO's own** outbound telemetry and
> diagnostics. It is not an "offline app" policy: functional network traffic
> keeps working. It also does not touch the **agent harnesses' own** telemetry
> (Codex, Claude Code, Kimi, Crush, …), which is the harness vendors' code — see
> [Out of scope](#out-of-scope).

## 1. Policy

For builds distributed by this fork:

- AO never initializes or constructs a remote analytics exporter.
- AO never initializes a PostHog client (renderer, daemon, or mobile).
- AO never activates a Sentry transport or any other diagnostic upload.
- AO never sends personal or install identifiers for tracking purposes.
- No environment variable, user preference, initial consent, post-install
  setting, remote config, or domain availability can re-enable these sends.
- No legacy telemetry queue is replayed or flushed to a third party.

This is a **code property**, not a runtime setting. The daemon and renderer
composition points are local-only *by construction*; the policy constants are an
explicit assertion, not a switch. Where each side's source of truth lives:

| Side | Source of truth | Composition points |
| --- | --- | --- |
| Go daemon | `backend/internal/daemon/fork_privacy.go` (`forkExternal{Telemetry,Diagnostics}ExportAllowed` assertions; `forkPrivacyPolicyViolated` checked at boot; `newDaemonSentryConfig` always blank DSN) | `newTelemetrySink` (local-only), `Run` (fail-closed assertion, blank Sentry DSN) |
| Electron/renderer | `frontend/src/shared/fork-privacy.ts` | `rendererTelemetryEnabled` / `buildTelemetryBootstrap`, `initTelemetry`, `initMainSentry`, renderer Sentry intake, daemon env stamping (`main.ts`) |
| Desktop release target | `frontend/src/shared/release-target.ts` | `forge.config.ts` (feed + publisher), `feature-builds.ts`, `desktop-version-floor.ts` |
| Mobile | `packages/mobile/lib/fork-privacy.ts` | `telemetry/config.ts` (`MOBILE_TELEMETRY_DISABLED`), `sentry.ts` (`initMobileSentry`) |

The Go constants are deliberately **not** branched on to reach an exporter:
`newTelemetrySink` builds only a local SQLite or no-op sink, and the daemon's
Sentry DSN is never read from configuration. `daemon.Run` calls
`forkPrivacyPolicyViolated()` and refuses to start if a policy constant is ever
flipped, so a change fails closed and loudly instead of exporting silently.

## 2. Base state

- Fork remote: `EIVRAJAM/agent-orchestrator`
- Base branch/SHA: `main` @ `7dceb0ec7d2eddd4360692123a64e5bd3daf78eb`
- Working branch: `privacy/disable-external-telemetry`
- Commit chain on the branch (oldest first):
  1. `6bfdccd2d` — initial policy: daemon/renderer/mobile export disabled by
     construction, updater repointed, guard + docs. **The controlled traffic run
     was taken here** (§8).
  2. `d77f30f26` — review corrections: failing guard gates, mobile Sentry early
     return, release-target authority, Go policy coherence.
  3. `cecf213db` — import-specifier scanner, widened coverage (`packages/**`),
     per-condition guard self-tests, doc/evidence precision.
  4. `ff51dbbe5` — multiline import/require detection, package-name boundaries,
     backtick specifiers, `overrides`/`resolutions` dependency scanning.
  5. `e66d5af01` — AST-based import analysis (no false positives from
     strings/templates/comments/regex) and indirection-alias detection (npm
     `npm:` aliases, `package.json` `imports`, tsconfig `paths`, Vite
     `resolve.alias`).
  6. `66e069c77` — broadened resolution-config discovery (Vite anywhere under
     `frontend/**`/`packages/**`, Babel `module-resolver`, Metro
     `extraNodeModules`) and a manifest-scoped dependency allowlist.
  7. `cdfdfea64` — strict static classification of alias targets
     (fail-closed on any partially literal / dynamic target), shorthand/spread/
     indirection coverage, and the full `babel-plugin-module-resolver` name.
  8. `7a78c78f` — indirect Vite exports resolved (`export default <binding>`,
     `defineConfig(<binding|() => obj>)`, `module.exports`, relative re-export)
     and verified provenance for the `path`/`url` helpers and the Babel plugin
     name.
  9. `0764a0c3` — `require` provenance, helper invalidation on member mutation,
     provenance-checked `__dirname`/`__filename`, and Vite+Vitest config
     discovery.
  10. `ff604abc` — helper mutation propagated through top-level identifier
      aliases, and writes restricted to TypeScript assignment operators
      (comparisons/arithmetic no longer invalidate a binding).

## 3. Inventory of mechanisms

Classification: **ET** external telemetry · **ED** external diagnostics ·
**LL** local logs/diagnostics · **IE** internal app events ·
**FC** functional communication · **NV** needs verification.

| # | Mechanism | Class | Location | Destination | Activation (upstream) | Fork treatment |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Daemon PostHog sink | ET | `backend/internal/adapters/telemetry/posthog.go` | `AO_TELEMETRY_POSTHOG_HOST` (default `https://us.i.posthog.com`)/`capture/` | `AO_TELEMETRY_REMOTE=posthog` + key + events consent | Composition never builds it; adapter kept dormant |
| 2 | Daemon aggregate/rate-limit/denylist wrappers | IE | `adapters/telemetry/{aggregate,ratelimit,denylist}.go` | — (only shape PostHog traffic) | same as #1 | Unwired with #1 |
| 3 | Daemon Sentry | ED | `backend/internal/observe/sentryobs/sentryobs.go`, `daemon.go` | DSN host via sentry-go | `AO_SENTRY_DSN` + policy gate | DSN never read from config; `Init` stays a permanent no-op |
| 4 | Agent-switch Sentry envelope sender | ED | `observe/sentryobs/agent_switch_sender.go`, `agent_switch_event.go`, `dsn.go` | `<dsn-host>/api/<project>/envelope/` | `domain.AgentSwitchFailureProductionEnabled` (already `false`) + durable consent + destination | `ParseAgentSwitchDSN("")` ⇒ unbuildable |
| 5 | Agent-switch outbox/dispatcher/policy | IE | `observe/agentswitch/` | local SQLite outbox | observer present | Observer never present; dispatcher stays unbuilt |
| 6 | Local SQLite telemetry events | LL | `adapters/telemetry/localsqlite.go` → `telemetry_event` table (30-day retention) | local disk only | events enabled | **Kept** (local diagnostics) |
| 7 | Renderer PostHog client | ET | `frontend/src/renderer/lib/telemetry.ts` | `https://us.i.posthog.com` via `posthog-js` | packaged build + bootstrap | `initTelemetry` returns false; bootstrap always null |
| 8 | Renderer→main Sentry intake | ED | `renderer/lib/sentry.ts`, `main/sentry-main.ts` | none (no transport) | production gate (already `false`) | Guards forced closed; `initMainSentry` always null |
| 9 | Agent-switch visibility envelope | ED | `main/agent-switch-observability.ts` | Sentry envelope (never wired) | production gate | Shipping composition passes `send: async () => undefined` |
| 10 | Desktop consent controller / policy file | IE | `main/desktop-telemetry-controller.ts`, `main/telemetry-policy-file.ts` | local `telemetry_policy.json` | user toggle | Kept; capture path inert |
| 11 | Install/distinct id | LL | `shared/telemetry.ts` `loadOrCreateTelemetryInstallId` | local file | bootstrap | Never generated for export (bootstrap always null) |
| 12 | Daemon env stamping from main | ET/ED | `frontend/src/main.ts` `telemetryOverrides()` | daemon process env | always | Remote pinned `off`; no PostHog key/host; no Sentry DSN |
| 13 | Internal event streams (`ao.http.5xx`, `ao.daemon.panic`, `ao.session.spawned`, `ao.app.active`, …) | IE | `httpd/`, `service/`, `daemon.go` | local sink / SSE | normal operation | **Kept**; recorded locally, never exported |
| 14 | Telemetry policy control endpoints | FC | `httpd/router.go` (`/internal/agent-switch-observability/*`) | loopback daemon | desktop → daemon | **Kept** (loopback only) |
| 15 | Loopback daemon API, CDC/SSE, terminal mux | FC | `httpd/`, `storage/sqlite`, `terminal/` | `127.0.0.1` | normal operation | **Kept** |
| 16 | GitHub/GitLab SCM + tracker intake | FC | `adapters/scm/*`, `adapters/tracker/*` | user's own provider | configured creds | **Kept** |
| 17 | Cloud control plane | FC | `config.go` (`https://api.aoagents.dev`) | AO cloud (hosted product) | cloud features | Unchanged (out of scope) |
| 18 | Expo push | FC | `push/expo.go` | `exp.host` | push enabled | Unchanged |
| 19 | Pricing catalog fetch | FC | `pricing/fetch.go` | `raw.githubusercontent.com` | usage pricing | Unchanged (no user data; residual risk) |
| 20 | Update feed (electron-updater) | FC | `frontend/forge.config.ts`, `main/auto-updater.ts` | GitHub releases | packaged build | Feed built from the authorized release target only |
| 21 | Desktop version floor | FC | `frontend/src/main/desktop-version-floor.ts`, `desktop-version-floor.json` | `raw.githubusercontent.com` (fork repo) | packaged build | Fetched from the fork; download URL restricted to the authorized repo |
| 22 | Mobile PostHog client | ET | `packages/mobile/lib/telemetry/runtime.ts` | `https://us.i.posthog.com` via `posthog-react-native` | packaged + `EXPO_PUBLIC_AO_TELEMETRY_DISABLED != "1"` | Disabled constant; no client |
| 23 | Mobile Sentry | ED | `packages/mobile/lib/sentry.ts` | separate Sentry project | `EXPO_PUBLIC_SENTRY_DSN` | `initMobileSentry` returns before DSN/SDK/init |
| 24 | Cloud (`cloud/`) | — | `cloud/` | AWS CloudWatch (server-side) | hosted product | Out of scope: separate module, no client SDK |
| 25 | `@sentry/electron` / `posthog-js` / `posthog-react-native` deps | NV | `package.json` files | none unless wired | never imported for export | Kept and allowlisted; see residual risk |
| 26 | Electron native crash reporter / minidumps | — | — | — | never started | None found; crash dumps only reparented under `~/.ao` |
| 27 | CI privacy guard | — | `scripts/privacy-guard.sh`, `.github/workflows/privacy-guard.yml` | — | on push/PR | **New**: fails on reintroduction |

Evidence: this inventory comes from reading the code (file references in the
table) plus the test suite and a controlled daemon run (§7). Where the code and a
prior note disagreed, the code wins.

## 4. What "external" means here, and what is preserved

Preserved on purpose (they are **not** telemetry):

- **Local logs and local diagnostics**, including the SQLite `telemetry_event`
  table (`LL`). These never leave the machine.
- **Internal application events, hooks, protocol messages, and CDC events**
  (`IE`). Internal names and shared interfaces are not, by themselves, evidence
  of external export; the daemon still records `ao.*` events locally and the
  SSE/CDC pipeline still delivers session state.
- **Functional communication** (`FC`): the loopback daemon API, WebSocket
  terminal mux, SCM/tracker clients, cloud control plane, Expo push, and update
  feeds. The fork blocks AO's tracking and error-reporting export, not the
  product's function.

### Out of scope

- **Agent-harness telemetry.** Codex, Claude Code, Kimi, Crush and similar
  harness binaries have their own telemetry configured by their vendors. This
  fork does not disable it from AO's code. Some harnesses expose their own
  opt-out (`cloud/internal/workerexec/command.go` shows the pattern);
  configuring those is a per-harness operator decision.
- **The hosted cloud product** (`cloud/`): a separate Go module with server-side
  CloudWatch monitoring only, no AO client SDK.
- **Mobile**: treated in-repo and disabled in code; its runtime is not validated
  by this change (§8).

## 5. Implementation

Backend (Go):

- `backend/internal/daemon/fork_privacy.go`: policy constants, the
  `forkPrivacyPolicyViolated()` boot assertion, and `newDaemonSentryConfig`
  (always-blank DSN, never read from config).
- `backend/internal/daemon/telemetry_wiring.go`: `newTelemetrySink` returns only
  a local SQLite sink (events enabled) or `NoopSink`; the PostHog chain and the
  `aggregatedEventNames` list it needed are gone.
- `backend/internal/daemon/daemon.go`: fail-closed `forkPrivacyPolicyViolated()`
  check at startup; `ParseAgentSwitchDSN("")`; `sentryobs.Init(newDaemonSentryConfig(...))`.
- Tests: `fork_privacy_test.go`, `telemetry_wiring_test.go`.

Frontend:

- `frontend/src/shared/fork-privacy.ts`: policy constants.
- `frontend/src/shared/telemetry.ts`: `rendererTelemetryEnabled` permanently
  false ⇒ `buildTelemetryBootstrap` always null.
- `frontend/src/renderer/lib/telemetry.ts`: `initTelemetry` returns before
  `posthog.init`.
- `frontend/src/main/sentry-main.ts`, `frontend/src/renderer/lib/sentry.ts`:
  intake and transport forced closed.
- `frontend/src/main.ts`: `telemetryOverrides()` pins `AO_TELEMETRY_REMOTE=off`
  and stamps no PostHog key/host or Sentry DSN.
- `frontend/vite.renderer.config.ts`: PostHog origins removed from the renderer
  CSP.
- `frontend/src/shared/release-target.ts` (**new**): the single authorized
  release-target authority (see §6).
- `frontend/forge.config.ts`, `frontend/src/main/feature-builds.ts`,
  `frontend/src/main/desktop-version-floor.ts`: route through the authority.
- Tests: `privacy-policy.test.ts`, `telemetry.test.ts`, `release-target.test.ts`,
  `telemetry.init.test.ts`, `sentry-main.test.ts`, `desktop-version-floor.test.ts`.

Mobile:

- `packages/mobile/lib/fork-privacy.ts` (**new**): the mobile policy constants.
- `packages/mobile/lib/telemetry/config.ts`:
  `MOBILE_TELEMETRY_DISABLED = !MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED`.
- `packages/mobile/lib/sentry.ts`: `initMobileSentry` returns before reading the
  DSN, loading the SDK, or calling `mod.init`; `options.loadSdk` is a test seam.
- Tests: `packages/mobile/lib/sentry.test.ts`,
  `packages/mobile/lib/telemetry/fork-privacy.test.ts`.

Guard and docs:

- `scripts/privacy-guard.sh` (hardened, see §7), `scripts/lib/privacy-scan.mjs`
  (the import/call/dependency scanner), `scripts/privacy-guard.test.sh`
  (self-tests), `.github/workflows/privacy-guard.yml`.
- `docs/privacy-fork.md` (this file), `docs/privacy-fork-traffic-report.md`.

No API contract, storage schema, or generated artifact changed. No new
dependency was added.

### Deliberately kept, and why

The PostHog adapter, the Sentry code, and the `posthog-js` /
`posthog-react-native` / `@sentry/electron` dependencies are retained but
**unreachable from any composition point**. Removing them wholesale would be a
large, merge-hostile diff; the fork policy instead neutralizes the composition
points, keeps shared interfaces, and catches a reintroduction in CI. The
residual risk is a future code change flipping a policy constant or rewiring a
composition point, which the guard is designed to fail on.

## 6. Updates and the release target

**Release target.** Upstream baked the feed from `AO_RELEASE_REPO` (default
`Untrivial-ai/agent-orchestrator`), so a fork build could poll and silently
stage an upstream artifact. The fork routes everything through
`frontend/src/shared/release-target.ts`:

- `AUTHORIZED_RELEASE_REPOS` is the explicit, reviewed allowlist (currently
  `EIVRAJAM/agent-orchestrator`). A legitimate rename is made here, deliberately.
- `UPSTREAM_RELEASE_REPOS` lists known upstream homes, rejected explicitly.
- A blank `AO_RELEASE_REPO` falls back to the authorized fork; a malformed,
  upstream, or any other unauthorized value **throws**, so a build fails instead
  of honoring it.
- `forge.config.ts` builds the baked `app-update.yml` from this authority and
  **re-reads and re-validates the generated file** in `prePackage`, so the check
  is on the artifact, not only the source. `feature-builds.ts` and
  `desktop-version-floor.ts` derive from the same authority, so updater, feature
  list, and floor always agree; the floor's download URL is also restricted to
  authorized repos.

**Manual update procedure** (until fork release infrastructure exists):

1. Build from the desired fork commit (`frontend/npm ci` then `npm run make`),
   or download a fork-built artifact from the fork's releases.
2. Install over the existing app. Do not install an upstream artifact; the
   updater will not offer one and the version floor will not point at one.
3. Record the source commit and run `bash scripts/privacy-guard.sh` on it.

**Mobile / Cloud.** Mobile initializations are disabled in code but its runtime
is not validated here (no Expo build). Cloud is a separate module and out of
scope.

## 7. Verification

`bash scripts/privacy-guard.sh` (also `npm run privacy:guard`), run locally and
by `.github/workflows/privacy-guard.yml`. It has three layers:

1. **Failing static gates** (not warnings):
   - policy constants closed (Go, frontend, mobile);
   - `scripts/lib/privacy-scan.mjs` uses the repository's own TypeScript
     compiler (`frontend/node_modules/typescript`) to parse each covered JS/TS
     file and reads only real import positions, so ordinary strings, template
     literals, comments and regex literals containing an SDK name are **not**
     imports. It catches `import … from`, `export … from`, bare `import "x"`,
     dynamic `import(...)` (including a backtick-delimited specifier), `require()`
     and TS `import = require`, across lines, under any local identifier. It scans
     `backend/**`, `frontend/**`, `packages/**` and `scripts/**` for `.go`, `.ts`,
     `.tsx`, `.js`, `.jsx`, `.mjs`, `.cjs`; Go imports are matched by path (any
     alias). Provider matching uses package-name boundaries — the exact unscoped
     name or a `/subpath`, or a `@scope/` — so an unrelated name that only shares
     a prefix (`posthog-analytics`) is not a false positive, while real subpaths
     (`posthog-js/dist/...`) and scopes (`@sentry/*`) still match. Only exact,
     reviewed files are allowlisted: the dormant implementations and their
     tests (`sentryobs.go`, `posthog.go`, `renderer/lib/telemetry.ts`,
     `packages/mobile/lib/telemetry/runtime.ts`, `packages/mobile/lib/sentry.ts`,
     and the reviewed tests). Construction/init call sites are kept as an extra
     check.
   - the dormant files still carry their fork-policy guard, and the daemon does
     not read a Sentry DSN from configuration;
   - the release target is the authorized fork repo and every consumer routes
     through the shared authority;
   - **dependency and alias discovery**: every `package.json` under `backend/`,
     `frontend/`, `packages/`, `scripts/` and the repo root (excluding
     `node_modules`, fixtures and generated output) is checked for observability
     packages by both name and **alias target**: npm aliases
     (`"reporter": "npm:@sentry/electron@^7.0.0"`), `resolutions`/`overrides`/
     `pnpm.overrides`, and the `package.json` `imports` map
     (`"#reporter": "@sentry/electron"`). The allowlist is **manifest-scoped**:
     `frontend/package.json` may declare `@sentry/electron` and `posthog-js`,
     `packages/mobile/package.json` may declare `posthog-react-native`, and a
     dormant dependency, self-alias, override or resolution in any other manifest
     fails (so a dormant dep cannot be added to a new package unreviewed, and a
     self-alias is only acceptable inside its authorized manifest). An alias that
     renames an observability package fails everywhere because it hides the
     import site.
   - **resolution configs**: discovered under `frontend/**` and `packages/**` —
     Vite and Vitest (`^(?:vite|vitest)(\.[^/\\]+)?\.config.{ts,js,mts,mjs,cts,cjs}`),
     Babel (`babel.config.*`) and Metro (`metro.config.*`) — and inspected through
     the AST (not every string): `resolve.alias` in object and
     `{find, replacement}` array form; Babel `module-resolver` (`module-resolver`,
     `babel-plugin-module-resolver`, scoped variants); Metro
     `resolver.extraNodeModules`. Shorthand properties, spreads, and
     `resolve`/`alias`/`extraNodeModules` stored in a variable are handled:
     resolved when a top-level `const` binding is fully static, rejected
     fail-closed otherwise. tsconfig values are checked too.
   - **indirect Vite exports**: the exported config is resolved through a static
     `const` binding (`export default config`), `defineConfig(<obj | binding |
     () => obj>)`, `module.exports = <obj | () => obj>`, and a relative
     `export { default } from "./…"` re-export (followed into the target file;
     this is how `frontend/vitest.config.ts` re-exports the renderer config). A
     branch, an arbitrary call, a mutation, a missing/cyclic re-export, a
     non-relative re-export, or anything not fully determinable is rejected
     fail-closed, and a file with no analyzable export is reported — never a
     silent empty list. `defineConfig` itself is matched by name (documented).
   - **helper provenance**: `path.resolve`/`path.join`/`path.posix.*` and
     `fileURLToPath`/`url.fileURLToPath` are treated as safe only when the callee
     resolves, through the file's own imports/requires, to `node:path`/`path` or
     `node:url`/`url` (default, namespace, named `as`, and
     `const { resolve } = require("node:path")` forms), **and `require` is the
     implicit CommonJS global** (a local declaration, function, parameter,
     reassignment or import of `require` disables require-based trust for the
     whole file). A local object/function with the same name, a reassigned or
     locally shadowed binding, a helper received as a parameter, an import from
     another package, or an alias (`const resolve = path.resolve`) with any
     non-static argument is rejected fail-closed.
   - **mutation, and its propagation through aliases**: a write to a helper or one
     of its members invalidates the whole helper. Writes are exactly the
     TypeScript assignment operators (`=`, `+=`, `-=`, `*=`, `/=`, `%=`, `**=`,
     `<<=`, `>>=`, `>>>=`, `&=`, `^=`, `|=`, `&&=`, `||=`, `??=`), plus `++`/`--`
     and `delete`; comparisons and arithmetic/bitwise expressions are **not**
     writes. A write to `path.resolve`, `path.posix.resolve`, `path["resolve"]`,
     a compound assignment, `++`, `delete`, or an object-level mutator
     (`Object.assign`/`Object.defineProperty`/`Reflect.set`) on the helper
     invalidates it, **and** the invalidation propagates to every top-level
     identifier alias that shares the object (`const p = path; p.resolve = fake;`
     invalidates `path`), including alias chains and cycles. Unmutated aliases
     stay trusted (`const p = path; p.resolve(__dirname, "src")` works). `new URL`
     is trusted only as the un-shadowed global or a `node:url`/`url` import.
   - **`__dirname` / `__filename`**: treated as an implicit base only when not
     declared, shadowed or reassigned; a static local `const` is resolved and
     analysed (a `const __dirname = "node_modules/@sentry"` is caught), while a
     dynamic or mutable one fails closed. `import.meta.url` remains an intrinsic,
     non-reassignable base.
   - **strict alias-target classification** (fail-closed): a target is accepted
     only when its value is **fully determined** — a string / no-substitution
     template literal, or a recognized local construction (`path.resolve`/
     `path.join` whose arguments are all static or known local bases such as
     `__dirname`, and `fileURLToPath(new URL(<static>, …))`). The mere presence of
     a benign literal inside an otherwise dynamic expression is **not** enough: a
     call, an identifier bound to a dynamic value, a concatenation, an
     interpolated template, a property access, `process.env`, a spread, or a
     destructured/reassigned binding are all rejected. A resolved path that names
     an observability package as a path segment (`node_modules/@sentry/electron`,
     `node_modules/posthog-js`) also fails. The Babel plugin name is resolved
     through static string bindings the same way; a reassigned/shadowed name
     binding fails closed, while an unknown dynamic plugin name is left alone
     (documented limit).
   - **deliberate exception**: Metro `resolver.resolveRequest` is a function, so
     only observability literals inside it are flagged; a **fully computed**
     module name there is not detected (documented gap).
2. **Guard self-tests** (`scripts/privacy-guard.test.sh`): one fixture per
   condition, each built from the real policy files with a single injection, so
   a working pattern cannot hide a broken one. Covered: direct `Sentry.init`;
   Sentry under an alias; `sentry-go` under an alias; `posthog.init` alone;
   `new PostHog` alone; dynamic `import()`; `require()`; **multiline** static
   import, `export … from`, `import(...)`, and `require(...)`; a multiline
   aliased import with an indirect call; an allowed import at an exact allowlist
   location (passes) versus the same import elsewhere (fails); init inside
   `packages/mobile/app/**`; import inside `packages/product-ui/**`; import
   inside `packages/cloud-client/**`; an observability dependency in
   `packages/product-ui/package.json`; one in another discovered package; a real
   SDK dependency (fails) versus prefix-only names (`posthog-analytics`,
   `segment-analytics`, `@sentryfoo/thing`) that must **not** false-positive; a
   legitimate subpath import outside the allowlist (fails); a backtick
   `import(...)`; an observability package pinned via `overrides` or a
   `resolutions` selector (fails); **npm aliases** (`npm:posthog-js`,
   `npm:@sentry/electron`) in `dependencies`/`devDependencies`/`overrides`/
   `resolutions`/`pnpm.overrides` (fail); the `package.json` `imports` map,
   including a conditional value, and an alias import followed by an
   arbitrary-identifier call (fail); a tsconfig `paths` and a Vite
   `resolve.alias` pointing at an SDK (fail); a non-SDK npm alias and a
   self-alias of an allowlisted dependency (pass); **resolution configs** —
   Vite `resolve.alias` in `packages/product-ui` (object), a conventional
   `vite.config.ts`, a config outside `frontend/`, and the
   `{ find, replacement }` array form (fail); a Vite config whose SDK string is
   not an alias and a benign Vite alias (pass); a dynamic Vite alias (fail,
   fail-closed); Babel `module-resolver` (short and `babel-plugin-` full name) and
   Metro `extraNodeModules` aliases and a Metro `resolveRequest` naming an SDK
   literally (fail); a Metro resolver with a computed name (pass — documented
   gap); the **strict static classification** cases — a call with a benign literal
   plus a dynamic identifier, a template literal with interpolation, and a
   partially dynamic concatenation (fail); a local `path.resolve()` and the
   repository's `fileURLToPath(new URL(…))` (pass) versus the same into
   `node_modules/@sentry/electron` (fail); a literal benign target (pass) versus a
   literal observability target (fail); **shorthand/spread/indirection** — a
   resolvable Vite shorthand and an indirect resolvable `resolve` (pass) versus a
   dynamic shorthand (Vite/Babel/Metro), a spread inside `resolve`/`alias`, and an
   indirect unresolvable `resolve` (fail); **indirect Vite exports** — `export
   default <const binding>`, `defineConfig(<binding>)`, `module.exports =
   <binding>`, `defineConfig(() => ({…}))` and `module.exports = () => ({…})`
   with an observability alias (fail) versus an arrow with a fully static benign
   return and an indirect static benign config (pass), plus a dynamic/unresolvable
   export (fail); **helper provenance** — a local `path` object or
   `fileURLToPath` function, an alias of `path.resolve` with a dynamic argument, a
   reassigned or shadowed helper, a helper received as a parameter, and a
   same-named import from another package (fail) versus `import * as path from
   "node:path"`, `const path = require("node:path")` and
   `const { resolve: pathResolve } = require("node:path")` (pass); **Babel plugin
   name via bindings** — short, full and scoped `module-resolver` names via a
   binding and a reassigned binding (fail) versus a benign plugin name via a
   binding (pass); **require provenance** — a local `const`/`function require`,
   a `require` parameter and a reassigned `require` (fail) versus the implicit
   `require` (pass); **helper member mutations** — assignment to `resolve`,
   `join`, `posix.resolve`, `posix.join`, `fileURLToPath`, `URL`, bracket
   notation, compound assignment, `++` and `Object.assign(path, …)` (fail) versus
   the same helpers left unmodified (pass); **alias mutation propagation** —
   mutating an alias (`const p = path; p.resolve = fake`), an alias chain, or an
   alias via bracket/compound/`++`/`delete`/`Object.assign`/`Object.defineProperty`
   invalidates the original helper (fail) versus unmutated ESM/require namespace
   aliases (pass); **assignment operators** — `-=`, `||=`, `??=` and `|=` are
   writes (fail) while comparisons and arithmetic/bitwise expressions are not
   (pass); **`__dirname`/`__filename`** — the
   implicit bases and `import.meta.url` (pass) versus a local const pointing at
   an SDK, a mutable `let`, and a parameter (fail), plus a benign static local
   const (pass); **relative re-exports and Vitest** — a Vitest config re-exporting
   a config with an observability alias, a missing re-export, a cycle, a
   non-relative re-export, and a direct observability alias in a discovered
   `vitest.config.ts` (fail) versus a benign relative re-export (pass); the
   **manifest-scoped dependency**
   cases — each dormant dependency in its own manifest (pass), a dormant
   dependency in another manifest via `dependencies`/`devDependencies`/
   `optionalDependencies`/`peerDependencies`, a self-alias outside its manifest,
   an override alias in a foreign manifest, and a new observability package in an
   authorized manifest (fail); and the benign-literal set — SDK names inside
   ordinary strings, template-literal examples, comments, regex literals, the
   plain word `from`, and mixed inner delimiters — which must **not**
   false-positive. A clean tree must pass, including the dormant implementations
   and their reviewed tests.
3. **Behavioral tests** for the known composition points: daemon composition (no
   remote sink even when configured against a live `httptest` server; Sentry
   never enabled with a DSN present), renderer/main policy, release-target
   resolution (absent/correct/upstream/malformed/unauthorized), and mobile (a
   valid DSN + an injected SDK + production env still never builds PostHog or
   calls `Sentry.init`).

The guard **refuses to run in CI** if any `PRIVACY_GUARD_SKIP_*` or
`PRIVACY_GUARD_STATIC_ONLY` variable is set, so a workflow cannot skip a
mandatory part.

### What this does and does not cover

- The behavioral tests protect the **known** composition points. They cannot
  vouch for a new module that those tests never import.
- The **import allowlist** detects new uses of the reviewed SDKs (under any
  alias) anywhere in the covered tree; the **dependency/alias analysis** detects
  new known observability SDKs, npm aliases, `imports` maps and
  tsconfig/Vite/Babel/Metro alias targets in any discovered manifest or config,
  with a manifest-scoped dependency allowlist.
- **No static search is a guarantee.** A hand-written HTTP exporter with no SDK
  and no known marker, a module specifier computed at runtime
  (`require(name)`), a Go raw-string/URL import, a **Metro `resolver.resolveRequest`
  that computes the module name without naming an SDK literally**, a
  **`defineConfig`-like helper faked under that name**, a **Babel plugin whose
  name is computed at runtime**, **a re-export from a non-relative package**
  (rejected), or a resolver alias the scanner does not know are not detectable
  here; the scan showing "OK" proves nothing about absence. An alias **target or
  map** that is not fully static — including a partially literal one, an
  interpolated template, a concatenation, a dynamic binding, a shorthand, or a
  spread — is *not* silently accepted: it is rejected fail-closed because it
  cannot be proven safe. The same applies to a dynamic Vite export, to a
  `path`/`url` helper whose provenance is not a real `node:path`/`node:url`
  binding (or whose members are mutated), and to a `__dirname`/`__filename`
  rebound to a non-static value. Mutation invalidation propagates through
  **top-level identifier aliases** and through the binding table for member access
  (`const p = path`), so a mutated alias invalidates the original helper
  transitively (chains and cycles included). A mutation the scanner cannot
  attribute to a helper — a target computed at runtime, a member reachable only
  through a value the scanner cannot resolve (e.g. an element access with a
  non-static key, or a `let`/destructured alias whose object identity is unclear)
  — is not detected; such a case is rejected fail-closed when it appears as an
  alias target, but a mutation hidden behind it is not. The deliberate exception
  is Metro `resolver.resolveRequest`, a function the repo already uses, where a
  fully computed module name cannot be rejected without breaking it.
- Upstream merges therefore still require **code review** and, where warranted,
  **traffic inspection**. A reintroduction that avoids the static patterns is
  not automatically caught by the current tests.

The CI workflow runs when anything that can change policy, packaging, the
release target, or dependencies changes: `backend/**`, `frontend/**`,
`packages/**`, `contracts/**`, `desktop-version-floor.json`, root
`package.json`/`package-lock.json`, `go.work`/`go.work.sum`, `scripts/**`, and
`.github/workflows/**` (which includes `build-artifacts.yml`).

## 8. Status: implemented, verified, pending

**Implemented:** the policy and composition changes in §5, the release-target
authority in §6, and the guard/scanner/self-tests/workflow in §7.

**Verified by automated tests on the current commit** (all green in this
environment):

- Go: `go build ./...`; `gofmt`/`go vet` clean; `golangci-lint v2.13.2` on the
  touched packages reports 0 issues; `go test ./internal/daemon/...` passes;
  the privacy tests assert the local-only sink, the blank Sentry DSN, and the
  closed policy assertion.
- Frontend: `npm run typecheck` passes; the privacy/release-target tests pass.
- Mobile: `npm run typecheck` passes; the mobile privacy tests pass.
- Guard: `scripts/privacy-guard.test.sh` passes (clean tree passes; the
  single-condition reintroductions — including the multiline, alias, and
  package-boundary cases — fail with the expected non-zero exit code);
  `scripts/privacy-guard.sh` returns OK end-to-end.

**Historical / comparative execution evidence (NOT the current HEAD):** the
controlled daemon-run traffic observation
([privacy-fork-traffic-report.md](privacy-fork-traffic-report.md)) was performed
on commit **`6bfdccd2d`**. The branch has since moved through a series of
hardening commits — `d77f30f26` (guard gates, mobile Sentry early return,
release-target authority, Go policy coherence), `cecf213db` (import-specifier
scanner, widened coverage, per-condition self-tests), `ff51dbbe5` (multiline
import/require detection, package-name boundaries, backtick, `overrides`/
`resolutions`), `e66d5af01` (AST-based import analysis and indirection-alias
detection), `66e069c77` (broadened Vite/Babel/Metro resolution-config discovery
and a manifest-scoped dependency allowlist), `cdfdfea64` (strict
fail-closed classification of alias targets, shorthand/spread/indirection),
`7a78c78f` (indirect Vite-export resolution and verified `path`/`url` helper
provenance), `0764a0c3` (`require` provenance, helper invalidation on member
mutation, `__dirname`/`__filename`, Vite+Vitest discovery), and `ff604abc`
(helper mutation propagation through identifier aliases; writes restricted to
assignment operators). None of those commits
change the daemon
outbound paths the run exercised (`newTelemetrySink` local-only, renderer/mobile
init, daemon env-stamping), and the Go Sentry change made the DSN path strictly
tighter (always blank), so the run remains relevant as **historical /
comparative** evidence for those paths.

There is **no full traffic capture of the final HEAD**, and the binary built from
the final commit has **not been observed**. Do not read the historical run as
dynamic validation of the final artifact; repeat it on the final commit (method
in the traffic report) before claiming execution evidence for it.

**Pending / not verifiable in this environment:**

- Full system/desktop traffic capture. Requires a supported desktop OS with a
  display; this environment has no Xvfb, `tcpdump`, or `strace`. The daemon
  observation is destination-canary + socket inspection, not a full capture.
- Re-run of the controlled daemon traffic test on the **final** commit.
- Real Electron desktop app run (launch, create/stop/restore a session, create
  and clean up worktrees, reconnect, packaged-equivalent and dev).
- Real launches of Codex and OpenCode; GitHub / PR / CI flows.
- Packaged artifact build and install, and effective updater behavior
  (download/install from the fork release target).
- Mobile app runtime (code-level disable only; no Expo build).
- Cloud: out of scope, not exercised.

**Comparative limitation (pre-existing, not a pass):** the full frontend suite
(`vitest run`) has a reproducible failure in `ContextMeter.test.tsx` and the full
mobile suite in `lib/chat/conversationChrome.test.ts` (a locale/`Intl` formatting
difference in this container). Both fail **identically on the base SHA**
`7dceb0ec7` in an isolated worktree, and neither is in a file this change
touches. They are recorded as environment/locale limitations, not as verified
passes.

## 9. Syncing with upstream

1. Fetch upstream and merge/rebase onto `main`.
2. Run `bash scripts/privacy-guard.test.sh` and `bash scripts/privacy-guard.sh`.
3. Review every conflict or automated change in the policy/composition files
   listed in §5, plus `frontend/src/shared/release-target.ts`,
   `frontend/forge.config.ts`, `frontend/src/main/feature-builds.ts`,
   `frontend/src/main/desktop-version-floor.ts`, `desktop-version-floor.json`,
   and `packages/mobile/lib/{fork-privacy.ts,sentry.ts,telemetry/config.ts}`.
   Upstream changes in observability, consent, or the updater can silently
   change this policy.
4. If upstream adds a new observability dependency or init site, the guard fails;
   decide deliberately whether it is a kept-dormant adapter or a removed surface,
   and update this document and the allowlist.
5. Regenerate generated artifacts only if the merge changed their sources
   (`npm run sqlc`, `npm run api`); this policy change required none.
6. Re-run the relevant full suites the repository requires before sharing a PR.

## 10. Building and installing the fork

Backend/CLI:

```bash
cd backend
go build ./...
go test ./...
```

Desktop (development against lab data — never the real `~/.ao`):

```bash
cd frontend
npm ci
npm run build:daemon -- --dev
AO_DATA_DIR=/tmp/ao-privacy-lab ./node_modules/.bin/electron-forge start
```

Packaged build:

```bash
cd frontend
npm ci
AO_RELEASE_REPO=EIVRAJAM/agent-orchestrator npm run make
```

Distribute only artifacts built from this fork. Do not publish releases as part
of a privacy change (see `frontend/docs/desktop-release.md`, "Hard rule: exactly
one publisher").
