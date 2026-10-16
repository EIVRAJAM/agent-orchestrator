import { beforeEach, describe, expect, it, vi } from "vitest";

// Fork privacy regression for mobile PostHog: the runtime must never construct a
// client, even in a production environment with EXPO_PUBLIC_* variables that try
// to opt in. The React Native / Expo boundaries are mocked so the test runs in a
// plain Node environment and so an accidental construction is observable. See
// packages/mobile/lib/fork-privacy.ts and docs/privacy-fork.md.

const constructed = vi.fn();
vi.mock("posthog-react-native", () => ({
	default: class {
		constructor() {
			constructed();
		}
	},
}));
vi.mock("@react-native-async-storage/async-storage", () => ({
	default: { getItem: vi.fn(async () => null), setItem: vi.fn(async () => undefined) },
}));
vi.mock("expo-constants", () => ({
	default: { expoConfig: { version: "1.2.3" }, nativeAppVersion: "1.2.3" },
}));
vi.mock("expo-device", () => ({ isDevice: true }));
vi.mock("react-native", () => ({ Platform: { OS: "ios" } }));

import { MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED } from "./../fork-privacy";
import { MOBILE_TELEMETRY_DISABLED } from "./config";
import { initMobileTelemetry } from "./runtime";

describe("mobile PostHog fork privacy policy", () => {
	beforeEach(() => {
		vi.stubGlobal("__DEV__", false);
	});

	it("keeps the telemetry gate closed", () => {
		expect(MOBILE_EXTERNAL_TELEMETRY_EXPORT_ALLOWED).toBe(false);
		expect(MOBILE_TELEMETRY_DISABLED).toBe(true);
	});

	it("never constructs a PostHog client, even in production with opt-in env vars", () => {
		process.env.EXPO_PUBLIC_AO_TELEMETRY_DISABLED = "0";
		process.env.EXPO_PUBLIC_AO_TELEMETRY_DISABLED_EVENTS = "";
		process.env.EXPO_PUBLIC_POSTHOG_KEY = "phc_should_never_be_used";
		process.env.EXPO_PUBLIC_POSTHOG_HOST = "https://us.i.posthog.com";

		expect(initMobileTelemetry()).toBeNull();
		expect(constructed).not.toHaveBeenCalled();
	});
});
