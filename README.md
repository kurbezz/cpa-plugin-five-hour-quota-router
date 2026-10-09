# CLIProxyAPI Five-Hour Quota Router

Quota-aware account routing for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). The plugin routes configured protected models away from Claude OAuth accounts when applicable shared or model-family quota windows reach a configurable cutoff, while leaving other models on CLIProxyAPI's native scheduler. The default cutoff remains 95%.

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

- **Applicable windows.** Routing evaluates shared five-hour and shared weekly usage, plus a recognized Fable, Opus, or Sonnet weekly window when the requested model identifies that family. Known exhausted applicable windows exclude that account for that request; a Fable-only limit does not block Sonnet or Opus. Existing priority/weight/AuthID ordering and overage configuration remain in effect.
- **Usage endpoint.** `/api/oauth/usage` is undocumented. The plugin accepts observed legacy fields (`five_hour`, `seven_day`, and family-specific weekly fields) and newer `limits[]` entries. Scoped limits are recognized only when `scope.model.id` or `scope.model.display_name` identifies a known family; custom aliases are not inferred. Usage utilization is expressed in percentage points.
- **Fast path for correlated active requests: response headers.** When CLIProxyAPI's after-auth hook correlates a successful Claude request's RequestID to its selected physical OAuth credential, the plugin observes shared `5h-*` and `7d-*` headers, and the `7d_oi-*` family signal only for an identifiable Fable request. Header utilization is a fraction (`0.23` = 23%) and is converted to percent. Partial observations update only supplied windows and do not erase other windows. Missing correlation, unknown/replaced credentials, or unsafe exhausted headers with no reset are ignored. **Only successful responses are observed**; Anthropic error/429 responses are not observed here.
- **Reliable startup/request-driven source: usage polling.** The worker refreshes enabled physical Claude OAuth credentials on startup. There is no time-driven polling loop. Protected request evaluation can asynchronously queue due refreshes, including excluded accounts with unknown recovery, while routing the current request from cache. Known future-reset exhaustion of shared five-hour or weekly windows may suppress redundant polls; model-only exhaustion does not suppress discovery for other scopes.
- **Per-account refresh pause after a failed usage fetch.** The next attempt waits for upstream `Retry-After` (or the earliest `anthropic-ratelimit-*-reset`) when present; otherwise 5 minutes, doubling per consecutive failure up to 30 minutes, plus up to 10% jitter. A 429 never pauses for less than 5 minutes. No path (startup/reload pass, request-driven refresh, revision check) fetches usage for that account during the pause; routing keeps using its last successful windows, and windows whose reset has passed count as reset. A successful fetch clears the pause. Each failed attempt logs one line with HTTP status, short upstream error text, failure number and next attempt time; status exposes `last_refresh_error`, `consecutive_failures` and `next_refresh_at` per account.
- Coalesces concurrent refreshes. A future reset suppresses refresh only when applicable shared windows are exhausted; model-family exhaustion alone does not prevent discovery for other scopes.
- A rejection-only design cannot enforce a pre-exhaustion cutoff: the rejection arrives only after the hard limit is reached.
- Applies only to exact, case-insensitive `protected-models` matches; an empty `protected-models` list matches every non-empty Claude model name.
- Excludes an account at or above `cutoff-percent-used`.
- **Five-hour scheduling exclusion is fail-closed for credentials that have never produced a valid five-hour sample**: that sample can come from a successful usage poll or a valid correlated successful-response header observation.
- **A window whose known reset has passed no longer blocks on its own**. Other still-active applicable shared or model-family windows may continue to exclude the credential.
- Never changes auth files or CLIProxyAPI's permanent disabled state.
- Exposes authenticated status at `GET /v0/management/plugins/five-hour-quota-router/status`.

## HTTP 429 and retry behavior

With `overage-fallback-enabled: false`, the request interceptor returns a **pre-upstream HTTP 429** only when **all current physical Claude OAuth credentials** are confirmed exhausted for the requested model. Applicable blockers include shared and recognized model-family windows. Every blocking window needs a known future reset; any available or unknown alternative, or a missing blocking reset, prevents the fleet 429.

For each account, recovery is the latest reset among its exhausted applicable windows; fleet recovery is the earliest account recovery. The 429 includes `Retry-After` (whole seconds, rounded up) and JSON `code: "five_hour_quota_exhausted"` only when every current account is exhausted and every blocking reset is known. Unknown or zero reset state never fabricates finite retry metadata or a fleet 429. The historical error code still says “five_hour” when weekly quota causes exhaustion.

The scheduler retains `five_hour_quota_exhausted` as a backstop after before-auth admission, including when the host supplies only an exhausted higher-priority candidate subset while another physical account remains healthy. Scheduler rejection now emits ABI `http_status: 429` and optional positive `retry_after_seconds`, computed with the same ceiling as its historical message metadata. Unknown candidates or missing/unrepresentable recovery omit the delay; selection and overage fallback policy are unchanged.

**Host compatibility:** stock CLIProxyAPI v7.3.8 consumes HTTP 429 but ignores the optional scheduler delay, so it cannot emit scheduler `Retry-After`. The [local v7.3.8 host patch and build guide](docs/patches/cpa-v7.3.8-scheduler-retry-after.md) provide structured delay transport and safe header writing without enabling arbitrary plugin headers or upstream header passthrough. Before-auth direct-response 429/Retry-After remains unchanged and works on stock hosts. Local process tests verify ordinary errors and initial streaming errors before SSE starts; they do not cover changing headers after SSE has already been committed.

**Client waiting is not verified.** The installed OpenCode CLI v2.0.21 isolated runtime test was blocked. A separate v2.0.20 source checkout suggests a possible 15-minute retry cap, but is not proof of installed-client behavior. A real `Retry-After` enables the client's retry policy; it does not promise a four-hour wait or automatic retry in every provider path. This is a local source/test change, not a release or deployment: production remains on the stock host and old plugin until separately approved.

## User-Agent

Anthropic's `/api/oauth/usage` endpoint is undocumented, and in practice it aggressively and persistently rate-limits requests whose `User-Agent` header doesn't match Claude Code's own client string. To work around this, the plugin sends `User-Agent: claude-code/2.1.80` by default on every usage request. This is an unofficial compatibility workaround, not sanctioned by Anthropic, and may need to be updated (via the `user-agent` config field) if Anthropic changes this behavior in the future.

Note: because `activeRuntime` (and its HTTP fetcher) is constructed once at plugin process init, before the first config load, changing `user-agent` in `config.yaml` requires a plugin process restart to take effect.

## Overage fallback (`overage-fallback-enabled`)

**Default: `true`.** When every Claude candidate for a request is confirmed exhausted for its applicable shared and model-family windows, the plugin may route to an excluded candidate as a last resort, preserving priority/weight/AuthID ordering. This deliberately accepts possible Anthropic Extra Usage/overage billing, trading cost-avoidance for availability.

**Critical safety boundary — read carefully:** unknown five-hour state remains excluded and cannot authorize overage fallback. Missing/unrecognized model-family quota alone does not exclude a credential (availability is the explicit policy), and is not a guarantee against Extra Usage. A model-specific exhausted window does not block unrelated model families.

Management status retains legacy account `blocked` as the five-hour blocked/excluded value and adds bounded per-window scope, utilization, reset/sample times, source, and blocked state. Window-level `blocked` is model-independent and reflects that window's own cutoff/reset; routing applies only relevant scopes.

To disable intentional fallback to confirmed-exhausted accounts, set `overage-fallback-enabled: false`. This does not guarantee zero Extra Usage: missing or unrecognized model quotas remain allowed by policy, and quota observations may be incomplete or stale.

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
