//go:build !race

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"mime"
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

func validateRetryTestModuleDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("directory is empty")
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("not a directory: %q", root)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	module := ""
	for _, line := range strings.Split(string(mod), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			module = strings.Trim(fields[1], `"`)
			break
		}
	}
	if module != "github.com/router-for-me/CLIProxyAPI/v7" && module != "github.com/router-for-me/CLIProxyAPI/v8" {
		return "", fmt.Errorf("unexpected module declaration %q", module)
	}
	info, err = os.Stat(filepath.Join(root, "cmd", "server"))
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("required cmd/server directory missing")
	}
	return root, nil
}

func retryTestModulePath(root string) (string, error) {
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(mod), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			module := strings.Trim(fields[1], `"`)
			if module == "github.com/router-for-me/CLIProxyAPI/v7" || module == "github.com/router-for-me/CLIProxyAPI/v8" {
				return module, nil
			}
			return "", fmt.Errorf("unexpected module declaration %q", module)
		}
	}
	return "", fmt.Errorf("module declaration missing")
}

func retryTestExpectHeader(module string) (bool, error) {
	if value, set := os.LookupEnv("CPA_RETRY_TEST_EXPECT_HEADER"); set {
		expect, err := strconv.ParseBool(value)
		if err != nil {
			return false, fmt.Errorf("CPA_RETRY_TEST_EXPECT_HEADER must be a boolean: %w", err)
		}
		return expect, nil
	}
	_, override := os.LookupEnv("CPA_RETRY_TEST_MODULE_DIR")
	if override && module == "github.com/router-for-me/CLIProxyAPI/v8" {
		return false, fmt.Errorf("v8 override requires explicit CPA_RETRY_TEST_EXPECT_HEADER=true or false")
	}
	// Preserve legacy v7 patched-override compatibility and stock-v7 default.
	return override, nil
}

func TestRetryTestModuleDirValidation(t *testing.T) {
	for _, tc := range []struct {
		name, module  string
		server, valid bool
	}{
		{name: "missing go.mod"},
		{name: "wrong module", module: "example.test/wrong", server: true},
		{name: "missing server", module: "github.com/router-for-me/CLIProxyAPI/v7"},
		{name: "valid", module: "github.com/router-for-me/CLIProxyAPI/v7", server: true, valid: true},
		{name: "valid v8", module: "github.com/router-for-me/CLIProxyAPI/v8", server: true, valid: true},
		{name: "unsupported v9", module: "github.com/router-for-me/CLIProxyAPI/v9", server: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.module != "" {
				if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+tc.module+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.server {
				if err := os.MkdirAll(filepath.Join(dir, "cmd", "server"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, err := validateRetryTestModuleDir(dir)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
	if _, err := validateRetryTestModuleDir(""); err == nil {
		t.Fatal("empty override accepted")
	}
}

func TestCLIProxyAPIProcessSchedulerRetry(t *testing.T) {
	for _, knownReset := range []bool{true, false} {
		name := "known_reset"
		if !knownReset {
			name = "unknown_reset"
		}
		t.Run(name, func(t *testing.T) {
			fixture := startSchedulerRetryProcess(t, knownReset)
			for _, stream := range []bool{false, true} {
				name := "nonstream"
				if stream {
					name = "stream_initial_error"
				}
				t.Run(name, func(t *testing.T) {
					payload := fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`, e2eProtectedModel, stream)
					request, err := http.NewRequest(http.MethodPost, fixture.baseURL+"/v1/messages", strings.NewReader(payload))
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Authorization", "Bearer "+e2eAPIKey)
					request.Header.Set("Anthropic-Version", "2023-06-01")
					request.Header.Set("Content-Type", "application/json")
					start := time.Now()
					response, err := fixture.client.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					body := readResponseBody(t, response)
					finish := time.Now()
					mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
					if response.StatusCode != 429 || mediaErr != nil || mediaType != "application/json" || !json.Valid(body) || !bytes.Contains(body, []byte(exhaustedErrorCode)) {
						t.Fatalf("want initial JSON quota HTTP429: status=%d content-type=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), body)
					}
					if bytes.Contains(body, []byte("retry_after_seconds=")) != knownReset {
						t.Fatalf("legacy retry metadata must match reset reliability: known=%v body=%s", knownReset, body)
					}
					patched := fixture.expectHeader
					if !knownReset || !patched {
						if len(response.Header.Values("Retry-After")) != 0 {
							t.Fatalf("unexpected Retry-After=%q", response.Header.Values("Retry-After"))
						}
					} else {
						retry, err := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 64)
						lo, hi := int64(math.Ceil(fixture.reset.Sub(finish).Seconds())), int64(math.Ceil(fixture.reset.Sub(start).Seconds()))
						if err != nil || retry < 1 || retry < lo || retry > hi {
							t.Fatalf("Retry-After=%q expected positive ceil reset bounds [%d,%d] err=%v", response.Header.Get("Retry-After"), lo, hi, err)
						}
					}
					if hits := fixture.proxyHits.Load(); hits != 0 {
						t.Fatalf("scheduler rejection reached proxy: hits=%d", hits)
					}
					observed, err := json.Marshal(map[string]any{"case": t.Name(), "patched_host": patched, "status": response.StatusCode, "content_type": mediaType, "retry_after": response.Header.Values("Retry-After"), "proxy_hits": fixture.proxyHits.Load(), "known_samples": 2, "healthy_a": true, "blocked_b": true, "body": json.RawMessage(body)})
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("RETRY_PROCESS_OBSERVED %s", observed)
				})
			}
		})
	}
}

type schedulerRetryProcess struct {
	baseURL      string
	client       *http.Client
	reset        time.Time
	proxyHits    *atomic.Int64
	expectHeader bool
}

func startSchedulerRetryProcess(t *testing.T, knownReset bool) schedulerRetryProcess {
	t.Helper()
	// Resolve/build only the stock module unless an explicit validated override is supplied.
	root, dir := cliProxyAPIModuleDir(t), t.TempDir()
	module, err := retryTestModulePath(root)
	if err != nil {
		t.Fatal(err)
	}
	expectHeader, err := retryTestExpectHeader(module)
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(90 * time.Second).UTC().Truncate(time.Second)
	resetJSON := "null"
	if knownReset {
		resetJSON = strconv.Quote(reset.Format(time.RFC3339))
	}
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		percent, recovery := 20, strconv.Quote(reset.Format(time.RFC3339))
		switch r.Header.Get("Authorization") {
		case "Bearer retry-token-a":
		case "Bearer retry-token-b":
			percent, recovery = 99, resetJSON
		default:
			http.Error(w, "unknown synthetic token", 401)
			return
		}
		fmt.Fprintf(w, `{"five_hour":{"utilization":%d,"resets_at":%s}}`, percent, recovery)
	}))
	t.Cleanup(usage.Close)
	hits := new(atomic.Int64)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); http.Error(w, "local rejecting proxy", 502) }))
	t.Cleanup(proxy.Close)
	pluginDir, authDir := filepath.Join(dir, "plugins"), filepath.Join(dir, "auth")
	for _, path := range []string{pluginDir, authDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	extension, serverName := ".so", "cliproxyapi"
	if runtime.GOOS == "darwin" {
		extension = ".dylib"
	}
	if runtime.GOOS == "windows" {
		extension, serverName = ".dll", "cliproxyapi.exe"
	}
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-ldflags", "-X=main.usageEndpoint="+usage.URL, "-o", filepath.Join(pluginDir, pluginName+extension), ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}
	serverPath := filepath.Join(dir, serverName)
	// The stock host starts an unrelated metadata updater even in local-model
	// mode. Redirect its string endpoint at link time, without modifying source.
	cmd = exec.Command("go", "build", "-ldflags", "-X="+module+"/internal/misc.antigravityHubLatestManifestURL="+usage.URL+"/metadata", "-o", serverPath, "./cmd/server")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build CPA: %v\n%s", err, out)
	}
	for priority, letter := range []string{"a", "b"} {
		body := fmt.Sprintf(`{"type":"claude","priority":%d,"email":"retry-%s@example.test","access_token":"retry-token-%s","refresh_token":"synthetic","expired":"2099-01-01T00:00:00Z"}`, priority, letter, letter)
		if err := os.WriteFile(filepath.Join(authDir, "retry-"+letter+".json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	port := unusedTCPPort(t)
	config := fmt.Sprintf("host: %q\nport: %d\nproxy-url: %q\nauth-dir: %q\napi-keys: [%q]\nremote-management:\n  allow-remote: false\n  secret-key: %q\n  disable-control-panel: true\n  disable-auto-update-panel: true\nlogging-to-file: false\ndebug: false\ndisable-cooling: true\nplugins:\n  enabled: true\n  dir: %q\n  configs:\n    five-hour-quota-router:\n      enabled: true\n      priority: 100\n      protected-models: [%q]\n      cutoff-percent-used: 95\n      poll-interval: 1m\n      request-timeout: 2s\n      overage-fallback-enabled: false\n", "127.0.0.1", port, proxy.URL, authDir, e2eAPIKey, e2eManagementKey, pluginDir, e2eProtectedModel)
	configPath, logPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "server.log")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(serverPath, "-config", configPath, "-local-model")
	server.Dir, server.Stdout, server.Stderr = root, log, log
	// Also confine background host metadata clients to the rejecting local proxy.
	server.Env = append(os.Environ(), "HOME="+filepath.Join(dir, "home"), "HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL, "ALL_PROXY="+proxy.URL, "NO_PROXY=127.0.0.1,localhost")
	if err := server.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	var processErr error
	go func() { processErr = server.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			server.Process.Kill()
			<-done
		}
		log.Close()
	})
	baseURL, client := fmt.Sprintf("http://127.0.0.1:%d", port), &http.Client{Timeout: 3 * time.Second}
	waitForProcessStatus(t, client, baseURL, done, &processErr, logPath, func(s cutoffStatusResponse) bool {
		if !s.Enabled || s.CutoffPercentUsed != 95 || len(s.Accounts) != 2 {
			return false
		}
		a, b := false, false
		for _, account := range s.Accounts {
			if !account.Known || account.FiveHourPercentUsed == nil {
				return false
			}
			if account.Name == "retry-a.json" {
				a = !account.Blocked && *account.FiveHourPercentUsed == 20
			}
			if account.Name == "retry-b.json" {
				accountReset, err := time.Parse(time.RFC3339Nano, account.ResetAt)
				b = account.Blocked && *account.FiveHourPercentUsed == 99 && err == nil && !accountReset.IsZero() == knownReset
			}
		}
		return a && b
	})
	waitForProcessModel(t, client, baseURL, done, &processErr, logPath, e2eProtectedModel)
	return schedulerRetryProcess{baseURL: baseURL, client: client, reset: reset, proxyHits: hits, expectHeader: expectHeader}
}

func TestRetryTestHeaderExpectation(t *testing.T) {
	const v7 = "github.com/router-for-me/CLIProxyAPI/v7"
	const v8 = "github.com/router-for-me/CLIProxyAPI/v8"
	for _, tc := range []struct {
		name, module, value            string
		override, explicit, want, fail bool
	}{
		{name: "stock v7", module: v7},
		{name: "legacy patched v7", module: v7, override: true, want: true},
		{name: "v8 requires expectation", module: v8, override: true, fail: true},
		{name: "stock v8", module: v8, override: true, explicit: true, value: "false"},
		{name: "patched v8", module: v8, override: true, explicit: true, value: "true", want: true},
		{name: "invalid expectation", module: v8, override: true, explicit: true, value: "invalid", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"CPA_RETRY_TEST_MODULE_DIR", "CPA_RETRY_TEST_EXPECT_HEADER"} {
				old, set := os.LookupEnv(key)
				t.Cleanup(func() {
					if set {
						_ = os.Setenv(key, old)
					} else {
						_ = os.Unsetenv(key)
					}
				})
				_ = os.Unsetenv(key)
			}
			if tc.override {
				t.Setenv("CPA_RETRY_TEST_MODULE_DIR", "synthetic")
			}
			if tc.explicit {
				t.Setenv("CPA_RETRY_TEST_EXPECT_HEADER", tc.value)
			}
			got, err := retryTestExpectHeader(tc.module)
			if (err != nil) != tc.fail || got != tc.want {
				t.Fatalf("got=%v err=%v want=%v fail=%v", got, err, tc.want, tc.fail)
			}
		})
	}
}
