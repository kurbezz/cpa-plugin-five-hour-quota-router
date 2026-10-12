package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// extraUsage is Anthropic's paid overage ("extra usage") state for one
// credential. It is display-only: routing, cutoff and overage fallback never
// read it.
//
// The percentage, reset and status come from the unified overage response
// headers on every observed request (rounded to whole percent upstream). The
// dollar amounts exist only in the /api/oauth/usage body, so they are kept from
// the plugin's own usage polls and carry their own sample time.
type extraUsage struct {
	Percent        float64
	HasPercent     bool
	ResetAt        time.Time
	Status         string
	DisabledReason string
	SampledAt      time.Time

	Enabled          *bool
	UsedCredits      float64
	MonthlyLimit     float64
	HasAmounts       bool
	Currency         string
	DecimalPlaces    *int
	AmountsSampledAt time.Time
}

func (e extraUsage) empty() bool {
	return !e.HasPercent && e.Status == "" && e.DisabledReason == "" && !e.HasAmounts && e.Enabled == nil
}

const maxExtraUsageTextLen = 64

func boundedHeaderText(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > maxExtraUsageTextLen {
		raw = raw[:maxExtraUsageTextLen]
	}
	return sanitizeUpstreamMessage(raw)
}

// parseClaudeOverageHeaders reads Anthropic-Ratelimit-Unified-Overage-*.
// Utilization is a fraction (0.05 == 5%).
func parseClaudeOverageHeaders(headers http.Header, observedAt time.Time) (extraUsage, bool) {
	if headers == nil {
		return extraUsage{}, false
	}
	const prefix = "Anthropic-Ratelimit-Unified-Overage-"
	var e extraUsage
	if raw := strings.TrimSpace(headers.Get(prefix + "Utilization")); raw != "" {
		if fraction, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(fraction) && !math.IsInf(fraction, 0) && fraction >= 0 {
			e.Percent, e.HasPercent = math.Min(100, fraction*100), true
		}
	}
	e.ResetAt, _ = parseClaudeHeaderResetTime(strings.TrimSpace(headers.Get(prefix + "Reset")))
	e.Status = strings.ToLower(boundedHeaderText(headers.Get(prefix + "Status")))
	e.DisabledReason = boundedHeaderText(headers.Get(prefix + "Disabled-Reason"))
	if !e.HasPercent && e.Status == "" && e.DisabledReason == "" {
		return extraUsage{}, false
	}
	e.SampledAt = observedAt
	return e, true
}

type usageExtraPayload struct {
	IsEnabled     *bool    `json:"is_enabled"`
	MonthlyLimit  *float64 `json:"monthly_limit"`
	UsedCredits   *float64 `json:"used_credits"`
	Utilization   *float64 `json:"utilization"`
	Currency      string   `json:"currency"`
	DecimalPlaces *int     `json:"decimal_places"`
}

func finiteNonNegative(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0
}

// parseUsageExtra reads the optional extra_usage object of /api/oauth/usage.
// A malformed object is ignored and never fails the usage poll.
func parseUsageExtra(raw json.RawMessage) (extraUsage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return extraUsage{}, false
	}
	var p usageExtraPayload
	if json.Unmarshal(raw, &p) != nil {
		return extraUsage{}, false
	}
	var e extraUsage
	if p.IsEnabled != nil {
		enabled := *p.IsEnabled
		e.Enabled = &enabled
	}
	if finiteNonNegative(p.UsedCredits) && finiteNonNegative(p.MonthlyLimit) && *p.MonthlyLimit > 0 {
		e.UsedCredits, e.MonthlyLimit, e.HasAmounts = *p.UsedCredits, *p.MonthlyLimit, true
	}
	if finiteNonNegative(p.Utilization) {
		e.Percent, e.HasPercent = math.Min(100, *p.Utilization), true
	} else if e.HasAmounts {
		e.Percent, e.HasPercent = math.Min(100, e.UsedCredits/e.MonthlyLimit*100), true
	}
	e.Currency = strings.ToUpper(boundedHeaderText(p.Currency))
	if p.DecimalPlaces != nil && *p.DecimalPlaces >= 0 && *p.DecimalPlaces <= 6 {
		places := *p.DecimalPlaces
		e.DecimalPlaces = &places
	}
	if e.empty() {
		return extraUsage{}, false
	}
	return e, true
}

// mergeExtraUsage folds a new observation into the stored state. Header
// observations own percent/reset/status; polls own the amounts and update the
// percent only when they are newer than the last header observation.
func mergeExtraUsage(current, next extraUsage, sampledAt time.Time, isPoll bool) (extraUsage, bool) {
	changed := false
	if isPoll {
		if next.Enabled != nil || next.HasAmounts || next.Currency != "" || next.DecimalPlaces != nil {
			current.Enabled = next.Enabled
			current.UsedCredits, current.MonthlyLimit, current.HasAmounts = next.UsedCredits, next.MonthlyLimit, next.HasAmounts
			current.Currency = next.Currency
			current.DecimalPlaces = next.DecimalPlaces
			current.AmountsSampledAt = sampledAt
			changed = true
		}
		if next.HasPercent && !sampledAt.Before(current.SampledAt) {
			current.Percent, current.HasPercent = next.Percent, true
			current.SampledAt = sampledAt
			changed = true
		}
		return current, changed
	}
	if !current.SampledAt.IsZero() && next.SampledAt.Before(current.SampledAt) {
		return current, false
	}
	if next.HasPercent {
		current.Percent, current.HasPercent = next.Percent, true
	}
	current.ResetAt = next.ResetAt
	current.Status = next.Status
	current.DisabledReason = next.DisabledReason
	current.SampledAt = next.SampledAt
	return current, true
}

// commitExtraUsage stores an extra-usage observation for the same credential
// incarnation that produced it.
func (c *quotaCache) commitExtraUsage(authID string, generation, incarnation uint64, next extraUsage, sampledAt time.Time, isPoll bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[authID]
	if !ok || sample.ObservedGeneration != generation || sample.Incarnation != incarnation {
		return false
	}
	merged, changed := mergeExtraUsage(sample.Extra, next, sampledAt, isPoll)
	if !changed {
		return false
	}
	sample.Extra = merged
	c.samples[authID] = sample
	return true
}

type extraUsageStatus struct {
	PercentUsed      *float64 `json:"percent_used,omitempty"`
	ResetAt          string   `json:"reset_at,omitempty"`
	Status           string   `json:"status,omitempty"`
	DisabledReason   string   `json:"disabled_reason,omitempty"`
	Enabled          *bool    `json:"enabled,omitempty"`
	UsedCredits      *float64 `json:"used_credits,omitempty"`
	MonthlyLimit     *float64 `json:"monthly_limit,omitempty"`
	Currency         string   `json:"currency,omitempty"`
	DecimalPlaces    *int     `json:"decimal_places,omitempty"`
	SampledAt        string   `json:"sampled_at,omitempty"`
	AmountsSampledAt string   `json:"amounts_sampled_at,omitempty"`
}

func extraUsageStatusFor(e extraUsage) *extraUsageStatus {
	if e.empty() {
		return nil
	}
	out := &extraUsageStatus{Status: e.Status, DisabledReason: e.DisabledReason, Enabled: e.Enabled, Currency: e.Currency, DecimalPlaces: e.DecimalPlaces}
	if e.HasPercent {
		percent := e.Percent
		out.PercentUsed = &percent
	}
	if !e.ResetAt.IsZero() {
		out.ResetAt = e.ResetAt.UTC().Format(time.RFC3339)
	}
	if e.HasAmounts {
		used, limit := e.UsedCredits, e.MonthlyLimit
		out.UsedCredits, out.MonthlyLimit = &used, &limit
	}
	if !e.SampledAt.IsZero() {
		out.SampledAt = e.SampledAt.UTC().Format(time.RFC3339Nano)
	}
	if !e.AmountsSampledAt.IsZero() {
		out.AmountsSampledAt = e.AmountsSampledAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}
