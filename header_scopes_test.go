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
