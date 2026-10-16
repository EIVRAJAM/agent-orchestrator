import { beforeEach, describe, expect, it, vi } from "vitest";

// Fork privacy policy regression: initTelemetry must never construct the
// PostHog client, so every capture helper has to stay a no-op no matter what
// bootstrap the main process could hand the renderer. This is the renderer
// composition point; see src/shared/fork-privacy.ts and docs/privacy-fork.md.

const posthogStub = {
	init: vi.fn(),
	register: vi.fn(),
	capture: vi.fn(() => ({})),
	captureException: vi.fn(),
	addExceptionStep: vi.fn(),
};

function mockPosthog() {
	vi.doMock("posthog-js/dist/module.full.no-external", () => ({
		default: posthogStub,
		PostHog: class {},
	}));
}

const enabledBootstrap = {
	appVersion: "1.2.3",
	platform: "darwin",
	distinctId: "ins_test",
	disabledEvents: [] as string[],
	eventsEnabled: true,
	consentGeneration: "generation-1",
};

describe("initTelemetry fork privacy policy", () => {
	beforeEach(() => {
		vi.resetModules();
		vi.clearAllMocks();
	});

	it("never constructs the PostHog client or fetches a bootstrap", async () => {
		const getBootstrap = vi.fn(async () => enabledBootstrap);
		mockPosthog();
		vi.doMock("./bridge", () => ({
			aoBridge: {
				telemetry: { getBootstrap },
				updateSettings: { get: vi.fn(async () => ({})) },
			},
		}));

		const { initTelemetry } = await import("./telemetry");
		expect(await initTelemetry()).toBe(false);
		// Memoized: still false, and the composition point never ran posthog.init.
		expect(await initTelemetry()).toBe(false);
		expect(posthogStub.init).not.toHaveBeenCalled();
		// No bootstrap round-trip means the per-install id is never read for export.
		expect(getBootstrap).not.toHaveBeenCalled();
	});

	it("drops every renderer capture even when a bootstrap is available", async () => {
		mockPosthog();
		vi.doMock("./bridge", () => ({
			aoBridge: {
				telemetry: { getBootstrap: vi.fn(async () => enabledBootstrap) },
				updateSettings: { get: vi.fn(async () => ({})) },
			},
		}));

		const { addRendererExceptionStep, captureRendererEvent, captureRendererException } = await import(
			"./telemetry"
		);
		await captureRendererEvent("ao.app.active", { channel: "renderer" });
		await captureRendererException(new Error("boom"), { source: "react-error-boundary" });
		await addRendererExceptionStep("a step");

		expect(posthogStub.capture).not.toHaveBeenCalled();
		expect(posthogStub.captureException).not.toHaveBeenCalled();
		expect(posthogStub.addExceptionStep).not.toHaveBeenCalled();
	});
});
