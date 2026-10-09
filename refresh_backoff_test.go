package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const rateLimitedBody = `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`

// scriptedFetcher returns queued replies in order and counts calls. When the
// queue is empty it returns a plain 429 without upstream hints.
type scriptedFetcher struct {
	mu      sync.Mutex
	replies []fetchReply
	calls   atomic.Int32
}

func (f *scriptedFetcher) fetch(context.Context, string, time.Duration) (usageResult, string) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.replies) == 0 {
		return rateLimitedReply().result, rateLimitedReply().category
	}
	reply := f.replies[0]
	f.replies = f.replies[1:]
	return reply.result, reply.category
}

func rateLimitedReply() fetchReply {
	return fetchReply{category: pollErrorRateLimited, result: usageResult{Failure: usageFailure{StatusCode: http.StatusTooManyRequests, Message: "Rate limited. Please try again later."}}}
}

func successReply(percent float64, resetAt time.Time) fetchReply {
	return fetchReply{result: usageResult{FiveHourPercentUsed: percent, ResetAt: resetAt}}
}

func backoffRuntime(t *testing.T, fetcher *scriptedFetcher, clock *testClock) (*pluginRuntime, *fakeHost) {
	t.Helper()
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token-a")},
	}
	r := newPluginRuntime(host, fetcher.fetch, clock.now)
	r.jitter = nil
	return r, host
}

func countLogs(host *fakeHost, needle string) int {
	return strings.Count(host.logText(), needle)
}

func TestRefreshBackoffAfter429BlocksEveryPathUntilPauseEnds(t *testing.T) {
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	clock := &testClock{value: now}
	fetcher := &scriptedFetcher{replies: []fetchReply{rateLimitedReply(), successReply(10, now.Add(time.Hour))}}
	r, host := backoffRuntime(t, fetcher, clock)
	cfg := defaultPluginConfig()

	r.pollOnce(context.Background(), cfg)
	if got := fetcher.calls.Load(); got != 1 {
		t.Fatalf("calls after first poll = %d, want 1", got)
	}
	sample := r.cache.snapshot("auth-a")
	if sample.RefreshFailures != 1 || !sample.NextRefreshAt.Equal(now.Add(refreshBackoffBase)) {
		t.Fatalf("backoff state = failures %d next %v", sample.RefreshFailures, sample.NextRefreshAt)
	}

	// Every trigger inside the pause: fleet pass (startup/reload), request-
	// driven due check and candidate claim, revision check.
	for _, at := range []time.Duration{time.Minute, 2 * time.Minute, refreshBackoffBase - time.Second} {
		clock.set(now.Add(at))
		r.pollOnce(context.Background(), cfg)
		if r.cache.usageRefreshDue("auth-a", cfg, clock.now()) {
			t.Fatalf("usage refresh due during pause at +%s", at)
		}
		if r.cache.claimPoll("auth-a", clock.now(), cfg.CutoffPercentUsed, cfg.PollInterval) {
			t.Fatalf("candidate claim succeeded during pause at +%s", at)
		}
		auth := physicalClaudeAuths(host.entries)[0]
		r.pollAuthWithRevisionIntent(context.Background(), auth, cfg, true, true)
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Fatalf("upstream calls during pause = %d, want 1", got)
	}
	if got := countLogs(host, "quota refresh failed"); got != 1 {
		t.Fatalf("failure log lines = %d, want 1 per upstream attempt", got)
	}

	clock.set(now.Add(refreshBackoffBase))
	r.pollOnce(context.Background(), cfg)
	if got := fetcher.calls.Load(); got != 2 {
		t.Fatalf("calls after pause = %d, want 2", got)
	}
}

func TestRefreshBackoffDoublesAndCaps(t *testing.T) {
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, expected := range want {
		if got := refreshBackoffDelay(i+1, pollErrorNetwork, usageFailure{}); got != expected {
			t.Fatalf("failure %d delay = %s, want %s", i+1, got, expected)
		}
	}

	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	clock := &testClock{value: now}
	fetcher := &scriptedFetcher{}
	r, _ := backoffRuntime(t, fetcher, clock)
	cfg := defaultPluginConfig()
	at := now
	for i, expected := range want {
		clock.set(at)
		r.pollOnce(context.Background(), cfg)
		sample := r.cache.snapshot("auth-a")
		if sample.RefreshFailures != i+1 || !sample.NextRefreshAt.Equal(at.Add(expected)) {
			t.Fatalf("failure %d: failures=%d next=%v want %v", i+1, sample.RefreshFailures, sample.NextRefreshAt, at.Add(expected))
		}
		at = sample.NextRefreshAt
	}
	if got := int(fetcher.calls.Load()); got != len(want) {
		t.Fatalf("calls = %d, want %d", got, len(want))
	}
}

func TestRefreshBackoffJitterIsSmallAndBounded(t *testing.T) {
	for i := 0; i < 200; i++ {
		if extra := defaultRefreshJitter(refreshBackoffBase); extra < 0 || extra > refreshBackoffBase/refreshBackoffJitterFraction {
			t.Fatalf("jitter = %s", extra)
		}
	}
	c := quotaCache{samples: map[string]quotaSample{}}
	auth := physicalClaudeAuth{ID: "a", Identity: "id"}
	c.reconcile([]physicalClaudeAuth{auth})
	_, inc, _ := c.bindingSnapshot(auth)
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	maxJitter := func(d time.Duration) time.Duration { return d / refreshBackoffJitterFraction }
	_, next, _ := c.recordRefreshFailure(auth, inc, pollErrorRateLimited, usageFailure{}, now, maxJitter)
	if want := now.Add(refreshBackoffBase + refreshBackoffBase/refreshBackoffJitterFraction); !next.Equal(want) {
		t.Fatalf("jittered next = %v, want %v", next, want)
	}
	for i := 0; i < 5; i++ {
		_, next, _ = c.recordRefreshFailure(auth, inc, pollErrorNetwork, usageFailure{}, now, maxJitter)
	}
	if want := now.Add(refreshBackoffMax); !next.Equal(want) {
		t.Fatalf("capped jittered next = %v, want %v", next, want)
	}
}

func TestRefreshBackoffRetryAfterHasPriority(t *testing.T) {
	// Longer than the exponential cap: honored.
	if got := refreshBackoffDelay(1, pollErrorRateLimited, usageFailure{StatusCode: 429, RetryAfter: time.Hour, HasRetryAfter: true}); got != time.Hour {
		t.Fatalf("429 Retry-After 1h delay = %s", got)
	}
	// Shorter than the doubled schedule on a non-429: honored.
	if got := refreshBackoffDelay(3, pollErrorServer, usageFailure{StatusCode: 503, RetryAfter: 90 * time.Second, HasRetryAfter: true}); got != 90*time.Second {
		t.Fatalf("503 Retry-After 90s delay = %s", got)
	}
	// A 429 never pauses for less than the base, even with a short hint.
	if got := refreshBackoffDelay(1, pollErrorRateLimited, usageFailure{StatusCode: 429, RetryAfter: 30 * time.Second, HasRetryAfter: true}); got != refreshBackoffBase {
		t.Fatalf("429 Retry-After 30s delay = %s, want base", got)
	}

	now := time.Now()
	header := http.Header{}
	header.Set("Retry-After", "3600")
	header.Set("Anthropic-Ratelimit-Unified-Reset", "1")
	if d, ok := upstreamRetryDelay(header, now); !ok || d != time.Hour {
		t.Fatalf("Retry-After seconds = %s %v", d, ok)
	}
	header = http.Header{}
	header.Set("Anthropic-Ratelimit-Requests-Reset", now.Add(40*time.Minute).UTC().Format(time.RFC3339))
	header.Set("Anthropic-Ratelimit-Tokens-Reset", now.Add(50*time.Minute).UTC().Format(time.RFC3339))
	if d, ok := upstreamRetryDelay(header, now); !ok || d < 39*time.Minute || d > 40*time.Minute {
		t.Fatalf("ratelimit reset delay = %s %v", d, ok)
	}

	// End to end through the HTTP fetcher and runtime.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2700")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(rateLimitedBody))
	}))
	defer server.Close()
	start := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token-a")},
	}
	r := newTestRuntime(host, newHTTPUsageFetcher(server.URL, server.Client().Transport, "").fetch, start)
	r.jitter = nil
	r.pollOnce(context.Background(), defaultPluginConfig())
	sample := r.cache.snapshot("auth-a")
	if !sample.NextRefreshAt.Equal(start.Add(45 * time.Minute)) {
		t.Fatalf("next refresh = %v, want Retry-After 45m", sample.NextRefreshAt)
	}
	if sample.LastRefreshError != "rate_limited: HTTP 429: Rate limited. Please try again later." {
		t.Fatalf("last refresh error = %q", sample.LastRefreshError)
	}
}

func TestRefreshBackoffConcurrentTriggersSingleRequest(t *testing.T) {
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	clock := &testClock{value: now}
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token-a")},
	}
	r := newPluginRuntime(host, func(ctx context.Context, _ string, _ time.Duration) (usageResult, string) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return usageResult{}, pollErrorCancelled
		}
		return rateLimitedReply().result, rateLimitedReply().category
	}, clock.now)
	r.jitter = nil
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	r.applyConfig(cfg)
	t.Cleanup(r.shutdown)
	<-started

	// While the startup fetch is in flight, fire every other trigger at once.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); r.pick(claudeRequest(candidate("auth-a", 0))) }()
		go func() { defer wg.Done(); r.interceptBeforeAuth(beforeAuthRequest()) }()
		go func() { defer wg.Done(); r.queueAllRefresh() }()
	}
	wg.Wait()
	close(release)
	waitFor(t, func() bool { return r.cache.snapshot("auth-a").RefreshFailures == 1 })

	// After the failure: more requests and a config reload inside the pause.
	clock.set(now.Add(2 * time.Minute))
	for i := 0; i < 10; i++ {
		r.pick(claudeRequest(candidate("auth-a", 0)))
		r.interceptBeforeAuth(beforeAuthRequest())
	}
	reload := cfg
	reload.CutoffPercentUsed = 90
	r.applyConfig(reload)
	waitFor(t, func() bool {
		r.refreshMu.Lock()
		defer r.refreshMu.Unlock()
		return !r.pendingAll && !r.inFlightAll && len(r.pendingIDs) == 0 && len(r.inFlightIDs) == 0
	})
	// pendingAll from the reload may still be dequeued; give the worker a beat.
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", got)
	}
	if got := countLogs(host, "quota refresh failed"); got != 1 {
		t.Fatalf("failure log lines = %d, want 1", got)
	}
}

func TestRefreshBackoffRoutesFromLastSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	clock := &testClock{value: now}
	fetcher := &scriptedFetcher{}
	r, _ := backoffRuntime(t, fetcher, clock)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	r.config.Store(&cfg)
	r.pollOnce(context.Background(), cfg) // registers membership; fails with 429
	auth := physicalClaudeAuths([]pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")})[0]
	gen, inc, _ := r.cache.bindingSnapshot(auth)
	batch := quotaWindowBatch{FiveHour: quotaWindow{Percent: 40, ResetAt: now.Add(time.Hour), Source: quotaSourceHeaders, Valid: true}}
	if !r.cache.commitWindowBatch(auth, gen, inc, batch, now, time.Time{}, false) {
		t.Fatal("seed snapshot rejected")
	}
	clock.set(now.Add(time.Minute))
	if s := r.cache.snapshot("auth-a"); !s.refreshPaused(clock.now()) {
		t.Fatalf("expected active pause: %+v", s)
	}
	if got, err := r.pick(claudeRequest(candidate("auth-a", 0))); err != nil || got.AuthID != "auth-a" {
		t.Fatalf("pick during pause = %+v %v, want last snapshot (40%%) to route", got, err)
	}

	// An exhausted snapshot keeps blocking during the pause...
	gen, inc, _ = r.cache.bindingSnapshot(auth)
	exhausted := quotaWindowBatch{FiveHour: quotaWindow{Percent: 99, ResetAt: now.Add(3 * time.Minute), SampledAt: now.Add(time.Minute), Source: quotaSourceHeaders, Valid: true}}
	if !r.cache.commitWindowBatch(auth, gen, inc, exhausted, now.Add(time.Minute), time.Time{}, false) {
		t.Fatal("exhausted snapshot rejected")
	}
	if got, err := r.pick(claudeRequest(candidate("auth-a", 0))); err == nil || got.Handled {
		t.Fatalf("exhausted snapshot routed: %+v", got)
	}
	// ...until its reset passes, then it counts as reset without a refresh.
	clock.set(now.Add(4 * time.Minute))
	if !r.cache.refreshPaused("auth-a", clock.now()) {
		t.Fatal("pause should still be active")
	}
	if got, err := r.pick(claudeRequest(candidate("auth-a", 0))); err != nil || got.AuthID != "auth-a" {
		t.Fatalf("pick after window reset = %+v %v", got, err)
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestRefreshBackoffNeverSampledStaysFailClosed(t *testing.T) {
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	clock := &testClock{value: now}
	fetcher := &scriptedFetcher{}
	r, _ := backoffRuntime(t, fetcher, clock)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	r.config.Store(&cfg)
	r.pollOnce(context.Background(), cfg)
	clock.set(now.Add(time.Minute))
	if got, err := r.pick(claudeRequest(candidate("auth-a", 0))); err == nil || got.Handled {
		t.Fatalf("never-sampled auth routed during pause: %+v", got)
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestRefreshBackoffResetsAfterSuccess(t *testing.T) {
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	clock := &testClock{value: now}
	fetcher := &scriptedFetcher{replies: []fetchReply{rateLimitedReply(), rateLimitedReply(), successReply(20, now.Add(5*time.Hour)), rateLimitedReply()}}
	r, host := backoffRuntime(t, fetcher, clock)
	cfg := defaultPluginConfig()

	r.pollOnce(context.Background(), cfg)
	clock.set(now.Add(refreshBackoffBase))
	r.pollOnce(context.Background(), cfg)
	if s := r.cache.snapshot("auth-a"); s.RefreshFailures != 2 {
		t.Fatalf("failures = %d, want 2", s.RefreshFailures)
	}
	successAt := now.Add(refreshBackoffBase + 2*refreshBackoffBase)
	clock.set(successAt)
	r.pollOnce(context.Background(), cfg)
	s := r.cache.snapshot("auth-a")
	if s.RefreshFailures != 0 || !s.NextRefreshAt.IsZero() || s.LastRefreshError != "" || s.FiveHourPercentUsed != 20 {
		t.Fatalf("after success: %+v", s)
	}
	if !strings.Contains(host.logText(), "quota refresh recovered") {
		t.Fatalf("missing recovery log: %s", host.logText())
	}
	failAt := successAt.Add(cfg.PollInterval)
	clock.set(failAt)
	r.pollOnce(context.Background(), cfg)
	if s := r.cache.snapshot("auth-a"); s.RefreshFailures != 1 || !s.NextRefreshAt.Equal(failAt.Add(refreshBackoffBase)) {
		t.Fatalf("after next failure: failures=%d next=%v", s.RefreshFailures, s.NextRefreshAt)
	}
}

func TestRefreshBackoffLogAndStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(rateLimitedBody))
	}))
	defer server.Close()
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")},
		authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token-secret")},
	}
	r := newTestRuntime(host, newHTTPUsageFetcher(server.URL, server.Client().Transport, "").fetch, now)
	r.jitter = nil
	r.pollOnce(context.Background(), defaultPluginConfig())

	logs := host.logText()
	for _, want := range []string{
		"quota refresh failed (rate_limited: HTTP 429: Rate limited. Please try again later.; failure #1; next attempt at 2026-10-09T18:05:00Z)",
		`"status":429`, `"consecutive_failures":1`, `"next_refresh_at":"2026-10-09T18:05:00Z"`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log missing %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "token-secret") || strings.Contains(logs, "rate_limit_error") {
		t.Fatalf("log leaked token or raw body: %s", logs)
	}

	response := r.handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementStatusFullPath})
	var status cutoffStatusResponse
	if err := json.Unmarshal(response.Body, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Accounts) != 1 {
		t.Fatalf("status = %+v", status)
	}
	account := status.Accounts[0]
	if account.ConsecutiveFailures != 1 || account.NextRefreshAt != "2026-10-09T18:05:00Z" || account.LastRefreshError != "rate_limited: HTTP 429: Rate limited. Please try again later." {
		t.Fatalf("account status = %+v", account)
	}
}
