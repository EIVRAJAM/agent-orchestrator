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
   - `scripts/lib/privacy-scan.mjs` matches observability SDK **module
     specifiers**, so an aliased import is caught regardless of the local
     identifier. It scans `backend/**`, `frontend/**`, `packages/**` and
     `scripts/**` for `.go`, `.ts`, `.tsx`, `.js`, `.jsx`, `.mjs`, `.cjs`, and
     covers `import`, `export … from`, bare `import "x"`, dynamic `import(...)`
     and `require(...)` in TS/JS plus Go import paths (any alias). Only exact,
     reviewed files are allowlisted: the dormant implementations and their
     tests (`sentryobs.go`, `posthog.go`, `renderer/lib/telemetry.ts`,
     `packages/mobile/lib/telemetry/runtime.ts`, `packages/mobile/lib/sentry.ts`,
     and the reviewed tests). Construction/init call sites are kept as an extra
     check.
   - the dormant files still carry their fork-policy guard, and the daemon does
     not read a Sentry DSN from configuration;
   - the release target is the authorized fork repo and every consumer routes
     through the shared authority;
   - **dependency discovery**: every `package.json` under `backend/`,
     `frontend/`, `packages/`, `scripts/` and the repo root (excluding
     `node_modules`, fixtures and generated output) is checked; an observability
     dependency anywhere — including `packages/product-ui/package.json` or
     `packages/cloud-client/package.json` — fails unless it is in the reviewed
     allowlist.
2. **Guard self-tests** (`scripts/privacy-guard.test.sh`): one fixture per
   condition, each built from the real policy files with a single injection, so
   a working pattern cannot hide a broken one. Covered: direct `Sentry.init`;
   Sentry under an alias; `sentry-go` under an alias; `posthog.init` alone;
   `new PostHog` alone; dynamic `import()`; `require()`; init inside
   `packages/mobile/app/**`; import inside `packages/product-ui/**`; import
   inside `packages/cloud-client/**`; an observability dependency in
   `packages/product-ui/package.json`; one in another discovered package; mobile
   telemetry reactivation (guard removed / constant flipped); an upstream and an
   unauthorized release target. A clean tree must pass, including the dormant
   implementations and their reviewed tests.
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
  alias) anywhere in the covered tree, and the **dependency analysis** detects
  new known observability SDKs in any discovered manifest.
- **No static search is a guarantee.** A hand-written HTTP exporter with no SDK
  and no known marker, or a mechanism not yet in the provider list, is not
  detectable here; the scan showing "OK" proves nothing about absence.
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
- Guard: `scripts/privacy-guard.test.sh` passes (clean tree passes; seventeen
  single-condition reintroductions fail with the expected non-zero exit code);
  `scripts/privacy-guard.sh` returns OK end-to-end.

**Historical / comparative execution evidence (NOT the current commit):** the
controlled daemon-run traffic observation
([privacy-fork-traffic-report.md](privacy-fork-traffic-report.md)) was performed
on the **previous commit `6bfdccd2d`, not on the current `d77f30f26`**. It
remains relevant for the paths that did not change — the daemon telemetry sink
(`newTelemetrySink` local-only), the renderer/mobile init, and the daemon
env-stamping are untouched by the current commit, whose changes are the guard,
docs, mobile-Sentry early return, release target, and the Go policy
representation (the latter strictly tighter: an always-blank DSN instead of a
branch). But the **binary for `d77f30f26` has not itself been observed**, so the
run is recorded as *historical/comparative*, not as execution evidence of the
final artifact. Repeat the run on the final commit before claiming execution
evidence for it (method in the traffic report).

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
