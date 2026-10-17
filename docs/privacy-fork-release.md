# Privacy fork release runbook

The privacy fork uses `.github/workflows/fork-linux-release.yml` to prepare an
attested Linux candidate for handoff to a separate private release conductor.
The public workflow cannot create or modify releases or tags.

The separation is intentional:

- `.github/workflows/build-artifacts.yml` builds unsigned artifacts at an exact
  source SHA and records their SHA-256 digests. It cannot create a release.
- `fork-linux-release.yml` accepts a successful build run, verifies the run's
  workflow, conclusion and source SHA, verifies the downloaded bytes against
  `digests.json`, and creates versioned AppImage, deb and rpm assets.
- `actions/attest` creates signed provenance for every file listed in
  `SHA256SUMS`, using a short-lived Sigstore certificate issued from GitHub's
  OIDC identity. The resulting portable bundle is included as
  `provenance.sigstore.json` and the attestation is recorded by GitHub.
- The signed candidate is retained as a workflow artifact for 30 days. The
  private conductor must redownload and verify it before staging a draft.
- The public flow creates **no** draft, release or tag. No draft or release
  exists until the private conductor stages one; this runbook only prepares and
  attests the candidate bytes.

This is cryptographic provenance for the exact downloadable bytes. It is not a
native apt/rpm repository signature. Windows Authenticode and Apple Developer
ID signing/notarization require external identities and remain blocked until
those credentials and platform verification jobs are configured. Unsigned
Windows and macOS artifacts must not be attached to a fork release.

## Prepare a signed Linux candidate

1. Run `Desktop build artifacts` with an exact 40-character source SHA and a
   version matching `X.Y.Z-privacy.N`.
2. Wait for all platform builds and the `digests` job to pass.
3. Run `Fork Linux signed candidate` with that run ID, the same SHA and the
   same version.
4. Download the resulting `fork-linux-X.Y.Z-privacy.N` workflow artifact from
   the successful run and hand it to the private conductor.

The public candidate workflow permits only the repository owner, rejects a
failed or unrelated build run, rejects stable-looking versions, and refuses to
prepare a candidate whose version already has a tag or release. These are
read-only checks; the workflow has no `contents: write` permission.

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

Before a private conductor stages a draft, confirm that the source SHA and
build run in `release-metadata.json` are the intended ones, all checksum and
attestation checks pass after downloading the temporary workflow artifact,
`latest-linux.yml` names the versioned AppImage, and installation/runtime smoke
tests pass on clean Linux hosts. The conductor must then verify the uploaded
draft assets again before publication. The public source repository must remain
read-only at the release/tag boundary. Publishing remains a separate, explicit
operation in exactly one private conductor; do not publish from a second
account, script or workflow.

## Private conductor requirement

Publication belongs in a private repository with an environment-protected
workflow. That workflow must accept the successful public candidate run ID,
version and source SHA; download the temporary artifact; verify
`SHA256SUMS`, `release-metadata.json` and the GitHub attestation; perform clean
installation/runtime smoke tests; create a draft release in
`EIVRAJAM/agent-orchestrator`; redownload and verify the draft; and require a
final environment approval before publishing it.

Prefer a GitHub App installation token limited to this repository with
`Contents: read and write` for tags and releases. A fine-grained personal access
token with the same single-repository permission is an acceptable fallback.
Do not store the operator's existing GitHub CLI OAuth token as a secret. Native
deb/rpm repository signing, Windows Authenticode, and Apple signing/notarization
remain separate credentials and gates; provenance attestation must not be
described as those native signatures.
