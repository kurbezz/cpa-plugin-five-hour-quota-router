# Task 1 Report: Confirmed Exhaustion Cache Predicate

## Status

Completed.

## Files Changed

- `cache.go`
  - Added `quotaCache.allConfirmedExhausted(authIDs, now, cutoff)`.
  - Empty credential lists return `false`.
  - The method locks the cache while checking every supplied ID and delegates to
    `quotaSample.blocked`, preserving the existing confirmed-sample and
    future-reset semantics. It intentionally does not use `excluded`, so an
    unknown credential cannot trigger admission blocking.
- `main_test.go`
  - Added `TestAllConfirmedExhausted` with cases for all credentials confirmed
    exhausted, an under-cutoff sibling, a never-sampled sibling, and an empty
    credential list.

## TDD and Test Commands

1. Added the focused tests before the implementation.
2. Ran:

   ```sh
   go test ./... -run TestAllConfirmedExhausted -v
   ```

   Result: expected build failure. `quotaCache.allConfirmedExhausted` was
   undefined at the four new test call sites.
3. Implemented the cache predicate using `blocked`.
4. Ran:

   ```sh
   gofmt -w cache.go main_test.go
   go test ./... -run TestAllConfirmedExhausted -v
   ```

   Result: PASS — 5 tests passed in 1 package (the parent test plus four
   subtests).

## Commit

- Commit: `d8bc2ff0dc830bba3dc2338351db15ee4b8c9182`
- Message: `Add confirmed exhaustion cache predicate`

## Concerns

- Only the task-required focused test command was run; the full test suite was
  not run per task instructions.
- The required report is intentionally uncommitted because the requested commit
  contains only the implementation and test changes.

## Review Follow-up

### Files Changed

- `cache.go`
  - Tightened `allConfirmedExhausted` to require a non-zero `ResetAt` in
    addition to the existing `blocked` predicate. Since `blocked` already
    rejects past and equal-to-now resets through `known`, confirmed exhaustion
    now requires an explicitly future reset.
- `main_test.go`
  - Added regression coverage for zero, past, and equal-to-now resets; each
    must return false even at or above cutoff.
  - Added duplicate-ID coverage proving duplicate exhausted IDs return true and
    duplicate unknown IDs return false.

### Commands and Results

1. After adding the regression tests, ran:

   ```sh
   go test ./... -run TestAllConfirmedExhausted -v
   ```

   Result: expected failure for the zero-reset case before the implementation
   update.
2. After updating the predicate, ran:

   ```sh
   gofmt -w cache.go main_test.go
   go test ./... -run TestAllConfirmedExhausted -v
   go test -race ./... -run TestAllConfirmedExhausted -v
   ```

   Result: PASS — 10 tests passed in 1 package for both the normal and race
   focused test commands.

### Follow-up Commit

- Pending at report update time.
