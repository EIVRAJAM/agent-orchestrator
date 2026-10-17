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
> final code tree recorded in [Re-run on the final code tree](#re-run-on-the-final-code-tree-9a962153b)
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

## Re-run on the final code tree (`9a962153b`)

**Date:** 2026-10-16. **Code tree:** `9a962153b` (last code commit; the worktree
was at `c0aa7f660`, whose only change over `9a962153b` is documentation). Every
commit after `9a962153b` (`1536c97d1`, `aa27ba666`, and this document's follow-up)
is **documentation-only**, so the dynamic evidence below applies to the code tree
that ships. This is the dynamic re-run the historical scope note called for; it
supersedes the run above for evidence purposes.

### Environment

- Debian GNU/Linux 13 (trixie), Linux 6.12.90, x86_64; real X11 display
  (`DISPLAY=:0`, KWin).
- Go 1.27.1, Node 24.21.0, npm 11.19.0, Electron 33.4.11.
- Available: `lsof`, `ss`, `nc`, `socat`, `import`/`spectacle`, `python3`.
  **Not** available: `tcpdump`, `strace`, `ltrace`, `tshark`, `mitmproxy`,
  `Xvfb`, Docker; no root (`sudo` needs a password).
- Codex and OpenCode CLIs present; `gh` authenticated as the fork owner.
- **Isolation and `~/.ao`:** the primary daemon/Electron runs used a temporary
  `AO_DATA_DIR`, `AO_DEV_ELECTRON_DIR` and (for the packaged app) `HOME`. The
  real `~/.ao` was **not** left untouched: two paths were created under it and
  later removed.
  - `~/.ao/dev/` — created by the **NetLog Electron dev** run. That run set
    `AO_DATA_DIR` and `AO_DEV_ELECTRON_DIR` but **not** `AO_RUN_FILE`, so the dev
    defaults applied: `runFilePath()` (`frontend/src/main.ts`) returns
    `~/.ao/dev/running.json` in dev when `AO_RUN_FILE` is unset, and the app
    writes `app-state.json` (`src/main/app-state.ts`) and `update-settings.json`
    (`src/main/update-settings.ts`) beside it. It contained `app-state.json`
    (298 B) and `update-settings.json` (123 B). **Cause:** a harness deficiency —
    the prescribed dev command omits `AO_RUN_FILE` — **not** a production defect:
    production honors `AO_RUN_FILE`/`AO_DATA_DIR`, and `~/.ao/...` is the
    documented default.
  - `~/.ao/repair-installer-tests/` (empty) — created by
    `frontend/scripts/build-mac-repair-installer.node-test.mjs`, whose `fixture()`
    base is `process.env.AO_DATA_DIR || path.join(homedir(), '.ao')`; it was run
    without `AO_DATA_DIR`, so it defaulted to the real home. **Cause:** running a
    test helper without a lab `AO_DATA_DIR`, **not** a production defect.
  - Both paths were **removed afterwards**, restoring `~/.ao` to the empty state
    an earlier listing had shown. Whether they pre-existed is therefore an
    inference from that listing, not an independent proof.
  - No production code ignores `AO_DATA_DIR`/`AO_RUN_FILE`.

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

### KWallet / secure storage (exact behaviour)

- On the first dev run (default password store), Chromium/Electron raised a
  **`kdewallet` (KWallet) access request** ("La aplicación … ha solicitado abrir
  la cartera «kdewallet»"). This is the OS keychain, **not telemetry and not
  PostHog/Sentry**.
- **No password was entered and the request was not granted.** No input-injection
  tool was available (no `xdotool`/root) and the prompt was left unanswered; it
  was neither satisfied nor explicitly cancelled by entering a password.
- With `--password-store=basic` (the task's prescribed flag) **no KWallet prompt
  appeared**, and a probe using the same Electron build (33.4.11) reported
  `safeStorage.getSelectedStorageBackend()` → **`basic_text`** and
  `safeStorage.isEncryptionAvailable()` → **`false`**. Without the flag the probe
  reported **`kwallet6`** with `isEncryptionAvailable()` → **`false`**.
- Because encryption is unavailable, the app's `cloud-auth.ts` refuses to persist
  the cloud auth token (`if (!safeStorage.isEncryptionAvailable()) return false`).
  **Persistent cloud authentication was therefore NOT validated with secure
  storage.**
- **Credential isolation is not fully proven**: what happened to the wallet after
  the unanswered prompt was not observable without the wallet password. This is
  recorded as a limit, not as a pass.

### Network inspection without root (Chromium NetLog)

Because `tcpdump`/`strace`/`tshark`/root are unavailable, both the dev app and the
extracted `.deb` were re-run with Chromium's own network log:

```
--log-net-log=/tmp/ao-privacy-netlog.json --net-log-capture-mode=Everything
```

The NetLog can contain URLs, cookies and headers; only hosts, ports and event
types were extracted — the file is **not** committed and not reproduced here.

- **Dev app URL hosts:** `http://localhost:5173` (Vite modules, 6040 requests),
  `http://127.0.0.1:13006` (daemon, 401), `https://redirector.gvt1.com` (4),
  `https://rr2---sn-0ox-jubl.gvt1.com` (2). **No PostHog/Sentry host.**
- **Dev app socket peers:** `[2800:e0:c79::d]:443` (resolved by the NetLog to
  `rr2.sn-0ox-jubl.gvt1.com`, Google `gvt1.com` CDN), `[2001:4860:4860::8888]:443`
  (Google DNS), `[::1]:5173` (Vite), `127.0.0.1:13006` (daemon),
  `100.100.100.100:53` (Tailscale DNS), `[2607:f8b0:…]:443` (Google).
- **The `posthog`/`sentry` strings in the NetLog are Vite module paths**
  (`/src/shared/posthog-config.ts`, `/src/renderer/lib/sentry.ts`), not request
  hosts.
- **Extracted `.deb` URL hosts:** `http://127.0.0.1:13007` (daemon),
  `redirector.gvt1.com`, `rr2---sn-0ox-jubl.gvt1.com`; same socket peers;
  **no PostHog/Sentry host**.
- **Scope:** the NetLog materially improves Chromium-level observation (it named
  the previously unknown IPv6) but it is still **not** a full system capture.

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

### Extracted `.deb` (no root)

- `dpkg-deb -x <artifact>.deb /tmp/ao-deb-extracted` extracted the tree without
  root (no `dpkg -i`). SHA-256 of the local `.deb`:
  `f4b062cea5d252ba7cd30446f9c11cf19ac583cca202408bd7a198f2aaa29f65`.
- `usr/lib/agent-orchestrator/resources/app-update.yml` → `owner: EIVRAJAM`,
  `repo: agent-orchestrator`; the deb control `Homepage` is
  `https://github.com/aoagents/agent-orchestrator` (a metadata link, not the
  updater feed).
- The extracted binary was run with `HOME`/`AO_DATA_DIR` under `/tmp`, the
  canaries, `--password-store=basic` and a NetLog: it booted, loaded its
  renderer, added a lab project, and made **no** PostHog/Sentry request.
- **Installation via `dpkg -i` remains pending** (no root); only extraction and
  direct execution were validated.

### Codex chat with a compatible, isolated version

- The daemon had resolved `~/.local/bin/codex` (0.144.5), below AO's tested
  minimum 0.146.0. A compatible **0.160.0** was already present at
  `~/.nvm/versions/node/v24.21.0/bin/codex`; no install or user config change was
  needed. A standalone lab daemon was run with that bin first on `PATH`, and AO
  resolved `codex` to the 0.160.0 binary.
- A **chat** session then provisioned successfully: worktree created, the prompt
  answered ("READY"), provider events
  (`turn.started` → `message.delta` → `message.completed` → `usage` →
  `turn.completed`), and a clean stop that removed the worktree.
- The agent's traffic is Codex's own service (agent provider); no AO telemetry.
- AO's separate Codex **account store** reported `account_storage_unsafe` in the
  lab data dir (a secure-file permission guard); it did not block the session.

### Host classification

| Host | Port | Classification |
| --- | --- | --- |
| `raw.githubusercontent.com` | 443 | Functional (pricing catalog) |
| `registry.npmjs.org` | 443 | Functional (harness update check) |
| `chatgpt.com` | 443 | Functional / agent (Codex install plan, Codex service) |
| `github.com` / `api.github.com` | 443 | Functional (GitHub auth/PR) |
| `opencode.ai` | 443 | Agent service (OpenCode) |
| `*.googleapis.com` / `*.gstatic.com` / `*.gvt1.com` (Chromium network service) | 443 | Chromium/OS (component updates, safe browsing); not AO |
| `2800:e0:c79::d` (Chromium network service) | 443 | **Unidentified by socket inspection alone; the NetLog re-run resolved it to `rr2.sn-0ox-jubl.gvt1.com` / `redirector.gvt1.com` (Google `gvt1.com` CDN).** It did not match known PostHog/Sentry destinations and produced no canary request |
| daemon loopback, agent local APIs, Vite dev server | various | Loopback / local |
| **`us.i.posthog.com` / `*.ingest.sentry.io`** | — | **None observed** |

### Functional connections review

- `raw.githubusercontent.com` — the pricing catalog (`internal/pricing/fetch.go`,
  `RemoteManifestURL`). It currently points at **upstream**
  `Untrivial-ai/agent-orchestrator`. It is a read-only GET of a public manifest
  with no user data. **Recommendation:** consider repointing it at the fork (or a
  build-time override) to remove the operational dependency on upstream.
  **Impact:** low privacy impact, moderate operational impact; it is a separate
  change and was **not** made here.
- `registry.npmjs.org` — installed-harness version checks
  (`systeminstall/manager_versions.go`); disabled with `AO_HARNESS_UPDATE_CHECKS=off`.
- `chatgpt.com` — Codex install-plan URL (`systeminstall/agentplans.go`).
- `github.com` / `api.github.com` — GitHub auth/PR client (functional).
- `opencode.ai` and Codex's `chatgpt.com` — agent-provider traffic (out of scope).
- Chromium services (`gvt1.com`, `googleapis.com`, Google DNS, Tailscale DNS) —
  browser/OS, not AO.
- **Documentary/UI links (not telemetry):** the Home page "Star Us" link points at
  upstream (`HomePage.tsx`), and the `.deb` `Homepage` metadata is
  `https://github.com/aoagents/agent-orchestrator`. Both are static links, not
  requests. **Recommendation:** repoint them at the fork in a separate task.

### Limits (re-run)

- Still no full packet capture (no `tcpdump`/`strace`/root): the method is
  destination canary + socket inspection + CONNECT-proxy naming.
- One Chromium network-service peer IPv6 (`2800:e0:c79::d`) was not resolvable by
  reverse DNS or socket inspection; the NetLog re-run resolved it to
  `rr2.sn-0ox-jubl.gvt1.com` / `redirector.gvt1.com` (Google `gvt1.com` CDN). It is
  not a known PostHog/Sentry destination and produced no canary request.
- **Signing, notarization, install and the effective updater download were not
  performed** (no root for `dpkg -i`, no published release, macOS unavailable).
- **rpm/AppImage and all macOS/Windows artifacts were not built** (missing
  tooling / other platforms).
- **`agent-ci run --all` was not run**: no Docker, Podman, rootless runtime or
  Docker socket is present.
- **Mobile runtime was not run**: no simulator/device, and Expo **web** is blocked
  because the package lacks `react-dom`/`react-native-web` (`expo export
  --platform web` refuses without them). Mobile evidence remains the code
  constants and the full mobile test suite (1912 pass / 1 pre-existing locale
  failure, reproduced on base).
- **Codex chat** now runs (a compatible 0.160.0 was already on the machine); the
  separate Codex account-store feature reports `account_storage_unsafe` in the
  lab data dir.

### Platform matrix (for future CI / release jobs)

| Platform / artifact | Status here | Official command / job |
| --- | --- | --- |
| Linux `.deb` (unsigned) | Built, extracted, run | `npm run make` (`build-artifacts.yml`) |
| Linux AppImage | **Blocked** (no `appimagetool`/`mksquashfs`) | `npm run make` |
| Linux rpm | **Blocked** (no `rpmbuild`) | `npm run make` |
| macOS `.zip` + `.dmg` | **Not run** (Linux only) | `npm run make` on macOS; verify with `frontend/scripts/verify-mac-artifact.sh` |
| macOS sign / notarize / staple | **Not run** | release workflow (`build-artifacts.yml`) |
| Windows installer (NSIS) | **Not run** | `npm run make` on Windows |
| Android / iOS | **Blocked** (no device/emulator; Expo web needs web deps) | `npm run android` / `npm run ios` |
| `agent-ci run --all` | **Blocked** (no container runtime) | `npx @redwoodjs/agent-ci run --all` |
| Full privileged capture | **Blocked** (no `tcpdump`/`strace`/root) | n/a |

### CI and full test suites

`agent-ci run --all` cannot run here (no Docker/Podman/rootless runtime and no
Docker socket). The reproducible workflow commands were run directly:

- **Go** (`go.yml`): `gofmt` clean; `go build ./...` and `go vet ./...` pass;
  `go test -race -count=1 -timeout=20m ./...` → 192 packages pass, **9 fail
  identically on the base `7dceb0ec7`** (reproduced in an isolated base worktree),
  so they are pre-existing/environment-sensitive, not branch regressions:
  `internal/adapters/agent/{fake,modelcatalog,opencode,opencodev2}`,
  `internal/adapters/reviewer/opencodev2`,
  `internal/adapters/workspace/gitworktree`, `internal/integration`,
  `internal/service/agent` (the Codex secure-credential/account-store tests), and
  `internal/storage/sqlite` (exceeds the `-race` timeout in this container — it
  timed out at 24m on **both** the branch and the base; the same package passes
  **without** `-race` in ~52s for all 107 tests, and the individual test passes
  under `-race`).
- **Frontend** (`frontend.yml`): `npm run typecheck` passes; `node --test
  scripts/*.node-test.mjs` pass; `vitest run` → **8056 pass / 6 fail** across 4
  files (`ChatWorkspace`, `ContextMeter`, `AgentModelCombobox`, `BillingSection`),
  all **reproduced identically on the base** (locale/`Intl` formatting).
  (`browser-profile-import.test.ts` fails only when the packaging step has rebuilt
  `better-sqlite3` for the bundled Node 20 ABI; with the Node 24 binary restored
  it passes 47/47 — a lab artifact, not a regression.)
- **Mobile** (`mobile.yml`): `npm run typecheck` passes; `vitest run` → 1912 pass
  / 1 fail (`conversationChrome`, locale), reproduced on base.
- **e2e-gate-tests**: `node --test scripts/ao-e2e-pod-gate.test.mjs` → 17/17.
- **version-floor-guard**: JSON structure valid; `downloadUrl` → the fork.
- **pricing-catalog-validate**: `go test ./cmd/pricingcatalog
  ./internal/pricing/catalogsync` passes; `go run ./cmd/pricingcatalog validate
  -root ..` → `valid`.
- **privacy-guard.yml**: `privacy-guard.sh` incl. `CI=true` passes; the CI-bypass
  env vars (`PRIVACY_GUARD_SKIP_GO`, `PRIVACY_GUARD_STATIC_ONLY`) are rejected
  (exit 2).
