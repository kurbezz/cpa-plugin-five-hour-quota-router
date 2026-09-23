# CLIProxyAPI Five-Hour Quota Router

Quota-aware account routing for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). The plugin routes configured protected models away from Claude OAuth accounts when Anthropic's rolling five-hour usage window reaches a configurable cutoff, while leaving other models on CLIProxyAPI's native scheduler. This release supports Anthropic's five-hour usage quota (`five_hour` in the `/api/oauth/usage` response, with a `limits[]` fallback for the newer response shape).

By default, `protected-models` is empty, which means **every** Claude model is protected. Set `protected-models` to a specific list if you only want to gate a subset of models.

## Install

Extract the built library into CLIProxyAPI's plugin directory, and configure it by plugin ID:

```yaml
plugins:
  enabled: true
  configs:
    five-hour-quota-router:
      enabled: true
      priority: 100
      # protected-models: [claude-opus-4-1]   # optional; empty/omitted protects ALL Claude models
      cutoff-percent-used: 95
      poll-interval: 60s
      request-timeout: 10s
      # user-agent: claude-code/2.1.80        # optional override, see "User-Agent" below
      overage-fallback-enabled: true          # optional; see "Overage fallback" below (default: true)
```

The library basename must be `five-hour-quota-router` with `.dylib`, `.so`, or `.dll` for the host platform.

`poll-interval` is the minimum cached-usage age before another refresh, not a continuous polling timer. Default: `60s`.

## Behavior

- Refreshes enabled physical Claude OAuth credentials when the worker starts; startup reconfiguration retries discovery only while the cache is empty. There is no time-driven polling.
- When a protected-model request selects an account whose cached usage is at least `poll-interval` old, queues one asynchronous refresh for that account while routing the current request from memory.
- Coalesces concurrent refreshes, and does not refresh a known excluded account again before its reported reset time.
- A rejection-only design cannot enforce a pre-exhaustion cutoff: the rejection arrives only after the hard limit is reached.
- Applies only to exact, case-insensitive `protected-models` matches; an empty `protected-models` list matches every non-empty Claude model name.
- Excludes an account at or above `cutoff-percent-used`.
- **Scheduling exclusion is fail-closed for credentials that have never produced a successful usage sample**: until a poll succeeds at least once for a given credential, it is excluded from protected-model scheduling. This avoids ever routing traffic to an account whose real quota state is unknown.
- **A credential whose last known reset time has passed is fail-open**: it is trusted to be available again immediately, without waiting for a fresh poll, because Anthropic's five-hour window genuinely rolls over at `resets_at`. This is a narrow, deliberate exception scoped to confirmed window expiry — not a general "unknown quota" fail-open.
- Never changes auth files or CLIProxyAPI's permanent disabled state.
- Exposes authenticated status at `GET /v0/management/plugins/five-hour-quota-router/status`.

## HTTP 429 and retry behavior

With `overage-fallback-enabled: false`, the request interceptor returns a **pre-upstream HTTP 429** only when **all physical Claude OAuth credentials** are confirmed exhausted for the current five-hour window. Confirmation requires a successful sample at or above the cutoff and a known reset time that is still in the future. An available credential, an unknown/unreachable/not-yet-polled credential, a zero reset, or a reset that has already passed does not trigger this HTTP gate.

When the earliest reset is known, the 429 includes `Retry-After` (whole seconds, rounded up) and a JSON body with `code: "five_hour_quota_exhausted"` plus a message containing `retry_after_seconds` and `resets_at`. The plugin never fabricates a wait: unknown, zero, or past reset state has no retry metadata and does not produce the pre-upstream 429.

The scheduler retains `five_hour_quota_exhausted` with the same reset-derived message metadata as a backstop if quota state changes after before-auth admission. Its ABI cannot emit an HTTP status or `Retry-After` header. Whether OpenCode retries this 429 (and honors its retry information) must be verified in the deployed provider path; do not assume OpenCode retry behavior from the plugin alone.

## User-Agent

Anthropic's `/api/oauth/usage` endpoint is undocumented, and in practice it aggressively and persistently rate-limits requests whose `User-Agent` header doesn't match Claude Code's own client string. To work around this, the plugin sends `User-Agent: claude-code/2.1.80` by default on every usage request. This is an unofficial compatibility workaround, not sanctioned by Anthropic, and may need to be updated (via the `user-agent` config field) if Anthropic changes this behavior in the future.

Note: because `activeRuntime` (and its HTTP fetcher) is constructed once at plugin process init, before the first config load, changing `user-agent` in `config.yaml` requires a plugin process restart to take effect.

## Overage fallback (`overage-fallback-enabled`)

**Default: `true`.** When every Claude candidate for a request has been **confirmed** to be over `cutoff-percent-used` (a successful usage sample exists, is not yet reset, and its `five_hour_percent_used >= cutoff-percent-used`), the plugin will, as a last resort, route the request to the confirmed-over-cutoff credential with the highest CPA `priority` (ties broken by lowest AuthID, matching the tie-break rule used for normal selection) instead of returning `five_hour_quota_exhausted`. This deliberately accepts Anthropic Extra Usage/overage billing on that one subscription, trading cost-avoidance for availability.

**Critical safety boundary — read carefully:** this fallback triggers *only* when exhaustion is confirmed for every candidate. If even one candidate is merely **unknown** — never successfully sampled, unreachable, or not yet polled — the fallback does **not** trigger, and the request still hard-blocks with `five_hour_quota_exhausted`, regardless of `overage-fallback-enabled`. The plugin will never blindly route billable traffic to a credential whose quota status it has not actually confirmed; it only does so for a credential it has confirmed is genuinely over its five-hour limit.

To restore strict hard-blocking once every account is exhausted (i.e. disable overage billing entirely), set:

```yaml
overage-fallback-enabled: false
```

## Build and test

Requires Go 1.26 and CLIProxyAPI v7.3.8 or newer.

```bash
make test
make build
```

Release tags matching `v*` build store-compatible archives and `checksums.txt` through GitHub Actions.

MIT licensed. This is a fork of [Smarty Pants Inc's cpa-plugin-quota-router v0.5.0](https://github.com/Smarty-Pants-Inc/cpa-plugin-quota-router). GitHub repository placeholder used for plugin metadata: https://github.com/kurbezz/five-hour-quota-router (not necessarily published).
