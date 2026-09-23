package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type fakeHost struct {
	mu        sync.Mutex
	entries   []pluginapi.HostAuthFileEntry
	authJSON  map[string]json.RawMessage
	getErrors map[string]error
	listError error
	listCalls int
	getCalls  []string
	logs      []string
}

func (h *fakeHost) listAuth() ([]pluginapi.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listCalls++
	if h.listError != nil {
		return nil, h.listError
	}
	return append([]pluginapi.HostAuthFileEntry(nil), h.entries...), nil
}

func (h *fakeHost) getAuth(authIndex string) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.getCalls = append(h.getCalls, authIndex)
	if err := h.getErrors[authIndex]; err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), h.authJSON[authIndex]...), nil
}

func (h *fakeHost) log(level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	raw, _ := json.Marshal(map[string]any{"level": level, "message": message, "fields": fields})
	h.logs = append(h.logs, string(raw))
}

func (h *fakeHost) counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listCalls, len(h.getCalls)
}

func (h *fakeHost) logText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.logs, "\n")
}

type fetchReply struct {
	result   usageResult
	category string
}

type fakeFetcher struct {
	mu      sync.Mutex
	replies map[string][]fetchReply
	calls   []string
}

func (f *fakeFetcher) fetch(_ context.Context, token string, _ time.Duration) (usageResult, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, token)
	replies := f.replies[token]
	if len(replies) == 0 {
		return usageResult{}, pollErrorNetwork
	}
	reply := replies[0]
	if len(replies) > 1 {
		f.replies[token] = replies[1:]
	}
	return reply.result, reply.category
}

func (f *fakeFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeFetcher) callTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type testClock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *testClock) set(value time.Time) {
	c.mu.Lock()
	c.value = value
	c.mu.Unlock()
}

func newTestRuntime(host hostClient, fetch usageFetcher, now time.Time) *pluginRuntime {
	return newPluginRuntime(host, fetch, func() time.Time { return now })
}

const testModel = "claude-opus-4-1"

func claudeRequest(candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return claudeModelRequest(testModel, candidates...)
}

func claudeModelRequest(model string, candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{
		Provider:   "claude",
		Providers:  []string{"claude"},
		Model:      model,
		Candidates: candidates,
	}
}

func candidate(id string, priority int) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: "claude", Priority: priority}
}

func beforeAuthRequest() pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{Model: testModel, RequestedModel: testModel}
}

func TestInterceptBeforeAuthConfirmedExhaustionTerminates(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{
		physicalEntry("auth-a", "index-a"), physicalEntry("auth-b", "index-b"),
	}}
	runtime := newTestRuntime(host, (&fakeFetcher{}).fetch, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.config.Store(&cfg)
	resetA := now.Add(1500 * time.Millisecond)
	runtime.cache.recordSuccess("auth-a", 96, resetA, now)
	runtime.cache.recordSuccess("auth-b", 100, now.Add(time.Hour), now)

	response := runtime.interceptBeforeAuth(beforeAuthRequest())
	if !response.Terminate || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("response = %#v", response)
	}
	if got, want := response.ResponseHeaders.Get("Retry-After"), "2"; got != want {
		t.Fatalf("Retry-After = %q, want %q", got, want)
	}
	if got, want := response.ResponseHeaders.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	var body map[string]string
	if err := json.Unmarshal(response.ResponseBody, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body["code"] != exhaustedErrorCode || body["message"] != exhaustedErrorMessage(now, resetA, true) {
		t.Fatalf("body = %#v", body)
	}
	if strings.Contains(string(response.ResponseBody), "auth-a") || strings.Contains(string(response.ResponseBody), "index-a") {
		t.Fatalf("identity leaked in body: %s", response.ResponseBody)
	}
	_, getCalls := host.counts()
	if getCalls != 0 {
		t.Fatalf("interceptor called host.auth.get %d times", getCalls)
	}
}

func TestInterceptBeforeAuthPassesThroughWhenNotSafelyExhausted(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name            string
		fallbackEnabled bool
		available       bool
		unknown         bool
	}{
		{name: "available sibling", available: true},
		{name: "unknown sibling", unknown: true},
		{name: "overage fallback enabled", fallbackEnabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{
				physicalEntry("auth-a", "index-a"), physicalEntry("auth-b", "index-b"),
			}}
			runtime := newTestRuntime(host, (&fakeFetcher{}).fetch, now)
			cfg := defaultPluginConfig()
			cfg.OverageFallbackEnabled = test.fallbackEnabled
			runtime.config.Store(&cfg)
			runtime.cache.recordSuccess("auth-a", 96, now.Add(time.Hour), now)
			if !test.unknown {
				percent := 100.0
				if test.available {
					percent = 94.9
				}
				runtime.cache.recordSuccess("auth-b", percent, now.Add(time.Hour), now)
			}
			if response := runtime.interceptBeforeAuth(beforeAuthRequest()); response.Terminate {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

func TestInterceptBeforeAuthPassesThroughOnMembershipDiscoveryFailure(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	host := &fakeHost{listError: errors.New("host.auth.list unavailable")}
	runtime := newTestRuntime(host, (&fakeFetcher{}).fetch, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.config.Store(&cfg)
	runtime.cache.recordSuccess("stale-auth", 99, now.Add(time.Hour), now)

	if response := runtime.interceptBeforeAuth(beforeAuthRequest()); response.Terminate {
		t.Fatalf("response = %#v", response)
	}
	_, getCalls := host.counts()
	if getCalls != 0 {
		t.Fatalf("interceptor called host.auth.get %d times", getCalls)
	}
}

func TestInterceptBeforeAuthRejectsExplicitNonClaudeProtectedModel(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")}}
	runtime := newTestRuntime(host, (&fakeFetcher{}).fetch, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	cfg.ProtectedModels = []string{"gpt-5"}
	runtime.config.Store(&cfg)
	runtime.cache.recordSuccess("auth-a", 100, now.Add(time.Hour), now)

	response := runtime.interceptBeforeAuth(pluginapi.RequestInterceptRequest{Model: "gpt-5", RequestedModel: "gpt-5"})
	if response.Terminate {
		t.Fatalf("non-Claude request was gated: %#v", response)
	}
	listCalls, getCalls := host.counts()
	if listCalls != 0 || getCalls != 0 {
		t.Fatalf("non-Claude request performed callbacks: list=%d get=%d", listCalls, getCalls)
	}
}

func TestInterceptBeforeAuthQueuesRefreshForIdentityInvalidation(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	host := &fakeHost{
		entries:  nil,
		authJSON: map[string]json.RawMessage{"index-new": credentialJSON("new-token")},
	}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{
		"new-token": {{result: usageResult{FiveHourPercentUsed: 10, ResetAt: now.Add(time.Hour)}}},
	}}
	runtime := newTestRuntime(host, fetcher.fetch, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.applyConfig(cfg)
	defer runtime.shutdown()
	waitFor(t, func() bool { listCalls, _ := host.counts(); return listCalls > 0 })

	old := physicalEntry("auth-a", "index-old")
	_, _ = runtime.cache.reconcile(physicalClaudeAuths([]pluginapi.HostAuthFileEntry{old}))
	runtime.cache.recordSuccess("auth-a", 99, now.Add(time.Hour), now)
	newEntry := physicalEntry("auth-a", "index-new")
	newEntry.Path = "/fixtures/replaced-auth-a.json"
	host.mu.Lock()
	host.entries = []pluginapi.HostAuthFileEntry{newEntry}
	host.mu.Unlock()

	response := runtime.interceptBeforeAuth(beforeAuthRequest())
	if response.Terminate {
		t.Fatalf("identity-invalidated sample was gated: %#v", response)
	}
	waitFor(t, func() bool { return fetcher.callCount() == 1 })
	if sample := runtime.cache.snapshot("auth-a"); !sample.HasSample || sample.AuthIndex != "index-new" || sample.FiveHourPercentUsed != 10 {
		t.Fatalf("refreshed sample = %#v", sample)
	}
}

func TestInterceptBeforeAuthReplacementIgnoresInFlightOldIdentity(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	oldEntry := physicalEntry("auth-a", "index-old")
	host := &fakeHost{
		entries: oldEntrySlice(oldEntry),
		authJSON: map[string]json.RawMessage{
			"index-old": credentialJSON("old-token"),
			"index-new": credentialJSON("new-token"),
		},
	}
	oldFetchStarted := make(chan struct{})
	releaseOldFetch := make(chan struct{})
	newFetchStarted := make(chan struct{})
	releaseNewFetch := make(chan struct{})
	var fetchMu sync.Mutex
	var fetches []string
	fetch := func(_ context.Context, token string, _ time.Duration) (usageResult, string) {
		fetchMu.Lock()
		fetches = append(fetches, token)
		fetchMu.Unlock()
		if token == "old-token" {
			close(oldFetchStarted)
			<-releaseOldFetch
			return usageResult{FiveHourPercentUsed: 99, ResetAt: now.Add(time.Hour)}, ""
		}
		close(newFetchStarted)
		<-releaseNewFetch
		return usageResult{FiveHourPercentUsed: 10, ResetAt: now.Add(time.Hour)}, ""
	}
	runtime := newTestRuntime(host, fetch, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.applyConfig(cfg)
	defer runtime.shutdown()
	<-oldFetchStarted

	newEntry := physicalEntry("auth-a", "index-new")
	newEntry.Path = "/fixtures/replaced-auth-a.json"
	host.mu.Lock()
	host.entries = []pluginapi.HostAuthFileEntry{newEntry}
	host.mu.Unlock()
	if response := runtime.interceptBeforeAuth(beforeAuthRequest()); response.Terminate {
		t.Fatalf("replacement was gated: %#v", response)
	}
	close(releaseOldFetch)
	<-newFetchStarted
	if sample := runtime.cache.snapshot("auth-a"); sample.Identity != physicalAuthIdentity(newEntry) || sample.HasSample {
		t.Fatalf("stale A result committed into B before B completed: %#v", sample)
	}
	close(releaseNewFetch)

	waitFor(t, func() bool { return runtime.cache.snapshot("auth-a").HasSample })
	fetchMu.Lock()
	gotFetches := append([]string(nil), fetches...)
	fetchMu.Unlock()
	if len(gotFetches) != 2 || gotFetches[0] != "old-token" || gotFetches[1] != "new-token" {
		t.Fatalf("fetches = %#v", gotFetches)
	}
	if sample := runtime.cache.snapshot("auth-a"); sample.Identity != physicalAuthIdentity(newEntry) || !sample.HasSample || sample.FiveHourPercentUsed != 10 {
		t.Fatalf("replacement sample = %#v", sample)
	}
}

func oldEntrySlice(entry pluginapi.HostAuthFileEntry) []pluginapi.HostAuthFileEntry {
	return []pluginapi.HostAuthFileEntry{entry}
}

func weightedCandidate(id string, priority int, weight string) pluginapi.SchedulerAuthCandidate {
	c := candidate(id, priority)
	c.Attributes = map[string]string{"weight": weight}
	return c
}

func physicalEntry(id, index string) pluginapi.HostAuthFileEntry {
	return pluginapi.HostAuthFileEntry{
		ID:        id,
		AuthIndex: index,
		Name:      id + ".json",
		Provider:  "claude",
		Path:      "/fixtures/" + id + ".json",
	}
}

func disabledEntry(id, index string) pluginapi.HostAuthFileEntry {
	entry := physicalEntry(id, index)
	entry.Disabled = true
	return entry
}

func credentialJSON(token string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type":          "claude",
		"access_token":  token,
		"refresh_token": "refresh-must-stay-secret",
	})
	return raw
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestCurrentCutoffIsDerivedFromConfig(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.config.Store(&cfg)
	runtime.cache.recordSuccess("auth-a", 96, now.Add(time.Hour), now)
	if _, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0))); decisionError == nil {
		t.Fatal("96% sample should be excluded at the default cutoff")
	}
	cfg = defaultPluginConfig()
	cfg.CutoffPercentUsed = 97
	cfg.OverageFallbackEnabled = false
	runtime.config.Store(&cfg)
	response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0)))
	if decisionError != nil || response.AuthID != "auth-a" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
}

func TestDisabledConfigLeavesSchedulerUnhandled(t *testing.T) {
	runtime := newTestRuntime(&fakeHost{}, nil, time.Now())
	cfg := defaultPluginConfig()
	cfg.Enabled = false
	runtime.config.Store(&cfg)
	response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0)))
	if decisionError != nil || response.Handled {
		t.Fatalf("response = %#v, error = %#v; want unhandled", response, decisionError)
	}
}

func TestSchedulerOnlyHandlesProtectedModels(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("auth-a", 96, now.Add(time.Hour), now)
	// Disable overage fallback so exhaustion still hard-blocks; this test is
	// about protected-model matching, not overage-fallback semantics (which
	// are covered separately).
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.config.Store(&cfg)

	// Default config has an empty protected-models list, meaning ALL Claude
	// models are protected.
	for _, model := range []string{testModel, " CLAUDE-HAIKU-4-5 ", "any-model-name"} {
		response, decisionError := runtime.pick(claudeModelRequest(model, candidate("auth-a", 0)))
		if response.Handled || decisionError == nil || decisionError.Code != exhaustedErrorCode {
			t.Fatalf("protected model %q: response=%#v error=%#v", model, response, decisionError)
		}
	}

	cfg = defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	cfg.ProtectedModels = []string{"claude-sonnet-4-6"}
	runtime.config.Store(&cfg)
	response, decisionError := runtime.pick(claudeModelRequest(testModel, candidate("auth-a", 0)))
	if decisionError != nil || response.Handled {
		t.Fatalf("removed protected model: response=%#v error=%#v", response, decisionError)
	}
	response, decisionError = runtime.pick(claudeModelRequest("claude-sonnet-4-6", candidate("auth-a", 0)))
	if response.Handled || decisionError == nil || decisionError.Code != exhaustedErrorCode {
		t.Fatalf("configured protected model: response=%#v error=%#v", response, decisionError)
	}
}

func TestIsProtectedModelEmptyListMatchesAll(t *testing.T) {
	if !isProtectedModel("claude-opus-4-1", nil) {
		t.Fatal("empty protected-models should match any non-empty model")
	}
	if !isProtectedModel("any-model-name", []string{}) {
		t.Fatal("empty protected-models should match any non-empty model")
	}
	if isProtectedModel("", nil) {
		t.Fatal("empty model name should never match")
	}
}

func TestIsProtectedModelExplicitListIsExact(t *testing.T) {
	models := []string{"claude-opus-4-1"}
	if !isProtectedModel("Claude-Opus-4-1", models) {
		t.Fatal("explicit list should match case-insensitively")
	}
	if isProtectedModel("claude-sonnet-4-6", models) {
		t.Fatal("explicit list should not match unrelated models")
	}
}

func TestDisableClearsCachedQuota(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("auth-a", 80, now.Add(time.Hour), now)
	cfg := defaultPluginConfig()
	cfg.Enabled = false
	runtime.applyConfig(cfg)
	if sample := runtime.cache.snapshot("auth-a"); sample != (quotaSample{}) {
		t.Fatalf("disabled cache = %#v", sample)
	}
}

func TestWhitespaceAuthIDIsIgnored(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.config.Store(&cfg)
	runtime.cache.recordSuccess("auth-a", 96, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(
		candidate("auth-a", 0),
		candidate(" auth-a ", 100),
	))
	if decisionError == nil || response.Handled {
		t.Fatalf("whitespace auth was selected: response=%#v error=%#v", response, decisionError)
	}
}

func TestCutoffBoundary(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		percent   float64
		wantAuth  string
		wantError bool
	}{
		{name: "94.9 remains eligible", percent: 94.9, wantAuth: "auth-a"},
		{name: "0 remains eligible", percent: 0, wantAuth: "auth-a"},
		{name: "exactly 95 is excluded", percent: 95, wantError: true},
		{name: "above 95 is excluded", percent: 99, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := newTestRuntime(&fakeHost{}, nil, now)
			// Disable overage fallback: this test is about the cutoff
			// boundary itself, not the single-candidate overage-fallback
			// self-selection case (which would otherwise mask the error).
			cfg := defaultPluginConfig()
			cfg.OverageFallbackEnabled = false
			runtime.config.Store(&cfg)
			runtime.cache.recordSuccess("auth-a", test.percent, now.Add(time.Hour), now)
			response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0)))
			if test.wantError {
				if decisionError == nil || decisionError.Code != exhaustedErrorCode {
					t.Fatalf("decision error = %#v, want %s", decisionError, exhaustedErrorCode)
				}
				return
			}
			if decisionError != nil {
				t.Fatalf("decision error = %v", decisionError)
			}
			if response.AuthID != test.wantAuth || !response.Handled {
				t.Fatalf("response = %#v, want handled auth %q", response, test.wantAuth)
			}
		})
	}
}

func TestActualEndpointUtilizationScaleIsPercentPoints(t *testing.T) {
	value := 0.5
	got, ok := normalizeUtilization(&value)
	if !ok || got != 0.5 {
		t.Fatalf("normalizeUtilization(0.5) = %v, %v; want 0.5, true", got, ok)
	}

	headerError := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			headerError <- "missing fixture authorization header"
			return
		}
		if r.Header.Get("anthropic-beta") != anthropicOAuthBeta {
			headerError <- "missing anthropic beta header"
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":9.0,"resets_at":"2026-07-25T00:00:00Z"}}`))
	}))
	defer server.Close()

	fetcher := newHTTPUsageFetcher(server.URL, server.Client().Transport, "")
	result, category := fetcher.fetch(context.Background(), "fixture-token", time.Second)
	select {
	case message := <-headerError:
		t.Fatal(message)
	default:
	}
	if category != "" {
		t.Fatalf("fetch category = %q", category)
	}
	if result.FiveHourPercentUsed != 9.0 {
		t.Fatalf("weekly percent = %v, want 9", result.FiveHourPercentUsed)
	}
	if result.ResetAt.Format(time.RFC3339) != "2026-07-25T00:00:00Z" {
		t.Fatalf("reset = %s", result.ResetAt)
	}
}

func TestMalformedOrMissingUsageIsClassifiedInvalidUsage(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		category string
	}{
		{name: "missing", body: `{}`, category: pollErrorInvalidUsage},
		{name: "null", body: `{"five_hour":{"utilization":null}}`, category: pollErrorInvalidUsage},
		{name: "negative", body: `{"five_hour":{"utilization":-1}}`, category: pollErrorInvalidUsage},
		{name: "over one hundred", body: `{"five_hour":{"utilization":101,"resets_at":"2026-07-25T00:00:00Z"}}`, category: pollErrorInvalidUsage},
		{name: "malformed reset", body: `{"five_hour":{"utilization":50,"resets_at":"not-a-time"}}`, category: pollErrorInvalidUsage},
		{name: "numeric reset", body: `{"five_hour":{"utilization":50,"resets_at":1784937600}}`, category: pollErrorInvalidUsage},
		{name: "malformed json", body: `{"five_hour":`, category: pollErrorInvalidJSON},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			fetcher := newHTTPUsageFetcher(server.URL, server.Client().Transport, "")
			_, category := fetcher.fetch(context.Background(), "token", time.Second)
			if category != test.category {
				t.Fatalf("category = %q, want %q", category, test.category)
			}
		})
	}

	// A never-sampled credential is fail-closed: it is excluded from scheduling
	// until at least one successful poll has happened.
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	_, decisionError := runtime.pick(claudeRequest(candidate("unknown", 0)))
	if decisionError == nil || decisionError.Code != exhaustedErrorCode {
		t.Fatalf("never-sampled auth should be fail-closed and excluded: error = %#v", decisionError)
	}
}

// TestNullOrMissingResetsAtIsAValidSampleNotAnError is a regression test for a
// production incident: Anthropic returns resets_at=null (or omits the field)
// when a credential has no active five-hour session - most commonly at
// utilization=0, right after a reset or before first use in the window. This
// is the healthiest possible account state, not a parse failure. Treating it
// as pollErrorInvalidUsage meant such a credential could NEVER produce a
// successful sample, so fail-closed permanently excluded it from scheduling -
// even though it had zero usage - while traffic kept flowing to a genuinely
// exhausted sibling credential via the overage fallback. This must not
// regress: null/missing resets_at is a valid sample with a zero ResetAt.
func TestNullOrMissingResetsAtIsAValidSampleNotAnError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "null resets_at at zero utilization", body: `{"five_hour":{"utilization":0,"resets_at":null}}`},
		{name: "null resets_at at nonzero utilization", body: `{"five_hour":{"utilization":50,"resets_at":null}}`},
		{name: "missing resets_at key entirely", body: `{"five_hour":{"utilization":0}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			fetcher := newHTTPUsageFetcher(server.URL, server.Client().Transport, "")
			result, category := fetcher.fetch(context.Background(), "token", time.Second)
			if category != "" {
				t.Fatalf("category = %q, want success (empty)", category)
			}
			if !result.ResetAt.IsZero() {
				t.Fatalf("ResetAt = %v, want zero time for null/missing resets_at", result.ResetAt)
			}
		})
	}
}

// TestZeroUtilizationWithNullResetIsImmediatelyAvailable is the end-to-end
// regression test for the same incident at the scheduler level: a credential
// whose only sample is {utilization: 0, resets_at: null} must be selectable
// on the very next pick, not fail-closed as unknown.
func TestZeroUtilizationWithNullResetIsImmediatelyAvailable(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("fresh-account", 0, time.Time{}, now)
	response, decisionError := runtime.pick(claudeRequest(candidate("fresh-account", 1)))
	if decisionError != nil || !response.Handled || response.AuthID != "fresh-account" {
		t.Fatalf("response = %#v, error = %#v, want fresh-account selected", response, decisionError)
	}
}

func TestHTTPFailuresAreBoundedAndClassified(t *testing.T) {
	statuses := []struct {
		status   int
		category string
	}{
		{http.StatusUnauthorized, pollErrorUnauthorized},
		{http.StatusForbidden, pollErrorForbidden},
		{http.StatusTooManyRequests, pollErrorRateLimited},
		{http.StatusInternalServerError, pollErrorServer},
		{http.StatusBadRequest, pollErrorHTTP},
	}
	for _, test := range statuses {
		t.Run(fmt.Sprintf("status_%d", test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte("complete-upstream-body-must-not-escape"))
			}))
			defer server.Close()
			fetcher := newHTTPUsageFetcher(server.URL, server.Client().Transport, "")
			_, category := fetcher.fetch(context.Background(), "secret-token", time.Second)
			if category != test.category {
				t.Fatalf("category = %q, want %q", category, test.category)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxUsageResponseBytes+1)))
	}))
	defer server.Close()
	fetcher := newHTTPUsageFetcher(server.URL, server.Client().Transport, "")
	_, category := fetcher.fetch(context.Background(), "secret-token", time.Second)
	if category != pollErrorBodyTooLarge {
		t.Fatalf("oversized category = %q, want %q", category, pollErrorBodyTooLarge)
	}

	timeoutServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":1}}`))
	}))
	defer timeoutServer.Close()
	timeoutFetcher := newHTTPUsageFetcher(timeoutServer.URL, timeoutServer.Client().Transport, "")
	started := time.Now()
	_, category = timeoutFetcher.fetch(context.Background(), "secret-token", 10*time.Millisecond)
	if category != pollErrorTimeout {
		t.Fatalf("timeout category = %q, want %q", category, pollErrorTimeout)
	}
	if elapsed := time.Since(started); elapsed >= 90*time.Millisecond {
		t.Fatalf("request timeout took %s, want less than upstream delay", elapsed)
	}
}

func TestBodyReadCancellationIsClassified(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan string, 1)
	go func() {
		fetcher := newHTTPUsageFetcher(server.URL, server.Client().Transport, "")
		_, category := fetcher.fetch(ctx, "secret-token", time.Second)
		result <- category
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("response body did not start")
	}
	cancel()
	select {
	case category := <-result:
		if category != pollErrorCancelled {
			t.Fatalf("body cancellation category = %q, want %q", category, pollErrorCancelled)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled body read did not return")
	}
}

func TestHTTPRedirectIsNotFollowed(t *testing.T) {
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	fetcher := newHTTPUsageFetcher(redirect.URL, redirect.Client().Transport, "")
	if _, category := fetcher.fetch(context.Background(), "secret-token", time.Second); category != pollErrorHTTP {
		t.Fatalf("redirect category = %q, want %q", category, pollErrorHTTP)
	}
	if followed {
		t.Fatal("redirect target received the bearer token")
	}
}

func TestNonClaudeRequestsAreUnhandled(t *testing.T) {
	runtime := newTestRuntime(&fakeHost{}, nil, time.Now())
	response, decisionError := runtime.pick(pluginapi.SchedulerPickRequest{
		Provider:   "codex",
		Providers:  []string{"codex"},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "codex-a", Provider: "codex"}},
	})
	if decisionError != nil || response.Handled {
		t.Fatalf("response = %#v, error = %#v; want unhandled", response, decisionError)
	}
}

func TestAllConfirmedExhausted(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)

	t.Run("every credential is sampled over cutoff with a future reset", func(t *testing.T) {
		cache := quotaCache{samples: map[string]quotaSample{}}
		cache.recordSuccess("a", 95, now.Add(30*time.Minute), now)
		cache.recordSuccess("b", 100, now.Add(10*time.Minute), now)

		if !cache.allConfirmedExhausted([]string{"a", "b"}, now, 95) {
			t.Fatal("want confirmed exhaustion")
		}
	})

	t.Run("an under-cutoff sibling prevents confirmed exhaustion", func(t *testing.T) {
		cache := quotaCache{samples: map[string]quotaSample{}}
		cache.recordSuccess("a", 95, now.Add(30*time.Minute), now)
		cache.recordSuccess("b", 94.9, now.Add(10*time.Minute), now)

		if cache.allConfirmedExhausted([]string{"a", "b"}, now, 95) {
			t.Fatal("under-cutoff sibling must prevent confirmed exhaustion")
		}
	})

	t.Run("a never-sampled sibling prevents confirmed exhaustion", func(t *testing.T) {
		cache := quotaCache{samples: map[string]quotaSample{}}
		cache.recordSuccess("a", 95, now.Add(30*time.Minute), now)

		if cache.allConfirmedExhausted([]string{"a", "b"}, now, 95) {
			t.Fatal("never-sampled sibling must prevent confirmed exhaustion")
		}
	})

	for _, test := range []struct {
		name    string
		resetAt time.Time
	}{
		{name: "zero reset", resetAt: time.Time{}},
		{name: "past reset", resetAt: now.Add(-time.Second)},
		{name: "reset equal to now", resetAt: now},
	} {
		t.Run(test.name+" prevents confirmed exhaustion", func(t *testing.T) {
			cache := quotaCache{samples: map[string]quotaSample{}}
			cache.recordSuccess("a", 95, test.resetAt, now)

			if cache.allConfirmedExhausted([]string{"a"}, now, 95) {
				t.Fatal("non-future reset must prevent confirmed exhaustion")
			}
		})
	}

	t.Run("duplicate exhausted credentials remain confirmed exhausted", func(t *testing.T) {
		cache := quotaCache{samples: map[string]quotaSample{}}
		cache.recordSuccess("a", 95, now.Add(time.Hour), now)

		if !cache.allConfirmedExhausted([]string{"a", "a"}, now, 95) {
			t.Fatal("duplicate exhausted credentials should remain confirmed exhausted")
		}
	})

	t.Run("duplicate unknown credentials prevent confirmed exhaustion", func(t *testing.T) {
		cache := quotaCache{samples: map[string]quotaSample{}}

		if cache.allConfirmedExhausted([]string{"a", "a"}, now, 95) {
			t.Fatal("duplicate unknown credentials must prevent confirmed exhaustion")
		}
	})

	t.Run("no credentials are not confirmed exhausted", func(t *testing.T) {
		cache := quotaCache{samples: map[string]quotaSample{}}
		if cache.allConfirmedExhausted(nil, now, 95) {
			t.Fatal("no credentials must not be confirmed exhausted")
		}
	})
}

func TestClaudeSelectionIgnoresExcludedCandidates(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("blocked-high", 95, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("eligible-low", 94.9, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(
		candidate("blocked-high", 100),
		candidate("eligible-low", 1),
	))
	if decisionError != nil || response.AuthID != "eligible-low" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
}

// TestTwoCandidatesOneExcludedOnePicksAvailable exercises isExcluded-based
// candidate filtering directly: A is over cutoff and not yet reset, B is
// available, scheduler must pick B.
func TestTwoCandidatesOneExcludedOnePicksAvailable(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("A", 99, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("B", 10, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(candidate("A", 100), candidate("B", 1)))
	if decisionError != nil || response.AuthID != "B" {
		t.Fatalf("response = %#v, error = %#v, want B", response, decisionError)
	}
}

// TestSchedulerHardExhaustionIncludesKnownResetRetryMetadata preserves the
// scheduler-level backstop for a state change after before-auth admission. It
// must return the reset-derived metadata when fallback is disabled and every
// scheduler candidate is confirmed exhausted.
func TestSchedulerHardExhaustionIncludesKnownResetRetryMetadata(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.config.Store(&cfg)
	resetA := now.Add(90 * time.Second)
	runtime.cache.recordSuccess("A", 99, resetA, now)
	runtime.cache.recordSuccess("B", 96, now.Add(2*time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(candidate("A", 100), candidate("B", 1)))
	wantMessage := "five_hour_quota_exhausted; retry_after_seconds=90; resets_at=2026-09-23T12:01:30Z"
	if response.Handled || decisionError == nil || decisionError.Code != exhaustedErrorCode || decisionError.Message != wantMessage {
		t.Fatalf("response = %#v, error = %#v, want exhausted", response, decisionError)
	}
}

// TestOverageFallbackRoutesToHighestPriorityConfirmedOverCutoff exercises the
// default (overage-fallback-enabled unset, so it defaults to true) behavior:
// when every candidate is CONFIRMED over cutoff, route to the one with the
// highest CPA priority instead of hard-blocking.
func TestOverageFallbackRoutesToHighestPriorityConfirmedOverCutoff(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	// Do not touch cfg.OverageFallbackEnabled: prove the default (true) applies.
	runtime.cache.recordSuccess("A", 99, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("B", 96, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(candidate("A", 100), candidate("B", 1)))
	if decisionError != nil || !response.Handled || response.AuthID != "A" {
		t.Fatalf("response = %#v, error = %#v, want overage fallback to A (highest priority)", response, decisionError)
	}
}

// TestOverageFallbackDisabledRestoresHardBlock is a regression check that
// explicitly setting overage-fallback-enabled: false restores the strict
// five_hour_quota_exhausted hard-block once every candidate is confirmed
// over cutoff.
func TestOverageFallbackDisabledRestoresHardBlock(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.config.Store(&cfg)
	runtime.cache.recordSuccess("A", 99, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("B", 96, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(candidate("A", 100), candidate("B", 1)))
	if response.Handled || decisionError == nil || decisionError.Code != exhaustedErrorCode {
		t.Fatalf("response = %#v, error = %#v, want hard-blocked exhausted error", response, decisionError)
	}
}

// TestOverageFallbackNeverTriggersForUnknownCandidate is the critical
// safety-boundary test: candidate A is CONFIRMED over cutoff (has a
// successful sample >= cutoff, not yet reset), but candidate B has NEVER
// been successfully sampled (unknown/unreachable state). Even with
// overage-fallback-enabled at its default (true), the fallback must NOT
// trigger, because we cannot confirm B's true quota state — blindly routing
// billable overage traffic to A while B's state is unconfirmed would defeat
// the entire point of requiring confirmed exhaustion for every candidate.
// The plugin must still return the hard five_hour_quota_exhausted error.
func TestOverageFallbackNeverTriggersForUnknownCandidate(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	// A: confirmed over cutoff.
	runtime.cache.recordSuccess("A", 99, now.Add(75*time.Second), now)
	// B: never sampled — intentionally no recordSuccess call. HasSample stays false.
	response, decisionError := runtime.pick(claudeRequest(candidate("A", 100), candidate("B", 1)))
	if response.Handled {
		t.Fatalf("overage fallback must not trigger when any candidate is unknown: response = %#v", response)
	}
	wantMessage := "five_hour_quota_exhausted; retry_after_seconds=75; resets_at=2026-09-23T12:01:15Z"
	if decisionError == nil || decisionError.Code != exhaustedErrorCode || decisionError.Message != wantMessage {
		t.Fatalf("expected hard-blocked exhausted error when a candidate is unknown, got error = %#v", decisionError)
	}
	if response.AuthID == "A" {
		t.Fatal("must never route to A while B's quota state is unconfirmed")
	}
}

func TestExhaustedErrorWithoutKnownFutureResetUsesLegacyMessage(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	t.Run("all candidates never sampled", func(t *testing.T) {
		runtime := newTestRuntime(&fakeHost{}, nil, now)
		response, decisionError := runtime.pick(claudeRequest(candidate("A", 1), candidate("B", 0)))
		if response.Handled || decisionError == nil || decisionError.Code != exhaustedErrorCode || decisionError.Message != exhaustedErrorCode {
			t.Fatalf("response = %#v, error = %#v, want legacy exhausted error", response, decisionError)
		}
	})
	t.Run("zero reset plus unknown", func(t *testing.T) {
		runtime := newTestRuntime(&fakeHost{}, nil, now)
		runtime.cache.recordSuccess("A", 99, time.Time{}, now)
		response, decisionError := runtime.pick(claudeRequest(candidate("A", 1), candidate("B", 0)))
		if response.Handled || decisionError == nil || decisionError.Code != exhaustedErrorCode || decisionError.Message != exhaustedErrorCode {
			t.Fatalf("response = %#v, error = %#v, want legacy exhausted error", response, decisionError)
		}
	})
}

func TestExhaustedErrorMessageFormatting(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(1100 * time.Millisecond)
	if got, want := exhaustedErrorMessage(now, resetAt, true), "five_hour_quota_exhausted; retry_after_seconds=2; resets_at=2026-09-23T12:00:01Z"; got != want {
		t.Fatalf("exhaustedErrorMessage() = %q, want %q", got, want)
	}
	for _, test := range []struct {
		name     string
		resetAt  time.Time
		hasReset bool
	}{
		{name: "no reset", hasReset: false},
		{name: "zero reset", resetAt: time.Time{}, hasReset: true},
		{name: "at now", resetAt: now, hasReset: true},
		{name: "past", resetAt: now.Add(-time.Second), hasReset: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := exhaustedErrorMessage(now, test.resetAt, test.hasReset); got != exhaustedErrorCode {
				t.Fatalf("exhaustedErrorMessage() = %q, want %q", got, exhaustedErrorCode)
			}
		})
	}
}

// TestOverageFallbackTieBreaksByLowestID verifies the deterministic
// tie-break rule (lowest AuthID wins on equal priority) applies to the
// overage-fallback candidate selection too, matching the existing
// convention used for normal selection.
func TestOverageFallbackTieBreaksByLowestID(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("z-auth", 99, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("a-auth", 96, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(candidate("z-auth", 10), candidate("a-auth", 10)))
	if decisionError != nil || !response.Handled || response.AuthID != "a-auth" {
		t.Fatalf("response = %#v, error = %#v, want overage fallback to a-auth (lowest ID tie-break)", response, decisionError)
	}
}

// TestOverageFallbackNeverInvokedWhenACandidateIsAvailable is a sanity
// regression: as soon as at least one candidate is not excluded, normal
// selection is used and fallback tracking is never consulted, even if other
// candidates are confirmed over cutoff.
func TestOverageFallbackNeverInvokedWhenACandidateIsAvailable(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	// A is confirmed over cutoff and has the highest priority; B is available
	// (under cutoff) but lower priority. Normal selection must still pick B,
	// since the overage fallback only applies when ALL candidates are excluded.
	runtime.cache.recordSuccess("A", 99, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("B", 10, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(candidate("A", 100), candidate("B", 1)))
	if decisionError != nil || !response.Handled || response.AuthID != "B" {
		t.Fatalf("response = %#v, error = %#v, want normal selection of B despite A's higher priority", response, decisionError)
	}
}

// TestNormalSelectionPrefersHigherWeightAmongSamePriorityAvailable verifies
// that among two AVAILABLE (under-cutoff) candidates tied on priority, the
// one with the higher host "weight" attribute is selected first for normal
// (free-quota) traffic, not just for the overage fallback.
func TestNormalSelectionPrefersHigherWeightAmongSamePriorityAvailable(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("low-weight", 10, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("high-weight", 10, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(
		weightedCandidate("low-weight", 5, "1"),
		weightedCandidate("high-weight", 5, "9"),
	))
	if decisionError != nil || !response.Handled || response.AuthID != "high-weight" {
		t.Fatalf("response = %#v, error = %#v, want normal selection to prefer high-weight", response, decisionError)
	}
}

// TestNormalSelectionWeightTieBreaksByLowestIDWhenWeightsEqual verifies the
// full tie-break chain for normal selection: equal priority, equal weight,
// lowest ID wins - matching the pre-weight behavior exactly when weights tie
// (including the common case of no weight attribute set on either side).
func TestNormalSelectionWeightTieBreaksByLowestIDWhenWeightsEqual(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("z-auth", 10, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("a-auth", 10, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(candidate("z-auth", 5), candidate("a-auth", 5)))
	if decisionError != nil || !response.Handled || response.AuthID != "a-auth" {
		t.Fatalf("response = %#v, error = %#v, want a-auth (lowest ID, weights both default to 1)", response, decisionError)
	}
}

// TestOverageFallbackPrefersHigherWeightAmongSamePriorityConfirmedExhausted
// verifies the same weight-based preference applies to the overage-fallback
// candidate: once every same-priority candidate is confirmed exhausted, the
// one with the higher "weight" attribute absorbs the billable traffic.
func TestOverageFallbackPrefersHigherWeightAmongSamePriorityConfirmedExhausted(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("low-weight", 99, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("high-weight", 96, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(
		weightedCandidate("low-weight", 5, "1"),
		weightedCandidate("high-weight", 5, "9"),
	))
	if decisionError != nil || !response.Handled || response.AuthID != "high-weight" {
		t.Fatalf("response = %#v, error = %#v, want overage fallback to prefer high-weight", response, decisionError)
	}
}

// TestCandidateWeightDefaultsToOneForMissingOrInvalidValues covers the
// candidateWeight() helper directly: missing Attributes, missing key, empty
// string, non-numeric, and negative values must all default to weight 1
// (not exclude the candidate, not panic, not treat as zero/unset priority).
func TestCandidateWeightDefaultsToOneForMissingOrInvalidValues(t *testing.T) {
	tests := []struct {
		name      string
		candidate *pluginapi.SchedulerAuthCandidate
	}{
		{name: "nil candidate", candidate: nil},
		{name: "nil attributes", candidate: &pluginapi.SchedulerAuthCandidate{ID: "x"}},
		{name: "missing key", candidate: &pluginapi.SchedulerAuthCandidate{ID: "x", Attributes: map[string]string{}}},
		{name: "empty value", candidate: &pluginapi.SchedulerAuthCandidate{ID: "x", Attributes: map[string]string{"weight": ""}}},
		{name: "whitespace value", candidate: &pluginapi.SchedulerAuthCandidate{ID: "x", Attributes: map[string]string{"weight": "   "}}},
		{name: "non-numeric", candidate: &pluginapi.SchedulerAuthCandidate{ID: "x", Attributes: map[string]string{"weight": "high"}}},
		{name: "negative", candidate: &pluginapi.SchedulerAuthCandidate{ID: "x", Attributes: map[string]string{"weight": "-5"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := candidateWeight(test.candidate); got != 1 {
				t.Fatalf("candidateWeight() = %d, want 1", got)
			}
		})
	}
}

// TestCandidateWeightParsesValidPositiveValue is the positive-path
// counterpart: a valid non-negative integer weight is parsed as-is.
func TestCandidateWeightParsesValidPositiveValue(t *testing.T) {
	c := &pluginapi.SchedulerAuthCandidate{ID: "x", Attributes: map[string]string{"weight": "42"}}
	if got := candidateWeight(c); got != 42 {
		t.Fatalf("candidateWeight() = %d, want 42", got)
	}
	zero := &pluginapi.SchedulerAuthCandidate{ID: "x", Attributes: map[string]string{"weight": "0"}}
	if got := candidateWeight(zero); got != 0 {
		t.Fatalf("candidateWeight(0) = %d, want 0 (explicit zero is preserved, not defaulted)", got)
	}
}

func TestHighestPriorityEligibleCandidateWins(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	for _, id := range []string{"low", "high", "middle"} {
		runtime.cache.recordSuccess(id, 1, now.Add(time.Hour), now)
	}
	response, decisionError := runtime.pick(claudeRequest(
		candidate("low", 1),
		candidate("high", 20),
		candidate("middle", 10),
	))
	if decisionError != nil || response.AuthID != "high" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
}

func TestEqualPriorityUsesLexicalAuthID(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	for _, id := range []string{"z-auth", "a-auth", "m-auth"} {
		runtime.cache.recordSuccess(id, 1, now.Add(time.Hour), now)
	}
	response, decisionError := runtime.pick(claudeRequest(
		candidate("z-auth", 10),
		candidate("a-auth", 10),
		candidate("m-auth", 10),
	))
	if decisionError != nil || response.AuthID != "a-auth" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
}

func TestSchedulerRespectsRequestCandidateList(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("not-supplied", 1, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("supplied", 1, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(candidate("supplied", 1)))
	if decisionError != nil || response.AuthID != "supplied" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
}

func TestAllClaudeCandidatesBlockedReturnsExplicitError(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	runtime.config.Store(&cfg)
	runtime.cache.recordSuccess("auth-a", 96, now.Add(time.Hour), now)
	runtime.cache.recordSuccess("auth-b", 99, now.Add(2*time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0), candidate("auth-b", 0)))
	wantMessage := "five_hour_quota_exhausted; retry_after_seconds=3600; resets_at=2026-09-23T13:00:00Z"
	if response.Handled || decisionError == nil || decisionError.Code != exhaustedErrorCode || decisionError.Message != wantMessage {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
}

func TestUnknownSampleFailsClosed(t *testing.T) {
	// A credential with zero successful samples ever is fail-closed: excluded
	// from scheduling until a poll succeeds.
	runtime := newTestRuntime(&fakeHost{}, nil, time.Now())
	response, decisionError := runtime.pick(claudeRequest(candidate("unknown", 0)))
	if response.Handled || decisionError == nil || decisionError.Code != exhaustedErrorCode {
		t.Fatalf("response = %#v, error = %#v, want fail-closed exclusion", response, decisionError)
	}
	if !runtime.cache.isExcluded("unknown", time.Now(), defaultCutoffPercentUsed) {
		t.Fatal("never-sampled credential should be isExcluded")
	}
}

func TestPollingFailureRetainsKnownBlockedState(t *testing.T) {
	now := time.Now().UTC()
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token-a")},
	}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{
		"token-a": {{category: pollErrorRateLimited}},
	}}
	runtime := newTestRuntime(host, fetcher.fetch, now)
	runtime.cache.recordSuccess("auth-a", 96, now.Add(time.Hour), now.Add(-time.Minute))
	runtime.pollOnce(context.Background(), defaultPluginConfig())

	sample := runtime.cache.snapshot("auth-a")
	if !sample.HasSample || !sample.blocked(now, 95) || sample.FiveHourPercentUsed != 96 || sample.LastErrorCategory != pollErrorRateLimited {
		t.Fatalf("sample = %#v", sample)
	}
	if !runtime.cache.isBlocked("auth-a", now, 95) {
		t.Fatal("blocked state was cleared by polling failure")
	}
	if !runtime.cache.isExcluded("auth-a", now, 95) {
		t.Fatal("excluded state was cleared by polling failure")
	}
}

// TestFailureDoesNotOverwritePriorSuccessfulSample verifies recordFailure
// never touches HasSample/FiveHourPercentUsed/ResetAt for a credential that
// already had a successful sample within its reset window.
func TestFailureDoesNotOverwritePriorSuccessfulSample(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	resetAt := now.Add(time.Hour)
	runtime.cache.recordSuccess("auth-a", 40, resetAt, now)
	before := runtime.cache.snapshot("auth-a")

	for _, category := range []string{pollErrorUnauthorized, pollErrorRateLimited, pollErrorTimeout} {
		runtime.cache.recordFailure("auth-a", category)
		after := runtime.cache.snapshot("auth-a")
		if after.HasSample != before.HasSample || after.FiveHourPercentUsed != before.FiveHourPercentUsed || !after.ResetAt.Equal(before.ResetAt) {
			t.Fatalf("recordFailure(%q) mutated sample data: before=%#v after=%#v", category, before, after)
		}
		if after.LastErrorCategory != category {
			t.Fatalf("recordFailure(%q) did not record category: %#v", category, after)
		}
	}
	// The credential remains selectable since it's still within its reset window and below cutoff.
	response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0)))
	if decisionError != nil || response.AuthID != "auth-a" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
}

func TestSuccessfulBelowCutoffRefreshClearsBlockedState(t *testing.T) {
	now := time.Now().UTC()
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token-a")},
	}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{
		"token-a": {{result: usageResult{FiveHourPercentUsed: 49.9, ResetAt: now.Add(time.Hour)}}},
	}}
	runtime := newTestRuntime(host, fetcher.fetch, now)
	runtime.cache.recordSuccess("auth-a", 80, now.Add(time.Hour), now.Add(-time.Minute))
	runtime.pollOnce(context.Background(), defaultPluginConfig())

	sample := runtime.cache.snapshot("auth-a")
	if !sample.HasSample || sample.blocked(now, 95) || sample.FiveHourPercentUsed != 49.9 || sample.LastErrorCategory != "" {
		t.Fatalf("sample = %#v", sample)
	}
}

func TestExpiredResetClearsStaleBlockedSample(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("auth-a", 80, now.Add(-time.Second), now.Add(-time.Hour))
	response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0)))
	if decisionError != nil || response.AuthID != "auth-a" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
	sample := runtime.cache.snapshot("auth-a")
	if sample.known(now) || sample.blocked(now, 95) {
		t.Fatalf("expired sample = %#v, want fail-open state", sample)
	}
}

// TestExpiredResetOverridesStaleOverCutoffPercent proves the excluded()
// reset-passed branch takes priority even when the stale cached percent was
// above cutoff: once resets_at has passed, the credential must be available
// again regardless of the stale percent value.
func TestExpiredResetOverridesStaleOverCutoffPercent(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	// 99% stale usage, but reset time is already in the past.
	runtime.cache.recordSuccess("auth-a", 99, now.Add(-time.Second), now.Add(-time.Hour))
	if runtime.cache.isExcluded("auth-a", now, 95) {
		t.Fatal("credential with expired reset should not be excluded, regardless of stale over-cutoff percent")
	}
	response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0)))
	if decisionError != nil || response.AuthID != "auth-a" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
}

func TestOneFailedAccountDoesNotStopRefreshPass(t *testing.T) {
	now := time.Now().UTC()
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{
			physicalEntry("auth-a", "index-a"),
			physicalEntry("auth-b", "index-b"),
		},
		authJSON:  map[string]json.RawMessage{"index-b": credentialJSON("token-b")},
		getErrors: map[string]error{"index-a": errors.New("fixture read failure")},
	}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{
		"token-b": {{result: usageResult{FiveHourPercentUsed: 10, ResetAt: now.Add(time.Hour)}}},
	}}
	runtime := newTestRuntime(host, fetcher.fetch, now)
	runtime.pollOnce(context.Background(), defaultPluginConfig())

	if runtime.cache.snapshot("auth-a").LastErrorCategory != pollErrorAuthGet {
		t.Fatalf("auth-a sample = %#v", runtime.cache.snapshot("auth-a"))
	}
	if !runtime.cache.snapshot("auth-b").HasSample {
		t.Fatalf("auth-b sample = %#v", runtime.cache.snapshot("auth-b"))
	}
	_, getCalls := host.counts()
	if getCalls != 2 || fetcher.callCount() != 1 {
		t.Fatalf("get calls = %d, fetch calls = %d", getCalls, fetcher.callCount())
	}
}

func TestPhysicalClaudeAuthMapping(t *testing.T) {
	entries := []pluginapi.HostAuthFileEntry{
		physicalEntry("z-auth", "index-z"),
		{ID: "runtime", AuthIndex: "runtime-index", Provider: "claude", RuntimeOnly: true},
		{ID: "codex", AuthIndex: "codex-index", Provider: "codex", Path: "/fixtures/codex.json"},
		{ID: "no-path", AuthIndex: "no-path-index", Provider: "claude"},
		disabledEntry("disabled", "disabled-index"),
		physicalEntry("a-auth", "index-a"),
		physicalEntry(" invalid", "invalid-index"),
	}
	got := physicalClaudeAuths(entries)
	if len(got) != 2 || got[0].ID != "a-auth" || got[0].AuthIndex != "index-a" || got[1].ID != "z-auth" {
		t.Fatalf("mapped auths = %#v", got)
	}
}

func TestCachePrunesRemovedAuths(t *testing.T) {
	now := time.Now().UTC()
	host := &fakeHost{
		entries:   []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		getErrors: map[string]error{"index-a": errors.New("fixture read failure")},
	}
	runtime := newTestRuntime(host, (&fakeFetcher{}).fetch, now)
	_, _ = runtime.cache.reconcile([]physicalClaudeAuth{
		{ID: "auth-a", AuthIndex: "index-a"},
		{ID: "removed", AuthIndex: "removed-index"},
	})
	runtime.pollOnce(context.Background(), defaultPluginConfig())
	if sample := runtime.cache.snapshot("removed"); sample != (quotaSample{}) {
		t.Fatalf("removed sample = %#v", sample)
	}
}

func TestCredentialReplacementClearsStaleBlock(t *testing.T) {
	now := time.Now().UTC()
	oldEntry := physicalEntry("auth-a", "index-a")
	oldEntry.Account = "same-account"
	oldEntry.ModTime = now
	newEntry := physicalEntry("auth-a", "index-a")
	newEntry.Account = "same-account"
	newEntry.ModTime = now
	newEntry.Path = "/fixtures/replacement.json"
	host := &fakeHost{
		entries:   []pluginapi.HostAuthFileEntry{newEntry},
		getErrors: map[string]error{"index-a": errors.New("fixture read failure")},
	}
	runtime := newTestRuntime(host, (&fakeFetcher{}).fetch, now)
	_, _ = runtime.cache.reconcile(physicalClaudeAuths([]pluginapi.HostAuthFileEntry{oldEntry}))
	runtime.cache.recordSuccess("auth-a", 80, now.Add(time.Hour), now.Add(-time.Minute))
	runtime.pollOnce(context.Background(), defaultPluginConfig())
	sample := runtime.cache.snapshot("auth-a")
	if sample.HasSample || sample.blocked(now, 50) || sample.LastErrorCategory != pollErrorAuthGet {
		t.Fatalf("replacement sample = %#v", sample)
	}
}

func TestCredentialReplacementClearsBeforeNetworkPolling(t *testing.T) {
	now := time.Now().UTC()
	oldEntry := physicalEntry("auth-b", "index-b")
	oldEntry.Path = "/fixtures/auth-b-old.json"
	newEntry := physicalEntry("auth-b", "index-b")
	newEntry.Path = "/fixtures/auth-b-new.json"
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{
			physicalEntry("auth-a", "index-a"),
			newEntry,
		},
		authJSON: map[string]json.RawMessage{
			"index-a": credentialJSON("token-a"),
			"index-b": credentialJSON("token-b"),
		},
	}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releasePoll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releasePoll()
	fetch := func(ctx context.Context, token string, _ time.Duration) (usageResult, string) {
		if token == "token-a" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return usageResult{}, pollErrorCancelled
			}
		}
		return usageResult{FiveHourPercentUsed: 10, ResetAt: now.Add(time.Hour)}, ""
	}
	runtime := newTestRuntime(host, fetch, now)
	_, _ = runtime.cache.reconcile(physicalClaudeAuths([]pluginapi.HostAuthFileEntry{oldEntry}))
	runtime.cache.recordSuccess("auth-b", 80, now.Add(time.Hour), now.Add(-time.Minute))
	done := make(chan struct{})
	go func() {
		runtime.pollOnce(context.Background(), defaultPluginConfig())
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first credential was not polled")
	}
	// Credential replacement is detected on reconcile (identity changed via
	// backing-file path), which clears the stale sample before the fresh poll for
	// auth-b runs. Under fail-closed semantics, that means auth-b is
	// excluded from scheduling until its own poll completes, even though
	// auth-a's slower poll is still in flight.
	_, decisionError := runtime.pick(claudeRequest(candidate("auth-b", 0)))
	if decisionError == nil || decisionError.Code != exhaustedErrorCode {
		t.Fatalf("replaced credential should be fail-closed pending its own refresh: error=%#v", decisionError)
	}
	releasePoll()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll did not finish")
	}
}

func TestMetadataOnlyAuthUpdatePreservesCredentialRevisionAndSample(t *testing.T) {
	now := time.Now().UTC()
	entry := physicalEntry("auth-a", "index-a")
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	auth := physicalClaudeAuths([]pluginapi.HostAuthFileEntry{entry})[0]
	_, _ = runtime.cache.reconcile([]physicalClaudeAuth{auth})
	bound, ok := runtime.cache.bindRevision(auth, claudeCredentialRevision(claudeCredential{AccessToken: "token-a"}))
	if !ok {
		t.Fatal("bind initial revision")
	}
	runtime.cache.recordSuccessForIdentity(bound, 99, now.Add(time.Hour), now)

	entry.Size = 12345
	entry.ModTime = now.Add(time.Minute)
	_, _ = runtime.cache.reconcile(physicalClaudeAuths([]pluginapi.HostAuthFileEntry{entry}))
	sample := runtime.cache.snapshot("auth-a")
	if !sample.HasSample || !sample.blocked(now, 95) {
		t.Fatalf("metadata-only update cleared confirmed sample: %#v", sample)
	}
}

func TestSamePathCredentialReplacementInvalidatesAndRejectsOldInFlightResult(t *testing.T) {
	now := time.Now().UTC()
	entry := physicalEntry("auth-a", "index-a")
	entry.Path = "/fixtures/same-path.json"
	entry.Email = ""
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{entry}, authJSON: map[string]json.RawMessage{"index-a": credentialJSON("old-token")}}
	oldFetchStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	newFetchStarted := make(chan struct{})
	releaseNew := make(chan struct{})
	fetch := func(ctx context.Context, token string, _ time.Duration) (usageResult, string) {
		switch token {
		case "old-token":
			close(oldFetchStarted)
			select {
			case <-releaseOld:
			case <-ctx.Done():
				return usageResult{}, pollErrorCancelled
			}
			return usageResult{FiveHourPercentUsed: 99, ResetAt: now.Add(time.Hour)}, ""
		case "new-token":
			close(newFetchStarted)
			select {
			case <-releaseNew:
			case <-ctx.Done():
				return usageResult{}, pollErrorCancelled
			}
			return usageResult{FiveHourPercentUsed: 10, ResetAt: now.Add(time.Hour)}, ""
		default:
			return usageResult{}, pollErrorNetwork
		}
	}
	runtime := newTestRuntime(host, fetch, now)
	auth := physicalClaudeAuths([]pluginapi.HostAuthFileEntry{entry})[0]
	_, _ = runtime.cache.reconcile([]physicalClaudeAuth{auth})

	doneOld := make(chan struct{})
	go func() { runtime.pollAuth(context.Background(), auth, defaultPluginConfig()); close(doneOld) }()
	<-oldFetchStarted
	host.mu.Lock()
	host.authJSON["index-a"] = credentialJSON("new-token")
	host.mu.Unlock()
	doneNew := make(chan struct{})
	go func() { runtime.pollAuth(context.Background(), auth, defaultPluginConfig()); close(doneNew) }()
	<-newFetchStarted
	if sample := runtime.cache.snapshot("auth-a"); sample.HasSample {
		t.Fatalf("same-path replacement retained old sample: %#v", sample)
	}
	close(releaseOld)
	<-doneOld
	if sample := runtime.cache.snapshot("auth-a"); sample.HasSample {
		t.Fatalf("old in-flight result committed to replacement: %#v", sample)
	}
	close(releaseNew)
	<-doneNew
	sample := runtime.cache.snapshot("auth-a")
	if !sample.HasSample || sample.FiveHourPercentUsed != 10 {
		t.Fatalf("replacement sample = %#v", sample)
	}
}

func TestBlockedSamePathReplacementMetadataChangeQueuesRevisionCheck(t *testing.T) {
	now := time.Now().UTC()
	entry := physicalEntry("auth-a", "index-a")
	entry.Path, entry.Email = "/fixtures/same-path.json", ""
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{entry}, authJSON: map[string]json.RawMessage{"index-a": credentialJSON("old-token")}}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{
		"old-token": {{result: usageResult{FiveHourPercentUsed: 99, ResetAt: now.Add(time.Hour)}}},
		"new-token": {{result: usageResult{FiveHourPercentUsed: 10, ResetAt: now.Add(time.Hour)}}},
	}}
	runtime := newTestRuntime(host, fetcher.fetch, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled, cfg.PollInterval = false, time.Nanosecond
	runtime.applyConfig(cfg)
	defer runtime.shutdown()
	waitFor(t, func() bool { return runtime.cache.snapshot("auth-a").HasSample })
	if !runtime.cache.snapshot("auth-a").blocked(now, cfg.CutoffPercentUsed) {
		t.Fatal("initial old credential is not blocked")
	}

	host.mu.Lock()
	host.authJSON["index-a"] = credentialJSON("new-token")
	updated := host.entries[0]
	updated.ModTime = updated.ModTime.Add(time.Second)
	host.entries[0] = updated
	host.mu.Unlock()
	if response := runtime.interceptBeforeAuth(beforeAuthRequest()); !response.Terminate {
		t.Fatalf("old confirmed sample should gate while async revision check starts: %#v", response)
	}
	waitFor(t, func() bool {
		sample := runtime.cache.snapshot("auth-a")
		return sample.HasSample && sample.FiveHourPercentUsed == 10
	})
	if response := runtime.interceptBeforeAuth(beforeAuthRequest()); response.Terminate {
		t.Fatalf("replacement recovery remained gated: %#v", response)
	}
}

func TestDisabledAuthIsNotPolled(t *testing.T) {
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{disabledEntry("auth-a", "index-a")}}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{}}
	runtime := newTestRuntime(host, fetcher.fetch, time.Now())
	runtime.pollOnce(context.Background(), defaultPluginConfig())
	listCalls, getCalls := host.counts()
	if listCalls != 1 || getCalls != 0 || fetcher.callCount() != 0 {
		t.Fatalf("callbacks: list=%d get=%d fetch=%d", listCalls, getCalls, fetcher.callCount())
	}
}

func TestCancelledPollSkipsHostCallbacks(t *testing.T) {
	host := &fakeHost{}
	runtime := newTestRuntime(host, (&fakeFetcher{}).fetch, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtime.pollOnce(ctx, defaultPluginConfig())
	listCalls, getCalls := host.counts()
	if listCalls != 0 || getCalls != 0 {
		t.Fatalf("callbacks after cancellation: list=%d get=%d", listCalls, getCalls)
	}
}

func TestReconfigureDoesNotStartDuplicateRefreshWorkers(t *testing.T) {
	host := &fakeHost{}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{}}
	runtime := newPluginRuntime(host, fetcher.fetch, time.Now)
	cfg := defaultPluginConfig()
	cfg.PollInterval = time.Hour
	runtime.applyConfig(cfg)
	waitFor(t, func() bool {
		listCalls, _ := host.counts()
		return listCalls > 0
	})
	firstDone := runtime.done
	for range 3 {
		runtime.applyConfig(cfg)
	}
	if runtime.done != firstDone {
		t.Fatal("reconfigure replaced the active refresh worker")
	}
	runtime.shutdown()
}

func TestShutdownTerminatesRefreshWorker(t *testing.T) {
	host := &fakeHost{}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{}}
	runtime := newPluginRuntime(host, fetcher.fetch, time.Now)
	cfg := defaultPluginConfig()
	cfg.PollInterval = time.Hour
	runtime.applyConfig(cfg)
	waitFor(t, func() bool {
		listCalls, _ := host.counts()
		return listCalls > 0
	})
	runtime.shutdown()
	if runtime.wake != nil || runtime.cancel != nil || runtime.done != nil {
		t.Fatalf("lifecycle handles not cleared: wake=%v cancel=%v done=%v", runtime.wake != nil, runtime.cancel != nil, runtime.done != nil)
	}
}

func TestIdleWorkerDoesNotRefreshOnInterval(t *testing.T) {
	host := &fakeHost{}
	runtime := newPluginRuntime(host, (&fakeFetcher{}).fetch, time.Now)
	cfg := defaultPluginConfig()
	cfg.PollInterval = 10 * time.Millisecond
	runtime.applyConfig(cfg)
	defer runtime.shutdown()

	waitFor(t, func() bool {
		listCalls, _ := host.counts()
		return listCalls == 1
	})
	time.Sleep(50 * time.Millisecond)
	listCalls, _ := host.counts()
	if listCalls != 1 {
		t.Fatalf("idle refresh calls = %d, want exactly the startup refresh", listCalls)
	}
}

func TestStaleRequestRefreshesOnlySelectedAuth(t *testing.T) {
	startedAt := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	clock := &testClock{value: startedAt}
	host := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{
			physicalEntry("auth-a", "index-a"),
			physicalEntry("auth-b", "index-b"),
		},
		authJSON: map[string]json.RawMessage{
			"index-a": credentialJSON("token-a"),
			"index-b": credentialJSON("token-b"),
		},
	}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{
		"token-a": {{result: usageResult{FiveHourPercentUsed: 10, ResetAt: startedAt.Add(time.Hour)}}},
		"token-b": {
			{result: usageResult{FiveHourPercentUsed: 10, ResetAt: startedAt.Add(time.Hour)}},
			{result: usageResult{FiveHourPercentUsed: 96, ResetAt: startedAt.Add(time.Hour)}},
		},
	}}
	runtime := newPluginRuntime(host, fetcher.fetch, clock.now)
	cfg := defaultPluginConfig()
	cfg.PollInterval = 5 * time.Minute
	runtime.applyConfig(cfg)
	defer runtime.shutdown()
	waitFor(t, func() bool { return fetcher.callCount() == 2 })

	clock.set(startedAt.Add(6 * time.Minute))
	response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0), candidate("auth-b", 10)))
	if decisionError != nil || response.AuthID != "auth-b" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
	waitFor(t, func() bool { return fetcher.callCount() == 3 })
	if calls := fetcher.callTokens(); len(calls) != 3 || calls[0] != "token-a" || calls[1] != "token-b" || calls[2] != "token-b" {
		t.Fatalf("fetch calls = %#v, want startup auth-a/auth-b then selected auth-b", calls)
	}
	if sample := runtime.cache.snapshot("auth-b"); !sample.blocked(clock.now(), cfg.CutoffPercentUsed) {
		t.Fatalf("selected auth did not refresh to blocked state: %#v", sample)
	}
}

func TestBlockedAuthSleepsUntilReset(t *testing.T) {
	startedAt := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	clock := &testClock{value: startedAt}
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token-a")},
	}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{
		"token-a": {
			{result: usageResult{FiveHourPercentUsed: 96, ResetAt: startedAt.Add(time.Hour)}},
			{result: usageResult{FiveHourPercentUsed: 5, ResetAt: startedAt.Add(8 * 24 * time.Hour)}},
		},
	}}
	runtime := newPluginRuntime(host, fetcher.fetch, clock.now)
	cfg := defaultPluginConfig()
	cfg.PollInterval = 5 * time.Minute
	// Disable overage fallback: this test is about reset-based cache
	// refresh timing, not the single-candidate overage-fallback
	// self-selection case (covered separately).
	cfg.OverageFallbackEnabled = false
	runtime.applyConfig(cfg)
	defer runtime.shutdown()
	waitFor(t, func() bool { return fetcher.callCount() == 1 })

	clock.set(startedAt.Add(6 * time.Minute))
	if _, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0))); decisionError == nil {
		t.Fatal("blocked auth should remain unavailable before reset")
	}
	time.Sleep(25 * time.Millisecond)
	if calls := fetcher.callCount(); calls != 1 {
		t.Fatalf("blocked auth refreshed before reset: %d calls", calls)
	}

	clock.set(startedAt.Add(time.Hour + time.Minute))
	response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0)))
	if decisionError != nil || response.AuthID != "auth-a" {
		t.Fatalf("post-reset response = %#v, error = %#v", response, decisionError)
	}
	waitFor(t, func() bool { return fetcher.callCount() == 2 })
	if sample := runtime.cache.snapshot("auth-a"); sample.blocked(clock.now(), cfg.CutoffPercentUsed) {
		t.Fatalf("post-reset refresh remained blocked: %#v", sample)
	}
}

func TestDisableClearsInflightSuccess(t *testing.T) {
	now := time.Now().UTC()
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token-a")},
	}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseFetch := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFetch()
	fetch := func(context.Context, string, time.Duration) (usageResult, string) {
		close(started)
		<-release
		return usageResult{FiveHourPercentUsed: 80, ResetAt: now.Add(time.Hour)}, ""
	}
	runtime := newPluginRuntime(host, fetch, func() time.Time { return now })
	cfg := defaultPluginConfig()
	cfg.PollInterval = time.Hour
	runtime.applyConfig(cfg)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	disabled := cfg
	disabled.Enabled = false
	done := make(chan struct{})
	go func() {
		runtime.applyConfig(disabled)
		close(done)
	}()
	waitFor(t, func() bool { return !runtime.loadedConfig().Enabled })
	releaseFetch()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disable did not finish")
	}
	if sample := runtime.cache.snapshot("auth-a"); sample != (quotaSample{}) {
		t.Fatalf("disabled cache retained in-flight success: %#v", sample)
	}
}

func TestTokensAndResponseBodiesDoNotAppearInLogsOrErrors(t *testing.T) {
	const token = "oauth-access-token-MUST-NOT-LEAK"
	const responseBody = "complete-response-body-MUST-NOT-LEAK"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(responseBody))
	}))
	defer server.Close()

	now := time.Now().UTC()
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		authJSON: map[string]json.RawMessage{"index-a": credentialJSON(token)},
	}
	httpFetcher := newHTTPUsageFetcher(server.URL, server.Client().Transport, "")
	runtime := newTestRuntime(host, httpFetcher.fetch, now)
	runtime.pollOnce(context.Background(), defaultPluginConfig())

	logs := host.logText()
	if strings.Contains(logs, token) || strings.Contains(logs, responseBody) || strings.Contains(logs, "refresh-must-stay-secret") {
		t.Fatalf("secret appeared in logs: %s", logs)
	}
	if sample := runtime.cache.snapshot("auth-a"); sample.LastErrorCategory != pollErrorServer {
		t.Fatalf("sample = %#v", sample)
	}
}

func TestSchedulerPickPerformsNoHTTPOrAuthCallbacks(t *testing.T) {
	now := time.Now().UTC()
	host := &fakeHost{}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{}}
	runtime := newTestRuntime(host, fetcher.fetch, now)
	runtime.cache.recordSuccess("auth-a", 10, now.Add(time.Hour), now)

	response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0)))
	if decisionError != nil || response.AuthID != "auth-a" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
	listCalls, getCalls := host.counts()
	if listCalls != 0 || getCalls != 0 || fetcher.callCount() != 0 {
		t.Fatalf("callbacks during pick: list=%d get=%d fetch=%d", listCalls, getCalls, fetcher.callCount())
	}
}

func TestManagementStatusRouteExposesOnlySchedulerState(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	runtime := newTestRuntime(&fakeHost{}, (&fakeFetcher{}).fetch, now)
	_, _ = runtime.cache.reconcile([]physicalClaudeAuth{
		{ID: "auth-a", AuthIndex: "index-a", Name: "claude-a.json"},
		{ID: "auth-b", AuthIndex: "index-b", Name: "claude-b.json"},
	})
	runtime.cache.recordSuccess("auth-a", 96, now.Add(2*time.Hour), now)
	runtime.cache.recordFailure("auth-b", pollErrorRateLimited)

	response := runtime.handleManagement(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   managementStatusFullPath,
	})
	if response.StatusCode != http.StatusOK || response.Headers.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("management response = %#v", response)
	}
	var status cutoffStatusResponse
	if err := json.Unmarshal(response.Body, &status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || len(status.ProtectedModels) != 0 || status.CutoffPercentUsed != defaultCutoffPercentUsed || len(status.Accounts) != 2 {
		t.Fatalf("status = %#v", status)
	}
	blocked := status.Accounts[0]
	if blocked.ID != "auth-a" || blocked.AuthIndex != "index-a" || blocked.Name != "claude-a.json" || !blocked.Known || !blocked.Blocked || blocked.FiveHourPercentUsed == nil || *blocked.FiveHourPercentUsed != 96 || blocked.SampledAt == "" || blocked.ResetAt == "" {
		t.Fatalf("blocked account = %#v", blocked)
	}
	unknown := status.Accounts[1]
	// Never-sampled credential: fail-closed, so Blocked (excluded) is true.
	if unknown.ID != "auth-b" || unknown.Known || !unknown.Blocked || unknown.FiveHourPercentUsed != nil || unknown.LastErrorCategory != pollErrorRateLimited {
		t.Fatalf("unknown account = %#v", unknown)
	}

	var rawStatus map[string]any
	if err := json.Unmarshal(response.Body, &rawStatus); err != nil {
		t.Fatal(err)
	}
	rawAccounts, ok := rawStatus["accounts"].([]any)
	if !ok {
		t.Fatalf("raw accounts = %#v", rawStatus["accounts"])
	}
	allowedKeys := map[string]bool{
		"id": true, "auth_index": true, "name": true, "known": true, "blocked": true,
		"five_hour_percent_used": true, "sampled_at": true, "reset_at": true, "last_error_category": true,
	}
	for _, rawAccount := range rawAccounts {
		account, okAccount := rawAccount.(map[string]any)
		if !okAccount {
			t.Fatalf("raw account = %#v", rawAccount)
		}
		for key := range account {
			if !allowedKeys[key] {
				t.Fatalf("unsafe or undocumented status field %q in %#v", key, account)
			}
		}
	}

	notFound := runtime.handleManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementStatusFullPath})
	if notFound.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong-method response = %#v", notFound)
	}
}

func TestManagementStatusExpiresStaleBlockedSample(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	runtime := newTestRuntime(&fakeHost{}, (&fakeFetcher{}).fetch, now)
	_, _ = runtime.cache.reconcile([]physicalClaudeAuth{{ID: "auth-a", AuthIndex: "index-a", Name: "claude-a.json"}})
	runtime.cache.recordSuccess("auth-a", 75, now.Add(-time.Second), now.Add(-time.Hour))
	runtime.cache.recordFailure("auth-a", pollErrorRateLimited)

	response := runtime.handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementStatusFullPath})
	var status cutoffStatusResponse
	if err := json.Unmarshal(response.Body, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Accounts) != 1 || status.Accounts[0].Known || status.Accounts[0].Blocked || status.Accounts[0].FiveHourPercentUsed != nil || status.Accounts[0].LastErrorCategory != pollErrorRateLimited {
		t.Fatalf("expired status = %#v", status)
	}
	if sample := runtime.cache.snapshot("auth-a"); sample.known(now) || sample.blocked(now, 50) {
		t.Fatalf("expired cache sample = %#v", sample)
	}
}

func TestManagementRegistration(t *testing.T) {
	registration := managementRegistration()
	if len(registration.Routes) != 1 || registration.Routes[0].Method != http.MethodGet || registration.Routes[0].Path != managementStatusRoute {
		t.Fatalf("management registration = %#v", registration)
	}
}

func TestConfigValidationAndRegistrationMetadata(t *testing.T) {
	enabled := true
	cutoff := 42.5
	raw, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: []byte(
		"enabled: true\nprotected-models: [claude-sonnet-4-6, CLAUDE-OPUS-4-1, claude-sonnet-4-6]\ncutoff-percent-used: 42.5\npoll-interval: 30s\nrequest-timeout: 2s\nuser-agent: custom-agent/1.0\noverage-fallback-enabled: false\n",
	)})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	cfg, errConfig := decodeLifecycleConfig(raw)
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	if cfg.Enabled != enabled || strings.Join(cfg.ProtectedModels, ",") != "claude-opus-4-1,claude-sonnet-4-6" || cfg.CutoffPercentUsed != cutoff || cfg.PollInterval != 30*time.Second || cfg.RequestTimeout != 2*time.Second || cfg.UserAgent != "custom-agent/1.0" || cfg.OverageFallbackEnabled != false {
		t.Fatalf("config = %#v", cfg)
	}
	defaults, errDefaults := decodeLifecycleConfig([]byte("{}"))
	if errDefaults != nil || len(defaults.ProtectedModels) != 0 || defaults.CutoffPercentUsed != defaultCutoffPercentUsed || defaults.PollInterval != defaultPollInterval || defaults.UserAgent != defaultAnthropicUserAgent || defaults.OverageFallbackEnabled != true {
		t.Fatalf("default config = %#v, error = %v", defaults, errDefaults)
	}

	// Empty protected-models is now valid (means "protect all").
	emptyList, errEmpty := decodeLifecycleConfig(mustMarshalLifecycle("protected-models: []\n"))
	if errEmpty != nil || len(emptyList.ProtectedModels) != 0 {
		t.Fatalf("empty protected-models = %#v, error = %v", emptyList, errEmpty)
	}

	for _, configYAML := range []string{
		"protected-models: ['']\n",
		"cutoff-percent-used: 101\n",
		"cutoff-percent-used: -1\n",
		"poll-interval: 0s\n",
		"request-timeout: nope\n",
	} {
		raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(configYAML)})
		if _, err := decodeLifecycleConfig(raw); err == nil {
			t.Fatalf("config %q unexpectedly succeeded", configYAML)
		}
	}

	registration := pluginRegistration()
	if registration.Metadata.Name != pluginName ||
		registration.Metadata.Version != pluginVersion ||
		registration.Metadata.Author != "kurbezz (fork of Smarty Pants Inc cpa-plugin-quota-router v0.5.0)" ||
		registration.Metadata.GitHubRepository != "https://github.com/kurbezz/five-hour-quota-router" ||
		!registration.Capabilities.Scheduler || !registration.Capabilities.RequestInterceptor || !registration.Capabilities.ManagementAPI {
		t.Fatalf("registration = %#v", registration)
	}
	fields := map[string]pluginapi.ConfigFieldType{}
	for _, field := range registration.Metadata.ConfigFields {
		fields[field.Name] = field.Type
	}
	if fields["protected-models"] != pluginapi.ConfigFieldTypeArray ||
		fields["cutoff-percent-used"] != pluginapi.ConfigFieldTypeNumber ||
		fields["poll-interval"] != pluginapi.ConfigFieldTypeString ||
		fields["request-timeout"] != pluginapi.ConfigFieldTypeString ||
		fields["user-agent"] != pluginapi.ConfigFieldTypeString ||
		fields["overage-fallback-enabled"] != pluginapi.ConfigFieldTypeBoolean {
		t.Fatalf("config fields = %#v", fields)
	}
}

func mustMarshalLifecycle(configYAML string) []byte {
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(configYAML)})
	return raw
}

// TestFiveHourLimitsFallbackShape verifies the "limits[]" fallback shape
// (five_hour: null, limits: [{"kind":"session","percent":X,"resets_at":Y}])
// parses into the same usageResult.FiveHourPercentUsed field.
func TestFiveHourLimitsFallbackShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour":null,"limits":[{"kind":"other","percent":10,"resets_at":"2026-01-01T00:00:00Z"},{"kind":"session","percent":33,"resets_at":"2026-02-06T22:00:00+00:00"}]}`))
	}))
	defer server.Close()
	fetcher := newHTTPUsageFetcher(server.URL, server.Client().Transport, "")
	result, category := fetcher.fetch(context.Background(), "token", time.Second)
	if category != "" {
		t.Fatalf("category = %q, want empty", category)
	}
	if result.FiveHourPercentUsed != 33 {
		t.Fatalf("FiveHourPercentUsed = %v, want 33", result.FiveHourPercentUsed)
	}
	if result.ResetAt.Format(time.RFC3339) != "2026-02-06T22:00:00Z" {
		t.Fatalf("reset = %s", result.ResetAt)
	}
}

// TestConcurrentPickAndRecordDoesNotRace exercises concurrent pick() calls
// alongside concurrent recordSuccess/recordFailure writes to the same
// authID. Run with -race to detect data races.
func TestConcurrentPickAndRecordDoesNotRace(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("auth-a", 10, now.Add(time.Hour), now)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			runtime.pick(claudeRequest(candidate("auth-a", 0)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			runtime.cache.recordSuccess("auth-a", float64(i%100), now.Add(time.Hour), now)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			runtime.cache.recordFailure("auth-a", pollErrorRateLimited)
		}
	}()
	wg.Wait()
}

// TestFiveHourZeroUtilizationIsSelectable exercises utilization = 0.
func TestFiveHourZeroUtilizationIsSelectable(t *testing.T) {
	now := time.Now().UTC()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.recordSuccess("auth-a", 0, now.Add(time.Hour), now)
	response, decisionError := runtime.pick(claudeRequest(candidate("auth-a", 0)))
	if decisionError != nil || response.AuthID != "auth-a" {
		t.Fatalf("response = %#v, error = %#v", response, decisionError)
	}
}
