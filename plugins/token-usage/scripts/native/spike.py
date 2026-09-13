#!/usr/bin/env python3
"""Real CPA HTTP executor -> usage manager -> native ABI -> private HTTP probe.

Only loopback upstreams and synthetic keys. The bootstrap probe requires a
nativefixture-tagged library. All runtime files live in a fresh /tmp directory.
This does NOT test persistence, original provider token completeness, or billing.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import Request, build_opener, ProxyHandler
from urllib.error import HTTPError, URLError

PIN = "7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974"
CLIENT_KEY = "synthetic-client-secret-canary"
MANAGEMENT_KEY = "synthetic-management-secret-canary"
UPSTREAM_KEY = "synthetic-upstream-secret-canary"
BODY_CANARY = "synthetic-failure-body-secret-canary"
STATUS = "/v0/management/plugins/token-usage/status"
OPENER = build_opener(ProxyHandler({}))
FIELDS = ("InputTokens", "OutputTokens", "TotalTokens", "ReasoningTokens",
          "CachedTokens", "CacheReadTokens", "CacheCreationTokens")


def detail(i=0, o=0, t=0, r=0, c=0, cr=0, cw=0):
    return dict(zip(FIELDS, (i, o, t, r, c, cr, cw)))


# Expected *native* observations, not an assertion that upstream losses are OK.
# Provider-reported raw data is separately defined in the mock below.
CASES = [
    ("oa-normal", False, detail(100, 20, 120, 3, 30, 30, 5), False),
    ("oa-final", True, detail(100, 20, 120, 3, 30, 30, 5), False),
    ("oa-cumulative", True, detail(100, 20, 120, 3, 30, 30, 5), False),
    ("oa-missing", True, detail(), False),
    ("oa-error", True, detail(), True),
    ("oa-disconnect", True, detail(), True),
    ("oa-clean-eof", True, detail(100, 20, 120, 3, 30, 30, 5), False),
    ("oa-http-failure", False, detail(), True),
    ("claude-normal", False, detail(100, 20, 155, 3, 30, 30, 5), False),
    ("claude-creation-only", False, detail(100, 20, 125, 3, 5, 0, 5), False),
    ("claude-final", True, detail(100, 20, 155, 3, 30, 30, 5), False),
    ("claude-split", True, detail(0, 20, 20), False),
    ("claude-cumulative", True, detail(100, 2, 137, 0, 30, 30, 5), False),
    ("claude-missing", True, None, False),
    ("claude-disconnect", True, detail(100, 2, 137, 0, 30, 30, 5), False),
]


def oa_usage(output=20):
    return {"prompt_tokens": 100, "completion_tokens": output, "total_tokens": 100 + output,
            "prompt_tokens_details": {"cached_tokens": 30, "cache_creation_tokens": 5},
            "completion_tokens_details": {"reasoning_tokens": 3 if output == 20 else 0}}


def claude_usage(output=20, read=30):
    return {"input_tokens": 100, "output_tokens": output, "cache_read_input_tokens": read,
            "cache_creation_input_tokens": 5,
            "output_tokens_details": {"thinking_tokens": 3 if output == 20 else 0}}


def sse(data, event=None):
    text = data if isinstance(data, str) else json.dumps(data)
    return (("event: " + event + "\n") if event else "") + "data: " + text + "\n\n"


class Mock(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    seen = []

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        case = body["model"]
        self.seen.append(case)
        if case == "oa-http-failure":
            self.reply(json.dumps({"error": {"message": BODY_CANARY}}), code=400)
            return
        if not body.get("stream"):
            if case.startswith("oa-"):
                result = {"id": "synthetic", "object": "chat.completion", "model": "physical-revision-not-attribution",
                          "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
                          "usage": oa_usage()}
            else:
                result = {"id": "msg_synthetic", "type": "message", "role": "assistant", "model": "physical-revision-not-attribution",
                          "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn",
                          "usage": claude_usage(read=0 if case == "claude-creation-only" else 30)}
            self.reply(json.dumps(result))
            return
        if case.startswith("oa-"):
            chunk = {"id": "synthetic", "object": "chat.completion.chunk", "model": case,
                     "choices": [{"index": 0, "delta": {"content": "ok"}, "finish_reason": None}]}
            frames = sse(chunk)
            if case == "oa-cumulative":
                frames += sse({"id": "synthetic", "choices": [], "usage": oa_usage(2)})
            if case != "oa-missing":
                frames += sse({"id": "synthetic", "choices": [], "usage": oa_usage()})
            if case == "oa-error":
                frames += sse({"error": {"message": BODY_CANARY, "code": "server_error"}}, "error")
            elif case not in ("oa-disconnect", "oa-clean-eof"):
                frames += sse("[DONE]")
        else:
            initial = {"type": "message_start", "message": {"id": "msg_synthetic", "type": "message", "role": "assistant",
                       "model": case, "content": [], "usage": claude_usage(1)}}
            frames = sse(initial, "message_start")
            frames += sse({"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}, "content_block_start")
            frames += sse({"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "ok"}}, "content_block_delta")
            frames += sse({"type": "content_block_stop", "index": 0}, "content_block_stop")
            if case in ("claude-cumulative", "claude-disconnect"):
                frames += sse({"type": "message_delta", "delta": {}, "usage": claude_usage(2)}, "message_delta")
            if case not in ("claude-missing", "claude-disconnect"):
                usage = {"output_tokens": 20} if case == "claude-split" else claude_usage()
                frames += sse({"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": usage}, "message_delta")
            if case != "claude-disconnect":
                frames += sse({"type": "message_stop"}, "message_stop")
        self.reply(frames, content_type="text/event-stream", broken=case.endswith("disconnect"))

    def reply(self, data, code=200, content_type="application/json", broken=False):
        raw = data.encode()
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        # Deliberately truncated HTTP body produces unexpected EOF at CPA.
        self.send_header("Content-Length", str(len(raw) + (100 if broken else 0)))
        self.send_header("X-Fixture-Secret", "synthetic-header-secret-canary")
        self.end_headers()
        self.wfile.write(raw)
        self.wfile.flush()
        if broken:
            self.close_connection = True
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()


def request(base, path, key=MANAGEMENT_KEY, body=None):
    headers = {"Authorization": "Bearer " + key} if key else {}
    if body is not None:
        headers["Content-Type"] = "application/json"
        headers["anthropic-version"] = "2023-06-01"
        body = json.dumps(body).encode()
    req = Request(base + path, data=body, headers=headers)
    try:
        with OPENER.open(req, timeout=10) as response:
            return response.status, response.read()
    except HTTPError as error:
        with error:
            return error.code, error.read()


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def config(directory, port, upstream, enabled):
    # JSON is a YAML subset; no YAML dependency is necessary for the harness.
    models = lambda prefix: [{"name": c, "alias": "alias-" + c} for c, *_ in CASES if c.startswith(prefix)]
    return {
        "host": "127.0.0.1", "port": port, "auth-dir": str(directory / "auth"),
        "api-keys": [CLIENT_KEY], "remote-management": {"allow-remote": False, "secret-key": MANAGEMENT_KEY,
            "disable-control-panel": True, "disable-auto-update-panel": True},
        "plugins": {"enabled": True, "dir": str(directory / "plugins"), "configs": {"token-usage": {
            "enabled": True, "database-path": str(directory / "data" / "usage.sqlite"),
            "batch-size": 1, "flush-interval": "10ms"}}},
        "usage-statistics-enabled": enabled, "request-retry": 0, "max-retry-interval": 0,
        "disable-cooling": True, "disable-claude-cloak-mode": True,
        "request-log": False, "logging-to-file": False,
        "openai-compatibility": [{"name": "fixture-openai", "base-url": upstream + "/v1",
            "api-key-entries": [{"api-key": UPSTREAM_KEY, "proxy-url": "direct"}], "models": models("oa-")}],
        "claude-api-key": [{"api-key": UPSTREAM_KEY, "base-url": upstream, "proxy-url": "direct",
            "cloak": {"mode": "never"}, "models": models("claude-")}],
    }


def run_setting(binary, library, directory, upstream, enabled):
    directory.mkdir(mode=0o700)
    (directory / "plugins").mkdir()
    (directory / "auth").mkdir()
    shutil.copy2(library, directory / "plugins" / "token-usage.so")
    port = free_port()
    base = "http://127.0.0.1:" + str(port)
    cfg = directory / "config.yaml"
    cfg.write_text(json.dumps(config(directory, port, upstream, enabled)))
    # Do not inherit provider/storage service environment variables or .env files.
    # Unknown external background requests are directed to a closed loopback port.
    env = {"PATH": os.environ["PATH"], "HOME": str(directory), "XDG_CACHE_HOME": str(directory / "cache"),
           "HTTP_PROXY": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1", "NO_PROXY": "127.0.0.1,localhost"}
    log = (directory / "cpa.log").open("wb")
    process = subprocess.Popen([str(binary), "-config", str(cfg)], cwd=directory, env=env, stdout=log, stderr=subprocess.STDOUT)
    results = []
    try:
        deadline = time.monotonic() + 30
        while True:
            if process.poll() is not None:
                raise RuntimeError("CPA exited during startup; inspect " + str(directory / "cpa.log"))
            try:
                code, raw = request(base, STATUS)
                if code == 200:
                    status = json.loads(raw)
                    if "fixture_records" in status:
                        break
            except (URLError, OSError):
                pass
            if time.monotonic() > deadline:
                raise RuntimeError("native plugin did not become ready; inspect " + str(directory / "cpa.log"))
            time.sleep(0.1)
        assert status["schema6_probe"] == "<model>&literal", status
        for key in (None, "invalid-synthetic-key"):
            code, raw = request(base, STATUS, key=key)
            assert code in (401, 403), ("unauthenticated status", code, raw)
            assert b"fixture_records" not in raw
        code, raw = request(base, "/v0/resource/token-usage/status", key=None)
        assert code == 404
        code, shell = request(base, "/v0/resource/plugins/token-usage/status", key=None)
        assert code == 200 and shell.startswith(b"<!doctype html>")
        assert b"fixture_records" not in shell and b"schema6_probe" not in shell
        for key in (None, MANAGEMENT_KEY, "invalid-synthetic-key"):
            assert request(base, "/v0/resource/plugins/token-usage/status?ignored=canary", key=key) == (200, shell)
        for case, stream, expected, failed in CASES:
            body = {"model": "alias-" + case, "messages": [{"role": "user", "content": "synthetic fixture"}],
                    "max_tokens": 64, "stream": stream}
            path = "/v1/messages" if case.startswith("claude-") else "/v1/chat/completions"
            code, response = request(base, path, key=CLIENT_KEY, body=body)
            assert code == (400 if case == "oa-http-failure" else 200), (case, code, response)
            deadline = time.monotonic() + (1.5 if expected is None else 5)
            observed = []
            while time.monotonic() < deadline:
                _, raw = request(base, STATUS)
                status = json.loads(raw)
                observed = [r for r in status["fixture_records"] or [] if r["Alias"] == "alias-" + case]
                if observed and expected is not None:
                    break
                time.sleep(0.05)
            if expected is None:
                assert observed == [], (case, "expected upstream omission", observed)
            else:
                assert len(observed) == 1, (case, "event count", observed)
                record = observed[0]
                assert record["Detail"] == expected, (case, "counter mismatch", expected, record)
                assert record["Failed"] is failed, (case, "outcome mismatch", record)
                assert record["Generate"] is True, (case, "Generate default", record)
                assert record["Model"] == case, (case, "execution model attribution", record)
                assert record["Provider"] == ("claude" if case.startswith("claude-") else "openai-compatible-fixture-openai"), record
                assert record["ExecutorType"] == ("ClaudeExecutor" if case.startswith("claude-") else "OpenAICompatExecutor"), record
                assert record["RequestedAt"] and not record["RequestedAt"].startswith("0001"), record
            results.append({"case": case, "usage_statistics_enabled": enabled,
                            "expected_native_detail": expected, "observed": observed, "http_status": code})
            print(json.dumps({"case": case, "statistics": enabled, "observed": observed}), flush=True)
        assert int(status["observed_events"]) == len(CASES) - 1, status
        assert status["rejected_events"] == "0", status
        serialized = json.dumps(status)
        for canary in (CLIENT_KEY, MANAGEMENT_KEY, UPSTREAM_KEY, BODY_CANARY, "synthetic-header-secret-canary"):
            assert canary not in serialized, "sensitive field escaped narrow probe"
        # This destructive read is only against our disposable synthetic host.
        # It verifies the setting actually changed the built-in sink, not just
        # that the config contained the requested spelling.
        queue_code, queue_raw = request(base, "/v0/management/usage-queue?count=100")
        assert queue_code == 200
        assert len(json.loads(queue_raw)) == (len(CASES) - 1 if enabled else 0)
        # Duplicate callback/model equality is not used for deduplication; the
        # separate unit test covers equal-looking native records without auth.
        return results
    finally:
        process.send_signal(signal.SIGTERM) if process.poll() is None else None
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
            raise RuntimeError("CPA did not stop in 15 seconds")
        finally:
            log.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", required=True, type=Path)
    parser.add_argument("--library", required=True, type=Path)
    args = parser.parse_args()
    directory = Path(tempfile.mkdtemp(prefix="token-usage-native-", dir="/tmp"))
    print("fixture artifacts: " + str(directory), flush=True)
    server = ThreadingHTTPServer(("127.0.0.1", 0), Mock)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        results = []
        for enabled in (False, True):
            results += run_setting(args.cpa.resolve(), args.library.resolve(), directory / str(enabled).lower(),
                                   "http://127.0.0.1:" + str(server.server_port), enabled)
        assert len(Mock.seen) == 2 * len(CASES), ("unexpected retries or missing mock execution", Mock.seen)
        (directory / "results.json").write_text(json.dumps({"cpa_pin": PIN, "results": results}, indent=2))
        print("PASS: %d executor-to-native fixtures; both queue statistics settings; private auth and schema 6 verified" % len(results))
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == "__main__":
    main()
