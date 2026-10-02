# CPA v8.0.2 scheduler Retry-After integration

This patch targets **only** CPA v8.0.2 base
`4a2c81864f31f39308e946c4c65e72147855da6e`; its reviewed source result is
`30a9b6c28d39ad2fd5e662a947bad706e4726ba7`. It contains ten host source/test
files, not plugin runtime changes. Plugin v0.5.1 remains unchanged.

Stock v8 does **not** expose the scheduler's retry metadata as a real HTTP
`Retry-After` header. The patch preserves a known positive retry duration across
the plugin ABI/RPC/scheduler error path. Unknown resets remain headerless.
Ordinary and initial-stream failures remain HTTP 429 JSON before any SSE begins.

## Local verification

In the plugin repository, select a local host checkout explicitly:

```sh
CPA_RETRY_TEST_MODULE_DIR=/absolute/path/to/stock-v8 \
CPA_RETRY_TEST_EXPECT_HEADER=false \
go test -run '^TestCLIProxyAPIProcessSchedulerRetry$' -count=1 -v .

CPA_RETRY_TEST_MODULE_DIR=/absolute/path/to/patched-v8 \
CPA_RETRY_TEST_EXPECT_HEADER=true \
go test -run '^TestCLIProxyAPIProcessSchedulerRetry$' -count=1 -v .
```

Only exact module declarations `github.com/router-for-me/CLIProxyAPI/v7` and
`github.com/router-for-me/CLIProxyAPI/v8` are accepted. A v8 override requires an
explicit boolean expectation; override does not imply patched. No override still
uses stock v7 and expects no header. Historical patched-v7 overrides without the
new variable still expect a header; set `false` explicitly to test stock-v7 copies.
The selected module path also controls the link-time metadata endpoint override,
which points to the local fixture, never an Internet fallback. All usage tokens,
management credentials, quota endpoints, and proxy endpoints in these tests are
synthetic/local; no production inference is sent.

## Build and compatibility

Apply with `git apply --check` and `git apply` in a separate checkout of the exact
base, never by overwriting an active checkout. Build Linux/amd64 with
**CGO_ENABLED=1**. CGO-disabled server binaries do not provide native plugin
loading and must not be shipped for this setup.

The pinned host's official Dockerfile uses `golang:1.26-bookworm` plus a Debian
bookworm runtime, **not Alpine/musl**. Local inspection of the available public
CPA runtime also found Debian 12/glibc. Do not use an Alpine toolchain based on
an assumption. The exact installed production image ID was not available locally;
the public runtime is a different image ID, so local runtime evidence must not be
represented as exact-production-image validation.

Build the pinned host in a Linux/amd64 Go 1.26 bookworm builder with GCC. Use
`-buildvcs=false -trimpath`, and set `main.Version=8.0.2`,
`main.Commit=30a9b6c28d39ad2fd5e662a947bad706e4726ba7`, and a truthful
`main.BuildDate` through `-ldflags -X`. `main` assigns these values into
`internal/buildinfo` during init; setting only the latter is insufficient.
Record `go version -m`, `ldd`, and SHA-256 for the resulting binary. Keep the
existing production plugin; the locally rebuilt v0.5.1 library is test evidence,
not a plugin upgrade or byte-identical copy of the installed plugin.

Local build outputs/evidence are under ignored `dist/v8.0.2-retry/` and
`.superpowers/sdd/2026-10-02-v8-retry-update/`. Consult `integration-report.md`
for completed versus pending checks and artifact hashes. No image is published.

## Manual production update boundaries

The user owns any eventual production update. After source review and all
compatibility gates, derive an image from the **exact retained old image ID**,
copying only the approved CGO-enabled binary. This preserves OS libraries and
the inherited entrypoint/command. The ignored artifact Dockerfile expresses this
shape; it is not evidence that the exact production base was tested locally.

The service is Dokploy compose project `easycliproxyapi-proxy-zsps7r`, service
`cli-proxy-api`. Determine whether its authoritative compose source is managed
raw YAML or a Git repository. Persist only the unique image reference and suitable
pull policy **there**, not by editing Dokploy's generated compose file. A local-only
image requires `pull_policy: never` and availability on the intended host; an
approved registry image requires its corresponding persistent source settings.
Do not use floating `latest` as an immutable rollback target.

Preserve all existing service properties, auth/plugin mounts, and config file
device/inode. Do not replace the mounted config, introduce a binary bind mount,
restart other services, or enable overage. Retain the exact old image under an
immutable rollback tag; rollback must restore that image and a compatible pull
policy in the same authoritative source. Validate native plugin loading and
non-inference readiness before acceptance. Exact deployment commands are omitted
because the authoritative Dokploy workflow remains unverified.

Installed OpenCode retry-wait behavior has not been verified. A valid Retry-After
header is not a promise that an installed client will wait four hours or retry
automatically. Local wire tests establish server behavior only.
