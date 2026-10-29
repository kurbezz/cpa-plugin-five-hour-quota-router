package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func quotaReq(key string) pluginapi.ManagementRequest {
	req := pluginapi.ManagementRequest{Method: http.MethodGet, Path: quotaResourceFullPath, Headers: http.Header{}}
	if key != "" {
		req.Headers["authorization"] = []string{"Bearer " + key}
	}
	return req
}

func codexEntry(index string) pluginapi.HostAuthFileEntry {
	return pluginapi.HostAuthFileEntry{ID: index, AuthIndex: index, Provider: "codex", Email: index + "@example.com", Label: "lbl-" + index}
}

type codexFixture struct {
	rt     *pluginRuntime
	clock  *testClock
	hits   atomic.Int32
	status atomic.Int32
	retry  string
	seen   atomic.Value // http.Header
}

func newCodexFixture(t *testing.T, entries ...pluginapi.HostAuthFileEntry) *codexFixture {
	t.Helper()
	f := &codexFixture{clock: &testClock{value: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}}
	f.status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.seen.Store(r.Header.Clone())
		if s := int(f.status.Load()); s != 200 {
			if f.retry != "" {
				w.Header().Set("Retry-After", f.retry)
			}
			w.WriteHeader(s)
			return
		}
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":12}}}`))
	}))
	t.Cleanup(srv.Close)
	old := codexUsageEndpoint
	codexUsageEndpoint = srv.URL
	t.Cleanup(func() { codexUsageEndpoint = old })

	jwt := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-jwt"}}`)) + ".y"
	host := &fakeHost{entries: entries, authJSON: map[string]json.RawMessage{}}
	for _, e := range entries {
		host.authJSON[e.AuthIndex] = json.RawMessage(`{"access_token":"tok-` + e.AuthIndex + `","id_token":"` + jwt + `"}`)
	}
	host.authJSON["notoken"] = json.RawMessage(`{}`)
	f.rt = newPluginRuntime(host, (&fakeFetcher{}).fetch, f.clock.now)
	f.rt.jitter = nil
	cfg := defaultPluginConfig()
	cfg.QuotaAPIKeys = []string{"k1", "k2"}
	f.rt.config.Store(&cfg)
	return f
}

func decodeQuota(t *testing.T, resp pluginapi.ManagementResponse) quotaRouteResponse {
	t.Helper()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, resp.Body)
	}
	var out quotaRouteResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQuotaResourceRegistrationAndConfig(t *testing.T) {
	reg := managementRegistration()
	if len(reg.Resources) != 1 || reg.Resources[0].Path != "/quota" {
		t.Fatalf("resources = %#v", reg.Resources)
	}
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte("quota-api-keys: [' a ', '', b]\n")})
	cfg, err := decodeLifecycleConfig(raw)
	if err != nil || len(cfg.QuotaAPIKeys) != 2 || cfg.QuotaAPIKeys[0] != "a" || cfg.QuotaAPIKeys[1] != "b" {
		t.Fatalf("cfg = %#v err=%v", cfg.QuotaAPIKeys, err)
	}
	found := false
	for _, f := range pluginRegistration().Metadata.ConfigFields {
		if f.Name == "quota-api-keys" && f.Type == pluginapi.ConfigFieldTypeArray {
			found = true
		}
	}
	if !found {
		t.Fatal("quota-api-keys config field missing")
	}
}

func TestQuotaResourceNotFoundWithoutKeys(t *testing.T) {
	f := newCodexFixture(t)
	cfg := defaultPluginConfig()
	f.rt.config.Store(&cfg)
	if resp := f.rt.handleManagement(quotaReq("k1")); resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestQuotaResourceUnauthorized(t *testing.T) {
	f := newCodexFixture(t, codexEntry("c1"))
	for _, key := range []string{"", "wrong"} {
		resp := f.rt.handleManagement(quotaReq(key))
		if resp.StatusCode != 401 || string(resp.Body) != `{"error":"unauthorized"}` {
			t.Fatalf("key %q: %d %s", key, resp.StatusCode, resp.Body)
		}
	}
	if f.hits.Load() != 0 {
		t.Fatal("upstream must not be called when unauthorized")
	}
}

func TestQuotaResourceSuccessAndCache(t *testing.T) {
	disabled := codexEntry("c2")
	disabled.Disabled = true
	f := newCodexFixture(t, codexEntry("c1"), disabled, physicalEntry("claude-a", "idx-a"))
	out := decodeQuota(t, f.rt.handleManagement(quotaReq("k2")))
	if len(out.Codex) != 1 || out.Codex[0].AuthIndex != "c1" || out.Codex[0].Email != "c1@example.com" || out.Codex[0].Label != "lbl-c1" || out.Codex[0].Error != "" || out.Codex[0].FetchedAt == "" {
		t.Fatalf("codex = %#v", out.Codex)
	}
	var usage map[string]any
	if json.Unmarshal(out.Codex[0].Usage, &usage) != nil || usage["plan_type"] != "plus" {
		t.Fatalf("usage = %s", out.Codex[0].Usage)
	}
	var claude cutoffStatusResponse
	if err := json.Unmarshal(out.Claude, &claude); err != nil || !claude.Enabled {
		t.Fatalf("claude = %s", out.Claude)
	}
	h := f.seen.Load().(http.Header)
	if h.Get("Authorization") != "Bearer tok-c1" || h.Get("Chatgpt-Account-Id") != "acct-jwt" || h.Get("User-Agent") != codexUserAgent || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", h)
	}
	decodeQuota(t, f.rt.handleManagement(quotaReq("k1")))
	if f.hits.Load() != 1 {
		t.Fatalf("hits = %d, want cached", f.hits.Load())
	}
	f.clock.set(f.clock.now().Add(codexUsageCacheTTL + time.Second))
	decodeQuota(t, f.rt.handleManagement(quotaReq("k1")))
	if f.hits.Load() != 2 {
		t.Fatalf("hits = %d, want refetch after TTL", f.hits.Load())
	}
}

func TestQuotaResourceMissingToken(t *testing.T) {
	f := newCodexFixture(t, codexEntry("notoken"))
	out := decodeQuota(t, f.rt.handleManagement(quotaReq("k1")))
	if len(out.Codex) != 1 || out.Codex[0].Error != pollErrorMissingToken || len(out.Codex[0].Usage) != 0 || f.hits.Load() != 0 {
		t.Fatalf("codex = %#v hits=%d", out.Codex, f.hits.Load())
	}
}

func TestQuotaResourceBackoffKeepsLastGood(t *testing.T) {
	f := newCodexFixture(t, codexEntry("c1"))
	decodeQuota(t, f.rt.handleManagement(quotaReq("k1")))
	f.clock.set(f.clock.now().Add(codexUsageCacheTTL + time.Second))
	f.status.Store(500)
	out := decodeQuota(t, f.rt.handleManagement(quotaReq("k1")))
	c := out.Codex[0]
	if c.Error == "" || len(c.Usage) == 0 || c.RetryAfterMS != refreshBackoffBase.Milliseconds() || f.hits.Load() != 2 {
		t.Fatalf("codex = %#v hits=%d", c, f.hits.Load())
	}
	// Paused: no new request within backoff.
	f.clock.set(f.clock.now().Add(time.Minute))
	decodeQuota(t, f.rt.handleManagement(quotaReq("k1")))
	if f.hits.Load() != 2 {
		t.Fatalf("hits = %d during backoff", f.hits.Load())
	}
	// After backoff: retry, second failure doubles.
	f.clock.set(f.clock.now().Add(refreshBackoffBase))
	out = decodeQuota(t, f.rt.handleManagement(quotaReq("k1")))
	if f.hits.Load() != 3 || out.Codex[0].RetryAfterMS != (2*refreshBackoffBase).Milliseconds() {
		t.Fatalf("hits=%d codex=%#v", f.hits.Load(), out.Codex[0])
	}
}

func TestQuotaResourceBackoffHonorsRetryAfter(t *testing.T) {
	f := newCodexFixture(t, codexEntry("c1"))
	f.status.Store(429)
	f.retry = "3600"
	out := decodeQuota(t, f.rt.handleManagement(quotaReq("k1")))
	if got := out.Codex[0].RetryAfterMS; got != time.Hour.Milliseconds() || len(out.Codex[0].Usage) != 0 {
		t.Fatalf("codex = %#v", out.Codex[0])
	}
}
