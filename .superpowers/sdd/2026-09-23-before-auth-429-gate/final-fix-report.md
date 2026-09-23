# Before-auth 429 gate final fix report

## Architecture changes
- Added an observed-change generation to each physical auth cache entry. List metadata changes increment it without clearing a committed quota sample; usage/revision work captures it and cannot commit after a later observation.
- Added a one-lock `confirmedExhaustedReset` cache query. The interceptor now derives both the all-exhausted decision and earliest reset from one cache snapshot, and always emits Retry-After for a valid 429.
- Revision checks now distinguish successful completion timestamp from attempt throttling. Transient targeted discovery/read failures reschedule the same ID using cancellation-aware bounded delay.
- Revision-only work checks usage eligibility before recording a usage attempt.

## Findings resolved
1. Stale serial/in-flight work is rejected using observed generations; deterministic barrier regression added.
2. Revision replacement preserves latest list revision and last successful revision-check bookkeeping; regression added.
3. Targeted revision work retries list/get failures without a fleet refresh and successful revision checks alone complete bookkeeping.
4. Revision-only checks no longer update `LastAttemptAt`; overdue normal usage remains eligible; regression added.
5. Interceptor uses atomic confirmation/reset snapshot and always includes Retry-After in a successful gate.
6. Existing E2E behavior retained (no E2E source change required in this fix wave).
7. README requirement updated to CLIProxyAPI v7.3.8+.

## Test evidence
- `go build ./...` passed
- `go vet ./...` passed
- `go test ./...` passed (128 tests)
- `go test -race ./...` passed (127 tests)
- `git diff --check` passed

## Remaining concerns
- Retry delay is intentionally a small fixed per-ID delay and remains cancellation-aware. No credentials, auth JSON, revisions, or headers are logged.

## Commit
ae8bffdd08b87b23f8dc9f1a0fe54d51a5e74a74 (followed by timing test commit)

## Final review fixes
- Delayed revision retries (including throttle waits) are now executed only by the refresh worker with a cancellation-aware timer; no detached retry goroutine can enqueue into a restarted runtime.
- Polling now carries revision retry intent independently from revision-only eligibility, so a targeted usage/revision overlap retains the revision signal on transient `auth.get` failure.
- Added deterministic cancellation/restart-queue and overlap failure regressions.

Additional validation after final review: `go build ./...`, `go vet ./...`, `go test ./...` (130), `go test -race ./...` (129), and `git diff --check` passed.

## Targeted deferred-retry fix
- Replaced worker-blocking revision throttle waits with worker-owned per-ID deferred deadlines. The refresh loop selects on wake, nearest deadline, and cancellation, then promotes due IDs atomically.
- Deferred state is lifecycle-owned under `refreshMu`, cleared on stop, and preserves overlap revision intent without detached goroutines.
- Added deterministic regression proving throttled revision A does not delay ready targeted usage B.

Validation: `go build ./...`, `go vet ./...`, `go test ./...` (131), `go test -race ./...` (130), and `git diff --check` passed.
