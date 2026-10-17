# Privacy fork release runbook

The privacy fork uses `.github/workflows/fork-linux-release.yml` as its only
release conductor. The workflow currently stages Linux prereleases only. It
does not publish them automatically.

The separation is intentional:

- `.github/workflows/build-artifacts.yml` builds unsigned artifacts at an exact
  source SHA and records their SHA-256 digests. It cannot create a release.
- `fork-linux-release.yml` accepts a successful build run, verifies the run's
  workflow, conclusion and source SHA, verifies the downloaded bytes against
  `digests.json`, and creates versioned AppImage, deb and rpm assets.
- `actions/attest` signs SLSA provenance for every file listed in
  `SHA256SUMS`, using a short-lived Sigstore certificate issued from GitHub's
  OIDC identity. The resulting portable bundle is included as
  `provenance.sigstore.json` and the attestation is recorded by GitHub.
- The final GitHub release remains a draft prerelease until it is reviewed and
  deliberately published by the designated release conductor.

This is cryptographic provenance for the exact downloadable bytes. It is not a
native apt/rpm repository signature. Windows Authenticode and Apple Developer
ID signing/notarization require external identities and remain blocked until
those credentials and platform verification jobs are configured. Unsigned
Windows and macOS artifacts must not be attached to a fork release.

## Stage a Linux candidate

1. Run `Desktop build artifacts` with an exact 40-character source SHA and a
   version matching `X.Y.Z-privacy.N`.
2. Wait for all platform builds and the `digests` job to pass.
3. Run `Fork Linux release conductor` with that run ID, the same SHA and the
   same version.
4. Review the `fork-release` environment deployment and the resulting draft.

The conductor permits only the repository owner, rejects a failed or unrelated
build run, rejects stable-looking versions, and refuses to reuse a tag or
modify an existing release.

## Verify a downloaded candidate

Download every Linux asset, then verify the checksums:

```bash
sha256sum --check SHA256SUMS
```

With a recent GitHub CLI, verify the GitHub-hosted attestation for an asset:

```bash
gh attestation verify Agent.Orchestrator-0.13.0-privacy.1.AppImage \
  --repo EIVRAJAM/agent-orchestrator
```

Before publishing the draft, confirm that the source SHA and build run in
`release-metadata.json` are the intended ones, all checksum and attestation
checks pass after downloading from the draft, `latest-linux.yml` names the
versioned AppImage, and installation/runtime smoke tests pass on clean Linux
hosts. Publishing remains a separate, explicit operation; do not publish from
a second account, script or workflow.
