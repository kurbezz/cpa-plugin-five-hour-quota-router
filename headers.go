package main

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// headerObservation is the parsed, non-secret result of inspecting a real
// successful upstream Claude response's rate-limit headers. It intentionally
// carries only numbers and a timestamp -- never raw header values, tokens, or
// any other credential material.
type headerObservation struct {
	PercentUsed float64
	ResetAt     time.Time
	ObservedAt  time.Time
	Valid       bool
}

// parseClaudeFiveHourHeaders extracts Anthropic's unified five-hour rate-limit
// headers from a successful upstream response.
//
//   - Anthropic-Ratelimit-Unified-5h-Utilization is a FRACTION (0.23 == 23%).
//     It is converted to the plugin's 0-100 percent scale and clamped to 100.
//   - Anthropic-Ratelimit-Unified-5h-Status == "rejected" forces percent to
//     100 regardless of (or in the absence of) a utilization value, since a
//     rejection is definitive evidence of exhaustion.
//   - Anthropic-Ratelimit-Unified-5h-Reset is accepted as unix seconds
//     (fractional allowed) or RFC3339. A missing/invalid reset stores the zero
//     time, matching the usage-API fallback's "no expiry known yet" semantics
//     -- it never by itself enables the pre-auth 429 gate.
//   - If utilization is absent/invalid AND status is not "rejected", the
//     observation is invalid and must be ignored entirely (no cache update).
func parseClaudeFiveHourHeaders(headers http.Header, observedAt time.Time) headerObservation {
	if headers == nil {
		return headerObservation{}
	}
	rejected := strings.EqualFold(strings.TrimSpace(headers.Get("Anthropic-Ratelimit-Unified-5h-Status")), "rejected")

	var percent float64
	haveUtilization := false
	if raw := strings.TrimSpace(headers.Get("Anthropic-Ratelimit-Unified-5h-Utilization")); raw != "" {
		if fraction, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(fraction) && !math.IsInf(fraction, 0) && fraction >= 0 {
			percent = fraction * 100
			if percent > 100 {
				percent = 100
			}
			haveUtilization = true
		}
	}
	if rejected {
		percent = 100
	} else if !haveUtilization {
		return headerObservation{}
	}

	resetAt, _ := parseClaudeHeaderResetTime(strings.TrimSpace(headers.Get("Anthropic-Ratelimit-Unified-5h-Reset")))
	return headerObservation{PercentUsed: percent, ResetAt: resetAt, ObservedAt: observedAt, Valid: true}
}

// parseClaudeHeaderResetTime accepts unix seconds (integer or fractional) or
// RFC3339. An empty or unparseable value reports false and the caller treats
// the reset as unknown (zero time), never as a parse failure that discards the
// whole observation.
func parseClaudeHeaderResetTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	if sec, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(sec) && !math.IsInf(sec, 0) && sec > 0 {
		secInt := int64(sec)
		nsec := int64((sec - float64(secInt)) * 1e9)
		return time.Unix(secInt, nsec).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	return time.Time{}, false
}
