package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type rawPluginConfig struct {
	Enabled                *bool     `yaml:"enabled"`
	ProtectedModels        *[]string `yaml:"protected-models"`
	CutoffPercentUsed      *float64  `yaml:"cutoff-percent-used"`
	PollInterval           string    `yaml:"poll-interval"`
	RequestTimeout         string    `yaml:"request-timeout"`
	UserAgent              string    `yaml:"user-agent"`
	OverageFallbackEnabled *bool     `yaml:"overage-fallback-enabled"`
}

type pluginConfig struct {
	Enabled                bool
	ProtectedModels        []string
	CutoffPercentUsed      float64
	PollInterval           time.Duration
	RequestTimeout         time.Duration
	UserAgent              string
	OverageFallbackEnabled bool
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	Scheduler              bool `json:"scheduler"`
	RequestInterceptor     bool `json:"request_interceptor"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
	ManagementAPI          bool `json:"management_api"`
}

type managementRegistrationResponse struct {
	Routes []managementRoute `json:"routes"`
}

type managementRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:                true,
		ProtectedModels:        nil,
		CutoffPercentUsed:      defaultCutoffPercentUsed,
		PollInterval:           defaultPollInterval,
		RequestTimeout:         defaultRequestTimeout,
		UserAgent:              defaultAnthropicUserAgent,
		OverageFallbackEnabled: true,
	}
}

func decodeLifecycleConfig(raw []byte) (pluginConfig, error) {
	var request lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return pluginConfig{}, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	cfg := defaultPluginConfig()
	if len(request.ConfigYAML) == 0 {
		return cfg, nil
	}
	var decoded rawPluginConfig
	if err := yaml.Unmarshal(request.ConfigYAML, &decoded); err != nil {
		return pluginConfig{}, fmt.Errorf("decode plugin config: %w", err)
	}
	if decoded.Enabled != nil {
		cfg.Enabled = *decoded.Enabled
	}
	if decoded.ProtectedModels != nil {
		protectedModels, err := normalizeProtectedModels(*decoded.ProtectedModels)
		if err != nil {
			return pluginConfig{}, err
		}
		cfg.ProtectedModels = protectedModels
	}
	if decoded.CutoffPercentUsed != nil {
		cfg.CutoffPercentUsed = *decoded.CutoffPercentUsed
	}
	if value := strings.TrimSpace(decoded.PollInterval); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil {
			return pluginConfig{}, fmt.Errorf("poll-interval must be a Go duration")
		}
		cfg.PollInterval = interval
	}
	if value := strings.TrimSpace(decoded.RequestTimeout); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return pluginConfig{}, fmt.Errorf("request-timeout must be a Go duration")
		}
		cfg.RequestTimeout = timeout
	}
	if value := strings.TrimSpace(decoded.UserAgent); value != "" {
		cfg.UserAgent = value
	}
	if decoded.OverageFallbackEnabled != nil {
		cfg.OverageFallbackEnabled = *decoded.OverageFallbackEnabled
	}
	if math.IsNaN(cfg.CutoffPercentUsed) || math.IsInf(cfg.CutoffPercentUsed, 0) || cfg.CutoffPercentUsed < 0 || cfg.CutoffPercentUsed > 100 {
		return pluginConfig{}, fmt.Errorf("cutoff-percent-used must be between 0 and 100")
	}
	if cfg.PollInterval <= 0 {
		return pluginConfig{}, fmt.Errorf("poll-interval must be greater than zero")
	}
	if cfg.RequestTimeout <= 0 {
		return pluginConfig{}, fmt.Errorf("request-timeout must be greater than zero")
	}
	return cfg, nil
}

func normalizeProtectedModels(models []string) ([]string, error) {
	normalized := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			return nil, fmt.Errorf("protected-models must not contain empty model names")
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		normalized = append(normalized, model)
	}
	sort.Strings(normalized)
	if len(normalized) == 0 {
		return nil, nil
	}
	return normalized, nil
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{Routes: []managementRoute{{
		Method: http.MethodGet,
		Path:   managementStatusRoute,
	}}}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "kurbezz (fork of Smarty Pants Inc cpa-plugin-quota-router v0.5.0)",
			GitHubRepository: "https://github.com/kurbezz/five-hour-quota-router",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "protected-models",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Exact Claude model IDs whose routing is paused at the five-hour cutoff. Default: empty, which protects ALL Claude models.",
				},
				{
					Name:        "cutoff-percent-used",
					Type:        pluginapi.ConfigFieldTypeNumber,
					Description: "Excludes a Claude auth from scheduling when its five_hour utilization (falling back to limits[] where kind=session) reaches this percent used. Default: 95.",
				},
				{
					Name:        "poll-interval",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Minimum cached-usage age before a protected-model request queues another refresh. Default: 60s.",
				},
				{
					Name:        "request-timeout",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Anthropic usage request timeout as a Go duration. Default: 10s.",
				},
				{
					Name:        "user-agent",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "User-Agent sent with requests to Anthropic's undocumented /api/oauth/usage endpoint. That endpoint's rate-limit bucketing is keyed by User-Agent, and requests not matching Claude Code's own client string are aggressively and persistently throttled. Default: claude-code/2.1.80. Requires a plugin process restart to take effect.",
				},
				{
					Name:        "overage-fallback-enabled",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "When every Claude credential is confirmed over the five-hour cutoff (not merely unknown/unreachable), route to the confirmed-over-cutoff credential with the highest CPA priority instead of blocking the request, accepting Anthropic Extra Usage/overage billing on that credential. Default: true. Set to false to restore hard-blocking (five_hour_quota_exhausted) once every account is exhausted.",
				},
			},
		},
		Capabilities: registrationCapabilities{
			Scheduler:              true,
			RequestInterceptor:     true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
			ManagementAPI:          true,
		},
	}
}
