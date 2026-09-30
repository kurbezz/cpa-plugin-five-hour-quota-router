package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestModelQuotaStatusWindows(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	const secret = "fixture-token-secret"
	c := quotaCache{samples: map[string]quotaSample{"acct": {
		Identity: "fixture-identity-marker", Revision: "fixture-digest-marker", Name: "safe-name", HasSample: true, FiveHourPercentUsed: 10,
		Windows: quotaWindowBatch{
			FiveHour:    quotaWindow{Percent: 10, SampledAt: now, Source: quotaSourceUsage, Valid: true},
			Weekly:      quotaWindow{Percent: 20, SampledAt: now, Source: quotaSourceHeaders, Valid: true},
			FableWeekly: quotaWindow{Percent: 99, ResetAt: now.Add(time.Hour), SampledAt: now, Source: quotaSourceUsage, Valid: true},
		},
	}}}
	status := cutoffStatusResponse{Accounts: c.statuses(now, 95)}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, secret) || strings.Contains(text, "fixture-identity-marker") || strings.Contains(text, "fixture-digest-marker") || strings.Contains(text, "arbitrary_scope") {
		t.Fatalf("sensitive/unbounded data in status: %s", text)
	}
	var got cutoffStatusResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].Blocked {
		t.Fatalf("account legacy five-hour state changed: %+v", got.Accounts)
	}
	if len(got.Accounts[0].Windows) != 3 {
		t.Fatalf("window status missing: %+v", got.Accounts[0])
	}
	allowedScopes := map[string]bool{"five_hour": true, "weekly": true, "fable_weekly": true, "opus_weekly": true, "sonnet_weekly": true}
	allowedSources := map[string]bool{quotaSourceUsage: true, quotaSourceHeaders: true}
	found := false
	for _, w := range got.Accounts[0].Windows {
		if !allowedScopes[w.Scope] || !allowedSources[w.Source] || !w.Known {
			t.Fatalf("unbounded status scope/source: %+v", w)
		}
		if w.Scope == "fable_weekly" {
			found = true
			if !w.Blocked || w.Source != quotaSourceUsage {
				t.Fatalf("fable status=%+v", w)
			}
		}
	}
	if !found {
		t.Fatal("normalized Fable scope missing")
	}
}
