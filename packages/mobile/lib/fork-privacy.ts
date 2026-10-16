/**
 * Fork privacy policy for the Agent Orchestrator mobile app.
 *
 * This fork ships with AO's own outbound telemetry and error/diagnostic
 * reporting (PostHog, Sentry) permanently disabled as a code property, not a
 * runtime setting. These constants are the mobile counterpart of
 * frontend/src/shared/fork-privacy.ts: the Expo/React Native package cannot
 * import from `frontend/src`, so the policy is duplicated here as the mobile
 * package's own reviewed source of truth, and the CI privacy guard
 * (scripts/privacy-guard.sh) checks both files.
 *
 * They are deliberately constants, never configuration: no EXPO_PUBLIC_* build
 * variable, consent record, or remote config can flip them, and there is no
 * opt-in switch that restores the export. Reverting an export requires editing
 * this file, which the mobile privacy tests and the privacy guard call out for
 * review. See docs/privacy-fork.md.
 */
export const MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED = false;
export const MOBILE_EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED = false;
