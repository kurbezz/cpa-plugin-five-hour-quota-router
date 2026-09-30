package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUsageWindows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":20,"resets_at":"2099-01-01T00:00:00Z"},"limits":[{"kind":"weekly_all","percent":30,"resets_at":"2099-01-03T00:00:00Z"},{"kind":"weekly_scoped","percent":96,"resets_at":"2099-01-05T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}]}`))
	}))
	defer server.Close()
	got, err := newHTTPUsageFetcher(server.URL, nil, "test").fetch(context.Background(), "fake", time.Second)
	if err != "" || got.Windows.FiveHour.Percent != 20 || got.Windows.Weekly.Percent != 30 || got.Windows.FableWeekly.Percent != 96 {
		t.Fatalf("fetch: %+v err=%q", got, err)
	}
}

func TestUsageOptionalWindowsIndependent(t *testing.T) {
	result, err := parseUsageWindows(map[string]json.RawMessage{"five_hour": json.RawMessage(`{"utilization":10,"resets_at":null}`), "seven_day": json.RawMessage(`{"utilization":"bad"}`), "limits": json.RawMessage(`[{"kind":"weekly_scoped","percent":99,"scope":{"model":{"id":"opus-sonnet","display_name":"Opus"}}},{"kind":"mystery","percent":99}]`)})
	if err != "" || !result.Windows.FiveHour.Valid || result.Windows.Weekly.Valid || result.Windows.SonnetWeekly.Valid {
		t.Fatalf("optional malformed handling: %+v %q", result, err)
	}
}

func TestConservativeWindow(t *testing.T) {
	base := quotaWindow{Percent: 99, Valid: true, ResetAt: time.Time{}}
	known := quotaWindow{Percent: 20, Valid: true, ResetAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, pair := range [][2]quotaWindow{{base, known}, {known, base}} {
		got := conservativeWindow(pair[0], pair[1])
		if got.Percent != 99 || !got.ResetAt.IsZero() {
			t.Errorf("aggregate: %+v", got)
		}
	}
	first := conservativeWindow(conservativeWindow(base, known), quotaWindow{Percent: 30, Valid: true, ResetAt: known.ResetAt.Add(time.Hour)})
	second := conservativeWindow(conservativeWindow(known, quotaWindow{Percent: 30, Valid: true, ResetAt: known.ResetAt.Add(time.Hour)}), base)
	for _, got := range []quotaWindow{first, second} {
		if got.Percent != 99 || !got.ResetAt.IsZero() {
			t.Errorf("three-entry aggregate: %+v", got)
		}
	}
	batch := quotaWindowBatch{FiveHour: quotaWindow{Percent: 10, Valid: true}, Weekly: first}
	decision := evaluateQuotaDecision(batch, quotaFamilyUnknown, time.Now(), 95)
	if !decision.Excluded || !decision.ConfirmedExhausted || decision.RecoveryKnown || !decision.RecoveryAt.IsZero() {
		t.Fatalf("decision: %+v", decision)
	}
}

func TestUsageLegacyWindowsAndPrecedence(t *testing.T) {
	cases := []struct {
		name, body string
		check      func(*testing.T, quotaWindowBatch)
	}{
		{"legacy", `{"five_hour":{"utilization":1,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":2,"resets_at":"2099-01-02T00:00:00Z"},"seven_day_opus":{"utilization":3,"resets_at":"2099-01-03T00:00:00Z"},"seven_day_sonnet":{"utilization":4,"resets_at":"2099-01-04T00:00:00Z"},"seven_day_fable":{"utilization":5,"resets_at":"2099-01-05T00:00:00Z"}}`, func(t *testing.T, b quotaWindowBatch) {
			for _, w := range []struct {
				got quotaWindow
				p   float64
				day int
			}{{b.FiveHour, 1, 1}, {b.Weekly, 2, 2}, {b.OpusWeekly, 3, 3}, {b.SonnetWeekly, 4, 4}, {b.FableWeekly, 5, 5}} {
				assertUsageWindow(t, w.got, w.p, w.day)
			}
		}},
		{"newer precedence", `{"five_hour":{"utilization":1},"seven_day":{"utilization":2,"resets_at":"2099-01-02T00:00:00Z"},"limits":[{"kind":"session","percent":6,"resets_at":"2099-01-06T00:00:00Z"},{"kind":"weekly_all","percent":7,"resets_at":"2099-01-07T00:00:00Z"},{"kind":"weekly_scoped","percent":8,"resets_at":"2099-01-08T00:00:00Z","scope":{"model":{"id":"opus","display_name":"Opus"}}}]}`, func(t *testing.T, b quotaWindowBatch) {
			assertUsageWindow(t, b.FiveHour, 6, 6)
			assertUsageWindow(t, b.Weekly, 7, 7)
			assertUsageWindow(t, b.OpusWeekly, 8, 8)
			if b.FableWeekly.Valid || b.SonnetWeekly.Valid {
				t.Fatal("unexpected scoped window")
			}
		}},
		{"invalid newer fallback", `{"five_hour":{"utilization":10},"seven_day":{"utilization":11},"limits":[{"kind":"weekly_all","percent":"bad"}]}`, func(t *testing.T, b quotaWindowBatch) {
			assertUsageWindow(t, b.FiveHour, 10, 0)
			assertUsageWindow(t, b.Weekly, 11, 0)
		}},
		{"optional malformed independent", `{"five_hour":{"utilization":10,"resets_at":null},"seven_day":{"utilization":"bad"},"seven_day_opus":{"utilization":8,"resets_at":7},"limits":[{"kind":"weekly_scoped","percent":99,"scope":{"model":{"id":"opus-sonnet","display_name":"Opus"}}},{"kind":"unknown","percent":99}]}`, func(t *testing.T, b quotaWindowBatch) {
			assertUsageWindow(t, b.FiveHour, 10, 0)
			if b.Weekly.Valid || b.OpusWeekly.Valid || b.SonnetWeekly.Valid {
				t.Fatalf("unexpected optional windows: %+v", b)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer srv.Close()
			result, err := newHTTPUsageFetcher(srv.URL, srv.Client().Transport, "").fetch(context.Background(), "fake", time.Second)
			if err != "" {
				t.Fatalf("fetch err %q", err)
			}
			tc.check(t, result.Windows)
		})
	}
}

func assertUsageWindow(t *testing.T, w quotaWindow, percent float64, day int) {
	t.Helper()
	if !w.Valid || w.Percent != percent || w.Source != quotaSourceUsage {
		t.Fatalf("window %+v want valid percent %v source usage", w, percent)
	}
	if day == 0 {
		if !w.ResetAt.IsZero() {
			t.Fatalf("reset %s want zero", w.ResetAt)
		}
		return
	}
	want := time.Date(2099, 1, day, 0, 0, 0, 0, time.UTC)
	if !w.ResetAt.Equal(want) {
		t.Fatalf("reset %s want %s", w.ResetAt, want)
	}
}
