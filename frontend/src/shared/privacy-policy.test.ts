// @vitest-environment node
import { describe, expect, it } from "vitest";
import { EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED, EXTERNAL_TELEMETRY_EXPORT_ALLOWED } from "./fork-privacy";
import { initMainSentry } from "../main/sentry-main";

// The single TypeScript source of truth for the fork's external-export policy,
// plus the main-process composition point it gates. Changing fork-privacy.ts to
// re-enable an export turns this red on purpose. See docs/privacy-fork.md.
describe("fork privacy policy", () => {
	it("keeps both external export gates closed", () => {
		expect(EXTERNAL_TELEMETRY_EXPORT_ALLOWED).toBe(false);
		expect(EXTERNAL_DIAGNOSTICS_EXPORT_ALLOWED).toBe(false);
	});

	it("never builds a main-process Sentry transport", async () => {
		await expect(initMainSentry("0.11.2", "/tmp/ao-privacy-test")).resolves.toBeNull();
	});
});
