# Codex readiness

AO separates two Codex signals:

- **Codex CLI login status** — `codex login status` can report
  `Logged in using ChatGPT` without exposing credential material.
- **AO launch readiness** — the daemon's installation, authentication, and
  optional account-store checks.

`auth unknown` is not a claim that the user is signed out. It means AO could not
complete a launch-grade authentication/account-store check and therefore cannot
present the harness as ready. A common cause is `account_storage_unsafe`: AO
refuses to use a Codex account store whose path has an ancestor writable by a
group or other users. The daemon reports this as a warning and fails closed
instead of silently trusting the directory.

A separate successful `codex login status` can show that native Codex credentials
exist, and an ordinary session may still launch using those native credentials.
That does not make the account store safe or turn `auth unknown` into an
authorized readiness result. Do not bypass the check by relaxing sandbox,
approval, or filesystem rules. Resolve the untrusted path condition separately;
until then, automated cohorts must stop rather than treating `unknown` as safe.
