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
> tighter (always blank instead of a branch), so it cannot widen the surface. The
> **binary built from the final commit has not been observed**, and there is no
> full traffic capture of the final HEAD. Repeat the run on the final commit
> before claiming execution evidence for the definitive artifact; the method
> below is the reproducible procedure.

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
