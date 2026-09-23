package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostClient interface {
	listAuth() ([]pluginapi.HostAuthFileEntry, error)
	getAuth(authIndex string) (json.RawMessage, error)
	log(level, message string, fields map[string]any)
}

// interceptBeforeAuth provides the narrow, fail-open HTTP admission gate used
// before an upstream credential is selected. The request-interceptor SDK shape
// intentionally has no Provider field at this lifecycle point, so Claude is
// identified from the model name for the default all-Claude configuration; an
// explicitly configured protected model remains an exact match.
func (r *pluginRuntime) interceptBeforeAuth(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	cfg := r.loadedConfig()
	if !cfg.Enabled || cfg.OverageFallbackEnabled || !isBeforeAuthProtectedClaudeRequest(req, cfg.ProtectedModels) || r.host == nil {
		return pluginapi.RequestInterceptResponse{}
	}

	// Membership discovery is the only synchronous host operation here. A
	// failure must not turn an uncertain credential set into a broad HTTP gate.
	entries, err := r.host.listAuth()
	if err != nil {
		r.log("warn", "five-hour quota router before-auth membership discovery failed", map[string]any{"category": "auth_list"})
		return pluginapi.RequestInterceptResponse{}
	}
	auths := physicalClaudeAuths(entries)
	r.cache.reconcile(auths)
	authIDs := make([]string, 0, len(auths))
	for _, auth := range auths {
		authIDs = append(authIDs, auth.ID)
	}
	now := r.now()
	if !r.cache.allConfirmedExhausted(authIDs, now, cfg.CutoffPercentUsed) {
		return pluginapi.RequestInterceptResponse{}
	}

	resetAt, hasReset := r.cache.earliestFutureReset(authIDs, now)
	body, err := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: exhaustedErrorCode, Message: exhaustedErrorMessage(now, resetAt, hasReset)})
	if err != nil {
		// This body is composed exclusively from fixed code and safe reset metadata,
		// but preserve admission fail-open behavior if marshaling ever changes.
		return pluginapi.RequestInterceptResponse{}
	}
	response := pluginapi.RequestInterceptResponse{
		Terminate:  true,
		StatusCode: http.StatusTooManyRequests,
		ResponseHeaders: http.Header{
			"Content-Type": []string{"application/json; charset=utf-8"},
		},
		ResponseBody: body,
	}
	if hasReset && now.Before(resetAt) {
		retryAfterSeconds := int64(math.Ceil(resetAt.Sub(now).Seconds()))
		if retryAfterSeconds < 1 {
			retryAfterSeconds = 1
		}
		response.ResponseHeaders.Set("Retry-After", strconv.FormatInt(retryAfterSeconds, 10))
	}
	return response
}

func isBeforeAuthProtectedClaudeRequest(req pluginapi.RequestInterceptRequest, protectedModels []string) bool {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(req.RequestedModel)
	}
	if !isProtectedModel(model, protectedModels) {
		return false
	}
	if len(protectedModels) > 0 {
		return true
	}
	return strings.HasPrefix(strings.ToLower(model), "claude")
}

type claudeCredential struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
}

type pluginRuntime struct {
	lifecycleMu sync.Mutex
	refreshMu   sync.Mutex
	config      atomic.Pointer[pluginConfig]
	cache       quotaCache
	host        hostClient
	fetch       usageFetcher
	now         func() time.Time
	wake        chan struct{}
	cancel      context.CancelFunc
	done        chan struct{}
	pendingAll  bool
	pendingIDs  map[string]struct{}
	inFlightAll bool
	inFlightIDs map[string]struct{}
}

func newPluginRuntime(host hostClient, fetch usageFetcher, now func() time.Time) *pluginRuntime {
	if now == nil {
		now = time.Now
	}
	runtime := &pluginRuntime{
		cache: quotaCache{samples: make(map[string]quotaSample)},
		host:  host,
		fetch: fetch,
		now:   now,
	}
	cfg := defaultPluginConfig()
	runtime.config.Store(&cfg)
	return runtime
}

func (r *pluginRuntime) applyConfig(cfg pluginConfig) {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	previous := r.loadedConfig()
	r.config.Store(&cfg)
	if !cfg.Enabled {
		r.stopLocked()
		r.cache.reconcile(nil)
		return
	}
	if r.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		wake := make(chan struct{}, 1)
		done := make(chan struct{})
		r.wake, r.cancel, r.done = wake, cancel, done
		go r.refreshLoop(ctx, wake, done)
		r.queueAllRefreshLocked()
		r.log("info", "five-hour quota router refresh worker started", map[string]any{
			"cutoff_percent_used": cfg.CutoffPercentUsed,
			"protected_models":    cfg.ProtectedModels,
			"minimum_refresh_age": cfg.PollInterval.String(),
			"request_timeout":     cfg.RequestTimeout.String(),
		})
		return
	}
	if r.cache.empty() || previous.CutoffPercentUsed != cfg.CutoffPercentUsed {
		r.queueAllRefreshLocked()
	}
	r.log("info", "five-hour quota router configuration reloaded", map[string]any{
		"cutoff_percent_used": cfg.CutoffPercentUsed,
		"protected_models":    cfg.ProtectedModels,
		"minimum_refresh_age": cfg.PollInterval.String(),
		"request_timeout":     cfg.RequestTimeout.String(),
	})
}

func (r *pluginRuntime) shutdown() {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.stopLocked()
}

func (r *pluginRuntime) stopLocked() {
	if r.cancel == nil {
		return
	}
	cancel, done := r.cancel, r.done
	cancel()
	if done != nil {
		<-done
	}
	r.wake, r.cancel, r.done = nil, nil, nil
	r.refreshMu.Lock()
	r.pendingAll, r.inFlightAll = false, false
	clear(r.pendingIDs)
	clear(r.inFlightIDs)
	r.refreshMu.Unlock()
	r.log("info", "five-hour quota router refresh worker stopped", nil)
}

func (r *pluginRuntime) loadedConfig() pluginConfig {
	if r == nil {
		return defaultPluginConfig()
	}
	if cfg := r.config.Load(); cfg != nil {
		return *cfg
	}
	return defaultPluginConfig()
}

func (r *pluginRuntime) queueAllRefreshLocked() {
	if r.wake == nil {
		return
	}
	r.refreshMu.Lock()
	r.pendingAll = true
	clear(r.pendingIDs)
	wake := r.wake
	r.refreshMu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (r *pluginRuntime) queueCandidateRefresh(authID string, cfg pluginConfig, now time.Time) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.wake == nil || r.cancel == nil || !r.loadedConfig().Enabled {
		return
	}
	r.refreshMu.Lock()
	if r.pendingAll || r.inFlightAll {
		r.refreshMu.Unlock()
		return
	}
	if _, exists := r.pendingIDs[authID]; exists {
		r.refreshMu.Unlock()
		return
	}
	if _, exists := r.inFlightIDs[authID]; exists {
		r.refreshMu.Unlock()
		return
	}
	if !r.cache.claimRefresh(authID, now, cfg.CutoffPercentUsed, cfg.PollInterval) {
		r.refreshMu.Unlock()
		return
	}
	if r.pendingIDs == nil {
		r.pendingIDs = make(map[string]struct{})
	}
	r.pendingIDs[authID] = struct{}{}
	wake := r.wake
	r.refreshMu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (r *pluginRuntime) refreshLoop(ctx context.Context, wake <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
		for {
			all, authIDs := r.takePendingRefresh()
			if !all && len(authIDs) == 0 {
				break
			}
			cfg := r.loadedConfig()
			if cfg.Enabled {
				r.refreshAuths(ctx, cfg, all, authIDs)
			}
			r.finishRefresh(all, authIDs)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func (r *pluginRuntime) takePendingRefresh() (bool, map[string]struct{}) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if r.pendingAll {
		r.pendingAll = false
		clear(r.pendingIDs)
		r.inFlightAll = true
		return true, nil
	}
	if len(r.pendingIDs) == 0 {
		return false, nil
	}
	authIDs := r.pendingIDs
	r.pendingIDs = make(map[string]struct{})
	if r.inFlightIDs == nil {
		r.inFlightIDs = make(map[string]struct{}, len(authIDs))
	}
	for authID := range authIDs {
		r.inFlightIDs[authID] = struct{}{}
	}
	return false, authIDs
}

func (r *pluginRuntime) finishRefresh(all bool, authIDs map[string]struct{}) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if all {
		r.inFlightAll = false
		return
	}
	for authID := range authIDs {
		delete(r.inFlightIDs, authID)
	}
}

func (r *pluginRuntime) pollOnce(ctx context.Context, cfg pluginConfig) {
	r.refreshAuths(ctx, cfg, true, nil)
}

func (r *pluginRuntime) refreshAuths(ctx context.Context, cfg pluginConfig, all bool, authIDs map[string]struct{}) {
	if r == nil || r.host == nil || r.fetch == nil || ctx.Err() != nil {
		return
	}
	entries, err := r.host.listAuth()
	if err != nil {
		r.log("warn", "five-hour quota router auth discovery failed", map[string]any{"category": "auth_list"})
		return
	}
	auths := physicalClaudeAuths(entries)
	r.cache.reconcile(auths)
	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		if !all {
			if _, selected := authIDs[auth.ID]; !selected {
				continue
			}
		}
		r.pollAuth(ctx, auth, cfg)
	}
}

func (r *pluginRuntime) pollAuth(ctx context.Context, auth physicalClaudeAuth, cfg pluginConfig) {
	r.cache.recordAttempt(auth.ID, r.now())
	rawAuth, err := r.host.getAuth(auth.AuthIndex)
	if err != nil {
		r.recordPollFailure(auth.ID, pollErrorAuthGet)
		return
	}
	var credential claudeCredential
	if json.Unmarshal(rawAuth, &credential) != nil {
		r.recordPollFailure(auth.ID, pollErrorAuthGet)
		return
	}
	token := strings.TrimSpace(credential.AccessToken)
	if !strings.EqualFold(strings.TrimSpace(credential.Type), "claude") || token == "" {
		r.recordPollFailure(auth.ID, pollErrorMissingToken)
		return
	}
	result, category := r.fetch(ctx, token, cfg.RequestTimeout)
	if category != "" {
		if category != pollErrorCancelled || ctx.Err() == nil {
			r.recordPollFailure(auth.ID, category)
		}
		return
	}
	r.cache.recordSuccess(auth.ID, result.FiveHourPercentUsed, result.ResetAt, r.now())
	r.log("debug", "five-hour quota router quota refreshed", map[string]any{
		"auth_id":                auth.ID,
		"five_hour_percent_used": result.FiveHourPercentUsed,
		"blocked":                result.FiveHourPercentUsed >= cfg.CutoffPercentUsed,
	})
}

func (r *pluginRuntime) recordPollFailure(authID, category string) {
	r.cache.recordFailure(authID, category)
	r.log("warn", "five-hour quota router quota refresh failed", map[string]any{
		"auth_id":  authID,
		"category": category,
	})
}

func (r *pluginRuntime) log(level, message string, fields map[string]any) {
	if r != nil && r.host != nil {
		r.host.log(level, message, fields)
	}
}
