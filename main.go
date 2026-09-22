package main

import "time"

const (
	pluginName                = "five-hour-quota-router"
	defaultCutoffPercentUsed  = 95.0
	defaultPollInterval       = 60 * time.Second
	defaultRequestTimeout     = 10 * time.Second
	defaultUsageEndpoint      = "https://api.anthropic.com/api/oauth/usage"
	defaultAnthropicUserAgent = "claude-code/2.1.80"
	anthropicOAuthBeta        = "oauth-2025-04-20"
	maxUsageResponseBytes     = 64 << 10
	exhaustedErrorCode        = "five_hour_quota_exhausted"
	managementStatusRoute     = "/plugins/five-hour-quota-router/status"
	managementStatusFullPath  = "/v0/management" + managementStatusRoute
	pollErrorAuthGet          = "auth_get"
	pollErrorMissingToken     = "missing_token"
	pollErrorInvalidUsage     = "invalid_usage"
	pollErrorInvalidJSON      = "invalid_json"
	pollErrorBodyTooLarge     = "body_too_large"
	pollErrorTimeout          = "timeout"
	pollErrorCancelled        = "cancelled"
	pollErrorNetwork          = "network"
	pollErrorUnauthorized     = "unauthorized"
	pollErrorForbidden        = "forbidden"
	pollErrorRateLimited      = "rate_limited"
	pollErrorServer           = "server_error"
	pollErrorHTTP             = "http_error"
	pollErrorRead             = "read_error"
)

var pluginVersion = "0.1.0"

// Variable only so the C-shared integration test can inject a local fixture.
var usageEndpoint = defaultUsageEndpoint

// activeRuntime is constructed at package-init time, before the first
// plugin.register/plugin.reconfigure call loads YAML config, so the fetcher's
// User-Agent is fixed to defaultAnthropicUserAgent here. Changing the
// `user-agent` config field therefore requires a plugin process restart to
// take effect (the fetcher closure captured at init time is not rebuilt on
// reconfigure).
var activeRuntime = newPluginRuntime(
	cgoHostClient{},
	newHTTPUsageFetcher(usageEndpoint, nil, defaultAnthropicUserAgent).fetch,
	time.Now,
)

func main() {}
