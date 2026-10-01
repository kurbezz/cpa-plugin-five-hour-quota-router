package main

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestSchedulerRetryEnvelope(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		delay   time.Duration
		unknown bool
		want    int64
	}{
		{name: "14712 seconds", delay: 14712 * time.Second, want: 14712},
		{name: "ceil 1.1 seconds", delay: 1100 * time.Millisecond, want: 2},
		{name: "missing reset"},
		{name: "unknown candidate", delay: time.Hour, unknown: true},
		{name: "unrepresentable ceiling", delay: time.Duration(math.MaxInt64/int64(time.Second))*time.Second + time.Nanosecond},
		{name: "maximum duration", delay: time.Duration(math.MaxInt64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRuntime(&fakeHost{}, nil, now)
			cfg := defaultPluginConfig()
			cfg.OverageFallbackEnabled = tc.unknown // Unknown candidates must prevent fallback even when enabled.
			r.config.Store(&cfg)
			var reset time.Time
			if tc.delay > 0 {
				reset = now.Add(tc.delay)
			}
			r.cache.recordSuccess("A", 99, reset, now)
			req := claudeRequest(candidate("A", 100))
			if tc.unknown {
				req.Candidates = append(req.Candidates, candidate("B", 1))
			}
			response, decisionError := r.pick(req)
			if response.Handled || response.AuthID != "" || decisionError == nil {
				t.Fatalf("response=%+v error=%+v, want exhausted rejection", response, decisionError)
			}
			wantMessage := exhaustedErrorCode
			if tc.want > 0 {
				wantMessage = fmt.Sprintf("%s; retry_after_seconds=%d; resets_at=%s", exhaustedErrorCode, tc.want, reset.Format(time.RFC3339))
			}
			if decisionError.Code != exhaustedErrorCode || decisionError.Message != wantMessage {
				t.Fatalf("legacy code/message changed: %+v", decisionError)
			}
			raw, err := json.Marshal(envelope{Error: decisionError})
			if err != nil {
				t.Fatal(err)
			}
			assertRetryEnvelope(t, raw, tc.want)
			previous := activeRuntime
			activeRuntime = r
			defer func() { activeRuntime = previous }()
			request, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, err = handleMethod(pluginabi.MethodSchedulerPick, request)
			if err != nil {
				t.Fatal(err)
			}
			assertRetryEnvelope(t, raw, tc.want)
		})
	}
}

func assertRetryEnvelope(t *testing.T, raw []byte, want int64) {
	t.Helper()
	var got struct {
		OK    bool `json:"ok"`
		Error struct {
			HTTPStatus int    `json:"http_status"`
			Retry      *int64 `json:"retry_after_seconds"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.OK || got.Error.HTTPStatus != 429 {
		t.Fatalf("want error HTTP 429, got %s", raw)
	}
	if want == 0 {
		if got.Error.Retry != nil {
			t.Fatalf("unknown recovery must omit delay: %s", raw)
		}
		var fields struct {
			Error map[string]json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields.Error["retry_after_seconds"]; present {
			t.Fatalf("unknown recovery must omit key, not encode null: %s", raw)
		}
	} else if got.Error.Retry == nil || *got.Error.Retry != want {
		t.Fatalf("want retry_after_seconds=%d, got %s", want, raw)
	}
}

func TestRetryABIOptionalEncoding(t *testing.T) {
	for _, field := range []string{"", `,"retry_after_seconds":14712`, `,"retry_after_seconds":0`} {
		raw := []byte(`{"ok":false,"error":{"code":"quota","message":"legacy","http_status":429` + field + `}}`)
		var decoded envelope
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != string(raw) {
			t.Fatalf("optional pointer/status round trip: got %s want %s", encoded, raw)
		}
	}
	if got := string(errorEnvelope("ordinary", "legacy")); got != `{"ok":false,"error":{"code":"ordinary","message":"legacy"}}` {
		t.Fatalf("ordinary error gained optional fields: %s", got)
	}
}

func TestRetryDelayRepresentability(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	maxSeconds := int64(math.MaxInt64 / int64(time.Second))
	for _, tc := range []struct {
		name  string
		reset time.Time
		want  string
	}{
		{name: "max representable seconds", reset: now.Add(time.Duration(maxSeconds) * time.Second), want: fmt.Sprintf("%s; retry_after_seconds=%d; resets_at=%s", exhaustedErrorCode, maxSeconds, now.Add(time.Duration(maxSeconds)*time.Second).Format(time.RFC3339))},
		{name: "ceil beyond bound", reset: now.Add(time.Duration(maxSeconds)*time.Second + time.Nanosecond), want: exhaustedErrorCode},
		{name: "saturated subtraction", reset: now.AddDate(400, 0, 0), want: exhaustedErrorCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seconds := resetRetryAfterSeconds(now, tc.reset, true)
			if tc.want == exhaustedErrorCode {
				if seconds != nil {
					t.Fatalf("invalid difference must omit structured delay: %d", *seconds)
				}
			} else if seconds == nil || *seconds != maxSeconds {
				t.Fatalf("maximum delay not preserved: %v", seconds)
			}
			if got := exhaustedErrorMessage(now, tc.reset, true); got != tc.want {
				t.Fatalf("message=%q want=%q", got, tc.want)
			}
		})
	}
}
