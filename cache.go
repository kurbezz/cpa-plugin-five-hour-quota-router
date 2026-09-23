package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

type cutoffStatusResponse struct {
	Enabled           bool                  `json:"enabled"`
	ProtectedModels   []string              `json:"protected_models"`
	CutoffPercentUsed float64               `json:"cutoff_percent_used"`
	Accounts          []cutoffAccountStatus `json:"accounts"`
}

type cutoffAccountStatus struct {
	ID                  string   `json:"id"`
	AuthIndex           string   `json:"auth_index,omitempty"`
	Name                string   `json:"name,omitempty"`
	Known               bool     `json:"known"`
	Blocked             bool     `json:"blocked"`
	FiveHourPercentUsed *float64 `json:"five_hour_percent_used,omitempty"`
	SampledAt           string   `json:"sampled_at,omitempty"`
	ResetAt             string   `json:"reset_at,omitempty"`
	LastErrorCategory   string   `json:"last_error_category,omitempty"`
}

type quotaSample struct {
	AuthIndex string
	Name      string
	Identity  string
	// Revision is a non-reversible digest derived asynchronously from the
	// credential JSON. It is intentionally never included in status or logs.
	Revision               string
	ListRevision           string
	LastRevisionCheckAt    time.Time
	RevisionCheckAttemptAt time.Time
	// ObservedGeneration is bumped for every observed list-metadata change.
	// It does not invalidate a committed sample, but makes older in-flight work
	// ineligible to commit after a newer observation.
	ObservedGeneration  uint64
	HasSample           bool
	FiveHourPercentUsed float64
	SampledAt           time.Time
	LastAttemptAt       time.Time
	ResetAt             time.Time
	LastErrorCategory   string
}

// confirmedFleetExhaustedReset atomically verifies exhaustion for and returns
// the earliest reset from every credential CURRENTLY in the cache -- never a
// caller-captured discovery snapshot -- under a single lock. Admission must
// never miss a credential that a concurrent, newer discovery reconciled into
// the cache after the caller's own membership list was captured: checking a
// stale, caller-supplied ID list here could confirm "exhaustion" against a
// membership that a newer reconciliation has already invalidated by adding an
// available or unknown alternative.
func (c *quotaCache) confirmedFleetExhaustedReset(now time.Time, cutoff float64) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.samples) == 0 {
		return time.Time{}, false
	}
	var earliest time.Time
	for _, sample := range c.samples {
		if !sample.HasSample || sample.ResetAt.IsZero() || !now.Before(sample.ResetAt) || sample.FiveHourPercentUsed < cutoff {
			return time.Time{}, false
		}
		if earliest.IsZero() || sample.ResetAt.Before(earliest) {
			earliest = sample.ResetAt
		}
	}
	return earliest, !earliest.IsZero()
}

func (s quotaSample) known(now time.Time) bool {
	return s.HasSample && (s.ResetAt.IsZero() || now.Before(s.ResetAt))
}

func (s quotaSample) blocked(now time.Time, cutoff float64) bool {
	return s.known(now) && s.FiveHourPercentUsed >= cutoff
}

// excluded reports whether this credential should be excluded from five-hour-protected
// scheduling. Fail-closed: a credential that has never produced a successful sample is
// excluded (we cannot assume it is safe). A sample whose reset time has passed is treated
// as available again without waiting for a fresh poll, because Anthropic's five-hour window
// genuinely rolls over at resets_at — this is a deliberate, scoped fail-open specific to
// confirmed window expiry, not a general unknown-quota fail-open.
func (s quotaSample) excluded(now time.Time, cutoff float64) bool {
	if !s.HasSample {
		return true
	}
	if !s.ResetAt.IsZero() && !now.Before(s.ResetAt) {
		return false
	}
	return s.FiveHourPercentUsed >= cutoff
}

type quotaCache struct {
	mu      sync.Mutex
	samples map[string]quotaSample
}

// earliestFutureReset returns the earliest future reset from successful cache
// samples for the supplied scheduler candidates. Unknown samples and missing,
// expired, or zero reset times intentionally provide no retry information.
func (c *quotaCache) earliestFutureReset(authIDs []string, now time.Time) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var earliest time.Time
	for _, authID := range authIDs {
		sample := c.samples[authID]
		if !sample.HasSample || sample.ResetAt.IsZero() || !now.Before(sample.ResetAt) {
			continue
		}
		if earliest.IsZero() || sample.ResetAt.Before(earliest) {
			earliest = sample.ResetAt
		}
	}
	return earliest, !earliest.IsZero()
}

// allConfirmedExhausted reports whether every supplied credential has a
// successful, unexpired sample at or above the cutoff. It deliberately uses
// blocked rather than excluded so unknown credentials cannot cause admission
// blocking.
func (c *quotaCache) allConfirmedExhausted(authIDs []string, now time.Time, cutoff float64) bool {
	if len(authIDs) == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, authID := range authIDs {
		sample := c.samples[authID]
		if sample.ResetAt.IsZero() || !sample.blocked(now, cutoff) {
			return false
		}
	}
	return true
}

// exhaustedErrorMessage adds safe retry metadata only when a future reset is
// known. The scheduler ABI has no HTTP status or Retry-After header support.
func exhaustedErrorMessage(now time.Time, resetAt time.Time, hasReset bool) string {
	if !hasReset || resetAt.IsZero() || !now.Before(resetAt) {
		return exhaustedErrorCode
	}
	retryAfterSeconds := int64(math.Ceil(resetAt.Sub(now).Seconds()))
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	return fmt.Sprintf("%s; retry_after_seconds=%d; resets_at=%s", exhaustedErrorCode, retryAfterSeconds, resetAt.UTC().Format(time.RFC3339))
}

func (c *quotaCache) empty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.samples) == 0
}

// reconcile updates membership and returns IDs invalidated by a physical
// identity replacement under the same auth ID.
func (c *quotaCache) reconcile(auths []physicalClaudeAuth) (map[string]struct{}, map[string]struct{}) {
	keep := make(map[string]struct{}, len(auths))
	replaced := make(map[string]struct{})
	changed := make(map[string]struct{})
	c.mu.Lock()
	for _, auth := range auths {
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		keep[auth.ID] = struct{}{}
		sample := c.samples[auth.ID]
		if sample.Identity != "" && auth.Identity != "" && sample.Identity != auth.Identity {
			sample = quotaSample{ObservedGeneration: sample.ObservedGeneration + 1}
			replaced[auth.ID] = struct{}{}
		}
		if sample.ListRevision != "" && auth.ListRevision != "" && sample.ListRevision != auth.ListRevision {
			changed[auth.ID] = struct{}{}
			sample.ObservedGeneration++
		}
		sample.AuthIndex = auth.AuthIndex
		sample.Name = strings.TrimSpace(auth.Name)
		sample.Identity = auth.Identity
		sample.ListRevision = auth.ListRevision
		c.samples[auth.ID] = sample
	}
	for authID := range c.samples {
		if _, ok := keep[authID]; !ok {
			delete(c.samples, authID)
		}
	}
	c.mu.Unlock()
	return replaced, changed
}

// claimRevisionCheck throttles an asynchronous auth.get revision check even
// when the quota sample is blocked. It never clears a sample itself.
func (c *quotaCache) claimRevisionCheck(authID string, now time.Time, minimumAge time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[authID]
	if !ok || (!sample.RevisionCheckAttemptAt.IsZero() && now.Before(sample.RevisionCheckAttemptAt.Add(minimumAge))) {
		return false
	}
	sample.RevisionCheckAttemptAt = now
	c.samples[authID] = sample
	return true
}

// completeRevisionCheck records only a successfully read and bound current
// credential revision. Failed auth.list/auth.get work therefore remains due.
func (c *quotaCache) completeRevisionCheck(auth physicalClaudeAuth, generation uint64, checkedAt time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[auth.ID]
	if !ok || !sampleMatchesAuth(sample, auth) || sample.ObservedGeneration != generation {
		return false
	}
	sample.LastRevisionCheckAt = checkedAt
	c.samples[auth.ID] = sample
	return true
}

func (c *quotaCache) revisionCheckDelay(authID string, now time.Time, minimumAge time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[authID]
	if !ok || sample.RevisionCheckAttemptAt.IsZero() {
		return 0
	}
	delay := sample.RevisionCheckAttemptAt.Add(minimumAge).Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

// shouldRefreshAfterRevisionCheck permits usage only for a changed revision or
// when normal refresh eligibility allows it. bindRevision clears HasSample on
// change, which makes this true for a replacement.
func (c *quotaCache) shouldRefreshAfterRevisionCheck(authID string, now time.Time, cutoff float64, minimumAge time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[authID]
	if !ok || !sample.HasSample {
		return true
	}
	if sample.blocked(now, cutoff) {
		return false
	}
	last := sample.SampledAt
	if sample.LastAttemptAt.After(last) {
		last = sample.LastAttemptAt
	}
	return last.IsZero() || !now.Before(last.Add(minimumAge))
}

func (c *quotaCache) claimRefresh(authID string, now time.Time, cutoff float64, minimumAge time.Duration) bool {
	if strings.TrimSpace(authID) == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sample := c.samples[authID]
	if sample.blocked(now, cutoff) {
		return false
	}
	lastCheck := sample.SampledAt
	if sample.LastAttemptAt.After(lastCheck) {
		lastCheck = sample.LastAttemptAt
	}
	if !lastCheck.IsZero() && now.Before(lastCheck.Add(minimumAge)) {
		return false
	}
	sample.LastAttemptAt = now
	c.samples[authID] = sample
	return true
}

func (c *quotaCache) recordAttempt(authID string, attemptedAt time.Time) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	c.mu.Lock()
	sample := c.samples[authID]
	sample.LastAttemptAt = attemptedAt
	c.samples[authID] = sample
	c.mu.Unlock()
}

// recordAttemptForIdentity records work only when the physical credential is
// still the one that started that work. A reused auth ID must not receive state
// from the credential it replaced.
func (c *quotaCache) recordAttemptForIdentity(auth physicalClaudeAuth, attemptedAt time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[auth.ID]
	if !ok || !sampleMatchesAuth(sample, auth) {
		return false
	}
	sample.LastAttemptAt = attemptedAt
	c.samples[auth.ID] = sample
	return true
}

// bindRevision associates a credential-specific, non-secret revision with a
// listed auth. A first observed revision preserves a valid sample because list
// metadata cannot prove a replacement. A later different revision invalidates
// that sample, making a same-path token replacement fail closed until polled.
func (c *quotaCache) bindRevision(auth physicalClaudeAuth, revision string, generation uint64) (physicalClaudeAuth, bool) {
	if revision == "" {
		return physicalClaudeAuth{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[auth.ID]
	if !ok || sample.Identity != auth.Identity || sample.ObservedGeneration != generation {
		return physicalClaudeAuth{}, false
	}
	if sample.Revision != "" && sample.Revision != revision {
		sample = quotaSample{AuthIndex: auth.AuthIndex, Name: auth.Name, Identity: auth.Identity, ListRevision: sample.ListRevision, LastRevisionCheckAt: sample.LastRevisionCheckAt, RevisionCheckAttemptAt: sample.RevisionCheckAttemptAt, ObservedGeneration: sample.ObservedGeneration}
	}
	sample.Revision = revision
	c.samples[auth.ID] = sample
	auth.Revision = revision
	return auth, true
}

func (c *quotaCache) observedGeneration(auth physicalClaudeAuth) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[auth.ID]
	return sample.ObservedGeneration, ok && sample.Identity == auth.Identity
}

func sampleMatchesAuth(sample quotaSample, auth physicalClaudeAuth) bool {
	if sample.Identity != auth.Identity {
		return false
	}
	return auth.Revision == "" || sample.Revision == auth.Revision
}

func (c *quotaCache) recordSuccess(authID string, percentUsed float64, resetAt, sampledAt time.Time) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	c.mu.Lock()
	sample := c.samples[authID]
	sample.HasSample = true
	sample.FiveHourPercentUsed = percentUsed
	sample.SampledAt = sampledAt
	sample.LastAttemptAt = sampledAt
	sample.ResetAt = resetAt
	sample.LastErrorCategory = ""
	c.samples[authID] = sample
	c.mu.Unlock()
}

func (c *quotaCache) recordSuccessForIdentity(auth physicalClaudeAuth, percentUsed float64, resetAt, sampledAt time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[auth.ID]
	if !ok || !sampleMatchesAuth(sample, auth) {
		return false
	}
	sample.HasSample = true
	sample.FiveHourPercentUsed = percentUsed
	sample.SampledAt = sampledAt
	sample.LastAttemptAt = sampledAt
	sample.ResetAt = resetAt
	sample.LastErrorCategory = ""
	c.samples[auth.ID] = sample
	return true
}

// recordSuccessForGeneration commits an /api/oauth/usage poll result.
// pollStartedAt is the time this poll began its work (before its host.auth.get
// and network round trip); if a sample already committed after that time --
// whether from header observation of a concurrent real request, or from
// another, faster usage poll -- this older poll must not overwrite it.
func (c *quotaCache) recordSuccessForGeneration(auth physicalClaudeAuth, generation uint64, percentUsed float64, resetAt, sampledAt, pollStartedAt time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[auth.ID]
	if !ok || !sampleMatchesAuth(sample, auth) || sample.ObservedGeneration != generation {
		return false
	}
	if sample.HasSample && !sample.SampledAt.IsZero() && sample.SampledAt.After(pollStartedAt) {
		return false
	}
	sample.HasSample, sample.FiveHourPercentUsed, sample.SampledAt = true, percentUsed, sampledAt
	sample.LastAttemptAt, sample.ResetAt, sample.LastErrorCategory = sampledAt, resetAt, ""
	c.samples[auth.ID] = sample
	return true
}

// recordHeaderObservation commits a five-hour quota sample derived from a real
// successful upstream response's rate-limit headers. It updates only an
// existing cache member (never creates one) and never touches Revision,
// ListRevision, or ObservedGeneration -- header observation is a pure quota
// update, not a membership/identity signal. A poll started before this
// observation's ObservedAt must not later overwrite it: callers use
// recordSuccessForGeneration's minSampledAt comparison (via pollStartedAt) for
// that; this method's own commit is unconditional on the current SampledAt
// except that it never regresses an observation with a newer one already
// recorded (compared by ObservedAt).
func (c *quotaCache) recordHeaderObservation(authID string, observation headerObservation) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || !observation.Valid {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[authID]
	if !ok {
		return false
	}
	if sample.HasSample && !sample.SampledAt.IsZero() && observation.ObservedAt.Before(sample.SampledAt) {
		// An older header observation must never overwrite a newer one already
		// committed (from either a header observation or a usage poll).
		return false
	}
	sample.HasSample = true
	sample.FiveHourPercentUsed = observation.PercentUsed
	sample.ResetAt = observation.ResetAt
	sample.SampledAt = observation.ObservedAt
	sample.LastErrorCategory = ""
	c.samples[authID] = sample
	return true
}

func (c *quotaCache) recordFailure(authID, category string) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	c.mu.Lock()
	sample := c.samples[authID]
	sample.LastErrorCategory = category
	c.samples[authID] = sample
	c.mu.Unlock()
}

func (c *quotaCache) recordFailureForIdentity(auth physicalClaudeAuth, category string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[auth.ID]
	if !ok || !sampleMatchesAuth(sample, auth) {
		return false
	}
	sample.LastErrorCategory = category
	c.samples[auth.ID] = sample
	return true
}

func (c *quotaCache) isBlocked(authID string, now time.Time, cutoff float64) bool {
	c.mu.Lock()
	sample := c.samples[authID]
	c.mu.Unlock()
	return sample.blocked(now, cutoff)
}

func (c *quotaCache) isExcluded(authID string, now time.Time, cutoff float64) bool {
	c.mu.Lock()
	sample := c.samples[authID]
	c.mu.Unlock()
	return sample.excluded(now, cutoff)
}

func (c *quotaCache) snapshot(authID string) quotaSample {
	c.mu.Lock()
	sample := c.samples[authID]
	c.mu.Unlock()
	return sample
}

func (c *quotaCache) statuses(now time.Time, cutoff float64) []cutoffAccountStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	accounts := make([]cutoffAccountStatus, 0, len(c.samples))
	for authID, sample := range c.samples {
		known := sample.known(now)
		account := cutoffAccountStatus{
			ID:                authID,
			AuthIndex:         sample.AuthIndex,
			Name:              sample.Name,
			Known:             known,
			Blocked:           sample.excluded(now, cutoff),
			LastErrorCategory: sample.LastErrorCategory,
		}
		if known {
			percentUsed := sample.FiveHourPercentUsed
			account.FiveHourPercentUsed = &percentUsed
			if !sample.SampledAt.IsZero() {
				account.SampledAt = sample.SampledAt.UTC().Format(time.RFC3339Nano)
			}
			account.ResetAt = sample.ResetAt.UTC().Format(time.RFC3339Nano)
		}
		accounts = append(accounts, account)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	return accounts
}
