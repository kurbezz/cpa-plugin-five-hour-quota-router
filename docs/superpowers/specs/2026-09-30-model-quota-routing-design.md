# Model-aware Claude subscription quota routing

## Goal and approved policy

Extend the published v0.4.0 plugin to check the common five-hour window, the
common weekly window, and applicable model-family weekly windows before routing.
Keep the existing cutoff (95% by default), protected-model settings, credential
weight ordering, and overage-fallback setting.

The user explicitly chose availability for unknown model quotas: a missing or
unrecognized model window does not exclude a credential. This is not a guarantee
against Extra Usage. Known exhausted model windows must not be bypassed by a
healthy shared window or by a response that omits model headers.

No production configuration changes, authenticated usage probing, installation,
publication, or release are part of implementation and validation.

## Sources and scope resolution

The OAuth usage endpoint is undocumented. Support its observed legacy fields
`five_hour`, `seven_day`, `seven_day_opus`, and `seven_day_sonnet`, and recognize
`seven_day_fable` when present. Support newer `limits[]` entries with `kind`
`session`, `weekly_all`, or `weekly_scoped`; scoped entries identify their model
through `scope.model.id` and/or `scope.model.display_name`, not a guessed top-level
`model` field. Percentage values are percentage points in this endpoint.

Evidence for the newer scoped shape includes public consumers:

- https://github.com/Yeachan-Heo/oh-my-claudecode/blob/main/src/hud/usage-api.ts
- https://github.com/seakee/CPA-Manager-Plus/blob/main/apps/web/src/utils/quota/providerRequests.test.ts
- https://github.com/decolua/9router/blob/master/open-sse/services/usage/claude.js

These are compatibility evidence, not an official Anthropic contract. Fixtures
must clearly be synthetic and must not include real accounts or billing data.

Normalize known families Fable, Opus, and Sonnet using whole family tokens in
Claude model IDs and recognized display names (for example `Fable`, `Fable 5`,
and `Claude 3.5 Fable`). Avoid arbitrary substring matches. Reject ambiguous
labels identifying multiple families. Unknown scopes must not become global
limits or apply to unrelated families. Custom aliases without an identifiable
family do not acquire an inferred model quota; document that limitation rather
than introducing an unrelated alias configuration system.

Successful response observations continue through the existing after-auth
RequestID correlation. Read `5h-*` and `7d-*` as shared window observations.
Read `7d_oi-*` as Fable-scoped only on an identifiable Fable request; never infer
an Opus quota from the suffix. CPA v7.3.8's
`internal/runtime/executor/helps/claude_ratelimit.go` and
`claude_executor_fable_ratelimit_test.go` explicitly distinguish this Fable-only
signal from shared exhaustion. No guessed additional header families are needed.
Header utilization is a fraction; convert to percent. Preserve existing
`rejected` handling and Unix/RFC3339 reset parsing.

When old and new usage shapes describe the same scope, prefer the valid newer
`limits[]` observation; if conflicting duplicate entries remain in that shape,
use the conservative higher utilization and later exhaustion reset. Invalid
optional windows do not discard an otherwise valid five-hour usage sample.

## Cache and observation lifecycle

Store window observations independently with utilization, reset, observation
time, and source. Keep existing five-hour status fields compatible. Shared and
model-scoped weekly windows require their own timestamps: a fresh five-hour
header must not prevent a concurrent older usage poll from updating a weekly
window that has not received a newer observation.

Usage and header commits retain membership, credential identity, revision,
generation, and non-reusable incarnation protections. Removal/re-addition and
revision-only replacement must invalidate all windows and outstanding header
correlations. Poll start timestamps resolve stale commits per window, with
header/newer sample winning timestamp ties.

An observation updates only the windows it contains. Missing or malformed weekly
windows do not erase previously confirmed exhaustion. A window whose known reset
has passed stops blocking, and asynchronous polling can refresh it. A blocked
window without a usable reset must remain refreshable after the configured poll
interval rather than becoming an indefinite lockout; preserve v0.4.0's policy of
ignoring high-utilization header observations with an unknown reset. A low
utilization observation with no reset remains valid.

Optional absent weekly windows are not required for routing. Preserve the
existing fail-closed behavior for credentials that have never produced a valid
five-hour sample. Expired windows retain the existing scoped reset-based
fail-open behavior. Do not introduce synchronous auth.get or usage fetches in
interceptors, scheduler calls, or status calls.

## Routing and admission

Evaluate a credential against the request model. Applicable windows are the
shared five-hour window, any known shared weekly window, and known weekly
windows for the resolved model family. Any unexpired applicable window at or
above cutoff excludes that credential for this request. A Fable-only exhausted
window must not exclude the credential for Sonnet or Opus.

Select available candidates using the existing order: priority, weight, lexical
AuthID. Only intentionally select an excluded credential when overage fallback
is enabled and every scheduler candidate is confirmed exhausted for this model;
unknown five-hour candidates cannot authorize overage.

With overage disabled, the before-auth HTTP 429 gate must atomically evaluate
every current physical Claude OAuth cache member for the requested model. Keep
the existing conservative fleet membership semantics: an available or unknown
alternative prevents fleet-wide rejection. Never turn missing quota or an
unrecognized model into confirmed exhaustion.

For each exhausted account, recovery is the latest reset among its currently
exhausted applicable windows. Fleet recovery is the earliest such account
recovery. Emit 429 and Retry-After only when every member is confirmed exhausted
and every member's applicable blocking windows have usable future resets. The
scheduler backstop uses the same model-aware recovery calculation for candidate
retry metadata. Preserve the existing error code for client compatibility and
document that its historical name now also covers weekly exhaustion.

## Diagnostics and security

Extend management status with shared and model-family window information,
including utilization, reset, sampled time, source, and window-level blocked
state. Do not redefine the existing account-level `blocked` field to imply that
every model is blocked; preserve and document its five-hour meaning.

Only bounded normalized scope identifiers and parsed numeric/time values are
eligible for new diagnostics. Never expose raw usage JSON, response headers,
metadata, token material, credential revision digests, account billing details,
or arbitrary upstream scope labels. Response handlers remain observe-only and
cannot change headers/body, drop stream chunks, or fail delivery. Successful
responses only are observable in CPA v7.3.8; upstream errors/429 remain outside
this hook's coverage.

## Integration invariants from design review

Use five fixed slots (five-hour, shared weekly, Fable weekly, Opus weekly,
Sonnet weekly), one pure model-aware evaluator, and one guarded batch merge.
Do not introduce a generic quota registry. Batch validation checks identity,
revision, generation and incarnation once; timestamp rejection remains per
window. Capture the bound incarnation for usage polls as well as responses.

Separate the usage attempt/success clock from header freshness. Frequent shared
headers cannot suppress normal request-driven weekly discovery. Missing optional
windows never cause unthrottled repeat fetches. Request-driven evaluation must
also enqueue due refresh for excluded accounts with unknown recovery, even when
no account is selected. No new autonomous fleet polling is required. Known
future-reset exhaustion of a shared window may suppress unnecessary fetches;
model-only exhaustion must not suppress discovery for other models.

The evaluator distinguishes excluded, confirmed exhausted, and known recovery.
Never-sampled five-hour is excluded but cannot confirm exhaustion. Shared windows
apply even to requests with unknown family. All blocking windows must have
future resets to supply finite recovery; healthy window resets are irrelevant.
Evaluate candidate decisions in one coherent cache snapshot, and admission over
live membership under one lock.

Conflicting recognized families in scope ID/display name are ambiguous and are
ignored. The normalized execution `Model` is authoritative when present;
`RequestedModel` is a fallback only when `Model` is empty. Do not combine or guess
families from conflicting lifecycle fields. Apply the same family resolver to
usage scopes, scheduler decisions, admission and response header observations.

## Verification gates

Use deterministic tests for:

1. Both usage shapes, known family normalization, invalid/unknown scopes,
   duplicates, missing/reset-null windows, and percent/fraction conversion.
2. Fable exhausted with shared windows healthy: Fable chooses another account
   or blocks, while Sonnet/Opus remain selectable; mirror for Opus/Sonnet.
3. Shared weekly exhaustion affects all protected models; unknown model quota
   permits routing using shared windows; unknown five-hour still fails closed.
4. Weight preference, no-overage backstop, and enabled overage across mixed
   scopes and unknown candidates.
5. Recovery uses max(blocking account resets), then min(account recovery);
   missing reset prevents a fabricated Retry-After or admission 429.
6. Partial headers preserve other windows; stale polls resolve per-window;
   replacement/revision/removal, retries, and RequestID bounds retain all v0.4
   race guarantees; payload chunks remain no-ops.
7. Expired scoped windows recover and polling can refresh incomplete data
   without repeated polling of confirmed future-reset exhaustion.
8. Real CPA v7.3.8 local process fixture: synthetic usage for an exhausted
   Fable window yields pre-upstream 429 with Retry-After and zero upstream hits;
   an unrelated model is not rejected by the plugin. Do not claim a successful
   OAuth upstream header fixture exists if TLS routing still prevents it.

Final gates: gofmt, build, vet, full tests, race tests, diff-check, and independent
review of model applicability, recovery, and per-window commit safety. No
publication before these gates pass.
