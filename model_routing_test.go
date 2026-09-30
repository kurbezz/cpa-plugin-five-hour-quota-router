package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestModelQuotaRouting(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	r := newTestRuntime(&fakeHost{}, nil, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	r.config.Store(&cfg)
	auths := []physicalClaudeAuth{{ID: "a", Identity: "a"}, {ID: "b", Identity: "b"}}
	r.cache.reconcile(auths)
	seed := func(id string, batch quotaWindowBatch) {
		a := physicalClaudeAuth{ID: id, Identity: id}
		g, inc, _ := r.cache.bindingSnapshot(a)
		if !r.cache.commitWindowBatch(a, g, inc, batch, now, time.Time{}, false) {
			t.Fatalf("seed %s", id)
		}
	}
	shared := quotaWindow{Percent: 20, ResetAt: now.Add(8 * time.Hour), SampledAt: now, Valid: true}
	fam := func(p float64) quotaWindow {
		return quotaWindow{Percent: p, ResetAt: now.Add(time.Hour), SampledAt: now, Valid: true}
	}
	seed("a", quotaWindowBatch{FiveHour: shared, Weekly: shared, FableWeekly: fam(99), OpusWeekly: fam(10), SonnetWeekly: fam(10)})
	seed("b", quotaWindowBatch{FiveHour: shared, Weekly: shared, FableWeekly: fam(20), OpusWeekly: fam(20), SonnetWeekly: fam(99)})
	for _, tc := range []struct{ model, want string }{{"claude-fable-5", "b"}, {"claude-sonnet-4", "a"}, {"claude-opus-4", "a"}} {
		got, err := r.pick(modelRequest(tc.model, candidate("a", 1), candidate("b", 1)))
		if err != nil || !got.Handled || got.AuthID != tc.want {
			t.Fatalf("model %s: got=%+v err=%v want=%s", tc.model, got, err, tc.want)
		}
	}
	unknown, err := r.pick(modelRequest("claude-custom-x", candidate("a", 1), candidate("b", 1)))
	if err != nil || !unknown.Handled || unknown.AuthID != "a" {
		t.Fatalf("unknown family should use shared windows: %+v %v", unknown, err)
	}
}

func TestModelQuotaAdmission(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	entry := physicalEntry("a", "index-a")
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{entry}, authJSON: map[string]json.RawMessage{"index-a": credentialJSON("synthetic-token")}}
	r := newTestRuntime(host, nil, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	r.config.Store(&cfg)
	a := physicalClaudeAuths(host.entries)[0]
	r.cache.reconcile([]physicalClaudeAuth{a})
	g, inc, _ := r.cache.bindingSnapshot(a)
	batch := quotaWindowBatch{FiveHour: quotaWindow{Percent: 96, ResetAt: now.Add(time.Hour), SampledAt: now, Valid: true}, Weekly: quotaWindow{Percent: 20, ResetAt: now.Add(20 * time.Hour), SampledAt: now, Valid: true}, FableWeekly: quotaWindow{Percent: 96, ResetAt: now.Add(3 * time.Hour), SampledAt: now, Valid: true}}
	r.cache.commitWindowBatch(a, g, inc, batch, now, time.Time{}, false)
	resp := r.interceptBeforeAuth(pluginapi.RequestInterceptRequest{Model: "claude-fable-5"})
	if !resp.Terminate || resp.StatusCode != 429 || resp.ResponseHeaders.Get("Retry-After") != "10800" {
		t.Fatalf("model admission response=%+v headers=%v", resp, resp.ResponseHeaders)
	}
	if _, gets := host.counts(); gets != 0 {
		t.Fatalf("synchronous auth.get calls=%d", gets)
	}
}

func TestModelQuotaAdmissionModelAuthorityAndRecovery(t *testing.T) {
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	entryA, entryB := physicalEntry("a", "index-a"), physicalEntry("b", "index-b")
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{entryA, entryB}, authJSON: map[string]json.RawMessage{}}
	r := newTestRuntime(host, nil, base)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	r.config.Store(&cfg)
	auths := physicalClaudeAuths(host.entries)
	r.cache.reconcile(auths)
	seed := func(a physicalClaudeAuth, b quotaWindowBatch) {
		g, inc, _ := r.cache.bindingSnapshot(a)
		if !r.cache.commitWindowBatch(a, g, inc, b, base, time.Time{}, false) {
			t.Fatalf("seed %s", a.ID)
		}
	}
	shared := quotaWindow{Percent: 20, ResetAt: base.Add(20 * time.Hour), SampledAt: base, Valid: true}
	seed(auths[0], quotaWindowBatch{FiveHour: quotaWindow{Percent: 99, ResetAt: base.Add(time.Hour), SampledAt: base, Valid: true}, Weekly: shared, FableWeekly: quotaWindow{Percent: 99, ResetAt: base.Add(8 * time.Hour), SampledAt: base, Valid: true}})
	seed(auths[1], quotaWindowBatch{FiveHour: shared, Weekly: shared, FableWeekly: quotaWindow{Percent: 99, ResetAt: base.Add(3*time.Hour + 500*time.Millisecond), SampledAt: base, Valid: true}})
	resp := r.interceptBeforeAuth(pluginapi.RequestInterceptRequest{Model: "claude-fable-5", RequestedModel: "claude-sonnet-4"})
	if !resp.Terminate || resp.ResponseHeaders.Get("Retry-After") != "10801" {
		t.Fatalf("authoritative Model / max-account recovery: %+v headers=%v", resp, resp.ResponseHeaders)
	}
	resp = r.interceptBeforeAuth(pluginapi.RequestInterceptRequest{RequestedModel: "claude-fable-5"})
	if !resp.Terminate {
		t.Fatal("RequestedModel fallback should resolve Fable and gate")
	}
	// A missing reset on any applicable blocker bars a finite retry/HTTP 429.
	seed(auths[1], quotaWindowBatch{FiveHour: shared, Weekly: shared, FableWeekly: quotaWindow{Percent: 99, SampledAt: base, Valid: true}})
	resp = r.interceptBeforeAuth(pluginapi.RequestInterceptRequest{Model: "claude-fable-5"})
	if resp.Terminate {
		t.Fatal("unknown blocking reset fabricated fleet 429")
	}
}

func TestModelQuotaSharedWeeklyAndOverageUnknown(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	r := newTestRuntime(&fakeHost{}, nil, now)
	cfg := defaultPluginConfig()
	cfg.OverageFallbackEnabled = false
	r.config.Store(&cfg)
	auths := []physicalClaudeAuth{{ID: "a", Identity: "a"}, {ID: "b", Identity: "b"}}
	r.cache.reconcile(auths)
	for i, a := range auths {
		g, inc, _ := r.cache.bindingSnapshot(a)
		p := float64(96 + i)
		batch := quotaWindowBatch{FiveHour: quotaWindow{Percent: 10, ResetAt: now.Add(time.Hour), SampledAt: now, Valid: true}, Weekly: quotaWindow{Percent: 99, ResetAt: now.Add(2 * time.Hour), SampledAt: now, Valid: true}}
		r.cache.commitWindowBatch(a, g, inc, batch, now, time.Time{}, false)
		_ = p
	}
	got, err := r.pick(modelRequest("claude-unknown", candidate("a", 1), candidate("b", 1)))
	if got.Handled || err == nil {
		t.Fatalf("shared weekly must block unknown family: %+v %v", got, err)
	}
	// Missing optional scoped window does not exclude when shared windows are healthy.
	a := auths[0]
	g, inc, _ := r.cache.bindingSnapshot(a)
	healthy := quotaWindowBatch{FiveHour: quotaWindow{Percent: 10, ResetAt: now.Add(time.Hour), SampledAt: now, Valid: true}, Weekly: quotaWindow{Percent: 10, ResetAt: now.Add(2 * time.Hour), SampledAt: now, Valid: true}}
	r.cache.commitWindowBatch(a, g, inc, healthy, now.Add(time.Second), time.Time{}, false)
	got, err = r.pick(modelRequest("claude-fable-5", candidate("a", 1)))
	if err != nil || !got.Handled {
		t.Fatalf("missing optional family window blocked: %+v %v", got, err)
	}
}

func modelRequest(model string, candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Provider: "claude", Providers: []string{"claude"}, Model: model, Candidates: candidates}
}
