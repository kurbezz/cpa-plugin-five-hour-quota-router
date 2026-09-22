package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

type usageResult struct {
	FiveHourPercentUsed float64
	ResetAt             time.Time
}

type usageFetcher func(context.Context, string, time.Duration) (usageResult, string)

type httpUsageFetcher struct {
	client    *http.Client
	endpoint  string
	userAgent string
}

func newHTTPUsageFetcher(endpoint string, transport http.RoundTripper, userAgent string) httpUsageFetcher {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return httpUsageFetcher{
		endpoint:  endpoint,
		userAgent: userAgent,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// usageResponse handles two observed shapes of Anthropic's undocumented
// /api/oauth/usage response:
//
//	Shape A (flat, common):  {"five_hour": {"utilization": 35.0, "resets_at": "..."}}
//	Shape B (newer):         {"five_hour": null, "limits": [{"kind": "session", "percent": 33, "resets_at": "..."}]}
type usageResponse struct {
	FiveHour *usageWindow `json:"five_hour"`
	Limits   []usageLimit `json:"limits"`
}

type usageWindow struct {
	Utilization *float64        `json:"utilization"`
	ResetsAt    json.RawMessage `json:"resets_at"`
}

type usageLimit struct {
	Kind     string          `json:"kind"`
	Percent  *float64        `json:"percent"`
	ResetsAt json.RawMessage `json:"resets_at"`
}

func (f httpUsageFetcher) fetch(ctx context.Context, token string, timeout time.Duration) (usageResult, string) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, f.endpoint, nil)
	if err != nil {
		return usageResult{}, pollErrorHTTP
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", anthropicOAuthBeta)
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return usageResult{}, pollErrorCancelled
		case errors.Is(err, context.DeadlineExceeded):
			return usageResult{}, pollErrorTimeout
		default:
			return usageResult{}, pollErrorNetwork
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return usageResult{}, pollErrorUnauthorized
		case http.StatusForbidden:
			return usageResult{}, pollErrorForbidden
		case http.StatusTooManyRequests:
			return usageResult{}, pollErrorRateLimited
		default:
			if resp.StatusCode >= 500 {
				return usageResult{}, pollErrorServer
			}
			return usageResult{}, pollErrorHTTP
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageResponseBytes+1))
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return usageResult{}, pollErrorCancelled
		case errors.Is(err, context.DeadlineExceeded):
			return usageResult{}, pollErrorTimeout
		default:
			return usageResult{}, pollErrorRead
		}
	}
	if len(body) > maxUsageResponseBytes {
		return usageResult{}, pollErrorBodyTooLarge
	}
	var payload usageResponse
	if json.Unmarshal(body, &payload) != nil {
		return usageResult{}, pollErrorInvalidJSON
	}

	if payload.FiveHour != nil && payload.FiveHour.Utilization != nil {
		percentUsed, ok := normalizeUtilization(payload.FiveHour.Utilization)
		if !ok {
			return usageResult{}, pollErrorInvalidUsage
		}
		// A null/absent resets_at means the account has no active five-hour
		// session (most commonly seen at utilization=0, just after a reset or
		// before first use this window). That is a valid, healthy sample, not
		// a parse failure: use the zero time.Time{} to mean "no expiry known
		// yet"; cache.go's known()/excluded() already treat a zero ResetAt as
		// "rely on the percentage alone", which is exactly correct here.
		resetAt, hasResetAt := parseResetTime(payload.FiveHour.ResetsAt)
		if !hasResetAt && !isNullOrEmptyRaw(payload.FiveHour.ResetsAt) {
			// resets_at was present but malformed (not a valid RFC3339 string
			// and not null) - that is a genuine parse failure.
			return usageResult{}, pollErrorInvalidUsage
		}
		return usageResult{FiveHourPercentUsed: percentUsed, ResetAt: resetAt}, ""
	}

	for _, entry := range payload.Limits {
		if !strings.EqualFold(entry.Kind, "session") || entry.Percent == nil {
			continue
		}
		percentUsed, ok := normalizeUtilization(entry.Percent)
		if !ok {
			return usageResult{}, pollErrorInvalidUsage
		}
		resetAt, hasResetAt := parseResetTime(entry.ResetsAt)
		if !hasResetAt && !isNullOrEmptyRaw(entry.ResetsAt) {
			return usageResult{}, pollErrorInvalidUsage
		}
		return usageResult{FiveHourPercentUsed: percentUsed, ResetAt: resetAt}, ""
	}

	return usageResult{}, pollErrorInvalidUsage
}

// isNullOrEmptyRaw reports whether raw JSON represents an absent or explicit
// null value, as opposed to a present-but-malformed one.
func isNullOrEmptyRaw(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// Anthropic reports percentage points: 1.0 means 1%, not 100%.
func normalizeUtilization(raw *float64) (float64, bool) {
	if raw == nil || math.IsNaN(*raw) || math.IsInf(*raw, 0) || *raw < 0 || *raw > 100 {
		return 0, false
	}
	return *raw, true
}

func parseResetTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text))
	return parsed, err == nil
}
