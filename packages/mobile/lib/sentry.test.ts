import { afterEach, describe, expect, it, vi } from "vitest";
import { MOBILE_EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED } from "./fork-privacy";
import { captureMobileApiError, captureMobileException, initMobileSentry } from "./sentry";

// No SDK installed yet: init must resolve and captures must never throw.
describe("sentry — SDK not installed", () => {
	it("captures are no-ops before init", () => {
		expect(() => captureMobileException(new Error("boom"), { category: "native_crash" })).not.toThrow();
		expect(() => captureMobileApiError("/api/v1/sessions/123", "http_5xx", 503)).not.toThrow();
	});

	it("init resolves without the SDK and captures stay no-ops", async () => {
		await expect(initMobileSentry({ release: "0.0.0@1" })).resolves.toBeUndefined();
		expect(() => captureMobileException(new Error("boom"), { category: "native_crash" })).not.toThrow();
		expect(() => captureMobileApiError("/api/v1/sessions/123", "http_5xx", 503)).not.toThrow();
	});
});

// Fork privacy regression: with a valid DSN, an available SDK, and a production
// environment, the sink must never read the DSN, load the SDK, or call
// Sentry.init. See packages/mobile/lib/fork-privacy.ts.
describe("sentry — fork privacy policy", () => {
	afterEach(() => {
		delete process.env.EXPO_PUBLIC_SENTRY_DSN;
	});

	it("keeps the diagnostics gate closed", () => {
		expect(MOBILE_EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED).toBe(false);
	});

	it("never loads the SDK or calls init, even with a valid DSN and an injected SDK", async () => {
		process.env.EXPO_PUBLIC_SENTRY_DSN = "https://abc123def456@o4511955966361600.ingest.us.sentry.io/4511967513935872";
		const init = vi.fn();
		const sdk = { init, captureException: vi.fn(), captureMessage: vi.fn(), addBreadcrumb: vi.fn() };
		const loadSdk = vi.fn(async () => sdk);

		await initMobileSentry({ release: "1.2.3-stable" }, { loadSdk });

		expect(loadSdk).not.toHaveBeenCalled();
		expect(init).not.toHaveBeenCalled();
	});

	it("drops exception and API captures instead of forwarding them", () => {
		expect(() => captureMobileException(new Error("boom"), { operation: "open_orchestrator" })).not.toThrow();
		expect(() => captureMobileApiError("/api/v1/sessions", "http_5xx", 500, "internal_error", "req-1")).not.toThrow();
	});
});
