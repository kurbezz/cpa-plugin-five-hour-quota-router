# Patched CPA container in the plugin repository

The user approved implementing, running and publishing a Linux/amd64 Docker
image from this repository to GHCR. Deployment remains user-owned.

## Contract

- Image: `ghcr.io/kurbezz/cpa-plugin-five-hour-quota-router`.
- Human-readable tag: `cpa-8.0.2-retry-after`; also publish a repository-commit
  tag and provide the registry digest for immutable deployment/rollback.
- Fetch public upstream source only at
  `4a2c81864f31f39308e946c4c65e72147855da6e` (CPA v8.0.2); validate checkout SHA.
- Apply the tracked, reviewed v8 patch and fail on a mismatch.
- Go 1.26 bookworm, Linux/amd64, CGO enabled, Debian bookworm runtime. Record
  upstream revision, plugin-repository revision and patch digest in OCI labels.
- Keep upstream working directory, command, port, example configuration and
  runtime layout compatible. Do not embed production config, auth, credentials
  or scratch artifacts. The installed plugin remains separate and mounted.
- Add a dedicated workflow; do not repurpose or silently change the existing
  plugin-release workflow/registry. Use minimal contents-read/packages-write
  permissions and the workflow GITHUB_TOKEN, never a copied user token.
- Pull requests may validate but must not push images. Publishing is restricted
  to approved manual dispatch or the designated default branch workflow.
- Before push, validate the exact candidate image binary with a synthetic
  native plugin and local fixtures: ordinary and initial-stream scheduler
  rejection give 429 plus positive Retry-After for known reset; unknown reset
  gives no header. Confirm native plugin loading and zero provider calls.
- Ensure test networking cannot call real providers; synthetic test endpoints
  and credentials only. Build-time public dependency/source fetch is allowed.
- Publish only after validation; independently download the published image by
  digest, verify labels/platform and run the non-inference check again.
- Anonymous GHCR pull is required for the user's deployment. If GitHub package
  visibility or permissions block it, report the precise manual step rather than
  treating an authenticated pull as proof of public availability.

## Evidence and boundaries

Parent owns reconciliation; one GPT implementation writer owns Docker/CI/tests
and docs. Independent GPT review checks source pinning, build context, token
permissions, image verification and publishing conditions. Publication follows
the review, and the parent verifies Actions results and the downloaded digest.
The image fixes server transport; installed OpenCode waiting policy is still not
verified and no zero-billing or multi-hour waiting guarantee is made.
