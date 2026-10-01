# Scheduler quota errors: HTTP 429 and Retry-After

## Approved goal and constraints

Return actual HTTP 429 and a reset-derived Retry-After header when the plugin
scheduler rejects its supplied candidates for confirmed quota exhaustion.
Preserve all existing quota, candidate, priority, weight and overage decisions.
Do not broaden before-auth admission to accounts it cannot prove eligible.
Development and verification are local. No production changes, publication,
installation, real Anthropic usage/inference requests, or secret extraction.
All delegated model work must explicitly use discovered GPT/OpenAI model IDs.

## Observed failure and dependency boundary

CPA v7.3.8 pluginabi.Error supports http_status but not retry delay. The plugin
currently emits only code/message, so scheduler errors default to HTTP 500.
rpcError preserves status but exposes no retry headers. ExecutionErrorMessage
and WriteErrorResponse therefore cannot emit the retry delay from message text.
OpenCode displays retry_after_seconds in that text yet retries after two seconds.
Current upstream OpenCode understands responseHeaders retry-after/retry-after-ms,
but no text-body metadata conversion was found. Installed CLI is v2.0.21; this
does not independently identify the GUI build or establish provider behavior.

## Chosen architecture

Extend the CPA ABI with optional integer retry_after_seconds, preserving omitted
and unknown recovery distinctly from a valid positive delay. Extend the plugin
error envelope independently so stock CPA can ignore the optional delay while
still consuming the existing http_status field.

For scheduler exhaustion, emit http_status 429. Add retry_after_seconds only when
the existing coherent candidate decision proves a future recovery. Compute the
text and structured delay from the same recovery snapshot and ceiling rule.
Do not derive delay by parsing the legacy message. Keep its historical error
code and message compatible, and do not change successful selection/fallback.

CPA preserves and validates the positive, representable structured delay in its
RPC error. Provide a narrow retry-header contract, restricted to HTTP 429 and
the integer delay, and write its Retry-After header through a safe error path
independent of the upstream passthrough-headers setting. Do not enable arbitrary
plugin error headers. Support wrapped errors with errors.As and preserve the
existing built-in safe auth headers, direct responses, and status/body behavior.

No known reset means no fabricated delay/header. Unknown-candidate rejection
still uses the existing scheduler fail-closed policy and becomes 429 without a
reliable delay; it must not imply confirmed fleet exhaustion or permit overage.

The added ABI field is optional/backward compatible. A new plugin on stock CPA
can expose 429 but cannot guarantee Retry-After on scheduler errors; an updated
CPA host is required for the complete fix. Existing before-auth direct-response
429 remains unchanged and continues to work on stock CPA.

## Alternatives rejected

- 429-only plugin change: smaller, but does not supply the header needed for the
  requested client delay.
- Reverse-proxy response adapter: possible workaround, but adds deployment and
  message parsing rather than correcting the structured host boundary.
- Broadening the fleet gate: may reject usable model candidates; not permitted.

## Local source and implementation boundaries

Prepare a separate writable CPA v7.3.8 source checkout beneath the project root,
not the shared Go module cache. Retain exact base provenance and generate a
reusable patch/commit for the CPA changes. Do not modify the production image.
Keep the published plugin repository's normal go.mod dependency unchanged;
use an explicit test-only alternate module file or local process-module override
for the patched host, documenting exact build/test commands.

Host and plugin changes can proceed in independent write scopes once the ABI
field and validation contract are fixed. Local real-process integration depends
on both. Parent owns integration and verification; one independent GPT reviewer
checks error transport, validation and response boundaries.

## Evidence path and acceptance

1. Unit-test plugin scheduler envelope status, positive ceil delay, omitted
   recovery, unchanged text/code, and no delay on unknown recovery.
2. Host ABI/RPC tests preserve optional delay, omit/reject zero/negative/out-of-
   range values safely, and do not fail requests on malformed retry metadata.
   Bound the representable delay without imposing arbitrary short caps.
3. Handler tests show actual Retry-After at passthrough default false, preserve
   built-in safe headers and nonquota errors, and cover wrapped RPC errors.
4. Real patched CPA process: healthy physical lower-priority A and exhausted
   higher-priority B. Before-auth fleet gate passes, scheduler rejects B. For
   nonstream and initial streaming requests assert wire 429, reset-derived
   Retry-After with ceiling bounds, and zero local rejecting-proxy hits.
5. Unknown recovery produces 429 with no fabricated header. Existing fleet
   admission, model-scoped routing and quota cache/race suites remain green.
6. Verify OpenCode v2.0.21/provider behavior against a localhost fake 429 endpoint
   with a short deterministic delay. No actual provider call. If runtime access
   or provider fidelity cannot be established, label waiting behavior unverified
   and do not claim that upstream source alone proves the installed client.

Minimum final checks: plugin build/vet/full/race, affected CPA package tests and
race coverage, real process cases, formatting/diff checks, and scoped independent
review. Reuse evidence only when its source/input/environment remains unchanged.
No release or production changes are included in this implementation approval.
