package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
// identified from the model name. This gate must never apply to a non-Claude
// request, even if an operator accidentally lists that model as protected.
func (r *pluginRuntime) interceptBeforeAuth(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	cfg := r.loadedConfig()
	if !cfg.Enabled || cfg.OverageFallbackEnabled || !isBeforeAuthProtectedClaudeRequest(req, cfg.ProtectedModels) || r.host == nil {
		return pluginapi.RequestInterceptResponse{}
	}

	// Membership discovery is the only synchronous host operation here. A
	// failure must not turn an uncertain credential set into a broad HTTP gate.
	auths, replaced, changed, current, err := r.discoverAuths()
	if err != nil {
		r.log("warn", "five-hour quota router before-auth membership discovery failed", map[string]any{"category": "auth_list"})
		return pluginapi.RequestInterceptResponse{}
	}
	// A list response that began before a newer completed response must not
	// prune that newer membership, and must not schedule work from the stale
	// response (the newer, current discovery already owns that signal).
	if current {
		for _, auth := range auths {
			// Ordinary unknowns retain normal per-ID throttle behavior. A replaced
			// identity needs a full pass that remains pending behind an old in-flight
			// poll sharing the same ID.
			if !r.cache.snapshot(auth.ID).HasSample {
				if _, wasReplaced := replaced[auth.ID]; wasReplaced {
					r.queueRevisionCheck(auth.ID)
				} else {
					r.queueCandidateRefresh(auth.ID, cfg, r.now())
				}
			}
			if _, listMetadataChanged := changed[auth.ID]; listMetadataChanged {
				// List metadata is merely a hint. Check the credential revision off the
				// request path, including while its old quota sample is blocked.
				r.queueRevisionCheck(auth.ID)
			}
		}
	}
	now := r.now()
	// Exhaustion is always validated against every credential CURRENTLY in the
	// cache under one lock, never against this call's own (possibly stale)
	// discovered ID list. A newer, concurrently completed discovery may have
	// already reconciled an available or unknown alternative into the cache
	// after this call captured its own membership; checking that stale list
	// here could confirm "exhaustion" against membership the cache no longer
	// holds.
	resetAt, hasReset := r.cache.confirmedFleetExhaustedReset(now, cfg.CutoffPercentUsed)
	if !hasReset {
		return pluginapi.RequestInterceptResponse{}
	}
	return r.exhaustedInterceptResponse(now, resetAt, hasReset)
}

func (r *pluginRuntime) exhaustedInterceptResponse(now time.Time, resetAt time.Time, hasReset bool) pluginapi.RequestInterceptResponse {
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
	if now.Before(resetAt) {
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
	if !isClaudeModelName(model) || !isProtectedModel(model, protectedModels) {
		return false
	}
	return true
}

// isClaudeModelName identifies a Claude model from its name, the same
// detection used for the before-auth admission gate. Header/stream
// observation reuses it directly: quota sampling (like ordinary usage
// polling) is not scoped to protected-models, only the admission gate is.
func isClaudeModelName(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude")
}

// selectedAuthIDFromMetadata reads the host-published selected-credential ID.
// It ignores anything that is not exactly a non-empty string: metadata is a
// best-effort snapshot and must never be trusted beyond its documented shape.
func selectedAuthIDFromMetadata(metadata map[string]any) (string, bool) {
	if len(metadata) == 0 {
		return "", false
	}
	raw, ok := metadata["selected_auth_id"]
	if !ok {
		return "", false
	}
	id, ok := raw.(string)
	id = strings.TrimSpace(id)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// observeResponseHeaders is the shared, observe-only implementation behind
// both the non-streaming response interceptor and the streaming header-init
// interceptor call. It never performs host or network I/O, never blocks, and
// silently ignores anything it cannot safely act on.
func (r *pluginRuntime) observeResponseHeaders(model, requestID string, headers http.Header, observedAt time.Time) {
	if r == nil || !isClaudeModelName(model) {
		return
	}
	correlation, ok := r.selectedAuthForRequest(requestID)
	if !ok {
		return
	}
	observation := parseClaudeFiveHourHeaders(headers, observedAt)
	if !observation.Valid {
		return
	}
	if !r.cache.recordHeaderObservation(correlation.authID, correlation.generation, correlation.incarnation, r.loadedConfig().CutoffPercentUsed, observation) {
		return
	}
	r.consumeSelectedAuth(requestID, correlation)
}

// interceptResponse observes real successful non-streaming upstream Claude
// responses to keep the five-hour quota cache current for active accounts
// without waiting for the next usage-API poll. It is strictly observe-only:
// it always returns an empty response (no header/body rewriting) and never
// performs host or network I/O.
func (r *pluginRuntime) interceptResponse(req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	r.observeResponseHeaders(req.Model, req.RequestID, req.ResponseHeaders, r.now())
	return pluginapi.ResponseInterceptResponse{}
}

// interceptStreamChunk observes the header-only stream initialization call
// (ChunkIndex == StreamChunkHeaderInitIndex) the same way as a non-streaming
// response. Every payload chunk (ChunkIndex >= 0) is an immediate no-op: the
// rate-limit headers are only meaningful once, at stream start, and per-chunk
// work here would scale with response size for no benefit.
func (r *pluginRuntime) interceptStreamChunk(req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		r.observeResponseHeaders(req.Model, req.RequestID, req.ResponseHeaders, r.now())
	}
	return pluginapi.StreamChunkInterceptResponse{}
}

type claudeCredential struct {
	Type             string `json:"type"`
	AccessToken      string `json:"access_token"`
	AccountUUID      string `json:"account_uuid"`
	OrganizationID   string `json:"organization_id"`
	OrganizationUUID string `json:"organization_uuid"`
}

type pluginRuntime struct {
	lifecycleMu sync.Mutex
	refreshMu   sync.Mutex
	// discoveryMu orders completed host.auth.list snapshots. Calls remain
	// concurrent, but a snapshot issued before a newer completed one is never
	// allowed to reconcile and delete its newer membership.
	discoveryMu         sync.Mutex
	discoveryIssued     uint64
	discoveryReconciled uint64
	config              atomic.Pointer[pluginConfig]
	cache               quotaCache
	host                hostClient
	fetch               usageFetcher
	now                 func() time.Time
	wake                chan struct{}
	cancel              context.CancelFunc
	done                chan struct{}
	pendingAll          bool
	pendingIDs          map[string]struct{}
	pendingRevisionIDs  map[string]struct{}
	// deferredRevisionIDs is owned as lifecycle state under refreshMu. Delayed
	// revision retries are promoted by refreshLoop's timer, never by detached
	// goroutines, so an old runtime cannot enqueue into a restarted one.
	deferredRevisionIDs map[string]time.Time
	inFlightAll         bool
	inFlightIDs         map[string]struct{}
	selectedAuthMu      sync.Mutex
	selectedAuths       map[string]selectedAuthCorrelation
}

const (
	selectedAuthCorrelationTTL  = 10 * time.Minute
	maxSelectedAuthCorrelations = 4096
)

type selectedAuthCorrelation struct {
	authID      string
	generation  uint64
	incarnation uint64
	expiresAt   time.Time
}

// discoverAuths reconciles a host.auth.list snapshot unless a later-issued
// discovery has already completed. This rejects out-of-order list responses
// while retaining concurrent host calls on the request and worker paths.
func (r *pluginRuntime) discoverAuths() ([]physicalClaudeAuth, map[string]struct{}, map[string]struct{}, bool, error) {
	r.discoveryMu.Lock()
	r.discoveryIssued++
	generation := r.discoveryIssued
	r.discoveryMu.Unlock()

	entries, err := r.host.listAuth()
	if err != nil {
		return nil, nil, nil, false, err
	}
	auths := physicalClaudeAuths(entries)

	r.discoveryMu.Lock()
	defer r.discoveryMu.Unlock()
	if generation < r.discoveryReconciled {
		return auths, nil, nil, false, nil
	}
	replaced, changed := r.cache.reconcile(auths)
	r.discoveryReconciled = generation
	return auths, replaced, changed, true, nil
}

// recordSelectedAuth records only the non-secret selected auth ID correlated
// with a request ID. CPA v7.3.8 copies execution metadata before auth choice,
// so response hooks cannot rely on their Metadata carrying this value; the
// after-auth request hook is the authoritative correlation point.
func (r *pluginRuntime) recordSelectedAuth(requestID string, metadata map[string]any) {
	if r == nil || strings.TrimSpace(requestID) == "" {
		return
	}
	// Each after-auth callback supersedes prior intent, including an unsafe
	// retry that cannot be correlated to a current cache member.
	r.selectedAuthMu.Lock()
	if r.selectedAuths != nil {
		delete(r.selectedAuths, requestID)
	}
	r.selectedAuthMu.Unlock()
	authID, ok := selectedAuthIDFromMetadata(metadata)
	if !ok {
		return
	}
	sample := r.cache.snapshot(authID)
	if sample.Identity == "" {
		return
	}
	now := r.now()
	r.selectedAuthMu.Lock()
	defer r.selectedAuthMu.Unlock()
	if r.selectedAuths == nil {
		r.selectedAuths = make(map[string]selectedAuthCorrelation)
	}
	for id, entry := range r.selectedAuths {
		if !now.Before(entry.expiresAt) {
			delete(r.selectedAuths, id)
		}
	}
	if len(r.selectedAuths) >= maxSelectedAuthCorrelations {
		var oldestID string
		var oldest time.Time
		for id, entry := range r.selectedAuths {
			if oldestID == "" || entry.expiresAt.Before(oldest) || (entry.expiresAt.Equal(oldest) && id < oldestID) {
				oldestID, oldest = id, entry.expiresAt
			}
		}
		delete(r.selectedAuths, oldestID)
	}
	r.selectedAuths[requestID] = selectedAuthCorrelation{authID: authID, generation: sample.ObservedGeneration, incarnation: sample.Incarnation, expiresAt: now.Add(selectedAuthCorrelationTTL)}
}

func (r *pluginRuntime) selectedAuthForRequest(requestID string) (selectedAuthCorrelation, bool) {
	if r == nil || strings.TrimSpace(requestID) == "" {
		return selectedAuthCorrelation{}, false
	}
	now := r.now()
	r.selectedAuthMu.Lock()
	defer r.selectedAuthMu.Unlock()
	entry, ok := r.selectedAuths[requestID]
	if !ok || !now.Before(entry.expiresAt) {
		delete(r.selectedAuths, requestID)
		return selectedAuthCorrelation{}, false
	}
	return entry, true
}

func (r *pluginRuntime) consumeSelectedAuth(requestID string, expected selectedAuthCorrelation) {
	r.selectedAuthMu.Lock()
	defer r.selectedAuthMu.Unlock()
	if current, ok := r.selectedAuths[requestID]; ok && current == expected {
		delete(r.selectedAuths, requestID)
	}
}

// requeueStaleDiscovery restores refresh/revision intent that a worker pass
// already dequeued from the pending queue before discovering that its own
// listAuth call was superseded by a newer, faster-completing discovery. It
// merges that intent back onto the same worker-owned pending queue used by
// queueCandidateRefresh/queueRevisionCheck (union with anything queued in the
// meantime) so refreshLoop's own inner retry loop picks it back up against a
// subsequent, necessarily-current discovery -- never dropping it, and never
// widening it into an unthrottled fleet poll or a detached goroutine.
func (r *pluginRuntime) requeueStaleDiscovery(all bool, authIDs, revisionIDs map[string]struct{}) {
	r.refreshMu.Lock()
	if all {
		r.pendingAll = true
		clear(r.pendingIDs)
	} else {
		if len(authIDs) > 0 {
			if r.pendingIDs == nil {
				r.pendingIDs = make(map[string]struct{}, len(authIDs))
			}
			for authID := range authIDs {
				r.pendingIDs[authID] = struct{}{}
			}
		}
		if len(revisionIDs) > 0 {
			if r.pendingRevisionIDs == nil {
				r.pendingRevisionIDs = make(map[string]struct{}, len(revisionIDs))
			}
			for authID := range revisionIDs {
				r.pendingRevisionIDs[authID] = struct{}{}
			}
		}
	}
	wake := r.wake
	r.refreshMu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
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
		_, _ = r.cache.reconcile(nil)
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
	clear(r.pendingRevisionIDs)
	clear(r.deferredRevisionIDs)
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

func (r *pluginRuntime) queueAllRefresh() {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.wake == nil || r.cancel == nil || !r.loadedConfig().Enabled {
		return
	}
	r.queueAllRefreshLocked()
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

// queueRevisionCheck records detection durably per ID. Throttling occurs when
// the worker executes the check, so a metadata signal received inside the
// throttle window is not lost.
func (r *pluginRuntime) queueRevisionCheck(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.wake == nil || r.cancel == nil || !r.loadedConfig().Enabled {
		return
	}
	r.queueRevisionCheckLocked(authID)
}

// queueRevisionCheckFromWorker is for refresh-loop work that has already
// passed lifecycle validation. It must not acquire lifecycleMu: stopLocked
// holds that mutex while waiting for the worker to exit, and a discovery pass
// may find changed metadata immediately before it observes cancellation.
func (r *pluginRuntime) queueRevisionCheckFromWorker(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	r.queueRevisionCheckLocked(authID)
}

// queueRevisionCheckLocked enqueues under refreshMu. Its caller either holds
// lifecycleMu (the external path) or is the active refresh worker.
func (r *pluginRuntime) queueRevisionCheckLocked(authID string) {
	r.refreshMu.Lock()
	if r.pendingRevisionIDs == nil {
		r.pendingRevisionIDs = make(map[string]struct{})
	}
	r.pendingRevisionIDs[authID] = struct{}{}
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
		delay, hasDeferred := r.nextDeferredRevisionDelay()
		var timer *time.Timer
		var timerC <-chan time.Time
		if hasDeferred {
			timer = time.NewTimer(delay)
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-wake:
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
		for {
			all, authIDs, revisionIDs := r.takePendingRefresh()
			if !all && len(authIDs) == 0 && len(revisionIDs) == 0 {
				break
			}
			cfg := r.loadedConfig()
			if cfg.Enabled {
				r.refreshAuths(ctx, cfg, all, authIDs, revisionIDs)
			}
			r.finishRefresh(all, authIDs)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func (r *pluginRuntime) takePendingRefresh() (bool, map[string]struct{}, map[string]struct{}) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	now := r.now()
	for authID, due := range r.deferredRevisionIDs {
		if !now.Before(due) {
			if r.pendingRevisionIDs == nil {
				r.pendingRevisionIDs = make(map[string]struct{})
			}
			r.pendingRevisionIDs[authID] = struct{}{}
			delete(r.deferredRevisionIDs, authID)
		}
	}
	if r.pendingAll {
		r.pendingAll = false
		clear(r.pendingIDs)
		r.inFlightAll = true
		return true, nil, nil
	}
	if len(r.pendingIDs) == 0 && len(r.pendingRevisionIDs) == 0 {
		return false, nil, nil
	}
	authIDs := r.pendingIDs
	r.pendingIDs = make(map[string]struct{})
	revisionIDs := r.pendingRevisionIDs
	r.pendingRevisionIDs = make(map[string]struct{})
	if r.inFlightIDs == nil {
		r.inFlightIDs = make(map[string]struct{}, len(authIDs))
	}
	for authID := range authIDs {
		r.inFlightIDs[authID] = struct{}{}
	}
	return false, authIDs, revisionIDs
}

func (r *pluginRuntime) nextDeferredRevisionDelay() (time.Duration, bool) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	var earliest time.Time
	for _, due := range r.deferredRevisionIDs {
		if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
	}
	if earliest.IsZero() {
		return 0, false
	}
	delay := earliest.Sub(r.now())
	if delay < 0 {
		delay = 0
	}
	return delay, true
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
	r.refreshAuths(ctx, cfg, true, nil, nil)
}

func (r *pluginRuntime) refreshAuths(ctx context.Context, cfg pluginConfig, all bool, authIDs, revisionIDs map[string]struct{}) {
	if r == nil || r.host == nil || r.fetch == nil || ctx.Err() != nil {
		return
	}
	auths, replaced, changed, current, err := r.discoverAuths()
	if err != nil {
		r.log("warn", "five-hour quota router auth discovery failed", map[string]any{"category": "auth_list"})
		for authID := range revisionIDs {
			r.retryRevisionCheck(ctx, authID)
		}
		return
	}
	if !current {
		// This pass's own listAuth call was superseded by a newer, faster-
		// completing discovery (interceptor or another worker pass) before it
		// could reconcile. The refresh/revision intent already dequeued by
		// takePendingRefresh for this pass must not be silently dropped: put
		// it back on the worker-owned pending queue so the same refreshLoop
		// iteration retries it against a fresh (necessarily current, since
		// discoveryIssued only grows) discovery, without introducing a
		// detached goroutine or a fleet-wide unthrottled poll.
		r.requeueStaleDiscovery(all, authIDs, revisionIDs)
		return
	}
	// A targeted worker pass still receives the complete auth listing. Preserve
	// metadata-change signals for every listed credential; only the selected ID
	// is polled in this pass, while other IDs receive their own later revision
	// check instead of silently losing replacement detection.
	for authID := range replaced {
		r.queueRevisionCheckFromWorker(authID)
	}
	for authID := range changed {
		r.queueRevisionCheckFromWorker(authID)
	}
	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		if !all {
			_, selected := authIDs[auth.ID]
			_, revisionCheck := revisionIDs[auth.ID]
			if !selected && !revisionCheck {
				continue
			}
			if revisionCheck && !selected {
				r.checkAuthRevision(ctx, auth, cfg)
				continue
			}
			if selected && revisionCheck {
				// The usage claim is already eligible, but preserve the independent
				// revision signal if auth.get transiently fails.
				r.pollAuthWithRevisionIntent(ctx, auth, cfg, false, true)
				continue
			}
		}
		r.pollAuth(ctx, auth, cfg, false)
	}
}

// retryRevisionCheck keeps a detected replacement pending after a transient
// discovery/read failure. It is per-ID, bounded and cancellation-aware; it
// never turns metadata handling into a fleet poll.
func (r *pluginRuntime) retryRevisionCheck(ctx context.Context, authID string) {
	if ctx.Err() != nil {
		return
	}
	r.deferRevisionCheckFromWorker(authID, r.now().Add(25*time.Millisecond))
}

func (r *pluginRuntime) deferRevisionCheckFromWorker(authID string, due time.Time) {
	r.refreshMu.Lock()
	if r.deferredRevisionIDs == nil {
		r.deferredRevisionIDs = make(map[string]time.Time)
	}
	if old, exists := r.deferredRevisionIDs[authID]; !exists || due.Before(old) {
		r.deferredRevisionIDs[authID] = due
	}
	wake := r.wake
	r.refreshMu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (r *pluginRuntime) checkAuthRevision(ctx context.Context, auth physicalClaudeAuth, cfg pluginConfig) {
	if !r.cache.claimRevisionCheck(auth.ID, r.now(), cfg.PollInterval) {
		// Preserve detection until the throttle window elapses without spinning
		// the worker or widening this per-ID check into a fleet refresh.
		delay := r.cache.revisionCheckDelay(auth.ID, r.now(), cfg.PollInterval)
		if ctx.Err() == nil {
			r.deferRevisionCheckFromWorker(auth.ID, r.now().Add(delay))
		}
		return
	}
	r.pollAuth(ctx, auth, cfg, true)
}

func (r *pluginRuntime) pollAuth(ctx context.Context, auth physicalClaudeAuth, cfg pluginConfig, revisionCheck bool) {
	r.pollAuthWithRevisionIntent(ctx, auth, cfg, revisionCheck, revisionCheck)
}

// pollAuthWithRevisionIntent separates revision-only usage eligibility from
// retry ownership. A selected usage refresh may overlap a revision signal: it
// should perform its eligible usage fetch, while retaining that signal if its
// credential read is transiently unavailable.
func (r *pluginRuntime) pollAuthWithRevisionIntent(ctx context.Context, auth physicalClaudeAuth, cfg pluginConfig, revisionCheck, revisionIntent bool) {
	// Captured before any credential/usage work so a concurrent, faster header
	// observation (from a real successful request) or a faster sibling poll
	// that commits in the meantime is never overwritten by this call's later,
	// now-stale usage result.
	pollStartedAt := r.now()
	generation, current := r.cache.observedGeneration(auth)
	if !current {
		return
	}
	rawAuth, err := r.host.getAuth(auth.AuthIndex)
	if err != nil {
		r.recordPollFailure(auth, pollErrorAuthGet)
		if revisionIntent {
			r.retryRevisionCheck(ctx, auth.ID)
		}
		return
	}
	var credential claudeCredential
	if json.Unmarshal(rawAuth, &credential) != nil {
		r.recordPollFailure(auth, pollErrorAuthGet)
		if revisionIntent {
			r.retryRevisionCheck(ctx, auth.ID)
		}
		return
	}
	token := strings.TrimSpace(credential.AccessToken)
	if !strings.EqualFold(strings.TrimSpace(credential.Type), "claude") || token == "" {
		r.recordPollFailure(auth, pollErrorMissingToken)
		return
	}
	revision := claudeCredentialRevision(credential)
	auth, ok := r.cache.bindRevision(auth, revision, generation)
	if !ok {
		return
	}
	if revisionCheck && !r.cache.completeRevisionCheck(auth, generation, r.now()) {
		return
	}
	if revisionCheck && !r.cache.shouldRefreshAfterRevisionCheck(auth.ID, r.now(), cfg.CutoffPercentUsed, cfg.PollInterval) {
		return
	}
	if !r.cache.recordAttemptForIdentity(auth, r.now()) {
		return
	}
	result, category := r.fetch(ctx, token, cfg.RequestTimeout)
	if category != "" {
		if category != pollErrorCancelled || ctx.Err() == nil {
			r.recordPollFailure(auth, category)
		}
		return
	}
	if !r.cache.recordSuccessForGeneration(auth, generation, result.FiveHourPercentUsed, result.ResetAt, r.now(), pollStartedAt) {
		return
	}
	r.log("debug", "five-hour quota router quota refreshed", map[string]any{
		"auth_id":                auth.ID,
		"five_hour_percent_used": result.FiveHourPercentUsed,
		"blocked":                result.FiveHourPercentUsed >= cfg.CutoffPercentUsed,
	})
}

// claudeCredentialRevision identifies a credential without retaining its raw
// JSON or token. It is used only inside the refresh worker's cache path.
func claudeCredentialRevision(credential claudeCredential) string {
	input := strings.Join([]string{
		strings.TrimSpace(credential.AccessToken),
		strings.TrimSpace(credential.AccountUUID),
		strings.TrimSpace(credential.OrganizationID),
		strings.TrimSpace(credential.OrganizationUUID),
	}, "\x00")
	sum := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", sum[:])
}

func (r *pluginRuntime) recordPollFailure(auth physicalClaudeAuth, category string) {
	if !r.cache.recordFailureForIdentity(auth, category) {
		return
	}
	r.log("warn", "five-hour quota router quota refresh failed", map[string]any{
		"auth_id":  auth.ID,
		"category": category,
	})
}

func (r *pluginRuntime) log(level, message string, fields map[string]any) {
	if r != nil && r.host != nil {
		r.host.log(level, message, fields)
	}
}
