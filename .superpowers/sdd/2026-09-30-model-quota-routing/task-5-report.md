# Task 5 implementation report

## Changes

- `process_e2e_test.go`: Added `TestCLIProxyAPIProcessModelQuotaEndToEnd`, a real local CPA v7.3.8 process test with synthetic usage responses and two synthetic Claude OAuth accounts. It registers standard Claude catalog model ID `claude-fable-5-1` plus Sonnet, checks five-hour/shared-weekly healthy status and Fable weekly exhaustion for both accounts, confirms the Fable request is rejected pre-upstream with a JSON exhausted code and reset-derived `Retry-After`, observes zero Fable proxy hits, and confirms Sonnet is not rejected by Fable quota and reaches the local rejecting proxy.
- `README.md`: Documented shared/model-family routing, percentage point vs header fraction, observed legacy/new usage shapes, no custom family aliases, partial successful-response header observations, model-specific retry recovery, window status and legacy blocked meaning, and availability/Extra Usage limitations.
- `model_status_test.go`: Seeded actual identity/revision marker values, asserts those markers aren't serialized, checks exact bounded scope/source allowlists, and verifies Fable window blocking doesn't redefine legacy account blocked.
- `model_routing_test.go`: Renamed the shared-weekly/optional-scope test to describe actual assertions and removed unused test calculation.

## Process evidence

Command: `go test -json -run TestCLIProxyAPIProcessModelQuotaEndToEnd -count=1 -v .` — PASS. Captured raw JSON at `/private/var/folders/y9/9xxmd1b12zz5t670sgtkhcs40000gn/T/task5-process.jsonl`.

Observed test log: initial run `TASK5_OBSERVED accounts=2 status_windows=five_hour,weekly,fable_weekly healthy_shared=true fable_http=429 retry_after=2280121341 fable_proxy_hits=0 sonnet_proxy_hits=1 usage_hits=2`. Retry was validated against request timing and synthetic 2099 resets. No actual Anthropic request/inference or successful upstream response-header process fixture was used.

Fixture corrections from reproduced failures: generated YAML host quoting was fixed; module v7.3.8's enabled Devin model uses registered ID `claude-fable-5-1` (not `devin/claude-fable-5-1`). No host registry/config source was edited.

## Verification

- `gofmt -l *.go`: PASS (no files listed).
- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `go test -json -run 'TestCLIProxyAPIProcessModelQuotaEndToEnd|TestCLIProxyAPIProcessEndToEnd|TestModelQuotaStatusWindows|TestModelQuotaRouting|TestModelQuotaAdmission' -count=1 -v .`: PASS; focused includes both local CPA process scenarios and model/status tests. Raw JSON: `/private/var/folders/y9/9xxmd1b12zz5t670sgtkhcs40000gn/T/task5-focused.jsonl`.
- `go test -json ./... -count=1`: PASS, exit 0. Raw JSON: `/private/var/folders/y9/9xxmd1b12zz5t670sgtkhcs40000gn/T/task5-full.jsonl`.
- `go test -json -race ./... -count=1`: PASS, exit 0. Raw JSON: `/private/var/folders/y9/9xxmd1b12zz5t670sgtkhcs40000gn/T/task5-race.jsonl`.
- `git diff --check`: PASS.

The new process scenario observed 2 accounts, bounded `five_hour`, `weekly`, and `fable_weekly` status, healthy shared windows, Fable pre-upstream 429 with reset-derived retry, zero Fable local proxy hits, and a Sonnet local proxy hit. Existing five-hour process behavior also passed and logged a 429 with zero proxy hits. These are synthetic fixtures only; no actual Anthropic inference/network or successful OAuth response-header end-to-end was exercised.

Known limitations: upstream usage format is undocumented; only known model family labels resolve, custom aliases are unsupported; missing model-family quota is allowed by policy and is not an absolute Extra Usage guarantee; only successful upstream response headers can be observed; historical error code retains `five_hour` wording for weekly exhaustion. Report copied to `.superpowers/model-quotas-2026-09-30/implementation-report.md`.

## Final accuracy corrections (2026-10-01)

- README clarifies disabling intentional fallback is not a zero-Extra-Usage guarantee; missing/unrecognized model quotas remain allowed and observations may be stale/incomplete.
- README distinguishes shared future-reset poll suppression from model-only exhaustion (which does not stop discovery for other scopes), states five-hour knowledge may come from polls or valid correlated successful headers, and scopes expiry recovery per window while other applicable windows can continue blocking.
- Process readiness now explicitly verifies each account's shared weekly status is known, below cutoff, and unblocked before claiming healthy shared status. The log now says `sonnet_proxy_hit_observed=true` rather than claiming exactly one hit.
- Clarified `claude-fable-5-1` is the standard Claude catalog ID; it is not an explicitly registered Devin model.
- Focused rerun: `go test -json -run TestCLIProxyAPIProcessModelQuotaEndToEnd -count=1 -v .` PASS. Raw output saved to `.superpowers/sdd/2026-09-30-model-quota-routing/workspace-final-fix-process.jsonl`. This rerun logs two accounts, verified healthy shared weekly and Fable-scoped windows, Fable HTTP 429 with reset-derived Retry-After, zero Fable proxy hits, and Sonnet proxy hit observed. No production behavior changed; the prior full/race/build/vet results remain applicable.
