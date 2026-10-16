# Controlled traffic report (privacy fork)

Appendix to [privacy-fork.md](privacy-fork.md). This records a controlled
observation of this fork's outbound behavior. It is deliberately explicit about
what was and was not observed, and about the limits of the method.

> **Evidence scope / historical classification.** This run was performed on
> commit **`6bfdccd2d`**. The branch has since moved through a series of hardening
> commits — `d77f30f26` (guard gates, mobile Sentry early return, release-target
> authority, Go policy coherence), `cecf213db` (import-specifier scanner, widened
> coverage, per-condition self-tests), `ff51dbbe5` (multiline import/require
> detection, package-name boundaries, backtick, `overrides`/`resolutions`),
> `e66d5af01` (AST-based import analysis and indirection-alias detection),
> `66e069c77` (broadened Vite/Babel/Metro resolution-config discovery and a
> manifest-scoped dependency allowlist), `cdfdfea64` (strict fail-closed
> classification of alias targets, shorthand/spread/indirection),
> `7a78c78f` (indirect Vite-export resolution and verified `path`/`url` helper
> provenance), and `0764a0c3` (`require` provenance, helper invalidation on
> member mutation, `__dirname`/`__filename`, Vite+Vitest discovery), and
> `ff604abc` (helper mutation propagation through identifier aliases; writes
> restricted to assignment operators), and `07eea972` (scope-aware
> declaration-identity model: aliases in blocks/functions/callbacks,
> destructuring write targets, and `++`/`--`/`delete` vs read-only unary
> operators), and `89d925b5d` (call arguments linked to parameters;
> `for`/`for…of`/`for…in` header scopes, named function expressions), and
> `87fc8276d` (arguments linked through default values and destructuring
> patterns), and `9a962153b` (`undefined`-activating defaults and duplicate
> object keys). It is kept as
> **historical / comparative** evidence, not as execution evidence of the current
> HEAD. It stays
> relevant because the daemon outbound paths it exercised — `newTelemetrySink`
> (local-only), the renderer/mobile init, and the daemon env-stamping — are
> unchanged by those commits, and the Go change made the Sentry DSN path strictly
> tighter (always blank instead of a branch), so it cannot widen the surface.
> **This historical run is superseded for evidence purposes by the re-run on the
> final HEAD recorded in [Re-run on the final HEAD](#re-run-on-the-final-head-c0aa7f660)
> below.** The method described here is the reproducible procedure.

## Verdict

In the scenarios exercised, the Go daemon made **no** request to a PostHog or
Sentry destination, even with remote telemetry fully "enabled" by environment,
while it still recorded internal events locally and kept serving the loopback
API. Two outbound HTTPS connections were observed and both are **functional**
(the pricing catalog and a harness readiness probe), not AO telemetry.

This is consistent with the code-level policy and the behavioral tests. It is
not, by itself, a guarantee — see [Limits](#limits).

## Environment

- Host: Debian GNU/Linux 13 (trixie), Linux 6.12.90, x86_64.
- Artifact under test: `ao` daemon built from commit `6bfdccd2d`
  (`go build -o /tmp/opencode/ao ./cmd/ao`) — the previous commit, see the scope
  note above.
- Data: isolated lab dir `AO_DATA_DIR=/tmp/opencode/ao-privacy-lab`, no personal
  credentials, no real `~/.ao` state.
- Tooling available: `ss`, `lsof`, `python3`. **Not** available: `tcpdump`,
  `strace`, `Xvfb` (no root, no display) — this bounds the method.

## Method

Three layers, because no single one is a full capture:

1. **Behavioral unit tests** (in the repo, no network): the daemon
   composition never builds a remote exporter and never enables Sentry; the
   renderer never constructs a PostHog client and never fetches a bootstrap;
   the main process never returns a Sentry transport. Run with
   `bash scripts/privacy-guard.sh`.
2. **Controlled daemon run with a destination canary.** A local HTTP listener
   (`/tmp/opencode/canary_server.py`) was configured as BOTH
   `AO_TELEMETRY_POSTHOG_HOST` (`http://127.0.0.1:59999`) and the Sentry DSN
   (`http://canary@127.0.0.1:59999/1`), and the daemon was started with remote
   telemetry fully enabled as if upstream defaults had been injected:

   ```bash
   AO_DATA_DIR=/tmp/opencode/ao-privacy-lab AO_PORT=47899 AO_AGENT=claude-code \
     AO_TELEMETRY_EVENTS=on AO_TELEMETRY_REMOTE=posthog \
     AO_TELEMETRY_POSTHOG_KEY=phc_canary_should_never_be_used \
     AO_TELEMETRY_POSTHOG_HOST=http://127.0.0.1:59999 \
     AO_SENTRY_DSN=http://canary@127.0.0.1:59999/1 \
     /tmp/opencode/ao daemon
   ```

   If any exporter were built, its request would land on the canary. The canary
   log stayed empty.
3. **Socket inspection.** While the daemon ran, `ss -tnp` and `lsof -nP -p PID
   -i` listed its sockets; the remote endpoints were resolved and classified.

## Scenarios exercised (≈10 s total, one daemon lifetime)

| Scenario | Action | Result |
| --- | --- | --- |
| Daemon boot | start `ao daemon` | `daemon listening addr=127.0.0.1:47899` |
| Identity probe | `GET /api/v1/identity` | `200` with host id |
| List projects | `GET /api/v1/projects` | `200` |
| Client error | `GET /api/v1/nope` | `404` (not a 5xx, so no Sentry capture path) |
| Idle + internal events | wait | internal events recorded locally |
| Shutdown | stop daemon | clean |

## Observed

- **Canary (configured PostHog host + Sentry DSN): zero requests.** Even with
  `AO_TELEMETRY_REMOTE=posthog` and a key/host/DSN present, no exporter was
  constructed and no request was made. This is the execution-level evidence that
  environment/config cannot reactivate the export.
- **Local diagnostics still work.** `telemetry_event` in the lab SQLite DB
  contained `ao.daemon.started` (count 1): internal events still flow to the
  local sink.
- **Outbound sockets from the daemon process (2), both functional:**
  - `2606:50c0:8000::154:443` → `raw.githubusercontent.com` — the local pricing
    catalog fetch (`pricing/fetch.go`), no user data.
  - `2606:4700::6810:522:443` → `registry.npmjs.org` — a harness readiness
    probe spawned by the daemon, not AO telemetry.
- **No connection to PostHog or Sentry.** `us.i.posthog.com` resolves to
  `2600:1f18:…` (AWS) and the Sentry DSN host to `34.160.81.0` / `2600:1901:…`;
  neither appeared in the daemon's sockets.

## Limits

- **No full system capture.** Without `tcpdump`/`strace`/root, this is
  destination-canary + socket inspection, not a packet capture of the whole
  system. The absence of traffic here is corroborated by tests and code review,
  not proven exhaustively.
- **Desktop (Electron main + renderer) was not run.** There is no display or
  Xvfb in this environment, and the desktop path is macOS-oriented. The
  renderer/main composition points are covered by unit tests only. A real
  desktop run (isolated worktree, `npm ci`, `AO_DATA_DIR` lab dir, the
  `ao-desktop-dev` skill, packaged-equivalent and dev, start/stop, session
  create/stop, controlled error, reconnect) is a **pending validation** and must
  be recorded as an appendix on a supported desktop OS.
- **The Sentry capture path was not triggered at the process level.** The
  daemon's Sentry is a no-op because the DSN is blank; that is asserted by unit
  test (`newDaemonSentryConfig` + `sentryobs.Enabled()`), not by forcing a 5xx.
- **Packaged artifact not built or installed.** The updater/version-floor change
  is verified by code review, the guard, and CI, not by a signed-artifact
  install test.
- **Mobile and Cloud not run.** Mobile's init is disabled in code; Cloud is out
  of scope.
- Traffic of the **agent harnesses** and of the **host system** is not
  attributed here; only the daemon process's sockets were inspected.
- **This run predates the current HEAD**; it observed the binary built from
  `6bfdccd2d`. See the scope note at the top.

## Re-run on the final HEAD (`c0aa7f660`)

**Date:** 2026-10-16. **HEAD:** `c0aa7f660` (docs) / `9a962153b` (last code
commit). This is the dynamic re-run the historical scope note called for; it
supersedes the run above for evidence purposes.

### Environment

- Debian GNU/Linux 13 (trixie), Linux 6.12.90, x86_64; real X11 display
  (`DISPLAY=:0`, KWin).
- Go 1.27.1, Node 24.21.0, npm 11.19.0, Electron 33.4.11.
- Available: `lsof`, `ss`, `nc`, `socat`, `import`/`spectacle`, `python3`.
  **Not** available: `tcpdump`, `strace`, `ltrace`, `tshark`, `mitmproxy`,
  `Xvfb`, Docker; no root (`sudo` needs a password).
- Codex and OpenCode CLIs present; `gh` authenticated as the fork owner.
- **Isolation:** a detached worktree at `/home/master-chief/ao-privacy-lab`, lab
  `AO_DATA_DIR`/`AO_DEV_ELECTRON_DIR`, a synthetic git repo plus a local bare
  origin, and a temporary `HOME` for the packaged app (whose `~/.ao/electron` is
  deliberately not overridable). The real `~/.ao` was never read or written.

### Method

1. **HTTP canaries** on `127.0.0.1`: one served as `AO_TELEMETRY_POSTHOG_HOST`
   (`:19001`) and `AO_SENTRY_DSN` (`:19002`). Zero requests arrived except a
   single `curl` self-test proving the listener works.
2. **Daemon run** (binary built from the HEAD) with the export "enabled" by
   environment: `AO_TELEMETRY_EVENTS=1`, `AO_TELEMETRY_METRICS=1`,
   `AO_TELEMETRY_REMOTE=posthog`, `AO_TELEMETRY_POSTHOG_KEY`,
   `AO_TELEMETRY_POSTHOG_HOST=<canary>`, `AO_SENTRY_DSN=<canary>`,
   `AO_TELEMETRY_RENDERER=1`, `VITE_AO_TELEMETRY=1`, `EXPO_PUBLIC_AO_TELEMETRY=1`.
3. **Socket inspection + CONNECT proxy.** `ss -tnp`/`lsof` listed the daemon's
   sockets; a local CONNECT-logging proxy named every external destination (Go
   never proxies loopback, so this only reveals external hosts).
4. **Real Electron app** (dev, isolated worktree) and the **packaged artifact**
   were run under the canaries with `ELECTRON_ENABLE_LOGGING=1` and driven
   through the Chrome DevTools Protocol (no `xdotool`/root available).

### Daemon result

- **Canary: zero requests** with the export fully enabled by environment.
- **Local records preserved:** `telemetry_event` held `ao.daemon.started`,
  `ao.projects.created`, `ao.onboarding.first_project_added`; `change_log` and
  `conversation_provider_events` were also populated. (In the desktop app the
  local `telemetry_event` sink is additionally gated by
  `telemetry_policy.json` `events_enabled:false`; the core durable records are
  not.)
- **External destinations (CONNECT proxy):**
  - `raw.githubusercontent.com:443` — pricing catalog manifest
    (`internal/pricing/fetch.go`). Functional, no user data.
  - `registry.npmjs.org:443` — installed-harness update check
    (`systeminstall/manager_versions.go`); absent with `AO_HARNESS_UPDATE_CHECKS=off`.
  - `chatgpt.com:443` — Codex install-plan URL (`systeminstall/agentplans.go`).
  - `github.com:443` — GitHub auth/PR client.
- **No PostHog/Sentry destination** appeared in any socket.

### Electron (dev) result

- Window "Orchestrator.inc (dev)" loaded (screenshot), the Kanban board rendered,
  a synthetic project appeared, and a real `opencode-v2` session ran in a created
  worktree and replied. Session detail, stop + worktree cleanup, app restart with
  recovery ("Exited"), and renderer reconnect were all exercised.
- The supervisor forces `AO_TELEMETRY_REMOTE=off` when spawning the daemon
  (`telemetryOverrides()` in `main.ts`) — a second, independent guarantee on top
  of the daemon's own fork policy.
- Profile scan: no analytics install id. The PostHog/Sentry strings in the
  Chromium cache are the app's own modules served by the Vite dev server
  (`localhost:5173/src/...`), not responses from an observability endpoint.

### Packaged artifact result

- `electron-forge make` produced a local **deb**
  (`out/make/deb/x64/agent-orchestrator_0.13.0_amd64.deb`). Building needed a lab
  workaround: bundled tmux is normally compiled from source (`yacc`/`bison`,
  absent here); the system tmux (3.5a, the exact pinned version) was placed in
  `build-tmux.mjs`'s cache. `rpm`/AppImage makers were skipped (no
  `rpmbuild`/`appimagetool`).
- `resources/app-update.yml` → `owner: EIVRAJAM`, `repo: agent-orchestrator`; no
  `Untrivial-ai` anywhere in the packaged resources.
- `desktop-version-floor.json` →
  `https://github.com/EIVRAJAM/agent-orchestrator/releases/latest`.
- Direct execution of `resolveReleaseRepo`/`buildAppUpdateYml` rejected
  `Untrivial-ai/agent-orchestrator` and `aoagents/agent-orchestrator` (upstream),
  `someone/else` (unauthorized) and malformed slugs; blank falls back to the fork.
- The packaged app booted (screenshot), loaded its renderer, and made **no**
  PostHog/Sentry request.

### Host classification

| Host | Port | Classification |
| --- | --- | --- |
| `raw.githubusercontent.com` | 443 | Functional (pricing catalog) |
| `registry.npmjs.org` | 443 | Functional (harness update check) |
| `chatgpt.com` | 443 | Functional / agent (Codex install plan, Codex service) |
| `github.com` / `api.github.com` | 443 | Functional (GitHub auth/PR) |
| `opencode.ai` | 443 | Agent service (OpenCode) |
| `*.googleapis.com` / `*.gstatic.com` / `*.gvt1.com` (Chromium network service) | 443 | Chromium/OS (component updates, safe browsing); not AO |
| daemon loopback, agent local APIs, Vite dev server | various | Loopback / local |
| **`us.i.posthog.com` / `*.ingest.sentry.io`** | — | **None observed** |

### Limits (re-run)

- Still no full packet capture (no `tcpdump`/`strace`/root): the method is
  destination canary + socket inspection + CONNECT-proxy naming.
- One Chromium network-service peer IPv6 (`2800:e0:c79::d`) was not resolved to a
  name; it is not a known observability endpoint and produced no canary hit.
- **Signing, notarization, install and the effective updater download were not
  performed** (no root for `dpkg -i`, no published release, macOS unavailable).
- **rpm/AppImage and all macOS artifacts were not built** (missing tooling).
- **`agent-ci run --all` was not run** (Docker absent). **Mobile runtime was not
  run** (no simulator/device; Expo deps absent) — mobile evidence remains the
  code constants and the automated tests. **Codex chat mode** is blocked by the
  installed Codex version (< AO's minimum); Codex TUI was exercised instead.
