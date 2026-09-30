package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestWindowRefreshPollingBatch(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")}, authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token")}, getErrors: map[string]error{}}
	calls := 0
	r := newTestRuntime(host, func(context.Context, string, time.Duration) (usageResult, string) {
		calls++
		return usageResult{FiveHourPercentUsed: 10, ResetAt: now.Add(time.Hour), Windows: quotaWindowBatch{FiveHour: quotaWindow{Percent: 10, Valid: true}, Weekly: quotaWindow{Percent: 40, Valid: true}}}, ""
	}, now)
	r.pollOnce(context.Background(), r.loadedConfig())
	s := r.cache.snapshot("auth-a")
	if calls != 1 || !s.Windows.Weekly.Valid || s.Windows.Weekly.Percent != 40 {
		t.Fatalf("poll result: calls=%d sample=%+v", calls, s)
	}
}

func TestWindowRefreshFailedUsageThrottled(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	clock := &testClock{value: now}
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")}, authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token")}}
	var calls atomic.Int32
	r := newPluginRuntime(host, func(context.Context, string, time.Duration) (usageResult, string) {
		calls.Add(1)
		return usageResult{}, pollErrorNetwork
	}, clock.now)
	cfg := defaultPluginConfig()
	cfg.PollInterval = time.Minute
	auth := physicalClaudeAuths(host.entries)[0]
	r.cache.reconcile([]physicalClaudeAuth{auth})
	r.pollAuthWithRevisionIntent(context.Background(), auth, cfg, true, true)
	if calls.Load() != 1 {
		t.Fatalf("first revision refresh calls=%d, want 1", calls.Load())
	}
	s := r.cache.snapshot(auth.ID)
	if !s.UsageAttemptAt.Equal(now) {
		t.Fatalf("failed fetch attempt timestamp=%v, want %v", s.UsageAttemptAt, now)
	}
	r.pollAuthWithRevisionIntent(context.Background(), auth, cfg, true, true)
	if calls.Load() != 1 {
		t.Fatalf("failed usage fetch retried before interval: calls=%d", calls.Load())
	}
	clock.set(now.Add(time.Minute))
	r.pollAuthWithRevisionIntent(context.Background(), auth, cfg, true, true)
	if calls.Load() != 2 {
		t.Fatalf("eligible retry calls=%d, want 2", calls.Load())
	}
}

func TestWindowRefreshSharedSuppression(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cfg := pluginConfig{CutoffPercentUsed: 95, PollInterval: time.Minute}
	c := quotaCache{samples: map[string]quotaSample{}}
	a := physicalClaudeAuth{ID: "shared", Identity: "id"}
	c.reconcile([]physicalClaudeAuth{a})
	g, inc, _ := c.bindingSnapshot(a)
	seed := quotaWindowBatch{
		FiveHour:    quotaWindow{Percent: 99, ResetAt: now.Add(time.Hour), SampledAt: now, Valid: true},
		Weekly:      quotaWindow{Percent: 99, ResetAt: now.Add(2 * time.Hour), SampledAt: now, Valid: true},
		FableWeekly: quotaWindow{Percent: 99, ResetAt: now.Add(time.Hour), SampledAt: now, Valid: true},
	}
	if !c.commitWindowBatch(a, g, inc, seed, now, time.Time{}, false) {
		t.Fatal("seed windows rejected")
	}
	if c.usageRefreshDue(a.ID, cfg, now.Add(time.Minute)) || c.claimPoll(a.ID, now.Add(time.Minute), 95, time.Minute) {
		t.Fatal("future-reset shared exhaustion should suppress refresh")
	}
	// Once shared windows are healthy, model-only exhaustion does not veto due polling.
	healthy := quotaWindowBatch{
		FiveHour:    quotaWindow{Percent: 10, ResetAt: now.Add(time.Hour), SampledAt: now.Add(2 * time.Minute), Valid: true},
		Weekly:      quotaWindow{Percent: 20, ResetAt: now.Add(2 * time.Hour), SampledAt: now.Add(2 * time.Minute), Valid: true},
		FableWeekly: quotaWindow{Percent: 99, ResetAt: now.Add(time.Hour), SampledAt: now.Add(2 * time.Minute), Valid: true},
	}
	if !c.commitWindowBatch(a, g, inc, healthy, now.Add(2*time.Minute), time.Time{}, false) {
		t.Fatal("healthy shared update rejected")
	}
	if !c.usageRefreshDue(a.ID, cfg, now.Add(3*time.Minute)) || !c.claimPoll(a.ID, now.Add(3*time.Minute), 95, time.Minute) {
		t.Fatal("model-only exhaustion vetoed normal refresh")
	}
	if c.claimPoll(a.ID, now.Add(3*time.Minute+time.Second), 95, time.Minute) {
		t.Fatal("repeated claim was not throttled")
	}
}

func TestWindowRefreshNoResetRequestRecovery(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	clock := &testClock{value: now}
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{physicalEntry("auth-a", "index-a")}, authJSON: map[string]json.RawMessage{"index-a": credentialJSON("token")}}
	var calls atomic.Int32
	completed := make(chan struct{}, 1)
	r := newPluginRuntime(host, func(context.Context, string, time.Duration) (usageResult, string) {
		if calls.Add(1) == 1 {
			return usageResult{FiveHourPercentUsed: 99, Windows: quotaWindowBatch{FiveHour: quotaWindow{Percent: 99, Source: quotaSourceUsage, Valid: true}}}, ""
		}
		select {
		case completed <- struct{}{}:
		default:
		}
		return usageResult{FiveHourPercentUsed: 10, Windows: quotaWindowBatch{FiveHour: quotaWindow{Percent: 10, Source: quotaSourceUsage, Valid: true}}}, ""
	}, clock.now)
	cfg := defaultPluginConfig()
	cfg.PollInterval = time.Minute
	cfg.OverageFallbackEnabled = false
	r.applyConfig(cfg)
	t.Cleanup(r.shutdown)
	waitFor(t, func() bool {
		s := r.cache.snapshot("auth-a")
		r.refreshMu.Lock()
		idle := len(r.inFlightIDs) == 0 && !r.inFlightAll && len(r.pendingIDs) == 0
		r.refreshMu.Unlock()
		return s.Windows.FiveHour.Valid && s.FiveHourPercentUsed == 99 && idle
	})
	if got, err := r.pick(claudeRequest(candidate("auth-a", 1))); got.Handled || err == nil {
		t.Fatalf("confirmed high usage selection = %+v, error=%v; want blocked error", got, err)
	}
	clock.set(now.Add(2 * time.Minute))
	response := r.interceptBeforeAuth(pluginapi.RequestInterceptRequest{Model: testModel})
	if response.Terminate {
		t.Fatal("unknown-reset sample fabricated an exhausted fleet response")
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("request-driven due refresh did not complete")
	}
	waitFor(t, func() bool {
		s := r.cache.snapshot("auth-a")
		return s.Windows.FiveHour.Valid && s.FiveHourPercentUsed == 10
	})
	if calls.Load() != 2 {
		t.Fatalf("fetch calls=%d, want 2", calls.Load())
	}
	if got, _ := r.pick(claudeRequest(candidate("auth-a", 1))); !got.Handled || got.AuthID != "auth-a" {
		t.Fatalf("low refreshed sample did not restore selection: %+v", got)
	}
}
