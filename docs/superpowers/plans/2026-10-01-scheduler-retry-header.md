# Scheduler Retry-After Implementation Plan

> **For agentic workers:** Use subagent-driven-development or executing-plans for the bounded lanes below.

**Goal:** Scheduler quota errors return wire HTTP 429 with a reliable reset-derived Retry-After.

**Architecture:** Optional structured integer delay crosses the plugin ABI, is validated by the host, and is written through a narrow safe-response-header contract. Selection and fleet admission remain unchanged.

**Tech Stack:** Go, CPA v7.3.8 source copy, c-shared plugin ABI, local HTTP/process fixtures, installed OpenCode v2.0.21.

## Authority and global constraints

Spec: `docs/superpowers/specs/2026-10-01-scheduler-retry-header-design.md`, user approved full host + plugin implementation.
No production changes, publishing, installation, real upstream inference or usage requests.
Every subagent explicitly uses a discovered GPT model. Never modify the shared Go module cache.
Parent owns integration and final evidence; writer owns scoped tests and validation.

## Work graph and ownership

1. Parent prepares `/Users/kurbezz/projects/auth-scheduler/CLIProxyAPI-retry-after`
   as a writable independent copy of exact cached v7.3.8; initializes local Git
   baseline for a reviewable patch. This is not a read-only dependency clone.
2. Host writer modifies only that CPA copy, focused ABI/RPC/handler tests.
3. Plugin writer independently modifies only `abi.go`, `scheduler.go`, narrowly
   shared reset-to-delay helper in `cache.go`, and plugin unit/ABI tests.
4. Client investigator independently tests installed OpenCode against localhost
   429 fixtures. It cannot write host/plugin source or change real providers.
5. Integration writer waits for host + plugin, then adds process test source
   override and exhausted-tier test, compatibility docs and reusable CPA patch.
6. Independent review checks final coordinated diff and verified error path.

## Shared ABI contract

```go
HTTPStatus int `json:"http_status,omitempty"`
RetryAfterSeconds *int64 `json:"retry_after_seconds,omitempty"`
```

Positive seconds only, bounded so conversion to Go duration is representable
(`0 < seconds <= math.MaxInt64 / int64(time.Second)`). Unknown/invalid delays
must not produce a header. HTTP status 429 remains independently valid.
Host provides a narrow `RetryAfterSeconds() int64` error contract paired with
`StatusCode() int`; safe extraction supports wrapped errors and only 429.
Existing built-in safe auth headers remain intact. Do not parse legacy message
text and do not permit arbitrary plugin headers. Invalid optional retry input
must not suppress the intended status/body; add decoding validation tests.

## Lane A: CPA host

- [ ] Add red tests for optional ABI delay, RPC preservation and invalid values,
  and handler wire header with passthrough disabled (including wrapped errors).
- [ ] Extend `sdk/pluginabi/types.go`, `internal/pluginhost/rpc_client.go` and
  the existing safe error-header helper in `sdk/cliproxy/auth/home_concurrency.go`
  or a small adjacent file. Touch handler writer only if the existing safe path
  is insufficient; cover ordinary and initial stream failure paths.
- [ ] Run focused affected package tests and race, build server, vet affected
  packages, formatting/diff checks. Commit local CPA change, report signatures,
  exact baseline provenance, command/output evidence and patch generation path.

## Lane B: Plugin

- [ ] Add scheduler/ABI tests that fail on missing http_status and delay fields.
- [ ] Compute both legacy text metadata and structured seconds from the same
  coherent reset decision. Emit 429 for scheduler exhausted errors; optional
  positive ceil delay only for reliable future recovery. Unknown candidates or
  unknown recovery do not fabricate delay. Keep selection/fallback unchanged.
- [ ] Run focused red/green, build/vet/full/race/diff. Commit only bounded source
  and tests; do not bump version, registry or normal SDK dependency.

## Lane C: Installed OpenCode

- [ ] Discover installed v2.0.21 CLI test/config facilities without printing
  credentials. Use isolated HOME/config and synthetic provider key only.
- [ ] Serve local synthetic 429 with Retry-After: 3, then success if safe.
  Record request timestamps and observable retry delay, for compatible provider
  path. Hard-stop fixture/CLI before any nonlocal outbound destination.
- [ ] If installed CLI behavior cannot be exercised faithfully, report precise
  limitation and version-pinned source evidence, not claimed runtime waiting.

## Integration gate

- [ ] Add optional explicit process module directory override so normal tests
  keep stock CPA and patched-host tests can select the writable copy. Fail
  clearly if an invalid override is supplied; do not change production go.mod.
- [ ] Real CPA fixture: healthy lower-priority A, exhausted higher-priority B,
  overage disabled, default passthrough disabled, finite future reset. Confirm
  fleet admission passes, scheduler subset rejects, wire 429/Retry-After ceil,
  zero local proxy hits. Test nonstream and stream initial failure.
- [ ] Unknown recovery emits 429 with no retry header. Run process assertion on
  stock CPA to show header absence and on patched host to show actual fix.
- [ ] Record compatibility: new plugin on stock CPA can emit 429 but scheduler
  Retry-After needs patched host. Preserve existing fleet/direct admission tests.
- [ ] Export minimal CPA patch into plugin `docs/patches/` with exact provenance
  and apply/build instructions; no published custom image or tag implied.
- [ ] Final evidence: plugin full/race plus affected CPA tests/race, actual
  process wire response and client fixture. Reuse unchanged valid lane evidence
  rather than repeating full suites. Parent inspects raw outputs and commits.

## Completion

No release/deploy is authorized. Keep changes locally committed and provide
the CPA patch, plugin commit and tested behavior/remaining limitations. Review
security, optional integer validation, passthrough independence, error wrapping,
stream behavior, zero upstream fixture hits, and unchanged account selection.
