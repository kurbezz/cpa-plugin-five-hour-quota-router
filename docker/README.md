# Patched CPA image (Linux/amd64)

Builds CPA v8.0.2 from exact public revision
`4a2c81864f31f39308e946c4c65e72147855da6e` plus the checksum-checked reviewed patch.
Go 1.26 bookworm and Debian bookworm base images are digest pinned. Server CGO is
enabled for native plugin loading. Runtime has upstream `/CLIProxyAPI` layout,
`./CLIProxyAPI` command, port 8317 and Asia/Shanghai timezone. No plugin, production
configuration, auth files, keys or local binaries enter the runtime image.

## Build and offline verification

From repository root:

```sh
revision=$(git rev-parse HEAD)
date=$(git show -s --format=%cI HEAD)
docker buildx build --platform linux/amd64 --load --target runtime \
  -f docker/cpa.Dockerfile --build-arg REPO_REV="$revision" \
  --build-arg BUILD_DATE="$date" -t cpa-candidate .
docker buildx build --platform linux/amd64 --load --target verify \
  -f docker/cpa.Dockerfile --build-arg REPO_REV="$revision" \
  --build-arg BUILD_DATE="$date" -t cpa-verifier .
docker run --rm --network none --entrypoint sha256sum cpa-candidate /CLIProxyAPI/CLIProxyAPI
docker run --rm --network none --entrypoint sha256sum cpa-verifier /CLIProxyAPI/CLIProxyAPI
# Digests above MUST match before accepting the verifier evidence.
docker run --rm --network none cpa-verifier
```

The verifier uses the final runtime binary, not a recompiled server. Its separate
test stage contains Python and a synthetic native plugin with a loopback-only
usage endpoint. Both healthy low-priority A and exhausted higher-priority B must
be known. Known reset requires positive ceiling-bounded Retry-After; unknown
requires omission; ordinary and initial-stream errors must be HTTP429 JSON with
zero proxy hits. Docker `--network none` prevents any real provider/metadata access.
The production runtime's plugin endpoint is never rewritten. Digest-pinned bases
and source do not imply a bit-for-bit reproducible apt/dependency build: apt
repositories remain external, and build-date/repository labels are inputs.

## Actions and publication

Dedicated `cpa-image.yml` validates PRs without package-write permission. Only
master push or manual dispatch on master in the designated repository may publish.
Publish job receives package-write permission, authenticates with GITHUB_TOKEN,
and loads/pushes the same image tar validated by the read-only job; it does not
rebuild. Tags are `cpa-8.0.2-retry-after` and `sha-<full repository commit>`, never
`latest`. SHA tags are a naming convention, not registry-enforced immutability;
use the reported digest for deployment. Concurrent publish runs are serialized.

Publication follows independent review. Local evidence is not CI/publication
evidence. After publication, independently pull anonymously by digest, check
Linux/amd64 and OCI labels, and verify the downloaded binary with the isolated
fixture. New GHCR packages may default to private: the package owner may need
to select package settings → visibility → public. Authenticated pull does not
prove anonymous availability; report any visibility/permissions blocker. The
first package defaults private even for a public repository; the OCI source label
links the package to its repository but does not make it public. Visibility is a
documented package-settings UI change, not an invented REST operation. Report
CI validation, publication, and anonymous digest pull as three separate gates.

## User-owned deployment

Keep installed v0.5.1 plugin and existing config/auth/plugin bind mounts. Persist
the digest image reference in Dokploy's authoritative raw/Git compose source;
do not edit regenerated host compose files. Retain old exact image digest for
rollback. Do not replace the mounted config inode or enable overage. No deployment
is automated here. Installed OpenCode retry-wait policy remains unverified; no
multi-hour waiting or zero-billing guarantee is made.
