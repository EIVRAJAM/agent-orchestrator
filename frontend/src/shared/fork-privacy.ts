/**
 * Fork privacy policy for Agent Orchestrator's desktop surfaces.
 *
 * This fork ships with AO-originated product analytics and error/diagnostic
 * reporting to third parties (PostHog, Sentry) permanently disabled as a code
 * property, not a runtime setting. These constants are the single TypeScript
 * source of truth consulted by every desktop composition point that could
 * otherwise build an outbound exporter:
 *
 *   - EXTERNAL_TELEMETRY_EXPORT_ALLOWED gates the renderer PostHog client and
 *     the bootstrap that would carry an install id to it.
 *   - EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED gates the Electron main Sentry
 *     transport and the renderer→main fault intake.
 *
 * They are deliberately constants, never configuration: no environment
 * variable (AO_TELEMETRY_RENDERER, AO_TELEMETRY_EVENTS, AO_SENTRY_DSN,
 * VITE_AO_*), consent record, user preference, or remote config can flip them,
 * and there is no opt-in switch that restores the export. Reverting an export
 * therefore requires editing this file, which the privacy regression tests
 * (src/shared/privacy-policy.test.ts) and the CI privacy guard
 * (../scripts/privacy-guard.sh) call out for review. See docs/privacy-fork.md.
 *
 * Scope: AO's own telemetry/diagnostics export only. Functional network traffic
 * — the loopback daemon API, the user's own GitHub/GitLab, the cloud control
 * plane, and the update feed — is unaffected. Agent-harness telemetry (Codex,
 * Claude Code, Kimi, and the like) is the harness vendors' own code and is out
 * of scope.
 */
export const EXTERNAL_TELEMETRY_EXPORT_ALLOWED = false;
export const EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED = false;
