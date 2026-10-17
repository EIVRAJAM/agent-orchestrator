import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";

import { prepareForkLinuxRelease } from "./prepare-fork-linux-release.mjs";

const VERSION = "0.13.0-privacy.1";
const SHA = "8af78ebae2214643c8c11cc1a4c4716634dbe12c";
const DATE = "2026-10-17T05:08:58.000Z";

function sha256(value) {
	return createHash("sha256").update(value).digest("hex");
}

function fixture() {
	const root = mkdtempSync(join(tmpdir(), "ao-fork-release-"));
	const inputDir = join(root, "input");
	const outputDir = join(root, "output");
	const files = {
		"linux/agent-orchestrator-linux-x64.AppImage": "appimage bytes",
		"linux-deb/agent-orchestrator-linux-x64.deb": "deb bytes",
		"linux-rpm/agent-orchestrator-linux-x64.rpm": "rpm bytes",
	};
	const digests = {};
	for (const [relative, contents] of Object.entries(files)) {
		const path = join(inputDir, relative);
		mkdirSync(dirname(path), { recursive: true });
		writeFileSync(path, contents);
		digests[path.split("/").at(-1)] = sha256(contents);
	}
	mkdirSync(join(inputDir, "digests"), { recursive: true });
	writeFileSync(join(inputDir, "digests", "digests.json"), JSON.stringify(digests));
	return { inputDir, outputDir };
}

test("verifies build digests and prepares deterministic Linux release assets", () => {
	const { inputDir, outputDir } = fixture();
	prepareForkLinuxRelease({
		inputDir,
		outputDir,
		version: VERSION,
		sourceSha: SHA,
		buildRunId: "39005191706",
		releaseDate: DATE,
	});

	const checksums = readFileSync(join(outputDir, "SHA256SUMS"), "utf8");
	assert.match(checksums, /Agent\.Orchestrator-0\.13\.0-privacy\.1\.AppImage/);
	assert.match(checksums, /agent-orchestrator_0\.13\.0-privacy\.1_amd64\.deb/);
	assert.match(checksums, /agent-orchestrator-0\.13\.0-privacy\.1-1\.x86_64\.rpm/);

	const feed = readFileSync(join(outputDir, "latest-linux.yml"), "utf8");
	assert.match(feed, /^version: 0\.13\.0-privacy\.1$/m);
	assert.match(feed, /^path: Agent\.Orchestrator-0\.13\.0-privacy\.1\.AppImage$/m);
	assert.match(feed, /^releaseDate: '2026-10-17T05:08:58\.000Z'$/m);
	assert.doesNotMatch(feed, /blockMapSize/);

	const metadata = JSON.parse(readFileSync(join(outputDir, "release-metadata.json"), "utf8"));
	assert.equal(metadata.source_sha, SHA);
	assert.equal(metadata.native_package_signing, false);
});

test("fails closed when an unsigned build artifact differs from digests.json", () => {
	const { inputDir, outputDir } = fixture();
	writeFileSync(join(inputDir, "linux", "agent-orchestrator-linux-x64.AppImage"), "tampered");
	assert.throws(
		() =>
			prepareForkLinuxRelease({
				inputDir,
				outputDir,
				version: VERSION,
				sourceSha: SHA,
				buildRunId: "39005191706",
				releaseDate: DATE,
			}),
		/SHA-256 mismatch/,
	);
});

test("rejects stable versions so this candidate flow cannot replace the stable channel", () => {
	const { inputDir, outputDir } = fixture();
	assert.throws(
		() =>
			prepareForkLinuxRelease({
				inputDir,
				outputDir,
				version: "0.13.0",
				sourceSha: SHA,
				buildRunId: "39005191706",
				releaseDate: DATE,
			}),
		/version must match/,
	);
});
