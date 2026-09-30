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

const quotaSourceUsage = "usage"

type usageResult struct {
	FiveHourPercentUsed float64
	ResetAt             time.Time
	Windows             quotaWindowBatch
}
type usageFetcher func(context.Context, string, time.Duration) (usageResult, string)
type httpUsageFetcher struct {
	client              *http.Client
	endpoint, userAgent string
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
func (f httpUsageFetcher) fetch(ctx context.Context, token string, timeout time.Duration) (usageResult, string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.endpoint, nil)
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
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return usageResult{}, pollErrorInvalidJSON
	}
	return parseUsageWindows(payload)
}

type usageLimitEntry struct {
	Kind        string          `json:"kind"`
	Percent     json.RawMessage `json:"percent"`
	Utilization json.RawMessage `json:"utilization"`
	Reset       json.RawMessage `json:"resets_at"`
	Scope       struct {
		Model struct {
			ID      string `json:"id"`
			Display string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

func parseUsageWindows(root map[string]json.RawMessage) (usageResult, string) {
	var out usageResult
	var batch quotaWindowBatch
	legacy := map[string]quotaWindow{"five_hour": {}, "seven_day": {}, "seven_day_fable": {}, "seven_day_opus": {}, "seven_day_sonnet": {}}
	legacyFiveInvalid := false
	if raw, ok := root["five_hour"]; ok && string(raw) == "null" {
		legacyFiveInvalid = true
	}
	for key := range legacy {
		raw := root[key]
		if len(raw) == 0 {
			continue
		}
		if string(raw) == "null" {
			if key == "five_hour" {
				legacyFiveInvalid = true
			}
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			if key == "five_hour" {
				legacyFiveInvalid = true
			}
			continue
		}
		v := obj["utilization"]
		if len(v) == 0 {
			if key == "five_hour" {
				legacyFiveInvalid = true
			}
			continue
		}
		w, ok := parseUsageWindowValue(v, obj["resets_at"])
		if !ok {
			if key == "five_hour" {
				legacyFiveInvalid = true
			}
			continue
		}
		legacy[key] = w
	}
	limitsRaw := root["limits"]
	var limits []usageLimitEntry
	if len(limitsRaw) > 0 && string(limitsRaw) != "null" {
		_ = json.Unmarshal(limitsRaw, &limits)
	}
	var newer quotaWindowBatch
	var seen [5]bool
	for _, entry := range limits {
		kind := strings.ToLower(strings.TrimSpace(entry.Kind))
		idx := -1
		switch kind {
		case "session":
			idx = 0
		case "weekly_all":
			idx = 1
		case "weekly_scoped":
			switch resolveQuotaScopeFamily(entry.Scope.Model.ID, entry.Scope.Model.Display) {
			case quotaFamilyFable:
				idx = 2
			case quotaFamilyOpus:
				idx = 3
			case quotaFamilySonnet:
				idx = 4
			}
		}
		if idx < 0 {
			continue
		}
		value := entry.Percent
		if len(value) == 0 {
			value = entry.Utilization
		}
		w, ok := parseUsageWindowValue(value, entry.Reset)
		if !ok {
			continue
		}
		slots := []*quotaWindow{&newer.FiveHour, &newer.Weekly, &newer.FableWeekly, &newer.OpusWeekly, &newer.SonnetWeekly}
		if !seen[idx] {
			*slots[idx] = w
			seen[idx] = true
		} else {
			*slots[idx] = conservativeWindow(*slots[idx], w)
		}
	}
	old := []quotaWindow{legacy["five_hour"], legacy["seven_day"], legacy["seven_day_fable"], legacy["seven_day_opus"], legacy["seven_day_sonnet"]}
	fresh := []quotaWindow{newer.FiveHour, newer.Weekly, newer.FableWeekly, newer.OpusWeekly, newer.SonnetWeekly}
	slots := []*quotaWindow{&batch.FiveHour, &batch.Weekly, &batch.FableWeekly, &batch.OpusWeekly, &batch.SonnetWeekly}
	for i := range slots {
		*slots[i] = old[i]
		if seen[i] {
			*slots[i] = fresh[i]
		}
	}
	if !batch.FiveHour.Valid || legacyFiveInvalid && !seen[0] {
		return usageResult{}, pollErrorInvalidUsage
	}
	out.Windows = batch
	out.FiveHourPercentUsed = batch.FiveHour.Percent
	out.ResetAt = batch.FiveHour.ResetAt
	return out, ""
}
func parseUsageWindowValue(percentRaw, resetRaw json.RawMessage) (quotaWindow, bool) {
	var p float64
	if len(percentRaw) == 0 || string(percentRaw) == "null" || json.Unmarshal(percentRaw, &p) != nil || math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 100 {
		return quotaWindow{}, false
	}
	reset := time.Time{}
	if len(resetRaw) > 0 && string(resetRaw) != "null" {
		parsed, ok := parseResetTime(resetRaw)
		if !ok {
			return quotaWindow{}, false
		}
		reset = parsed
	}
	return quotaWindow{Percent: p, ResetAt: reset, Source: quotaSourceUsage, Valid: true}, true
}
func conservativeWindow(a, b quotaWindow) quotaWindow {
	resetMissing := a.ResetAt.IsZero() || b.ResetAt.IsZero()
	if b.Percent > a.Percent {
		a.Percent = b.Percent
	}
	if resetMissing {
		a.ResetAt = time.Time{}
	} else if b.ResetAt.After(a.ResetAt) {
		a.ResetAt = b.ResetAt
	}
	return a
}
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
