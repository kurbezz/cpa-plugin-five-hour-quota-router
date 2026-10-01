# Local CLIProxyAPI v7.3.8 scheduler Retry-After patch

This patch is a local, reviewed source change, not a published custom image or
release. Applying/building it does not authorize installation or deployment.

## Exact provenance

- Upstream module: `github.com/router-for-me/CLIProxyAPI/v7@v7.3.8`.
- Writable copy baseline commit: `3cc6fc62c1744a4f541b4c352d9264f5c641d973`.
- Final host commit: `0f5ca46100383e73dc7f0fd6a6cf6661f32f6f0b`.
- Patch: [cpa-v7.3.8-scheduler-retry-after.patch](cpa-v7.3.8-scheduler-retry-after.patch), exact nine-file source/test diff between those commits; scratch host evidence excluded.
- Plugin status/delay implementation: `de6bf2e169ada8d4aac2d6b7e868a609eab74f75`;
  process fixture: `12ee77ae1aa8019c34637814fb618e1f0025d92a`.

The optional ABI integer delay is accepted only when positive and at most
`math.MaxInt64 / int64(time.Second)`. Malformed optional delay metadata does not
discard intended status/body. RPC transport preserves the delay and the safe
response-header contract handles wrapped HTTP429 errors. No message parsing or
arbitrary plugin error headers are enabled; upstream passthrough remains off.

## Prepare an independent writable host copy

Run from the plugin repository; set the destination to a **new** local directory.
Never patch the shared module cache. The plugin's normal `go.mod` stays unchanged.

```bash
PLUGIN_ROOT="$PWD"
CPA_STOCK="$(go list -m -f '{{.Dir}}' github.com/router-for-me/CLIProxyAPI/v7)"
CPA_LOCAL="/absolute/path/to/new-cpa-v7.3.8-retry-after"
test ! -e "$CPA_LOCAL" || exit 1
cp -R "$CPA_STOCK" "$CPA_LOCAL"
chmod -R u+w "$CPA_LOCAL"
git -C "$CPA_LOCAL" apply --check "$PLUGIN_ROOT/docs/patches/cpa-v7.3.8-scheduler-retry-after.patch"
git -C "$CPA_LOCAL" apply "$PLUGIN_ROOT/docs/patches/cpa-v7.3.8-scheduler-retry-after.patch"
```

Go 1.26 and the platform's cgo toolchain are required. Build both sides locally:

```bash
mkdir -p "$PLUGIN_ROOT/.superpowers/sdd/local-build"
(cd "$CPA_LOCAL" && go build -o "$PLUGIN_ROOT/.superpowers/sdd/local-build/cliproxyapi" ./cmd/server)
# macOS: .dylib; Linux: .so; Windows: .dll
go build -buildmode=c-shared -o "$PLUGIN_ROOT/.superpowers/sdd/local-build/five-hour-quota-router.dylib" .
```

These commands produce local artifacts only. No dependency replacement, version
bump, image publication, installation, or production change is included.

## Explicit process verification

Default tests continue resolving stock CPA. The override is test-only, validates
the module declaration and `cmd/server`, and does not change production config:

```bash
# Stock compatibility: HTTP429, no scheduler Retry-After.
env -u CPA_RETRY_TEST_MODULE_DIR go test -count=1 -v -run '^TestCLIProxyAPIProcessSchedulerRetry$' ./...
# Patched pair: known recovery HTTP429 + positive ceiling-bounded Retry-After;
# unknown recovery HTTP429 without Retry-After.
CPA_RETRY_TEST_MODULE_DIR="$CPA_LOCAL" go test -count=1 -v -run '^TestCLIProxyAPIProcessSchedulerRetry$' ./...
```

The fixture uses two synthetic physical Claude OAuth credentials: healthy A at
priority 0 and exhausted B at priority 1. Fleet admission passes because A is
healthy; scheduler subset rejection of B produces the error. Ordinary and
initial stream requests are verified against a local rejecting proxy with zero
hits. Usage responses and the host's background metadata endpoint are redirected
to localhost. No real usage or inference is needed. Already-committed SSE header
changes are outside this tested scope.

## Compatibility and client limitation

A new plugin on stock CPA exposes scheduler HTTP429 but cannot guarantee its
Retry-After header; use the patched host/plugin pair for complete transport.
Existing before-auth direct responses still work on stock CPA.

Installed OpenCode CLI v2.0.21 waiting behavior is **unverified**: its isolated
runtime test was blocked. A v2.0.20 source checkout's possible 15-minute cap is
not installed-runtime proof. Do not promise four-hour waits; the actual header
is input to client policy. Production remains stock host/old plugin pending
separate deployment approval.
