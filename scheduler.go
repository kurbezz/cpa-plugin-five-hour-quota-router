package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// candidateWeight reads the host's "weight" attribute (sdk/cliproxy/auth
// AttributeWeight), which CLIProxyAPI's own weighted-round-robin strategy
// also reads from the same auth JSON field. It is only consulted here as a
// tie-break for the overage-fallback candidate (which credential absorbs
// billable Extra Usage once every candidate has confirmed-exhausted its free
// five-hour quota), never for normal selection among available candidates.
// Missing, empty, or invalid values default to 1 so operators who never set
// a weight see no behavior change (every candidate ties at 1, and the
// existing lowest-ID tie-break still decides).
func candidateWeight(candidate *pluginapi.SchedulerAuthCandidate) int64 {
	if candidate == nil || candidate.Attributes == nil {
		return 1
	}
	raw := strings.TrimSpace(candidate.Attributes["weight"])
	if raw == "" {
		return 1
	}
	weight, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || weight < 0 {
		return 1
	}
	return weight
}

type physicalClaudeAuth struct {
	ID        string
	AuthIndex string
	Name      string
	Identity  string
	Revision  string
}

func physicalClaudeAuths(entries []pluginapi.HostAuthFileEntry) []physicalClaudeAuth {
	auths := make([]physicalClaudeAuth, 0, len(entries))
	for _, entry := range entries {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(entry.Type))
		}
		if provider != "claude" || entry.Disabled || strings.EqualFold(strings.TrimSpace(entry.Status), "disabled") || entry.RuntimeOnly || strings.TrimSpace(entry.Path) == "" {
			continue
		}
		id := strings.TrimSpace(entry.ID)
		if id == "" || id != entry.ID || strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		auths = append(auths, physicalClaudeAuth{
			ID:        entry.ID,
			AuthIndex: entry.AuthIndex,
			Name:      strings.TrimSpace(entry.Name),
			Identity:  physicalAuthIdentity(entry),
		})
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	return auths
}

func physicalAuthIdentity(entry pluginapi.HostAuthFileEntry) string {
	// File metadata intentionally is not identity: CPA can update an auth file
	// after an ordinary request without replacing its credential. A metadata-only
	// change must not erase a confirmed quota sample and bypass before-auth
	// admission.
	return fmt.Sprintf("index:%q|path:%q|account:%q|email:%q",
		entry.AuthIndex,
		entry.Path,
		strings.TrimSpace(entry.Account),
		strings.ToLower(strings.TrimSpace(entry.Email)),
	)
}

func (r *pluginRuntime) pick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, *envelopeError) {
	cfg := r.loadedConfig()
	if !cfg.Enabled || !isClaudeRequest(req) || !isProtectedModel(req.Model, cfg.ProtectedModels) {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	now := r.now()
	var selected *pluginapi.SchedulerAuthCandidate
	var fallbackCandidate *pluginapi.SchedulerAuthCandidate
	claudeCandidates, blockedCandidates, confirmedOverCutoffCount := 0, 0, 0
	claudeCandidateIDs := make([]string, 0, len(req.Candidates))
	for i := range req.Candidates {
		candidate := &req.Candidates[i]
		provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if provider != "" && provider != "claude" {
			continue
		}
		if candidate.ID == "" || strings.TrimSpace(candidate.ID) != candidate.ID {
			continue
		}
		claudeCandidates++
		claudeCandidateIDs = append(claudeCandidateIDs, candidate.ID)
		// Track the preferred overage-fallback candidate across ALL claude
		// candidates unconditionally, so it's available regardless of which
		// branch executes below. Tie-break order: highest Priority first
		// (matches normal selection and the host's own tiering), then
		// highest "weight" attribute (lets an operator pick which credential
		// absorbs billable Extra Usage among same-priority candidates once
		// every free five-hour quota is confirmed exhausted), then lowest ID
		// for determinism when priority and weight both tie.
		if fallbackCandidate == nil ||
			candidate.Priority > fallbackCandidate.Priority ||
			(candidate.Priority == fallbackCandidate.Priority && candidateWeight(candidate) > candidateWeight(fallbackCandidate)) ||
			(candidate.Priority == fallbackCandidate.Priority && candidateWeight(candidate) == candidateWeight(fallbackCandidate) && candidate.ID < fallbackCandidate.ID) {
			fallbackCandidate = candidate
		}
		if r.cache.isExcluded(candidate.ID, now, cfg.CutoffPercentUsed) {
			blockedCandidates++
			if r.cache.isBlocked(candidate.ID, now, cfg.CutoffPercentUsed) {
				confirmedOverCutoffCount++
			}
			continue
		}
		// Same tie-break order as the fallback candidate above: Priority,
		// then "weight" (so operators can prefer one free/under-cutoff
		// credential over another same-priority sibling before either one
		// is ever exhausted), then lowest ID.
		if selected == nil ||
			candidate.Priority > selected.Priority ||
			(candidate.Priority == selected.Priority && candidateWeight(candidate) > candidateWeight(selected)) ||
			(candidate.Priority == selected.Priority && candidateWeight(candidate) == candidateWeight(selected) && candidate.ID < selected.ID) {
			selected = candidate
		}
	}
	if selected != nil {
		r.queueCandidateRefresh(selected.ID, cfg, now)
		return pluginapi.SchedulerPickResponse{AuthID: selected.ID, Handled: true}, nil
	}
	if claudeCandidates > 0 && blockedCandidates == claudeCandidates {
		// Only fall back to overage billing when every excluded candidate is
		// CONFIRMED over cutoff (isBlocked), never when any candidate is merely
		// unknown/unreachable (isExcluded but not isBlocked).
		if cfg.OverageFallbackEnabled && confirmedOverCutoffCount == claudeCandidates && fallbackCandidate != nil {
			r.log("warn", "five-hour quota router routing to confirmed over-cutoff credential (overage fallback)", map[string]any{
				"auth_id":  fallbackCandidate.ID,
				"priority": fallbackCandidate.Priority,
			})
			r.queueCandidateRefresh(fallbackCandidate.ID, cfg, now)
			return pluginapi.SchedulerPickResponse{AuthID: fallbackCandidate.ID, Handled: true}, nil
		}
		// This is the scheduler-level backstop for state that changes after
		// before-auth admission. Keep the safe reset-derived retry metadata on
		// this error when it is known; the scheduler ABI itself cannot emit an
		// HTTP Retry-After header.
		resetAt, hasReset := r.cache.earliestFutureReset(claudeCandidateIDs, now)
		return pluginapi.SchedulerPickResponse{}, &envelopeError{Code: exhaustedErrorCode, Message: exhaustedErrorMessage(now, resetAt, hasReset)}
	}
	return pluginapi.SchedulerPickResponse{Handled: false}, nil
}

func isClaudeRequest(req pluginapi.SchedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(req.Provider), "claude") {
		return true
	}
	if strings.TrimSpace(req.Provider) != "" || len(req.Providers) != 1 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Providers[0]), "claude")
}

func isProtectedModel(model string, protectedModels []string) bool {
	model = strings.TrimSpace(model)
	if len(protectedModels) == 0 {
		return model != ""
	}
	if model == "" {
		return false
	}
	for _, protectedModel := range protectedModels {
		if strings.EqualFold(model, protectedModel) {
			return true
		}
	}
	return false
}

func (r *pluginRuntime) handleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodGet) || strings.TrimRight(strings.TrimSpace(req.Path), "/") != managementStatusFullPath {
		body, _ := json.Marshal(map[string]string{"error": "not found"})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       body,
		}
	}
	cfg := r.loadedConfig()
	body, _ := json.Marshal(cutoffStatusResponse{
		Enabled:           cfg.Enabled,
		CutoffPercentUsed: cfg.CutoffPercentUsed,
		ProtectedModels:   cfg.ProtectedModels,
		Accounts:          r.cache.statuses(r.now(), cfg.CutoffPercentUsed),
	})
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}
