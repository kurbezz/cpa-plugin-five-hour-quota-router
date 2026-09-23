# Before-Auth Quota 429 Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Return HTTP 429 with `Retry-After` before credential selection or upstream execution when every Claude OAuth credential has confirmed-exhausted its five-hour quota and Extra Usage fallback is disabled.

**Architecture:** Extend the existing shared-library plugin with CLIProxyAPI v7.3.8 `request_interceptor`. `request.intercept_before` and `scheduler.pick` use the same `pluginRuntime` quota cache. The interceptor is admission-only: it terminates only known full exhaustion, while the scheduler remains responsible for selecting available credentials and protecting against races.

**Tech Stack:** Go; CLIProxyAPI v7.3.8 plugin C ABI; `pluginapi.RequestInterceptor`; `net/http`; current mutex-protected `quotaCache`.

---

### Task 1: Cache-level confirmed exhaustion predicate

**Files:**
- Modify: `cache.go`
- Test: `main_test.go`

- [ ] **Step 1: Write failing tests for all-confirmed exhaustion**

Add tests that seed `quotaCache` with successful samples and assert a new `allConfirmedExhausted(authIDs, now, cutoff)` helper returns true only when every ID is sampled, future-reset, and at/above cutoff.

```go
now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
cache := quotaCache{samples: map[string]quotaSample{}}
cache.recordSuccess("a", 95, now.Add(30*time.Minute), now)
cache.recordSuccess("b", 100, now.Add(10*time.Minute), now)
if !cache.allConfirmedExhausted([]string{"a", "b"}, now, 95) {
    t.Fatal("want confirmed exhaustion")
}
```

Cover an under-cutoff sibling and a never-sampled sibling; both must return false.

- [ ] **Step 2: Run the focused test to verify it fails**

Run: `go test ./... -run TestAllConfirmedExhausted -v`

Expected: compile failure because the helper does not exist.

- [ ] **Step 3: Implement the cache predicate**

Add this method to `cache.go`:

```go
func (c *quotaCache) allConfirmedExhausted(authIDs []string, now time.Time, cutoff float64) bool {
    if len(authIDs) == 0 {
        return false
    }
    c.mu.Lock()
    defer c.mu.Unlock()
    for _, authID := range authIDs {
        if !c.samples[authID].blocked(now, cutoff) {
            return false
        }
    }
    return true
}
```

Use `blocked`, not `excluded`: unknown samples must not receive a fabricated 429 wait time.

- [ ] **Step 4: Verify and commit**

Run: `go test ./... -run TestAllConfirmedExhausted -v`

Expected: PASS.

```bash
git add cache.go main_test.go
git commit -m "Add confirmed exhaustion cache predicate"
```

### Task 2: Add before-auth request interception

**Files:**
- Modify: `config.go`
- Modify: `abi.go`
- Modify: `runtime.go`
- Test: `main_test.go`
- Test: `abi_integration_test.go`

- [ ] **Step 1: Write failing runtime tests**

Use the exact `pluginapi.RequestInterceptRequest` schema from CLIProxyAPI v7.3.8. Cover these cases:

```go
// both sampled/exhausted; fallback disabled -> Terminate=true, StatusCode=429
// Retry-After is ceil(earliestReset.Sub(now).Seconds())
// response JSON contains five_hour_quota_exhausted, retry_after_seconds, resets_at
// available sibling -> Terminate=false
// unknown sibling -> Terminate=false
// fallback enabled -> Terminate=false even when all are exhausted
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./... -run TestInterceptBeforeAuth -v`

Expected: compile failure because `interceptBeforeAuth` does not exist.

- [ ] **Step 3: Advertise and dispatch the capability**

Add `RequestInterceptor bool \`json:"request_interceptor"\`` to `registrationCapabilities` in `config.go`; set it true in `pluginRegistration()`.

In `abi.go`, dispatch the v7.3.8 `pluginabi.MethodRequestInterceptBefore` method: decode `pluginapi.RequestInterceptRequest`, call `activeRuntime.interceptBeforeAuth`, and return it through `okEnvelope`.

- [ ] **Step 4: Implement the admission gate**

Implement `interceptBeforeAuth` on `pluginRuntime`:

1. Return a non-terminating response when plugin disabled, request is not Claude/protected, or `OverageFallbackEnabled` is true.
2. Derive the current physical Claude OAuth membership safely via `host.auth.list` and existing `physicalClaudeAuths`; reconcile it with the existing cache. Do not terminate based on stale/deleted auth IDs.
3. Call `allConfirmedExhausted` using that current membership. Return non-terminating response when false.
4. Find `earliestFutureReset`; use `exhaustedErrorMessage` for the safe message.
5. Return this terminated downstream response when full exhaustion is confirmed:

```go
pluginapi.RequestInterceptResponse{
    Terminate:  true,
    StatusCode: http.StatusTooManyRequests,
    ResponseHeaders: http.Header{
        "Content-Type": []string{"application/json; charset=utf-8"},
        "Retry-After":  []string{strconv.FormatInt(retryAfterSeconds, 10)},
    },
    ResponseBody: []byte(body),
}
```

Where `body` is JSON with code/message only; do not include tokens, auth IDs, emails, headers, raw usage bodies, or account metadata. If no future reset is known, still return 429 but omit `Retry-After`.

- [ ] **Step 5: Add ABI dispatch coverage**

Add an ABI test that invokes `handleMethod(pluginabi.MethodRequestInterceptBefore, ...)`, decodes the envelope, and confirms `Terminate=true`, status 429, and retry header. Add malformed JSON coverage.

- [ ] **Step 6: Verify and commit**

Run:

```bash
go test ./... -run 'TestInterceptBeforeAuth|Test.*RequestIntercept' -v
```

Expected: PASS.

```bash
git add abi.go config.go runtime.go main_test.go abi_integration_test.go
git commit -m "Add before-auth HTTP 429 quota gate"
```

### Task 3: Preserve scheduler backstop and document retry behavior

**Files:**
- Modify: `scheduler.go`
- Modify: `README.md`
- Test: `main_test.go`

- [ ] **Step 1: Keep scheduler exhaustion metadata regression coverage**

Verify `scheduler.pick` still returns `five_hour_quota_exhausted; retry_after_seconds=...; resets_at=...` when fallback is disabled and a future reset is known. This remains a backstop if scheduler state changes after before-auth admission.

- [ ] **Step 2: Document semantics**

Document that `overage-fallback-enabled: false` causes a pre-upstream 429 only when all physical Claude OAuth credentials have confirmed five-hour exhaustion. Document `Retry-After` and JSON retry metadata when a reset is known, no fabricated wait for unknown state, and that OpenCode retry behavior must be verified in the deployed provider path.

- [ ] **Step 3: Full verification and commit**

Run:

```bash
gofmt -w abi.go cache.go config.go runtime.go scheduler.go main_test.go abi_integration_test.go
gofmt -l .
go build ./...
go vet ./...
go test ./...
go test -race ./...
git diff --check
```

Expected: no formatting/diff-check output; build, vet, normal tests, and race tests pass.

```bash
git add scheduler.go README.md main_test.go
git commit -m "Document quota 429 retry behavior"
```

### Task 4: Local CPA v7.3.8 integration check

**Files:**
- No source changes required unless a durable automated test is added.

- [ ] **Step 1: Build Linux amd64 plugin**

Run:

```bash
docker run --rm --platform linux/amd64 -v "$(pwd)":/src -w /src -e GOOS=linux -e GOARCH=amd64 -e CGO_ENABLED=1 golang:1.26 sh -c "apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq gcc >/dev/null 2>&1; go build -trimpath -buildmode=c-shared -ldflags '-s -w -X main.pluginVersion=0.3.0' -o dist/five-hour-quota-router.so ."
```

- [ ] **Step 2: Verify real terminated response in local sandbox**

Use fake credentials and a local fixture usage endpoint; seed both physical Claude credentials at/above cutoff and set `overage-fallback-enabled: false`. Send a protected `/v1/messages` request.

Expected: HTTP 429, `Content-Type: application/json`, correct `Retry-After`, JSON code `five_hour_quota_exhausted`, and no fixture upstream model call.

- [ ] **Step 3: Do not commit generated artifacts or fake credentials**

Commit only durable source/tests/docs from prior tasks.
