//go:build !race

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	e2eAPIKey        = "e2e-api-key"
	e2eManagementKey = "e2e-management-key"
)

var e2eUsageHits atomic.Int64

const e2eProtectedModel = "claude-opus-4-1-20250805"

func TestCLIProxyAPIProcessEndToEnd(t *testing.T) {
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e2eUsageHits.Add(1)
		if r.Header.Get("anthropic-beta") != anthropicOAuthBeta {
			http.Error(w, "missing beta header", http.StatusBadRequest)
			return
		}
		utilization := 0
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "e2e-token-a":
			utilization = 96
		case "e2e-token-b":
			utilization = 99
		default:
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"five_hour":{"utilization":%d,"resets_at":"2099-01-01T00:00:00Z"}}`, utilization)
	}))
	defer usageServer.Close()

	proxyHits := make(chan string, 16)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case proxyHits <- r.Method + " " + r.Host:
		default:
		}
		http.Error(w, "fixture proxy rejects upstream", http.StatusBadGateway)
	}))
	defer proxyServer.Close()

	dir := t.TempDir()
	root := cliProxyAPIModuleDir(t)
	pluginDir, authDir := filepath.Join(dir, "plugins"), filepath.Join(dir, "auth")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}

	extension, serverName := ".so", "cliproxyapi"
	switch runtime.GOOS {
	case "darwin":
		extension = ".dylib"
	case "windows":
		extension, serverName = ".dll", "cliproxyapi.exe"
	}
	pluginPath := filepath.Join(pluginDir, pluginName+extension)
	buildPlugin := exec.Command("go", "build", "-buildmode=c-shared", "-ldflags", "-X=main.usageEndpoint="+usageServer.URL, "-o", pluginPath, ".")
	if output, errBuild := buildPlugin.CombinedOutput(); errBuild != nil {
		t.Fatalf("build plugin: %v\n%s", errBuild, output)
	}

	serverPath := filepath.Join(dir, serverName)
	buildServer := exec.Command("go", "build", "-o", serverPath, "./cmd/server")
	buildServer.Dir = root
	if output, errBuild := buildServer.CombinedOutput(); errBuild != nil {
		t.Fatalf("build CLIProxyAPI: %v\n%s", errBuild, output)
	}

	for i, fixture := range []struct {
		name  string
		token string
	}{
		{name: "claude-a.json", token: "e2e-token-a"},
		{name: "claude-b.json", token: "e2e-token-b"},
	} {
		body := fmt.Sprintf(`{"type":"claude","email":"e2e-%d@example.com","access_token":%q,"refresh_token":"fixture-refresh","expired":"2099-01-01T00:00:00Z"}`, i, fixture.token)
		if err := os.WriteFile(filepath.Join(authDir, fixture.name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	port := unusedTCPPort(t)
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := fmt.Sprintf(`host: "127.0.0.1"
port: %d
proxy-url: %q
auth-dir: %q
api-keys: [%q]
remote-management:
  allow-remote: false
  secret-key: %q
  disable-control-panel: true
  disable-auto-update-panel: true
logging-to-file: false
debug: false
disable-cooling: true
plugins:
  enabled: true
  dir: %q
  configs:
    five-hour-quota-router:
      enabled: true
      priority: 100
      protected-models: [%s]
      cutoff-percent-used: 95
      poll-interval: 50ms
      request-timeout: 1s
      overage-fallback-enabled: false
`, port, proxyServer.URL, authDir, e2eAPIKey, e2eManagementKey, pluginDir, e2eProtectedModel)
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(serverPath, "-config", configPath, "-local-model")
	server.Dir = root
	server.Stdout, server.Stderr = logFile, logFile
	server.Env = append(os.Environ(),
		"HOME="+filepath.Join(dir, "home"),
		"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
		"NO_PROXY=127.0.0.1,localhost",
	)
	if err := server.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	processDone := make(chan struct{})
	var processErr error
	go func() {
		processErr = server.Wait()
		close(processDone)
	}()
	t.Cleanup(func() {
		select {
		case <-processDone:
		default:
			_ = server.Process.Kill()
			<-processDone
		}
		_ = logFile.Close()
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}
	status := waitForProcessStatus(t, client, baseURL, processDone, &processErr, logPath, func(status cutoffStatusResponse) bool {
		if !status.Enabled || len(status.ProtectedModels) != 1 || status.ProtectedModels[0] != e2eProtectedModel || status.CutoffPercentUsed != 95 || len(status.Accounts) != 2 {
			return false
		}
		for _, account := range status.Accounts {
			if !account.Known || !account.Blocked {
				return false
			}
		}
		return true
	})
	if len(status.Accounts) != 2 {
		t.Fatalf("initial status = %#v", status)
	}
	startupUsageHits := e2eUsageHits.Load()
	time.Sleep(250 * time.Millisecond)
	if hits := e2eUsageHits.Load(); hits != startupUsageHits {
		t.Fatalf("idle worker refreshed usage: hits=%d, want %d", hits, startupUsageHits)
	}
	waitForProcessModel(t, client, baseURL, processDone, &processErr, logPath, "claude-sonnet-4-6")

	unprotectedResponse := postClaudeMessage(t, client, baseURL, "claude-sonnet-4-6")
	if bytes.Contains(unprotectedResponse, []byte(exhaustedErrorCode)) {
		t.Fatalf("unprotected request was cutoff-blocked: %s", unprotectedResponse)
	}
	select {
	case hit := <-proxyHits:
		if !strings.Contains(hit, "api.anthropic.com:443") {
			t.Fatalf("unexpected upstream proxy target: %s", hit)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("unprotected request never reached upstream proxy: %s\nserver log:\n%s", unprotectedResponse, readLog(logPath))
	}
drainProxyHits:
	for {
		select {
		case <-proxyHits:
			continue
		default:
			break drainProxyHits
		}
	}
	if hits := e2eUsageHits.Load(); hits != startupUsageHits {
		t.Fatalf("unprotected request refreshed usage: hits=%d, want %d", hits, startupUsageHits)
	}
	waitForProcessStatus(t, client, baseURL, processDone, &processErr, logPath, func(status cutoffStatusResponse) bool {
		if len(status.Accounts) != 2 {
			return false
		}
		for _, account := range status.Accounts {
			if !account.Known || !account.Blocked {
				return false
			}
		}
		return true
	})

	fixtureReset, err := time.Parse(time.RFC3339, "2099-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	requestStarted := time.Now()
	blockedStatus, blockedHeaders, blockedResponse := postClaudeMessageObserved(t, client, baseURL, e2eProtectedModel)
	requestFinished := time.Now()
	t.Logf("TASK4_OBSERVED protected_status=%d content_type=%q retry_after=%q body=%s", blockedStatus, blockedHeaders.Get("Content-Type"), blockedHeaders.Get("Retry-After"), blockedResponse)
	if blockedStatus != http.StatusTooManyRequests {
		t.Fatalf("protected request status=%d, want 429; body=%s\nserver log:\n%s", blockedStatus, blockedResponse, readLog(logPath))
	}
	contentType := blockedHeaders.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		t.Fatalf("protected Content-Type=%q parsed=%q err=%v, want application/json", contentType, mediaType, err)
	}
	retryAfter, err := strconv.ParseInt(blockedHeaders.Get("Retry-After"), 10, 64)
	if err != nil || retryAfter < 1 {
		t.Fatalf("protected Retry-After=%q parsed=%d err=%v, want positive seconds", blockedHeaders.Get("Retry-After"), retryAfter, err)
	}
	// The server rounds up using its request-time clock. Bound the header by
	// both sides of this request rather than comparing it to a later test clock.
	maxRemaining := int64(math.Ceil(fixtureReset.Sub(requestStarted).Seconds()))
	minRemaining := int64(math.Ceil(fixtureReset.Sub(requestFinished).Seconds()))
	if retryAfter < minRemaining || retryAfter > maxRemaining+1 {
		t.Fatalf("protected Retry-After=%d, reset bounds=[%d,%d]", retryAfter, minRemaining, maxRemaining+1)
	}
	var blockedBody struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(blockedResponse, &blockedBody); err != nil || blockedBody.Code != exhaustedErrorCode {
		t.Fatalf("protected JSON body=%s decoded=%#v err=%v", blockedResponse, blockedBody, err)
	}
	select {
	case hit := <-proxyHits:
		t.Fatalf("protected request reached upstream proxy: %s", hit)
	default:
		t.Log("TASK4_OBSERVED protected_upstream_fixture_hits=0")
	}
	// Before-auth membership reconciliation may discover a new or replaced
	// physical credential and queue its refresh asynchronously. The interceptor
	// itself still makes no synchronous auth.get or usage HTTP call.

	patch := []byte(`{"cutoff-percent-used":100}`)
	request, err := http.NewRequest(http.MethodPatch, baseURL+"/v0/management/plugins/"+pluginName+"/config", bytes.NewReader(patch))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+e2eManagementKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("patch plugin config: status=%d body=%s", response.StatusCode, responseBody)
	}

	waitForProcessStatus(t, client, baseURL, processDone, &processErr, logPath, func(status cutoffStatusResponse) bool {
		if status.CutoffPercentUsed != 100 || len(status.Accounts) != 2 {
			return false
		}
		for _, account := range status.Accounts {
			if !account.Known || account.Blocked {
				return false
			}
		}
		return true
	})

	eligibleResponse := postClaudeMessage(t, client, baseURL, e2eProtectedModel)
	if bytes.Contains(eligibleResponse, []byte(exhaustedErrorCode)) {
		t.Fatalf("eligible request remained cutoff-blocked: %s", eligibleResponse)
	}
	select {
	case hit := <-proxyHits:
		if !strings.Contains(hit, "api.anthropic.com:443") {
			t.Fatalf("unexpected upstream proxy target: %s", hit)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("eligible request never reached upstream proxy: %s\nserver log:\n%s", eligibleResponse, readLog(logPath))
	}
	waitFor(t, func() bool { return e2eUsageHits.Load() > startupUsageHits })
}

func cliProxyAPIModuleDir(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/router-for-me/CLIProxyAPI/v7")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve CLIProxyAPI module: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func unusedTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func waitForProcessStatus(t *testing.T, client *http.Client, baseURL string, processDone <-chan struct{}, processErr *error, logPath string, ready func(cutoffStatusResponse) bool) cutoffStatusResponse {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-processDone:
			t.Fatalf("CLIProxyAPI exited before readiness: %v\n%s", *processErr, readLog(logPath))
		default:
		}
		request, _ := http.NewRequest(http.MethodGet, baseURL+managementStatusFullPath, nil)
		request.Header.Set("Authorization", "Bearer "+e2eManagementKey)
		response, err := client.Do(request)
		if err == nil {
			var status cutoffStatusResponse
			body := readResponseBody(t, response)
			if response.StatusCode == http.StatusOK && json.Unmarshal(body, &status) == nil && ready(status) {
				return status
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("CLIProxyAPI status did not converge\n%s", readLog(logPath))
	return cutoffStatusResponse{}
}
func waitForProcessModel(t *testing.T, client *http.Client, baseURL string, processDone <-chan struct{}, processErr *error, logPath, model string) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-processDone:
			t.Fatalf("CLIProxyAPI exited before model registration: %v\n%s", *processErr, readLog(logPath))
		default:
		}
		request, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+e2eAPIKey)
		response, err := client.Do(request)
		if err == nil {
			body := readResponseBody(t, response)
			if response.StatusCode == http.StatusOK && bytes.Contains(body, []byte(model)) {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("CLIProxyAPI model %q was not registered\n%s", model, readLog(logPath))
}

func postClaudeMessage(t *testing.T, client *http.Client, baseURL, model string) []byte {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`, model))
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+e2eAPIKey)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return readResponseBody(t, response)
}

func postClaudeMessageObserved(t *testing.T, client *http.Client, baseURL, model string) (int, http.Header, []byte) {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`, model))
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+e2eAPIKey)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header.Clone(), readResponseBody(t, response)
}

func readResponseBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	var body bytes.Buffer
	if _, err := body.ReadFrom(response.Body); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func readLog(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}
