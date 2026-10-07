"""Verify the final runtime binary; execute with docker run --network none."""
import datetime
import hashlib
import http.server
import json
import math
import pathlib
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

MODEL = "claude-opus-4-1-20250805"
state = {"reset": "", "known": True, "proxy_hits": 0, "background_hits": []}
# CPA >= v8.0.20 refreshes the Grok CLI version from npm through proxy-url at
# startup regardless of -local-model. It is metadata, not provider traffic.
BACKGROUND_TARGETS = {("CONNECT", "registry.npmjs.org:443")}


class Fixture(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        if self.path == "/usage":
            token = self.headers.get("Authorization")
            assert token in ("Bearer synthetic-a", "Bearer synthetic-b"), token
            reset = state["reset"] if token.endswith("a") or state["known"] else None
            body = json.dumps({"five_hour": {"utilization": 20 if token.endswith("a") else 99, "resets_at": reset}}).encode()
            self.send_response(200)
            self.end_headers()
            self.wfile.write(body)
        elif (self.command, self.path) in BACKGROUND_TARGETS:
            state["background_hits"].append(f"{self.command} {self.path}")
            self.send_error(502)
        else:
            state["proxy_hits"] += 1
            self.send_error(502)

    do_POST = do_GET
    do_CONNECT = do_GET


def request(url, payload=None, management=False):
    headers = {"Authorization": "Bearer synthetic-management" if management else "Bearer synthetic-api"}
    if payload is not None:
        headers.update({"Content-Type": "application/json", "Anthropic-Version": "2023-06-01"})
    req = urllib.request.Request(url, data=None if payload is None else json.dumps(payload).encode(), headers=headers)
    try:
        return urllib.request.urlopen(req, timeout=3)
    except urllib.error.HTTPError as error:
        return error


def run_case(known):
    state.update(known=known, reset=datetime.datetime.fromtimestamp(math.floor(time.time()) + 90, datetime.timezone.utc).isoformat().replace("+00:00", "Z"), proxy_hits=0, background_hits=[])
    with tempfile.TemporaryDirectory() as directory:
        root = pathlib.Path(directory)
        auth = root / "auth"
        auth.mkdir()
        for priority, letter in enumerate(("a", "b")):
            (auth / f"retry-{letter}.json").write_text(json.dumps({"type": "claude", "priority": priority, "email": f"{letter}@example.test", "access_token": f"synthetic-{letter}", "refresh_token": "synthetic", "expired": "2099-01-01T00:00:00Z"}))
        config = root / "config.yaml"
        config.write_text(f'''host: "127.0.0.1"
port: 19092
proxy-url: "http://127.0.0.1:19091/proxy"
auth-dir: "{auth}"
api-keys: ["synthetic-api"]
remote-management:
  allow-remote: false
  secret-key: "synthetic-management"
  disable-control-panel: true
  disable-auto-update-panel: true
disable-cooling: true
plugins:
  enabled: true
  dir: /test-plugin
  configs:
    five-hour-quota-router:
      enabled: true
      priority: 100
      protected-models: ["{MODEL}"]
      cutoff-percent-used: 95
      poll-interval: 1m
      request-timeout: 2s
      overage-fallback-enabled: false
''')
        log = root / "server.log"
        with log.open("w") as output:
            process = subprocess.Popen(["/CLIProxyAPI/CLIProxyAPI", "-config", str(config), "-local-model"], stdout=output, stderr=output)
            try:
                deadline = time.monotonic() + 30
                while True:
                    assert process.poll() is None, log.read_text()
                    try:
                        with request("http://127.0.0.1:19092/v0/management/plugins/five-hour-quota-router/status", management=True) as response:
                            status = json.load(response)
                        accounts = {a["name"]: a for a in status.get("accounts", [])}
                        a, b = accounts.get("retry-a.json", {}), accounts.get("retry-b.json", {})
                        if status.get("enabled") and a.get("known") and b.get("known") and not a.get("blocked") and b.get("blocked") and a.get("five_hour_percent_used") == 20 and b.get("five_hour_percent_used") == 99:
                            break
                    except (OSError, ValueError):
                        pass
                    assert time.monotonic() < deadline, log.read_text()
                    time.sleep(0.1)
                for stream in (False, True):
                    start = time.time()
                    with request("http://127.0.0.1:19092/v1/messages", {"model": MODEL, "stream": stream, "max_tokens": 16, "messages": [{"role": "user", "content": "synthetic"}]}) as response:
                        body = response.read()
                        finish = time.time()
                        assert response.status == 429, (response.status, body)
                        assert response.headers.get_content_type() == "application/json", response.headers
                        assert "five_hour_quota_exhausted" in json.loads(body)["error"]["message"], body
                        values = response.headers.get_all("Retry-After") or []
                        if known:
                            reset = datetime.datetime.fromisoformat(state["reset"].replace("Z", "+00:00")).timestamp()
                            assert len(values) == 1 and values[0].isdigit(), values
                            assert 1 <= int(values[0]) and math.ceil(reset - finish) <= int(values[0]) <= math.ceil(reset - start), values
                        else:
                            assert not values, values
                        assert state["proxy_hits"] == 0, state
                        print(json.dumps({"known_reset": known, "stream": stream, "status": response.status, "retry_after": values, "content_type": response.headers.get_content_type(), "proxy_hits": 0, "background_hits": sorted(set(state["background_hits"])), "healthy_a": True, "blocked_priority_b": True}), flush=True)
            finally:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


if __name__ == "__main__":
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 19091), Fixture)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    print(json.dumps({"candidate_binary_sha256": hashlib.sha256(pathlib.Path("/CLIProxyAPI/CLIProxyAPI").read_bytes()).hexdigest()}), flush=True)
    try:
        run_case(True)
        run_case(False)
    finally:
        server.shutdown()
