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

This is a **code property**, not a runtime setting. Each side has one
declared source of truth and the composition points consult it:

| Side | Source of truth | Composition points that consult it |
| --- | --- | --- |
| Go daemon | `backend/internal/daemon/fork_privacy.go` | `newTelemetrySink` (`telemetry_wiring.go`), `daemonSentryDSN` / `newDaemonSentryConfig` (`daemon.go`) |
| Electron/renderer | `frontend/src/shared/fork-privacy.ts` | `rendererTelemetryEnabled` / `buildTelemetryBootstrap` (`shared/telemetry.ts`), `initTelemetry` (`renderer/lib/telemetry.ts`), `initMainSentry` (`main/sentry-main.ts`), renderer Sentry intake (`renderer/lib/sentry.ts`), daemon env stamping (`main.ts`) |
| Mobile | `packages/mobile/lib/telemetry/config.ts` | `initMobileTelemetry` (`packages/mobile/lib/telemetry/runtime.ts`) |

## 2. Base state

- Fork remote: `EIVRAJAM/agent-orchestrator`
- Base branch/SHA at the start of this work: `main` @ `7dceb0ec7d2eddd4360692123a64e5bd3daf78eb`
- Working branch: `privacy/disable-external-telemetry`
- Working tree at start: clean

## 3. Inventory of mechanisms

Classification: **ET** external telemetry · **ED** external diagnostics ·
**LL** local logs/diagnostics · **IE** internal app events ·
**FC** functional communication · **NV** needs verification.

| # | Mechanism | Class | Location | Destination | Activation (upstream) | Fork treatment |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Daemon PostHog sink | ET | `backend/internal/adapters/telemetry/posthog.go`, wired in `daemon/telemetry_wiring.go` | `AO_TELEMETRY_POSTHOG_HOST` (default `https://us.i.posthog.com`)/`capture/` | `AO_TELEMETRY_REMOTE=posthog` + key + events consent | Composition no longer builds it; adapter kept dormant |
| 2 | Daemon aggregate/rate-limit/denylist wrappers | IE | `adapters/telemetry/{aggregate,ratelimit,denylist}.go` | — (only shape PostHog traffic) | same as #1 | Unwired with #1 |
| 3 | Daemon Sentry | ED | `backend/internal/observe/sentryobs/sentryobs.go`, `daemon.go` | DSN host via sentry-go | `AO_SENTRY_DSN` + policy gate | DSN forced blank; `Init` stays a no-op |
| 4 | Agent-switch Sentry envelope sender | ED | `observe/sentryobs/agent_switch_sender.go`, `agent_switch_event.go`, `dsn.go` | `<dsn-host>/api/<project>/envelope/` | `domain.AgentSwitchFailureProductionEnabled` (already `false`) + durable consent + destination | DSN blank ⇒ destination unparsable ⇒ sender never built |
| 5 | Agent-switch outbox/dispatcher/policy | IE | `observe/agentswitch/` | local SQLite outbox | observer present | Observer never present; dispatcher stays unbuilt |
| 6 | Local SQLite telemetry events | LL | `adapters/telemetry/localsqlite.go` → `telemetry_events` table (30-day retention) | local disk only | events enabled | **Kept** (local diagnostics) |
| 7 | Renderer PostHog client | ET | `frontend/src/renderer/lib/telemetry.ts` | `https://us.i.posthog.com` via `posthog-js` | packaged build + bootstrap | `initTelemetry` returns false; bootstrap always null |
| 8 | Renderer→main Sentry intake | ED | `renderer/lib/sentry.ts`, `main/sentry-main.ts` | none (no transport) | `AGENT_SWITCH_FAILURE_PRODUCTION_ENABLED` (already `false`) | Guards forced closed; `initMainSentry` always null |
| 9 | Agent-switch visibility envelope | ED | `main/agent-switch-observability.ts` | Sentry envelope (never wired) | production gate | Shipping composition passes `send: async () => undefined` |
| 10 | Desktop consent controller / policy file | IE | `main/desktop-telemetry-controller.ts`, `main/telemetry-policy-file.ts` | local `telemetry_policy.json` | user toggle | Kept; capture path inert |
| 11 | Install/distinct id | LL | `shared/telemetry.ts` `loadOrCreateTelemetryInstallId` | local file | bootstrap | Never generated for export (bootstrap always null) |
| 12 | Daemon env stamping from main | ET/ED | `frontend/src/main.ts` `telemetryOverrides()` | daemon process env | always | Remote pinned `off`; no PostHog key/host; no Sentry DSN |
| 13 | Internal event streams (`ao.http.5xx`, `ao.daemon.panic`, `ao.session.spawned`, `ao.app.active`, …) | IE | `httpd/`, `service/`, `daemon.go` | local sink / SSE | normal operation | **Kept**; recorded locally, never exported |
| 14 | Telemetry policy control endpoints | FC | `httpd/router.go` (`/internal/agent-switch-observability/*`) | loopback daemon | desktop → daemon | **Kept** (loopback only) |
| 15 | Loopback daemon API, CDC/SSE, terminal mux | FC | `httpd/`, `storage/sqlite`, `terminal/` | `127.0.0.1` | normal operation | **Kept** |
| 16 | GitHub/GitLab SCM + tracker intake | FC | `adapters/scm/*`, `adapters/tracker/*` | user's own provider | configured creds | **Kept** |
| 17 | Cloud control plane | FC | `config.go` (`https://api.aoagents.dev`) | AO cloud (hosted product) | cloud features (out of scope in this fork) | Unchanged |
| 18 | Expo push | FC | `push/expo.go` | `exp.host` | push enabled | Unchanged |
| 19 | Pricing catalog fetch | FC | `pricing/fetch.go` | `raw.githubusercontent.com/Untrivial-ai/...` | usage pricing | Unchanged (no user data; see residual risk) |
| 20 | Update feed (electron-updater) | FC | `frontend/forge.config.ts`, `main/auto-updater.ts` | GitHub releases | packaged build | Repointed to the fork repo |
| 21 | Desktop version floor | FC | `frontend/src/main/desktop-version-floor.ts`, `desktop-version-floor.json` | `raw.githubusercontent.com` | packaged build | Repointed to the fork repo |
| 22 | Mobile PostHog client | ET | `packages/mobile/lib/telemetry/runtime.ts` | `https://us.i.posthog.com` via `posthog-react-native` | packaged + `EXPO_PUBLIC_AO_TELEMETRY_DISABLED != "1"` | Disabled constant; no client |
| 23 | Mobile Sentry | ED | `packages/mobile/lib/sentry.ts` | separate Sentry project | `EXPO_PUBLIC_SENTRY_DSN` | Already stubbed; client gated off |
| 24 | Cloud (`cloud/`) | — | `cloud/` | AWS CloudWatch (server-side) | hosted product | Out of scope: separate module, no client SDK |
| 25 | `@sentry/electron` dependency | NV | `frontend/package.json` | none (unused) | never imported | Kept and allowlisted; see residual risk |
| 26 | Electron native crash reporter / minidumps | — | — | — | never started | None found; crash dumps only reparented under `~/.ao` |

Evidence: this inventory comes from reading the code (file/line references in
the table) plus the existing test suite. It supersedes any earlier draft
summary; where the code and a prior note disagreed, the code wins.

## 4. What "external" means here, and what is preserved

Preserved on purpose (they are **not** telemetry):

- **Local logs and local diagnostics**, including the SQLite `telemetry_events`
  table (`LL`). These never leave the machine and are what a maintainer debugs
  with.
- **Internal application events, hooks, protocol messages, and CDC events**
  (`IE`). Internal names and shared interfaces are not, by themselves, evidence
  of external export. The daemon still emits `ao.*` events to its local sink and
  the SSE/CDC pipeline still delivers session state.
- **Functional communication** (`FC`): the loopback daemon API, WebSocket
  terminal mux, SCM/tracker clients, cloud control plane, Expo push, and update
  feeds. The fork blocks AO's tracking and error-reporting export, not the
  product's function.

### Out of scope

- **Agent-harness telemetry.** Codex, Claude Code, Kimi, Crush and similar
  harness binaries have their own telemetry, configured by their own vendors.
  This fork does not and cannot disable it from AO's code. Some harnesses expose
  their own opt-out (`cloud/internal/workerexec/command.go` shows the pattern);
  configuring those is a per-harness operator decision, not an AO fork change.
- **The hosted cloud product** (`cloud/`): a separate Go module with server-side
  CloudWatch monitoring only, no AO client SDK. Its existence in this repo is
  not an install path for the desktop fork.
- **Mobile**: treated in-repo (see #22/#23) but not built or run as part of this
  change; its independent initializations are disabled in code and its runtime
  validation is a pending item.

## 5. Implementation

Backend (Go):

- `backend/internal/daemon/fork_privacy.go` (new): the policy constants and the
  DSN resolver.
- `backend/internal/daemon/telemetry_wiring.go`: `newTelemetrySink` no longer
  constructs the PostHog chain; it returns a local SQLite sink (events enabled)
  or a `NoopSink` (events disabled). The `aggregatedEventNames` list that only
  existed to shape PostHog traffic is gone.
- `backend/internal/daemon/daemon.go`: the Sentry DSN is resolved through the
  policy (`daemonSentryDSN`) for both `sentryobs.ParseAgentSwitchDSN` and
  `sentryobs.Init(newDaemonSentryConfig(...))`, so neither the daemon Sentry
  client nor the agent-switch sender is ever built.
- `backend/internal/daemon/telemetry_wiring_test.go`,
  `backend/internal/daemon/fork_privacy_test.go`: regression tests (see §7).

Frontend:

- `frontend/src/shared/fork-privacy.ts` (new): the policy constants.
- `frontend/src/shared/telemetry.ts`: `rendererTelemetryEnabled` is permanently
  false, so `buildTelemetryBootstrap` always returns null (no PostHog client, no
  install id generated for export).
- `frontend/src/renderer/lib/telemetry.ts`: `initTelemetry` returns before
  `posthog.init`.
- `frontend/src/main/sentry-main.ts`, `frontend/src/renderer/lib/sentry.ts`:
  intake and transport are forced closed.
- `frontend/src/main.ts`: `telemetryOverrides()` pins `AO_TELEMETRY_REMOTE=off`
  and stamps no PostHog key/host or Sentry DSN onto the daemon.
- `frontend/vite.renderer.config.ts`: PostHog origins removed from the renderer
  CSP (defense in depth).
- Tests: `frontend/src/shared/privacy-policy.test.ts`,
  `frontend/src/shared/telemetry.test.ts`,
  `frontend/src/renderer/lib/telemetry.init.test.ts`.

Mobile:

- `packages/mobile/lib/telemetry/config.ts`: `MOBILE_TELEMETRY_DISABLED` is a
  constant `true`, so `initMobileTelemetry` never constructs a client.

No API contract, storage schema, or generated artifact changed. No new
dependency was added.

### Deliberately kept, and why

The PostHog adapter, the Sentry code, and the `posthog-js` /
`posthog-react-native` / `@sentry/electron` dependencies are retained but
**unreachable from any composition point**. Removing them wholesale would be a
large, merge-hostile diff across shared interfaces and their test suites; the
fork policy prefers neutralizing the composition points and keeping the shared
interfaces, then detecting a reintroduction in CI (see §7). This is a conscious
trade-off: the residual risk is a future code change flipping a policy constant
or rewiring a composition point, which the guard is designed to catch.

## 6. Updates and cross-product scope

**Updater.** Upstream bakes the release feed from `AO_RELEASE_REPO` in
`frontend/forge.config.ts`. A stock checkout defaulted to
`Untrivial-ai/agent-orchestrator`, so a packaged fork build would poll upstream
and could silently stage an upstream artifact (different privacy policy) on
quit. The fork changes the default (and the matching fallback in
`frontend/src/main/feature-builds.ts`) to `EIVRAJAM/agent-orchestrator`. CI
already sets `AO_RELEASE_REPO` to the repo it runs in. With no fork releases
yet, update checks fail harmlessly; they never fall back to upstream.

**Version floor.** `frontend/src/main/desktop-version-floor.ts` (and
`desktop-version-floor.json`) fetched a floor file from upstream and could
force-quit a fork install or point at an upstream download. Both now target the
fork repo. If the fork publishes no floor file, the fetch 404s and the check is
a silent no-op.

**Manual update procedure** (until fork release infrastructure exists):

1. Build from the desired fork commit (`frontend/npm ci` then
   `npm run make`), or download a fork-built artifact from the fork's releases.
2. Install over the existing app. Do not install an upstream artifact; the
   updater will not offer one, and the version floor will not point at one.
3. Record the source commit and verify the privacy guard on that commit
   (`bash scripts/privacy-guard.sh`).

**Mobile / Cloud.** Mobile initializations are disabled in code (see §3) but its
runtime is not validated here (no Expo build). Cloud is a separate module with
server-side monitoring only and is out of scope. Do not read either as
"validated" — see §8.

## 7. Verification

Automated (checked in):

- `bash scripts/privacy-guard.sh` (also `npm run privacy:guard`), run by
  `.github/workflows/privacy-guard.yml` and locally. It combines:
  - backend privacy tests (`newTelemetrySink` never builds a remote exporter;
    the composed Sentry config never enables Sentry even with a DSN present);
  - frontend privacy tests (the export gate is closed for every env; the
    bootstrap is always withheld; `initTelemetry` never constructs a client and
    every capture is dropped; `initMainSentry` returns null);
  - static alarms: policy constants closed, no production `NewPostHogSink`, no
    upstream owner in the updater/floor files, no unreviewed observability
    dependency, and a printed list of analytics/error init sites for review.

The tests assert **behavior at the composition points**, not constant matches:
they build the real config/objects and assert that no exporter/transport is
produced and that outbound requests stay at zero, using injected HTTP test
servers and fakes.

A controlled daemon-run traffic observation (destination canary + socket
inspection) is recorded in
[privacy-fork-traffic-report.md](privacy-fork-traffic-report.md), with its scope
and limits.

The static checks are an alarm, never the guarantee; a reintroduction that
avoids the grepped shapes must still be caught by the behavioral tests.

## 8. Residual risks and pending validations

Residual risks:

1. **A future code change flips a policy constant or rewires a composition
   point.** This is the main risk. The guard and CI workflow exist to make such
   a change fail and force review; they do not make it impossible.
2. **Dormant adapters and SDKs remain in the tree** (§5). A reintroduction only
   needs a composition change, which the guard flags. Removing them entirely is
   deferred to keep the fork mergeable.
3. **`@sentry/electron` is an unused declared dependency.** It is allowlisted in
   the guard. Removing it is a candidate follow-up that touches the lockfile.
4. **Pricing catalog fetch** (#19) still contacts `raw.githubusercontent.com`
   for a static catalog. It sends no user data, but it is an upstream-owned
   endpoint; repointing it to the fork is a candidate follow-up.
5. **Agent-harness telemetry** is unchanged and out of scope (§4). Users who
   need it off must configure each harness.

Pending validations (not done here — do not read as passed):

- **Real desktop traffic capture.** Not performed in this environment. The
  required method (isolated worktree, `npm ci`, `AO_DATA_DIR` lab dir, the
  `ao-desktop-dev` skill, run packaged-equivalent and dev, observe start/stop,
  session create/stop, controlled error, reconnect) must be run on a supported
  desktop OS and recorded as an appendix to this document, distinguishing AO
  traffic from agent traffic and system traffic. In-process interception alone
  is not a full system capture.
- **Mobile runtime.** No Expo build/run here; only the code-level disable is
  done.
- **Cloud.** Out of scope; no validation attempted.
- **Packaged-artifact verification on macOS/Windows.** Not run; the updater
  change is verified by code review and the guard, not by a signed-artifact
  install test.

## 9. Syncing with upstream

1. Fetch upstream and merge/rebase onto `main`.
2. Re-run `bash scripts/privacy-guard.sh`; the privacy workflow will also run on
   the PR.
3. Review every conflict or automated change in these files specifically:
   `backend/internal/daemon/fork_privacy.go`,
   `backend/internal/daemon/telemetry_wiring.go`,
   `backend/internal/daemon/daemon.go`,
   `frontend/src/shared/fork-privacy.ts`,
   `frontend/src/shared/telemetry.ts`,
   `frontend/src/renderer/lib/telemetry.ts`,
   `frontend/src/main.ts`,
   `frontend/src/main/sentry-main.ts`,
   `frontend/src/renderer/lib/sentry.ts`,
   `frontend/vite.renderer.config.ts`,
   `frontend/forge.config.ts`,
   `frontend/src/main/feature-builds.ts`,
   `frontend/src/main/desktop-version-floor.ts`,
   `desktop-version-floor.json`,
   `packages/mobile/lib/telemetry/config.ts`.
   Upstream changes in observability, consent, or the updater are the ones that
   can silently change this policy.
4. If upstream adds a new observability dependency or init site, the guard
   flags it; decide deliberately whether it is a kept-dormant adapter or a
   removed surface, and update this document.
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
