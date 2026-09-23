package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// --- parseClaudeFiveHourHeaders unit tests -------------------------------

func TestParseClaudeFiveHourHeadersFractionToPercent(t *testing.T) {
	now := time.Now().UTC()
	headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.23"}}
	observation := parseClaudeFiveHourHeaders(headers, now)
	if !observation.Valid || observation.PercentUsed != 23 {
		t.Fatalf("observation = %#v", observation)
	}
}

func TestParseClaudeFiveHourHeadersClampsAboveOne(t *testing.T) {
	now := time.Now().UTC()
	headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"1.5"}}
	observation := parseClaudeFiveHourHeaders(headers, now)
	if !observation.Valid || observation.PercentUsed != 100 {
		t.Fatalf("observation = %#v, want percent=100", observation)
	}
}

func TestParseClaudeFiveHourHeadersRejectedForcesFullPercent(t *testing.T) {
	now := time.Now().UTC()
	headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Status": []string{"rejected"}}
	observation := parseClaudeFiveHourHeaders(headers, now)
	if !observation.Valid || observation.PercentUsed != 100 {
		t.Fatalf("observation = %#v, want percent=100 on rejected status with no utilization header", observation)
	}

	// Rejected also overrides a present-but-lower utilization value.
	headers = http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status":      []string{"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.10"},
	}
	observation = parseClaudeFiveHourHeaders(headers, now)
	if !observation.Valid || observation.PercentUsed != 100 {
		t.Fatalf("observation = %#v, want percent=100 on rejected status overriding utilization", observation)
	}
}

func TestParseClaudeFiveHourHeadersInvalidUtilizationIgnored(t *testing.T) {
	now := time.Now().UTC()
	for _, raw := range []string{"not-a-number", "NaN", "-0.5", ""} {
		headers := http.Header{}
		if raw != "" {
			headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", raw)
		}
		observation := parseClaudeFiveHourHeaders(headers, now)
		if observation.Valid {
			t.Fatalf("raw=%q: observation = %#v, want invalid", raw, observation)
		}
	}
}

func TestParseClaudeFiveHourHeadersMissingHeadersInvalid(t *testing.T) {
	if observation := parseClaudeFiveHourHeaders(nil, time.Now()); observation.Valid {
		t.Fatalf("nil headers observation = %#v, want invalid", observation)
	}
	if observation := parseClaudeFiveHourHeaders(http.Header{}, time.Now()); observation.Valid {
		t.Fatalf("empty headers observation = %#v, want invalid", observation)
	}
}

func TestParseClaudeFiveHourHeadersResetUnixSeconds(t *testing.T) {
	now := time.Now().UTC()
	resetUnix := now.Add(90 * time.Minute).Unix()
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.5"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       []string{strconv.FormatInt(resetUnix, 10)},
	}
	observation := parseClaudeFiveHourHeaders(headers, now)
	if !observation.Valid || observation.ResetAt.Unix() != resetUnix {
		t.Fatalf("observation = %#v, want resetAt unix=%d", observation, resetUnix)
	}
}

func TestParseClaudeFiveHourHeadersResetRFC3339(t *testing.T) {
	now := time.Now().UTC()
	resetAt := now.Add(2 * time.Hour).Truncate(time.Second)
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.5"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       []string{resetAt.Format(time.RFC3339)},
	}
	observation := parseClaudeFiveHourHeaders(headers, now)
	if !observation.Valid || !observation.ResetAt.Equal(resetAt) {
		t.Fatalf("observation = %#v, want resetAt=%s", observation, resetAt)
	}
}

func TestParseClaudeFiveHourHeadersMissingResetIsZero(t *testing.T) {
	now := time.Now().UTC()
	headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.5"}}
	observation := parseClaudeFiveHourHeaders(headers, now)
	if !observation.Valid || !observation.ResetAt.IsZero() {
		t.Fatalf("observation = %#v, want zero resetAt", observation)
	}
	headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", "not-a-timestamp")
	observation = parseClaudeFiveHourHeaders(headers, now)
	if !observation.Valid || !observation.ResetAt.IsZero() {
		t.Fatalf("observation with malformed reset = %#v, want valid observation with zero resetAt (never discarded)", observation)
	}
}

// --- non-streaming ResponseInterceptor tests -----------------------------

func TestInterceptResponseUpdatesSelectedAuthSample(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	auth := physicalClaudeAuth{ID: "auth-a", Identity: "same"}
	runtime.cache.reconcile([]physicalClaudeAuth{auth})

	resetUnix := now.Add(3 * time.Hour).Unix()
	response := runtime.interceptResponse(pluginapi.ResponseInterceptRequest{
		Model:    "claude-opus-4-1",
		Metadata: map[string]any{"selected_auth_id": "auth-a"},
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.42"},
			"Anthropic-Ratelimit-Unified-5h-Reset":       []string{strconv.FormatInt(resetUnix, 10)},
		},
		StatusCode: http.StatusOK,
	})
	if response.Headers != nil || response.Body != nil || response.ClearHeaders != nil {
		t.Fatalf("response interceptor was not a no-op: %#v", response)
	}
	sample := runtime.cache.snapshot("auth-a")
	if !sample.HasSample || sample.FiveHourPercentUsed != 42 || sample.ResetAt.Unix() != resetUnix {
		t.Fatalf("sample = %#v", sample)
	}
}

func TestInterceptResponseIgnoresMissingOrUnknownSelectedAuthID(t *testing.T) {
	now := time.Now().UTC()
	headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.42"}}

	t.Run("missing metadata", func(t *testing.T) {
		runtime := newTestRuntime(&fakeHost{}, nil, now)
		runtime.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", Identity: "same"}})
		runtime.interceptResponse(pluginapi.ResponseInterceptRequest{Model: "claude-opus-4-1", ResponseHeaders: headers})
		if sample := runtime.cache.snapshot("auth-a"); sample.HasSample {
			t.Fatalf("sample updated without selected_auth_id: %#v", sample)
		}
	})

	t.Run("non-string metadata value", func(t *testing.T) {
		runtime := newTestRuntime(&fakeHost{}, nil, now)
		runtime.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", Identity: "same"}})
		runtime.interceptResponse(pluginapi.ResponseInterceptRequest{
			Model:           "claude-opus-4-1",
			Metadata:        map[string]any{"selected_auth_id": 12345},
			ResponseHeaders: headers,
		})
		if sample := runtime.cache.snapshot("auth-a"); sample.HasSample {
			t.Fatalf("sample updated from non-string selected_auth_id: %#v", sample)
		}
	})

	t.Run("unknown auth ID", func(t *testing.T) {
		runtime := newTestRuntime(&fakeHost{}, nil, now)
		runtime.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", Identity: "same"}})
		runtime.interceptResponse(pluginapi.ResponseInterceptRequest{
			Model:           "claude-opus-4-1",
			Metadata:        map[string]any{"selected_auth_id": "not-a-member"},
			ResponseHeaders: headers,
		})
		if sample := runtime.cache.snapshot("not-a-member"); sample.HasSample {
			t.Fatalf("sample created for unknown auth ID: %#v", sample)
		}
	})
}

func TestInterceptResponseIgnoresNonClaudeModel(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", Identity: "same"}})
	runtime.interceptResponse(pluginapi.ResponseInterceptRequest{
		Model:           "gpt-5",
		Metadata:        map[string]any{"selected_auth_id": "auth-a"},
		ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.9"}},
	})
	if sample := runtime.cache.snapshot("auth-a"); sample.HasSample {
		t.Fatalf("sample updated for non-Claude model: %#v", sample)
	}
}

// --- StreamChunkInterceptor tests -----------------------------------------

func TestInterceptStreamChunkHeaderInitUpdatesSample(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", Identity: "same"}})

	response := runtime.interceptStreamChunk(pluginapi.StreamChunkInterceptRequest{
		Model:           "claude-opus-4-1",
		Metadata:        map[string]any{"selected_auth_id": "auth-a"},
		ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.6"}},
		ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
	})
	if response.Headers != nil || response.Body != nil || response.ClearHeaders != nil || response.DropChunk {
		t.Fatalf("stream header-init interceptor was not a no-op: %#v", response)
	}
	sample := runtime.cache.snapshot("auth-a")
	if !sample.HasSample || sample.FiveHourPercentUsed != 60 {
		t.Fatalf("sample = %#v", sample)
	}
}

func TestInterceptStreamChunkPayloadIsNoOpAndDoesNotUpdate(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", Identity: "same"}})

	for _, index := range []int{0, 1, 42} {
		response := runtime.interceptStreamChunk(pluginapi.StreamChunkInterceptRequest{
			Model:           "claude-opus-4-1",
			Metadata:        map[string]any{"selected_auth_id": "auth-a"},
			ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.6"}},
			ChunkIndex:      index,
			Body:            []byte("data: {}\n\n"),
		})
		if response.Headers != nil || response.Body != nil || response.ClearHeaders != nil || response.DropChunk {
			t.Fatalf("payload chunk index=%d interceptor was not a no-op: %#v", index, response)
		}
	}
	if sample := runtime.cache.snapshot("auth-a"); sample.HasSample {
		t.Fatalf("payload chunk updated sample: %#v", sample)
	}
}

// --- stale in-flight usage poll vs. header observation --------------------

// TestStaleUsagePollDoesNotOverwriteNewerHeaderObservation reproduces: an
// /api/oauth/usage poll begins (captures pollStartedAt), a concurrent real
// request commits a newer header observation, and the slower usage poll's
// result must be rejected rather than regressing the cache.
func TestStaleUsagePollDoesNotOverwriteNewerHeaderObservation(t *testing.T) {
	now := time.Now().UTC()
	entry := physicalEntry("auth-a", "index-a")
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{entry}, authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token-a")}}

	clock := &testClock{value: now}
	pollStarted := make(chan struct{})
	releasePoll := make(chan struct{})
	runtime := newPluginRuntime(host, func(ctx context.Context, token string, _ time.Duration) (usageResult, string) {
		close(pollStarted)
		<-releasePoll
		return usageResult{FiveHourPercentUsed: 99, ResetAt: now.Add(time.Hour)}, ""
	}, clock.now)
	auth := physicalClaudeAuths([]pluginapi.HostAuthFileEntry{entry})[0]
	runtime.cache.reconcile([]physicalClaudeAuth{auth})

	done := make(chan struct{})
	go func() { runtime.pollAuth(context.Background(), auth, defaultPluginConfig(), false); close(done) }()
	<-pollStarted

	// A concurrent real request observes headers and commits a newer sample
	// while the usage poll (started earlier, at the pre-advance clock value)
	// is still in flight.
	clock.set(now.Add(time.Second))
	runtime.interceptResponse(pluginapi.ResponseInterceptRequest{
		Model:           "claude-opus-4-1",
		Metadata:        map[string]any{"selected_auth_id": "auth-a"},
		ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.10"}},
	})
	if sample := runtime.cache.snapshot("auth-a"); !sample.HasSample || sample.FiveHourPercentUsed != 10 {
		t.Fatalf("header observation did not commit: %#v", sample)
	}

	close(releasePoll)
	<-done

	sample := runtime.cache.snapshot("auth-a")
	if sample.FiveHourPercentUsed != 10 {
		t.Fatalf("stale in-flight usage poll overwrote newer header observation: %#v", sample)
	}
}

// TestOlderHeaderObservationDoesNotOverwriteNewerSample guards the header
// path's own ordering, independent of usage polling.
func TestOlderHeaderObservationDoesNotOverwriteNewerSample(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", Identity: "same"}})

	newer := headerObservation{PercentUsed: 80, ObservedAt: now.Add(time.Second), Valid: true}
	older := headerObservation{PercentUsed: 5, ObservedAt: now, Valid: true}
	if !runtime.cache.recordHeaderObservation("auth-a", newer) {
		t.Fatal("newer observation should commit")
	}
	if runtime.cache.recordHeaderObservation("auth-a", older) {
		t.Fatal("older observation should not commit")
	}
	if sample := runtime.cache.snapshot("auth-a"); sample.FiveHourPercentUsed != 80 {
		t.Fatalf("sample = %#v, want percent=80 preserved", sample)
	}
}

// --- integration with confirmedFleetExhaustedReset / before-auth gate -----

func TestHeaderObservationAtCutoffTriggersBeforeAuth429(t *testing.T) {
	now := time.Now().UTC()
	entryA := physicalEntry("auth-a", "index-a")
	entryB := physicalEntry("auth-b", "index-b")
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{entryA, entryB}}
	runtime := newTestRuntime(host, (&fakeFetcher{}).fetch, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.config.Store(&cfg)

	auths := physicalClaudeAuths(host.entries)
	runtime.cache.reconcile(auths)
	// B is already confirmed exhausted from an earlier poll.
	runtime.cache.recordSuccess("auth-b", 99, now.Add(2*time.Hour), now)

	// Not yet exhausted: A is still healthy.
	if response := runtime.interceptBeforeAuth(beforeAuthRequest()); response.Terminate {
		t.Fatalf("gate fired before A's header observation: %#v", response)
	}

	resetUnix := now.Add(45 * time.Minute).Unix()
	runtime.interceptResponse(pluginapi.ResponseInterceptRequest{
		Model:    "claude-opus-4-1",
		Metadata: map[string]any{"selected_auth_id": "auth-a"},
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Status": []string{"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Reset":  []string{strconv.FormatInt(resetUnix, 10)},
		},
	})

	response := runtime.interceptBeforeAuth(beforeAuthRequest())
	if !response.Terminate || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("gate did not fire after header observation confirmed both exhausted: %#v", response)
	}
	retryAfter := response.ResponseHeaders.Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("missing Retry-After after header-observation-driven 429")
	}
	seconds, err := strconv.ParseInt(retryAfter, 10, 64)
	if err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q, want positive seconds", retryAfter)
	}
	// Header observation's reset (A, sooner) must be the earliest reset used,
	// since it is before B's later reset.
	if seconds > 45*60 {
		t.Fatalf("Retry-After = %d, want <= 2700s (A's header-derived reset)", seconds)
	}
}

// --- ABI dispatch: no-op observe path with malformed payload --------------

func TestResponseAndStreamChunkInterceptorABIDispatchNoOpOnMalformedPayload(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	previous := activeRuntime
	activeRuntime = runtime
	defer func() { activeRuntime = previous }()

	raw, err := handleMethod(pluginabi.MethodResponseInterceptAfter, []byte("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	var respResult struct {
		OK     bool                                `json:"ok"`
		Result pluginapi.ResponseInterceptResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &respResult); err != nil {
		t.Fatal(err)
	}
	if !respResult.OK || respResult.Result.Headers != nil || respResult.Result.Body != nil {
		t.Fatalf("malformed response intercept payload was not a safe no-op: %#v", respResult)
	}

	raw, err = handleMethod(pluginabi.MethodResponseInterceptStreamChunk, []byte("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	var chunkResult struct {
		OK     bool                                   `json:"ok"`
		Result pluginapi.StreamChunkInterceptResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &chunkResult); err != nil {
		t.Fatal(err)
	}
	if !chunkResult.OK || chunkResult.Result.Headers != nil || chunkResult.Result.Body != nil || chunkResult.Result.DropChunk {
		t.Fatalf("malformed stream chunk intercept payload was not a safe no-op: %#v", chunkResult)
	}
}

// TestResponseInterceptorABIDispatchHostShapedRPCEnvelope decodes the exact
// wire shape CLIProxyAPI's internal/pluginhost/rpc_client.go sends over the
// C-shared ABI boundary for response.intercept_after: pluginapi.
// ResponseInterceptRequest's own fields plus a sibling host_callback_id field
// (see rpcResponseInterceptRequest in CLIProxyAPI's source). Unknown fields
// are ignored by json.Unmarshal, so this also proves the plugin's decode
// tolerates that host wrapper shape, not merely the SDK type in isolation.
//
// This is the most direct proof available without terminating real upstream
// TLS in-process (see the header-observation report for why a full local
// upstream-fixture e2e is infeasible here): the host is guaranteed by
// sdk/cliproxy/auth/conductor_execution.go's publishSelectedAuthMetadata to
// place selected_auth_id into this exact opts.Metadata map before Anthropic's
// response reaches applyResponseInterceptors, which passes that same map
// through unmodified as this request's Metadata field.
func TestResponseInterceptorABIDispatchHostShapedRPCEnvelope(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", Identity: "same"}})
	previous := activeRuntime
	activeRuntime = runtime
	defer func() { activeRuntime = previous }()

	type hostShapedResponseInterceptRequest struct {
		pluginapi.ResponseInterceptRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	envelope := hostShapedResponseInterceptRequest{
		ResponseInterceptRequest: pluginapi.ResponseInterceptRequest{
			Model:    "claude-opus-4-1",
			Metadata: map[string]any{"selected_auth_id": "auth-a", "selected_auth_index": "index-a"},
			ResponseHeaders: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.71"},
			},
			StatusCode: http.StatusOK,
		},
		HostCallbackID: "cb-123",
	}
	request, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleMethod(pluginabi.MethodResponseInterceptAfter, request); err != nil {
		t.Fatal(err)
	}
	if sample := runtime.cache.snapshot("auth-a"); !sample.HasSample || sample.FiveHourPercentUsed != 71 {
		t.Fatalf("sample = %#v", sample)
	}
}

func TestResponseInterceptorABIDispatchUpdatesSample(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", Identity: "same"}})
	previous := activeRuntime
	activeRuntime = runtime
	defer func() { activeRuntime = previous }()

	req := pluginapi.ResponseInterceptRequest{
		Model:    "claude-opus-4-1",
		Metadata: map[string]any{"selected_auth_id": "auth-a"},
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.33"},
		},
		StatusCode: http.StatusOK,
	}
	request, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleMethod(pluginabi.MethodResponseInterceptAfter, request); err != nil {
		t.Fatal(err)
	}
	if sample := runtime.cache.snapshot("auth-a"); !sample.HasSample || sample.FiveHourPercentUsed != 33 {
		t.Fatalf("sample = %#v", sample)
	}
}
