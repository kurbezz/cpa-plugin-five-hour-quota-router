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
