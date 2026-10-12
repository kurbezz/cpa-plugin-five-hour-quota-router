package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func overageHeaders(utilization, reset, status string) http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.19")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1791859800")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	if utilization != "" {
		h.Set("Anthropic-Ratelimit-Unified-Overage-Utilization", utilization)
	}
	if reset != "" {
		h.Set("Anthropic-Ratelimit-Unified-Overage-Reset", reset)
	}
	if status != "" {
		h.Set("Anthropic-Ratelimit-Unified-Overage-Status", status)
	}
	return h
}

func TestParseClaudeOverageHeaders(t *testing.T) {
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	e, ok := parseClaudeOverageHeaders(overageHeaders("0.05", "1793491200", "allowed"), at)
	if !ok || !e.HasPercent || e.Percent != 5 || e.Status != "allowed" || !e.ResetAt.Equal(time.Unix(1793491200, 0)) || !e.SampledAt.Equal(at) {
		t.Fatalf("overage = %+v ok=%v", e, ok)
	}
	if _, ok := parseClaudeOverageHeaders(overageHeaders("", "", ""), at); ok {
		t.Fatal("absent overage headers produced an observation")
	}
	e, ok = parseClaudeOverageHeaders(overageHeaders("", "", "Rejected"), at)
	if !ok || e.HasPercent || e.Status != "rejected" {
		t.Fatalf("status-only overage = %+v", e)
	}
}

func TestParseUsageExtra(t *testing.T) {
	e, ok := parseUsageExtra(json.RawMessage(`{"is_enabled":true,"monthly_limit":100000,"used_credits":4575,"utilization":4.575,"currency":"usd"}`))
	if !ok || !e.HasAmounts || e.UsedCredits != 4575 || e.MonthlyLimit != 100000 || e.Percent != 4.575 || e.Currency != "USD" || e.Enabled == nil || !*e.Enabled {
		t.Fatalf("extra = %+v", e)
	}
	e, ok = parseUsageExtra(json.RawMessage(`{"is_enabled":true,"monthly_limit":5000,"used_credits":1234}`))
	if !ok || e.Percent != 24.68 {
		t.Fatalf("derived percent = %+v", e)
	}
	for _, raw := range []string{``, `null`, `"x"`, `{}`} {
		if _, ok := parseUsageExtra(json.RawMessage(raw)); ok {
			t.Fatalf("parseUsageExtra(%q) accepted", raw)
		}
	}
	// A malformed extra_usage never fails the usage poll.
	var payload map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"five_hour":{"utilization":10,"resets_at":null},"extra_usage":{"monthly_limit":"bad"}}`), &payload)
	if result, category := parseUsageWindows(payload); category != "" || result.HasExtra {
		t.Fatalf("malformed extra_usage: category=%q hasExtra=%v", category, result.HasExtra)
	}
}

func TestExtraUsageFromHeadersAndPollReachesStatus(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	clock := &testClock{value: now}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":19,"resets_at":"2026-10-13T02:50:00Z"},"extra_usage":{"is_enabled":true,"monthly_limit":100000,"used_credits":4575,"utilization":4.575,"currency":"USD"}}`))
	}))
	defer server.Close()
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token-a")},
	}
	r := newPluginRuntime(host, newHTTPUsageFetcher(server.URL, server.Client().Transport, "").fetch, clock.now)
	r.pollOnce(context.Background(), defaultPluginConfig())

	sample := r.cache.snapshot("auth-a")
	if !sample.Extra.HasAmounts || sample.Extra.UsedCredits != 4575 || sample.Extra.Percent != 4.575 || !sample.Extra.AmountsSampledAt.Equal(now) {
		t.Fatalf("poll extra = %+v", sample.Extra)
	}

	// A later correlated response header updates percent/reset/status but keeps amounts.
	headerAt := now.Add(2 * time.Minute)
	clock.set(headerAt)
	r.recordSelectedAuth("req-1", map[string]any{"selected_auth_id": "auth-a"})
	r.observeResponseHeaders(testModel, "req-1", overageHeaders("0.06", "1793491200", "allowed_warning"), headerAt)

	response := r.handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementStatusFullPath})
	var raw struct {
		Accounts []struct {
			ExtraUsage map[string]any `json:"extra_usage"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body, &raw); err != nil || len(raw.Accounts) != 1 {
		t.Fatalf("status = %s err=%v", response.Body, err)
	}
	got := raw.Accounts[0].ExtraUsage
	want := map[string]any{
		"percent_used": 6.0, "reset_at": "2026-11-01T00:00:00Z", "status": "allowed_warning", "enabled": true,
		"used_credits": 4575.0, "monthly_limit": 100000.0, "currency": "USD",
		"sampled_at": headerAt.Format(time.RFC3339Nano), "amounts_sampled_at": now.Format(time.RFC3339Nano),
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("extra_usage[%q] = %#v, want %#v (all: %#v)", k, got[k], v, got)
		}
	}

	// An older poll result never overwrites the newer header percent.
	merged, _ := mergeExtraUsage(r.cache.snapshot("auth-a").Extra, extraUsage{Percent: 1, HasPercent: true}, now.Add(time.Minute), true)
	if merged.Percent != 6 {
		t.Fatalf("older poll overwrote header percent: %+v", merged)
	}
}

func TestOverageOnlyHeadersAreObserved(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	r := newTestRuntime(&fakeHost{}, nil, now)
	_, _ = r.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", AuthIndex: "index-a", Identity: "id"}})
	r.recordSelectedAuth("req-1", map[string]any{"selected_auth_id": "auth-a"})
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	r.observeResponseHeaders(testModel, "req-1", h, now)
	if s := r.cache.snapshot("auth-a"); s.Extra.Status != "rejected" || s.HasSample {
		t.Fatalf("sample = %+v", s)
	}
}
