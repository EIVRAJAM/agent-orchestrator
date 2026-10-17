#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
	copyFileSync,
	existsSync,
	mkdirSync,
	readdirSync,
	readFileSync,
	statSync,
	writeFileSync,
} from "node:fs";
import { basename, join } from "node:path";
import { fileURLToPath } from "node:url";

const VERSION_RE = /^\d+\.\d+\.\d+-privacy\.\d+$/;
const SHA_RE = /^[0-9a-f]{40}$/;

const INPUTS = [
	["linux/agent-orchestrator-linux-x64.AppImage", "AppImage"],
	["linux-deb/agent-orchestrator-linux-x64.deb", "deb"],
	["linux-rpm/agent-orchestrator-linux-x64.rpm", "rpm"],
];

function sha256(path) {
	return createHash("sha256").update(readFileSync(path)).digest("hex");
}

function sha512(path) {
	return createHash("sha512").update(readFileSync(path)).digest("base64");
}

function outputName(version, kind) {
	switch (kind) {
		case "AppImage":
			return `Agent.Orchestrator-${version}.AppImage`;
		case "deb":
			return `agent-orchestrator_${version}_amd64.deb`;
		case "rpm":
			return `agent-orchestrator-${version}-1.x86_64.rpm`;
		default:
			throw new Error(`unsupported artifact kind: ${kind}`);
	}
}

function requireEmptyDirectory(path) {
	if (!existsSync(path)) {
		mkdirSync(path, { recursive: true });
		return;
	}
	if (!statSync(path).isDirectory() || readdirSync(path).length !== 0) {
		throw new Error(`output directory must be empty: ${path}`);
	}
}

export function prepareForkLinuxRelease({ inputDir, outputDir, version, sourceSha, buildRunId, releaseDate }) {
	if (!VERSION_RE.test(version)) {
		throw new Error(`version must match X.Y.Z-privacy.N: ${version}`);
	}
	if (!SHA_RE.test(sourceSha)) {
		throw new Error(`source SHA must be 40 lowercase hex characters: ${sourceSha}`);
	}
	if (!/^\d+$/.test(String(buildRunId)) || Number(buildRunId) <= 0) {
		throw new Error(`build run ID must be a positive integer: ${buildRunId}`);
	}
	const parsedDate = new Date(releaseDate);
	if (!releaseDate || Number.isNaN(parsedDate.valueOf())) {
		throw new Error(`release date must be an ISO timestamp: ${releaseDate}`);
	}
	const normalizedReleaseDate = parsedDate.toISOString();

	const digestPath = join(inputDir, "digests", "digests.json");
	const digests = JSON.parse(readFileSync(digestPath, "utf8"));
	requireEmptyDirectory(outputDir);

	const prepared = [];
	for (const [relativeInput, kind] of INPUTS) {
		const input = join(inputDir, relativeInput);
		const inputName = basename(input);
		const expected = digests[inputName];
		if (!/^[0-9a-f]{64}$/.test(expected ?? "")) {
			throw new Error(`missing or invalid SHA-256 in digests.json for ${inputName}`);
		}
		const actual = sha256(input);
		if (actual !== expected) {
			throw new Error(`SHA-256 mismatch for ${inputName}: expected ${expected}, got ${actual}`);
		}

		const name = outputName(version, kind);
		const output = join(outputDir, name);
		copyFileSync(input, output);
		prepared.push({ name, path: output, kind });
	}

	const appImage = prepared.find(({ kind }) => kind === "AppImage");
	const feed = [
		`version: ${version}`,
		"files:",
		`  - url: ${appImage.name}`,
		`    sha512: ${sha512(appImage.path)}`,
		`    size: ${statSync(appImage.path).size}`,
		`path: ${appImage.name}`,
		`sha512: ${sha512(appImage.path)}`,
		`releaseDate: '${normalizedReleaseDate}'`,
		"",
	].join("\n");
	writeFileSync(join(outputDir, "latest-linux.yml"), feed);

	const metadata = {
		version,
		source_repository: "EIVRAJAM/agent-orchestrator",
		source_sha: sourceSha,
		build_run_id: Number(buildRunId),
		release_date: normalizedReleaseDate,
		signing: "GitHub artifact attestation backed by Sigstore",
		native_package_signing: false,
	};
	writeFileSync(join(outputDir, "release-metadata.json"), `${JSON.stringify(metadata, null, 2)}\n`);

	const checksumNames = [...prepared.map(({ name }) => name), "latest-linux.yml", "release-metadata.json"].sort();
	const checksums = checksumNames.map((name) => `${sha256(join(outputDir, name))}  ${name}`).join("\n") + "\n";
	writeFileSync(join(outputDir, "SHA256SUMS"), checksums);

	return { prepared: checksumNames, feed: "latest-linux.yml", checksums: "SHA256SUMS" };
}

function usage() {
	return "usage: prepare-fork-linux-release.mjs <input-dir> <output-dir> <version> <source-sha> <build-run-id> <release-date>";
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
	const [inputDir, outputDir, version, sourceSha, buildRunId, releaseDate] = process.argv.slice(2);
	if (!releaseDate) {
		process.stderr.write(`${usage()}\n`);
		process.exit(2);
	}
	try {
		const result = prepareForkLinuxRelease({ inputDir, outputDir, version, sourceSha, buildRunId, releaseDate });
		process.stdout.write(`${JSON.stringify(result)}\n`);
	} catch (error) {
		process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
		process.exit(1);
	}
}
