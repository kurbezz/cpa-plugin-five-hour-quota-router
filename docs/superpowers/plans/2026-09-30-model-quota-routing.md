# Model-aware Claude Quota Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Apply shared weekly and known model-family weekly quotas to Claude routing without blocking unrelated models.

**Architecture:** Five bounded window slots, independent timestamps, one pure decision evaluator, and one identity-guarded batch merge. Reuse request-driven asynchronous workers and after-auth RequestID correlation; do not introduce synchronous usage I/O or change production.

**Tech Stack:** Go, CPA SDK v7.3.8, c-shared ABI, httptest and real local CPA process fixture.

---

## Ownership, dependencies and verification budget

- Parent owns coordination, scope and final evidence reconciliation.
- One implementation writer owns source, tests and README below. Sequential tasks
  1–5 share the cache/runtime contract and must not have overlapping writers.
- A read-only specialist may independently audit public usage fixtures without
  modifying the repository. Its result is advisory, not production evidence.
- Independent reviewer owns family applicability, atomic admission, incarnation,
  batch freshness and recovery review after writer completion.
- Writer runs build/vet/full/race and process tests once on the final state;
  parent inspects evidence/diff and repeats only changed or uncertain checks.
- No pushes, releases, registry changes, production SSH or actual Anthropic calls.
- Work directly in the existing checkout (prior user explicitly declined worktrees).

## File responsibility map

- Create `quota_windows.go`: bounded scopes, observations, family resolution,
  pure decision and recovery evaluator.
- Create `quota_windows_test.go`: parser-independent decision/resolver tests.
- Modify `usage.go`: decode all supported endpoint shapes into a batch.
- Create `usage_windows_test.go`: httptest parser fixtures, legacy compatibility.
- Modify `headers.go`: successful-response batch parser, existing 5h compatibility.
- Modify `cache.go`: per-window storage, guarded merge, coherent decisions,
  independent poll eligibility, compatible status.
- Modify `runtime.go`: batch poll/header commits, due excluded-account refresh,
  model-aware admission and unchanged lifecycle protections.
- Modify `scheduler.go`: model-aware candidate snapshot/weight/overage/retry.
- Modify `headers_test.go`, `main_test.go`: preserve v0.4 guarantees and add races.
- Modify `process_e2e_test.go`: real process model-scoped usage gate.
- Modify `README.md`: supported shapes, applicability, missing-data limitations,
  reset arithmetic and status semantics.
- Leave `registry.json`, plugin version and release artifacts unchanged.

## Task 1: Bounded domain and pure routing decision

- [ ] Add resolver table tests before production code. Cases: `claude-fable-5`,
  `CLAUDE-FABLE-5-1`, `claude-opus-4-1-20250805`, `claude-sonnet-4-6`, unknown
  `custom-chat`, `claude-sonnetish`, `Fable`, `Claude 3.5 Fable`, conflicting
  ID `opus`/display `Sonnet`. Unknown/conflicting cases yield no scoped family.
- [ ] Run `go test ./... -run 'TestModelFamily|TestQuotaDecision' -count=1` and
  record the expected missing-feature failure before implementation.
- [ ] Define five fixed scopes and a window observation (`percent`, `reset`,
  `sampledAt`, `source`, `valid`). Keep scope names bounded and sanitized.
  Decisions expose excluded, confirmedExhausted, recoveryKnown, recoveryAt.
  All applicable blocking windows contribute to recovery; unknown five-hour
  never confirms exhaustion. Core aggregation:

```go
// Within one account's evaluator, for each applicable blocking window:
if window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
    recoveryKnown = false
} else if window.ResetAt.After(recoveryAt) {
    recoveryAt = window.ResetAt
}
// Across fully confirmed accounts with known recovery:
if earliest.IsZero() || accountRecovery.Before(earliest) {
    earliest = accountRecovery
}
```

- [ ] Tests: A blocked five-hour +1h and Fable +8h; B Fable +3h gives fleet
  +3h for Fable, not +1h; Sonnet unaffected by Fable; shared weekly blocks all;
  absent scoped quota allowed; missing blocking reset has no finite recovery;
  healthy resets irrelevant; expired resets stop blocking.
- [ ] Run focused tests, then commit domain/tests only.

## Task 2: Usage and header observation batches

- [ ] Add endpoint tests using httptest. Representative synthetic response:

```json
{"five_hour":{"utilization":20,"resets_at":"2099-01-01T00:00:00Z"},"limits":[{"kind":"weekly_all","percent":30,"resets_at":"2099-01-03T00:00:00Z"},{"kind":"weekly_scoped","percent":96,"resets_at":"2099-01-05T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}]}
```

- [ ] Test legacy seven_day/opus/sonnet/fable, session array fallback, invalid
  optional quota preserving valid session, null/missing reset, conflicting scope
  IDs/labels, unknown kind, flat/array precedence, conflicting duplicate scopes.
- [ ] Run `go test ./... -run 'TestUsageWindows|TestHeaderWindows' -count=1`;
  record a missing-window failure.
- [ ] Extend usageResult with a bounded observation batch while retaining existing
  five-hour fields/test seams as needed. Prefer valid newer arrays per scope;
  conservative duplicates use max utilization/latest exhaustion reset. Do not
  emit raw labels or optional parse errors containing response bodies.
- [ ] Add header batch tests: 5h, shared 7d, Fable-only 7d_oi; latter ignored on
  Opus/unknown requests. Fractions convert to percent, rejected forces 100;
  high missing-reset header is ignored, low missing-reset is accepted.
- [ ] Implement batch parsers, run targeted tests and commit.

## Task 3: Guarded independent cache merges and refresh eligibility

- [ ] Add deterministic channel/clock tests for a poll started at T, 5h header
  at T+1, poll completion T+2: old 5h rejected, untouched weekly accepted.
  Test timestamp ties, partial headers, invalid observations, all replacement
  classes (path/identity, revision-only, remove/re-add), unknown retries.
- [ ] Run `go test ./... -run 'TestWindowMerge|TestWindowRefresh' -count=1` and
  record the expected stale-update/eligibility failure.
- [ ] Store per-window states and poll-only attempt/success times. Batch commit
  validates physical binding/generation/incarnation once under the cache lock,
  then independently checks timestamps. Capture bound incarnation for poll work;
  rejected earlier work cannot populate a replacement. Never clear omitted
  windows or consume correlation before processing the entire header batch.
- [ ] Update existing test helper writes to preserve old five-hour behavior
  without creating a second production decision path. Preserve status fields.
- [ ] Queue request-driven due recovery even if every account is excluded.
  Header freshness cannot advance poll clock. Shared future-reset exhaustion
  can avoid redundant fetches, model-only exhaustion cannot veto discovery for
  other scopes. High no-reset usage samples stay excluded but can refresh after
  the interval. Optional absence never causes immediate retry loops.
- [ ] Tests: frequent 5h headers do not starve weekly poll, Fable blocked does not
  suppress Sonnet/shared discovery, sole excluded/no-reset account refreshes on
  next request and low sample restores routing, all confirmed shared blocked
  accounts remain idle, shutdown/config/revision tests remain valid.
- [ ] Run focused/race tests and commit cache/worker changes.

## Task 4: Scheduler, model-aware HTTP gate and status

- [ ] Add tests for Fable routing around exhausted accounts while Sonnet selects
  the original high-weight account; mirror Opus/Sonnet scopes. Include shared
  weekly, unknown model quota, unknown session and mixed exhausted scopes.
- [ ] Run `go test ./... -run 'TestModelQuotaRouting|TestModelQuotaAdmission' -count=1`
  and record expected routing/gate failure.
- [ ] Take a coherent cache decision snapshot for scheduler candidates. Reuse
  priority/weight/ID order and prohibit overage unless every candidate is
  confirmed exhausted for this model. Queue due refresh for excluded candidates.
- [ ] Admission uses authoritative nonempty Model or RequestedModel fallback.
  Atomically evaluate live fleet membership with the same domain evaluator.
  Preserve conservative alternative/unknown fail-open admission semantics,
  request protection, old error code and zero synchronous auth.get/network.
- [ ] Test 429 only overage-disabled/all exhausted/known blocking resets;
  ceil Retry-After from min account max blocking resets. Missing reset supplies
  no fabricated retry; newly discovered member prevents stale fleet 429.
- [ ] Add window-level status while retaining account blocked's historical
  five-hour meaning. Assert serialized status/logs contain no raw metadata,
  arbitrary scope labels, credential digests or fixture token secrets.
- [ ] Run focused tests/race and commit routing/status changes.

## Task 5: Real CPA boundary, documentation and final gates

- [ ] Extend local process fixture with a model-quota scenario: both accounts
  five-hour healthy, Fable scoped weekly >=95 with known resets, protected Fable
  model registered. Verify status shows both scoped samples, Fable HTTP429,
  Retry-After correct and zero upstream proxy hits. A separately registered
  protected Sonnet request must not be quota-rejected solely by Fable and reaches
  the rejecting fixture proxy (no external inference).
- [ ] Run that process test before implementing scenario support to verify the
  new assertion fails on old behavior, then rerun after integration. Maintain
  existing process coverage for shared five-hour and overage behavior.
- [ ] Update README with examples and known limitations: undocumented endpoint,
  absent scoped limits allowed, custom aliases unsupported, error responses not
  observed, window-level status and Extra Usage not absolutely guaranteed.
- [ ] Run final gates without shell pipelines masking failures:

```bash
gofmt -l *.go
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
git diff --check
git status --short
```

- [ ] Report normal/race counts and process evidence (status/header/zero hits),
  changed files, commit hashes and known limitations. Save a report under
  `.superpowers/model-quotas-2026-09-30/implementation-report.md` (ignored).
- [ ] Commit source/tests/docs only; no push/version/registry/release.
- [ ] Parent requests one independent review against approved spec and this
  plan. Fix only concrete findings with reproducing tests and proportionate
  revalidation; reconcile final evidence before claiming completion.
