/**
 * Authorized release target for this fork of Agent Orchestrator.
 *
 * This module is the single source of truth for which GitHub repository the
 * desktop build may publish to, update from, and read its version floor from.
 * It exists so an install of this fork can never be silently replaced by an
 * upstream artifact with a different privacy policy (see docs/privacy-fork.md).
 *
 * Rules:
 *   - `AUTHORIZED_RELEASE_REPOS` is the explicit, reviewed allowlist. A
 *     legitimate fork rename or ownership change is made HERE, deliberately.
 *   - `UPSTREAM_RELEASE_REPOS` are known upstream homes that must never be a
 *     build target; they are rejected explicitly, not silently defaulted past.
 *   - A blank `AO_RELEASE_REPO` falls back to the authorized fork.
 *   - Any other value throws, so a build cannot point the updater at an
 *     arbitrary repository by setting an environment variable.
 *
 * `buildAppUpdateYml` produces the baked `app-update.yml` from this authority,
 * and `assertAuthorizedAppUpdateYml` re-validates the *written* artifact, so the
 * check is on the generated file, not only on the source strings.
 */

/** Reviewed allowlist of repositories this fork may build/publish/update from. */
export const AUTHORIZED_RELEASE_REPOS = ["EIVRAJAM/agent-orchestrator"] as const;

/** Known upstream homes. Never a valid build target for this fork. */
export const UPSTREAM_RELEASE_REPOS = [
	"Untrivial-ai/agent-orchestrator",
	"aoagents/agent-orchestrator",
	"AgentWrapper/agent-orchestrator",
] as const;

/** Default release repo when AO_RELEASE_REPO is unset. */
export const DEFAULT_RELEASE_REPO: string = AUTHORIZED_RELEASE_REPOS[0];

export type ReleaseRepo = { owner: string; name: string };

const REPO_SLUG = /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/;

function normalized(value: string): string {
	return value.trim().replace(/\.git$/i, "");
}

function splitSlug(slug: string): ReleaseRepo {
	const [owner, name] = slug.split("/");
	return { owner, name };
}

function matches(list: readonly string[], slug: string): boolean {
	return list.some((entry) => entry.toLowerCase() === slug.toLowerCase());
}

/**
 * Resolve an "owner/repo" value (typically AO_RELEASE_REPO) to the authorized
 * release repo. Throws on malformed, upstream, or unauthorized values so a build
 * fails loudly instead of producing an artifact pointed at the wrong feed.
 */
export function resolveReleaseRepo(value: string | undefined): ReleaseRepo {
	const raw = normalized(value ?? "");
	if (!raw) return splitSlug(DEFAULT_RELEASE_REPO);
	if (!REPO_SLUG.test(raw)) {
		throw new Error(
			`invalid AO_RELEASE_REPO ${JSON.stringify(value)}: expected "owner/repo"`,
		);
	}
	if (matches(UPSTREAM_RELEASE_REPOS, raw)) {
		throw new Error(
			`refusing upstream release repo ${raw}: this fork must not publish to or update from an upstream repository`,
		);
	}
	if (!matches(AUTHORIZED_RELEASE_REPOS, raw)) {
		throw new Error(
			`unauthorized AO_RELEASE_REPO ${raw}: add it to AUTHORIZED_RELEASE_REPOS in frontend/src/shared/release-target.ts after review`,
		);
	}
	return splitSlug(raw);
}

/** Build the baked app-update.yml from the authorized release target. */
export function buildAppUpdateYml(value: string | undefined): string {
	const { owner, name } = resolveReleaseRepo(value);
	return [
		"provider: github",
		`owner: ${owner}`,
		`repo: ${name}`,
		"updaterCacheDirName: agent-orchestrator-updater",
		"",
	].join("\n");
}

/** Parse owner/repo out of an app-update.yml body. Throws if either is absent. */
export function parseAppUpdateYml(yml: string): ReleaseRepo {
	const owner = /^owner:\s*(.+)$/m.exec(yml)?.[1]?.trim();
	const repo = /^repo:\s*(.+)$/m.exec(yml)?.[1]?.trim();
	if (!owner || !repo) throw new Error("app-update.yml is missing owner or repo");
	return { owner, name: repo };
}

/**
 * Validate the *generated* app-update.yml, so the check is on the artifact that
 * ships rather than only on the source that produced it.
 */
export function assertAuthorizedAppUpdateYml(yml: string): void {
	const { owner, name } = parseAppUpdateYml(yml);
	resolveReleaseRepo(`${owner}/${name}`);
}
