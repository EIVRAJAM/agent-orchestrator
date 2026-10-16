// PostHog project the mobile app reports to.
//
// This is the APP project, the same one the desktop app uses, so desktop and
// mobile usage sit together and split by the `client` context property. The
// marketing site is a separate project. The key is a public project key (it
// ships in every client build by design, like the desktop's), not a secret, so
// hardcoding it mirrors the desktop's shared constant. An EXPO_PUBLIC_ override
// lets a build point elsewhere without a code change.
export const MOBILE_POSTHOG_KEY =
	process.env.EXPO_PUBLIC_POSTHOG_KEY?.trim() || "phc_uXAqS8nokL2QLSGBZSEMHTUNVXsFeXu3SrcWG7fjEyVH";

export const MOBILE_POSTHOG_HOST =
	process.env.EXPO_PUBLIC_POSTHOG_HOST?.trim() || "https://us.i.posthog.com";

import { MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED } from "../fork-privacy";

/**
 * Fork privacy policy: mobile PostHog telemetry is permanently disabled as a
 * code property, matching the desktop's src/shared/fork-privacy.ts. This is not
 * a build-time flag any more: no EXPO_PUBLIC_* environment variable, consent
 * record, or remote config can re-enable the client, because
 * initMobileTelemetry() returns early on this value. Reverting it requires
 * editing packages/mobile/lib/fork-privacy.ts. See docs/privacy-fork.md.
 *
 * Upstream behavior, retained only as documentation: this used to read
 * `EXPO_PUBLIC_AO_TELEMETRY_DISABLED === "1"`, so a shipped binary could not be
 * hotfixed and the runtime kill switch was the PostHog-side ingestion drop rule
 * (see docs/posthog-cost-controls.md).
 */
export const MOBILE_TELEMETRY_DISABLED = !MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED;

export const MOBILE_DISABLED_EVENTS = (process.env.EXPO_PUBLIC_AO_TELEMETRY_DISABLED_EVENTS ?? "")
	.split(",")
	.map((name: string) => name.trim())
	.filter((name: string) => name.length > 0);
