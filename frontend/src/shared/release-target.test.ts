import { describe, expect, it } from "vitest";
import {
	AUTHORIZED_RELEASE_REPOS,
	DEFAULT_RELEASE_REPO,
	UPSTREAM_RELEASE_REPOS,
	assertAuthorizedAppUpdateYml,
	buildAppUpdateYml,
	parseAppUpdateYml,
	resolveReleaseRepo,
} from "./release-target";

// The release-target authority is what stops a fork build from publishing to or
// updating from an upstream repository. These are the negative cases that must
// throw. See docs/privacy-fork.md.
describe("release target authority", () => {
	it("falls back to the authorized fork when AO_RELEASE_REPO is absent", () => {
		expect(resolveReleaseRepo(undefined)).toEqual({ owner: "EIVRAJAM", name: "agent-orchestrator" });
		expect(resolveReleaseRepo("")).toEqual({ owner: "EIVRAJAM", name: "agent-orchestrator" });
		expect(DEFAULT_RELEASE_REPO).toBe("EIVRAJAM/agent-orchestrator");
		expect(AUTHORIZED_RELEASE_REPOS).toContain("EIVRAJAM/agent-orchestrator");
	});

	it("accepts the authorized fork repo (case-insensitive, .git tolerated)", () => {
		expect(resolveReleaseRepo("EIVRAJAM/agent-orchestrator")).toEqual({ owner: "EIVRAJAM", name: "agent-orchestrator" });
		expect(resolveReleaseRepo(" eivrajam/Agent-Orchestrator.git ")).toEqual({
			owner: "eivrajam",
			name: "Agent-Orchestrator",
		});
	});

	it("rejects known upstream repos", () => {
		for (const upstream of UPSTREAM_RELEASE_REPOS) {
			expect(() => resolveReleaseRepo(upstream)).toThrow(/upstream/i);
		}
	});

	it("rejects malformed values", () => {
		for (const bad of ["justrepo", "a/b/c", "/repo", "owner/", "owner/ name", "https://github.com/a/b"]) {
			expect(() => resolveReleaseRepo(bad)).toThrow(/invalid/i);
		}
	});

	it("rejects an unauthorized but well-formed repo", () => {
		expect(() => resolveReleaseRepo("someone/else")).toThrow(/unauthorized/i);
	});

	it("builds and re-validates the generated app-update.yml", () => {
		const yml = buildAppUpdateYml(undefined);
		expect(parseAppUpdateYml(yml)).toEqual({ owner: "EIVRAJAM", name: "agent-orchestrator" });
		expect(() => assertAuthorizedAppUpdateYml(yml)).not.toThrow();
	});

	it("refuses to validate a generated app-update.yml that names upstream", () => {
		const upstreamYml = ["provider: github", "owner: Untrivial-ai", "repo: agent-orchestrator", ""].join("\n");
		expect(() => assertAuthorizedAppUpdateYml(upstreamYml)).toThrow(/upstream/i);
	});

	it("refuses to build a feed from an upstream or unauthorized repo", () => {
		expect(() => buildAppUpdateYml("Untrivial-ai/agent-orchestrator")).toThrow(/upstream/i);
		expect(() => buildAppUpdateYml("attacker/fork")).toThrow(/unauthorized/i);
	});
});
