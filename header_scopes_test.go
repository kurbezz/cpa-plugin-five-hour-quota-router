package main

import (
	"net/http"
	"testing"
	"time"
)

func TestHeaderWindows(t *testing.T) {
	h := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.2"}, "Anthropic-Ratelimit-Unified-5h-Reset": []string{"4102444800"}}
	got := parseClaudeQuotaHeaders(h, "claude-fable-5", time.Unix(1, 0), 95)
	if got.FiveHour.Percent != 20 || !got.FiveHour.Valid {
		t.Fatalf("5h: %+v", got.FiveHour)
	}
	if got.FiveHour.SampledAt != time.Unix(1, 0) || got.FiveHour.Source != quotaSourceHeaders {
		t.Fatalf("metadata: %+v", got.FiveHour)
	}
	h.Set("Anthropic-Ratelimit-Unified-7d_oi-Utilization", "0.96")
	h.Set("Anthropic-Ratelimit-Unified-7d_oi-Reset", "4102444800")
	if f := parseClaudeQuotaHeaders(h, "claude-fable-5", time.Unix(1, 0), 95).FableWeekly; !f.Valid || f.Percent != 96 {
		t.Fatalf("Fable weekly: %+v", f)
	}
	if parseClaudeQuotaHeaders(h, "claude-opus-4-1", time.Unix(1, 0), 95).FableWeekly.Valid {
		t.Fatal("Fable signal applied to Opus")
	}
	if parseClaudeQuotaHeaders(h, "custom", time.Unix(1, 0), 95).FableWeekly.Valid {
		t.Fatal("Fable signal applied to unknown model")
	}
	h.Del("Anthropic-Ratelimit-Unified-5h-Reset")
	h.Del("Anthropic-Ratelimit-Unified-5h-Status")
	h.Del("Anthropic-Ratelimit-Unified-5h-Utilization")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.99")
	if parseClaudeQuotaHeaders(h, "claude-fable-5", time.Unix(1, 0), 95).FiveHour.Valid {
		t.Fatal("high no-reset header accepted")
	}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.5")
	if w := parseClaudeQuotaHeaders(h, "claude-fable-5", time.Unix(1, 0), 95).FiveHour; !w.Valid || !w.ResetAt.IsZero() {
		t.Fatalf("low no-reset header: %+v", w)
	}
}

func TestHeaderWindowsIndependentAndRejected(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization":    {"0.99"},
		"Anthropic-Ratelimit-Unified-5h-Status":         {"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset":          {"bad"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":    {"0.2"},
		"Anthropic-Ratelimit-Unified-7d-Reset":          {"4102444800"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.98"},
	}
	b := parseClaudeQuotaHeaders(h, "claude-fable-5", now, 95)
	if b.FiveHour.Valid || !b.Weekly.Valid || b.Weekly.Percent != 20 || b.FableWeekly.Valid {
		t.Fatalf("independent parse %+v", b)
	}
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "4102444800")
	b = parseClaudeQuotaHeaders(h, "claude-fable-5", now, 95)
	if !b.FiveHour.Valid || b.FiveHour.Percent != 100 {
		t.Fatalf("rejected parse %+v", b.FiveHour)
	}
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.99")
	h.Del("Anthropic-Ratelimit-Unified-7d-Reset")
	b = parseClaudeQuotaHeaders(h, "claude-fable-5", now, 95)
	if b.Weekly.Valid || !b.FiveHour.Valid {
		t.Fatalf("high weekly missing reset not isolated: %+v", b)
	}
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.5")
	b = parseClaudeQuotaHeaders(h, "claude-fable-5", now, 95)
	if !b.Weekly.Valid || !b.Weekly.ResetAt.IsZero() {
		t.Fatalf("low weekly missing reset: %+v", b.Weekly)
	}
	for _, model := range []string{"claude-opus-4-1", "claude-sonnet-4-6", "custom-chat", "claude-opus-sonnet"} {
		if parseClaudeQuotaHeaders(h, model, now, 95).FableWeekly.Valid {
			t.Errorf("7d_oi applied to %s", model)
		}
	}
}
