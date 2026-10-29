package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	quotaResourceRoute    = "/quota"
	quotaResourceFullPath = "/v0/resource/plugins/" + pluginName + quotaResourceRoute

	defaultCodexUsageEndpoint = "https://chatgpt.com/backend-api/wham/usage"
	codexUserAgent            = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
	codexUsageCacheTTL        = 60 * time.Second
	codexFetchConcurrency     = 4
	maxCodexUsageBytes        = 256 << 10
)

// Variable so tests can point it at an httptest server.
var codexUsageEndpoint = defaultCodexUsageEndpoint

type codexAccountQuota struct {
	AuthIndex    string          `json:"auth_index"`
	Email        string          `json:"email,omitempty"`
	Label        string          `json:"label,omitempty"`
	Usage        json.RawMessage `json:"usage,omitempty"`
	Error        string          `json:"error,omitempty"`
	RetryAfterMS int64           `json:"retry_after_ms,omitempty"`
	FetchedAt    string          `json:"fetched_at,omitempty"`
}

type quotaRouteResponse struct {
	Claude json.RawMessage     `json:"claude"`
	Codex  []codexAccountQuota `json:"codex"`
}

// codexState holds per-auth cache/backoff state. mu is held for the whole
// refresh so concurrent route calls for one auth never stampede upstream.
type codexState struct {
	mu        sync.Mutex
	usage     json.RawMessage
	fetchedAt time.Time
	failures  int
	nextAt    time.Time
	lastErr   string
}

type codexStates struct {
	mu     sync.Mutex
	states map[string]*codexState
}

func (s *codexStates) get(authIndex string) *codexState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = make(map[string]*codexState)
	}
	st := s.states[authIndex]
	if st == nil {
		st = &codexState{}
		s.states[authIndex] = st
	}
	return st
}

func (s *codexStates) prune(keep map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.states {
		if _, ok := keep[k]; !ok {
			delete(s.states, k)
		}
	}
}

func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func headerValue(h http.Header, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// quotaKeyAuthorized compares the bearer token against every configured key in
// constant time (per key, over fixed-length digests).
func quotaKeyAuthorized(h http.Header, keys []string) bool {
	value := strings.TrimSpace(headerValue(h, "Authorization"))
	if len(value) < 7 || !strings.EqualFold(value[:7], "bearer ") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(value[7:])))
	match := 0
	for _, key := range keys {
		want := sha256.Sum256([]byte(key))
		match |= subtle.ConstantTimeCompare(got[:], want[:])
	}
	return match == 1
}

func (r *pluginRuntime) statusBody(cfg pluginConfig) []byte {
	body, _ := json.Marshal(cutoffStatusResponse{
		Enabled:           cfg.Enabled,
		CutoffPercentUsed: cfg.CutoffPercentUsed,
		ProtectedModels:   cfg.ProtectedModels,
		Accounts:          r.cache.statuses(r.now(), cfg.CutoffPercentUsed),
	})
	return body
}

func (r *pluginRuntime) handleQuotaResource(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := r.loadedConfig()
	if len(cfg.QuotaAPIKeys) == 0 {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
	if !quotaKeyAuthorized(req.Headers, cfg.QuotaAPIKeys) {
		return jsonResponse(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
	return jsonResponse(http.StatusOK, quotaRouteResponse{
		Claude: r.statusBody(cfg),
		Codex:  r.codexQuotas(cfg),
	})
}

func (r *pluginRuntime) codexQuotas(cfg pluginConfig) []codexAccountQuota {
	out := []codexAccountQuota{}
	entries, err := r.host.listAuth()
	if err != nil {
		return out
	}
	var accounts []pluginapi.HostAuthFileEntry
	keep := map[string]struct{}{}
	for _, e := range entries {
		if e.Disabled || e.AuthIndex == "" {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(e.Provider), "codex") && !strings.EqualFold(strings.TrimSpace(e.Type), "codex") {
			continue
		}
		accounts = append(accounts, e)
		keep[e.AuthIndex] = struct{}{}
	}
	r.codex.prune(keep)
	out = make([]codexAccountQuota, len(accounts))
	sem := make(chan struct{}, codexFetchConcurrency)
	var wg sync.WaitGroup
	for i, acct := range accounts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = r.codexAccountQuota(acct, cfg)
		}()
	}
	wg.Wait()
	return out
}

type codexCredential struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	IDToken     string `json:"id_token"`
}

func chatgptAccountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return strings.TrimSpace(claims.Auth.AccountID)
}

func (r *pluginRuntime) codexAccountQuota(acct pluginapi.HostAuthFileEntry, cfg pluginConfig) codexAccountQuota {
	st := r.codex.get(acct.AuthIndex)
	st.mu.Lock()
	defer st.mu.Unlock()

	now := r.now()
	if st.lastErr == "" && len(st.usage) > 0 && now.Sub(st.fetchedAt) < codexUsageCacheTTL {
		return codexView(acct, st, now)
	}
	if now.Before(st.nextAt) {
		return codexView(acct, st, now)
	}

	rawAuth, err := r.host.getAuth(acct.AuthIndex)
	var cred codexCredential
	if err != nil || json.Unmarshal(rawAuth, &cred) != nil {
		st.lastErr = pollErrorAuthGet
		return codexView(acct, st, now)
	}
	token := strings.TrimSpace(cred.AccessToken)
	if token == "" {
		st.lastErr = pollErrorMissingToken
		return codexView(acct, st, now)
	}
	accountID := strings.TrimSpace(cred.AccountID)
	if accountID == "" {
		accountID = chatgptAccountIDFromJWT(cred.IDToken)
	}

	body, category, failure := fetchCodexUsage(context.Background(), token, accountID, cfg.RequestTimeout)
	now = r.now()
	if category != "" {
		st.failures++
		delay := refreshBackoffDelay(st.failures, category, failure)
		if r.jitter != nil && delay > 0 {
			delay += r.jitter(delay)
		}
		if !failure.HasRetryAfter && delay > refreshBackoffMax {
			delay = refreshBackoffMax
		}
		st.nextAt = now.Add(delay)
		st.lastErr = describeRefreshFailure(category, failure)
		return codexView(acct, st, now)
	}
	st.usage, st.fetchedAt = body, now
	st.failures, st.nextAt, st.lastErr = 0, time.Time{}, ""
	return codexView(acct, st, now)
}

func codexView(acct pluginapi.HostAuthFileEntry, st *codexState, now time.Time) codexAccountQuota {
	view := codexAccountQuota{
		AuthIndex: acct.AuthIndex,
		Email:     acct.Email,
		Label:     acct.Label,
		Error:     st.lastErr,
	}
	if len(st.usage) > 0 {
		view.Usage = st.usage
		view.FetchedAt = st.fetchedAt.UTC().Format(time.RFC3339)
	}
	if st.lastErr != "" && now.Before(st.nextAt) {
		view.RetryAfterMS = st.nextAt.Sub(now).Milliseconds()
	}
	return view
}

func fetchCodexUsage(ctx context.Context, token, accountID string, timeout time.Duration) (json.RawMessage, string, usageFailure) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageEndpoint, nil)
	if err != nil {
		return nil, pollErrorHTTP, usageFailure{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", codexUserAgent)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return nil, pollErrorCancelled, usageFailure{}
		case errors.Is(err, context.DeadlineExceeded):
			return nil, pollErrorTimeout, usageFailure{}
		}
		return nil, pollErrorNetwork, usageFailure{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		failure := usageFailure{StatusCode: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		failure.RetryAfter, failure.HasRetryAfter = upstreamRetryDelay(resp.Header, time.Now())
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, pollErrorUnauthorized, failure
		case resp.StatusCode == http.StatusForbidden:
			return nil, pollErrorForbidden, failure
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, pollErrorRateLimited, failure
		case resp.StatusCode >= 500:
			return nil, pollErrorServer, failure
		}
		return nil, pollErrorHTTP, failure
	}
	ok := usageFailure{StatusCode: resp.StatusCode}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCodexUsageBytes+1))
	if err != nil {
		return nil, pollErrorRead, ok
	}
	if len(data) > maxCodexUsageBytes {
		return nil, pollErrorBodyTooLarge, ok
	}
	var parsed json.RawMessage
	if json.Unmarshal(data, &parsed) != nil {
		return nil, pollErrorInvalidJSON, ok
	}
	compact, _ := json.Marshal(parsed)
	return compact, "", usageFailure{}
}
