#!/usr/bin/env python3
"""Production CPA executor -> native -> committed SQLite -> private API -> restart.

All HTTP is loopback in the runner's network-disabled container. Synthetic only.
The separate spike checks sanitized raw observations; this file never uses them.
"""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import shutil
import signal
import sqlite3
import subprocess
import tempfile
import threading
import time
from http.server import ThreadingHTTPServer
from urllib.error import URLError
from urllib.parse import urlencode

import spike

BASE = "/v0/management/plugins/token-usage/"
TOKENS = ("input_tokens", "output_tokens", "total_tokens", "reasoning_tokens",
          "cached_tokens", "cache_read_tokens", "cache_creation_tokens")
BIG = 9007199254740993
CANARIES = (spike.CLIENT_KEY, spike.MANAGEMENT_KEY, spike.UPSTREAM_KEY,
            spike.BODY_CANARY, "synthetic-header-secret-canary", "synthetic-request-body-secret-canary",
            "synthetic-large-upstream-key")


class LargeMock(spike.Mock):
    seen = []

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.seen.append(body["model"])
        assert body["model"] == "claude-large"
        self.reply(json.dumps({"id": "msg_synthetic", "type": "message", "role": "assistant",
            "model": "physical-revision-not-attribution", "content": [{"type": "text", "text": "ok"}],
            "stop_reason": "end_turn", "usage": {"input_tokens": BIG, "output_tokens": 20}}))


def now():
    return datetime.now(timezone.utc).isoformat()


def no_leak(raw):
    for canary in CANARIES:
        assert canary.encode() not in raw, "sensitive value in plugin output/database"
    assert b"physical-revision-not-attribution" not in raw


def query(base, route, interval, **filters):
    code, raw = spike.request(base, BASE + route + "?" + urlencode({**interval, **filters}))
    no_leak(raw)
    assert code == 200, (route, code, raw)
    result = json.loads(raw)
    assert result["source"] == "cpa_reported" and result["upstream_completeness"] == "unknown"
    assert result["api_schema"] == 1 and result["auxiliary_counters_additive"] is False
    return result


def start(binary, directory, config, generation):
    path = directory / "config.yaml"
    path.write_text(json.dumps(config))
    env = {"PATH": os.environ["PATH"], "HOME": str(directory), "XDG_CACHE_HOME": str(directory / "cache"),
           "HTTP_PROXY": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1", "NO_PROXY": "127.0.0.1,localhost"}
    log_path = directory / f"cpa-{generation}.log"
    log = log_path.open("wb")
    proc = subprocess.Popen([str(binary), "-config", str(path)], cwd=directory, env=env, stdout=log, stderr=subprocess.STDOUT)
    base = "http://127.0.0.1:" + str(config["port"])
    try:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if proc.poll() is not None:
                raise RuntimeError("CPA exited: " + str(log_path))
            try:
                code, raw = spike.request(base, BASE + "status")
                if code == 200:
                    status = json.loads(raw)
                    assert status["storage"] == "sqlite" and status["version"] == "0.1.0", status
                    assert "fixture_records" not in status and "schema6_probe" not in status
                    no_leak(raw)
                    return proc, log, base, status
            except (URLError, OSError):
                pass
            time.sleep(0.1)
        raise RuntimeError("Production plugin not ready: " + str(log_path))
    except BaseException:
        stop(proc, log)
        raise


def stop(proc, log):
    try:
        if proc.poll() is None:
            proc.send_signal(signal.SIGTERM)
        try:
            result = proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
            raise RuntimeError("CPA did not stop cleanly in 15 seconds")
        assert result == 0, ("CPA exit", result)
    finally:
        log.close()


def wait_committed(base, expected):
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        code, raw = spike.request(base, BASE + "status")
        assert code == 200
        no_leak(raw)
        status = json.loads(raw)
        count = int(status["collection"]["diagnostics"]["committed_events"])
        assert count <= expected, ("unexpected duplicate observation", count, expected)
        if count == expected:
            assert status["collection"]["previous_unclean_runs"] == "0", status
            for name in ("dropped_queue", "dropped_storage", "dropped_stopped"):
                assert status["collection"]["diagnostics"][name] == "0", status
            assert status["collection"]["diagnostics"]["rejected_events"] == "0", status
            return status
        time.sleep(0.04)
    raise AssertionError(("commit timeout", expected, status))


def auth_checks(base, interval):
    for route in ("status", "summary", "models"):
        # A valid request resets CPA's failed-auth counter; do not accidentally
        # test only the IP-ban response for later routes (or ban this fixture).
        assert spike.request(base, BASE + "status")[0] == 200
        suffix = "" if route == "status" else "?" + urlencode(interval)
        for key in (None, "invalid-synthetic-key"):
            code, raw = spike.request(base, BASE + route + suffix, key=key)
            assert code in (401, 403), (route, code, raw)
            no_leak(raw)
            assert not any(word in raw for word in (b"reported_tokens", b"observed_events", b"oa-normal", b"claude-", b"usage.sqlite"))
        for prefix in ("/v0/resource/token-usage/", "/v0/resource/plugins/token-usage/"):
            code, raw = spike.request(base, prefix + route, key=None)
            assert code == 404, ("public route", route, code)
            no_leak(raw)
            assert b"reported_tokens" not in raw and b"oa-normal" not in raw


def send(base, model, stream=False, case=None):
    case = case or model.removeprefix("alias-")
    path = "/v1/messages" if case.startswith("claude-") else "/v1/chat/completions"
    code, raw = spike.request(base, path, key=spike.CLIENT_KEY, body={"model": model,
        "messages": [{"role": "user", "content": "synthetic-request-body-secret-canary"}], "max_tokens": 64, "stream": stream})
    assert code == (400 if case == "oa-http-failure" else 200), (case, code, raw)


def check_database(database, expected):
    with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as db:
        assert db.execute("PRAGMA integrity_check").fetchone() == ("ok",)
        assert db.execute("SELECT COUNT(*) FROM usage_events").fetchone() == (expected,)
        assert db.execute("SELECT input_tokens,typeof(input_tokens) FROM usage_events WHERE model='claude-large'").fetchall() == [(BIG, "integer")]
        assert db.execute("SELECT COUNT(*) FROM usage_events WHERE model='oa-normal' AND alias='alias-second'").fetchone() == (1,)
        assert db.execute("SELECT COUNT(*) FROM collection_runs WHERE clean=0").fetchone() == (0,)
        no_leak("\n".join(db.iterdump()).encode())
    for file in database.parent.iterdir():
        if file.is_file():
            no_leak(file.read_bytes())
            assert file.stat().st_mode & 0o777 == 0o600


def run(binary, library, directory, upstream, large_upstream, enabled):
    directory.mkdir(mode=0o700)
    (directory / "plugins").mkdir()
    (directory / "auth").mkdir()
    shutil.copy2(library, directory / "plugins/token-usage.so")
    config = spike.config(directory, spike.free_port(), upstream, enabled)
    config["openai-compatibility"].append({"name": "fixture-second", "base-url": upstream + "/v1",
        "api-key-entries": [{"api-key": spike.UPSTREAM_KEY, "proxy-url": "direct"}],
        "models": [{"name": "oa-normal", "alias": "alias-second"}]})
    config["claude-api-key"].append({"api-key": "synthetic-large-upstream-key", "base-url": large_upstream,
        "proxy-url": "direct", "cloak": {"mode": "never"}, "models": [{"name": "claude-large", "alias": "alias-claude-large"}]})
    proc, log, base, initial = start(binary, directory, config, 1)
    expected = {}
    observed = 0

    def add(provider, model, tokens, failed=False):
        nonlocal observed
        group = expected.setdefault((provider, model), {"events": 0, "failed": 0, "tokens": [0] * 7})
        group["events"] += 1
        group["failed"] += int(failed)
        group["tokens"] = [x + y for x, y in zip(group["tokens"], tokens)]
        observed += 1

    def assert_queries(interval):
        summary = query(base, "summary", interval)
        totals = summary["totals"]
        assert totals["observed_events"] == str(observed), totals
        assert totals["failed_events"] == str(sum(row["failed"] for row in expected.values()))
        for index, name in enumerate(TOKENS):
            assert totals["reported_tokens"][name] == str(sum(row["tokens"][index] for row in expected.values())), totals
        models = query(base, "models", interval)
        rows = models["models"]
        assert [(row["provider"], row["model"]) for row in rows] == sorted(expected)
        for row in rows:
            want = expected[(row["provider"], row["model"])]
            assert row["observed_events"] == str(want["events"])
            assert row["reported_tokens"] == dict(zip(TOKENS, map(str, want["tokens"])))
            assert row["executor_types"] == (["ClaudeExecutor"] if row["provider"] == "claude" else ["OpenAICompatExecutor"])
        second = query(base, "summary", interval, provider="openai-compatible-fixture-second", model="oa-normal")
        assert second["totals"]["observed_events"] == "1"
        assert second["totals"]["reported_tokens"]["input_tokens"] == "100"
        page = query(base, "models", interval, limit=1, offset=0)
        assert page["models"] == rows[:1] and page["has_more"] is True
        return summary, models

    try:
        for case, stream, detail, failed in spike.CASES:
            send(base, "alias-" + case, stream, case)
            if detail is not None:
                add("claude" if case.startswith("claude-") else "openai-compatible-fixture-openai", case, list(detail.values()), failed)
                wait_committed(base, observed)
        send(base, "alias-oa-normal") # A distinct equal-looking execution must count.
        add("openai-compatible-fixture-openai", "oa-normal", [100, 20, 120, 3, 30, 30, 5])
        send(base, "alias-second", case="oa-normal")
        add("openai-compatible-fixture-second", "oa-normal", [100, 20, 120, 3, 30, 30, 5])
        send(base, "alias-claude-large")
        add("claude", "claude-large", [BIG, 20, BIG + 20, 0, 0, 0, 0])
        status = wait_committed(base, observed)
        interval = {"from": status["coverage"]["from"], "to": now()}
        auth_checks(base, interval)
        summary, models = assert_queries(interval)
        # Authenticated input errors and uncovered range must never become zeros.
        for path, code in (("summary", 400), ("models?from=bad&to=bad", 400),
            ("summary?" + urlencode({"from": "2020-01-01T00:00:00Z", "to": "2020-01-01T00:01:00Z"}), 416)):
            actual, raw = spike.request(base, BASE + path)
            assert actual == code, (path, actual, raw)
            no_leak(raw)
        queue_code, queue_raw = spike.request(base, "/v0/management/usage-queue?count=100")
        assert queue_code == 200 and len(json.loads(queue_raw)) == (observed if enabled else 0)
    finally:
        stop(proc, log)
    database = directory / "data/usage.sqlite"
    check_database(database, observed)
    proc, log, base, restarted = start(binary, directory, config, 2)
    try:
        assert restarted["collection"]["previous_unclean_runs"] == "0"
        assert_queries(interval) # Exact prior interval/history unchanged after restart.
        send(base, "alias-oa-normal")
        add("openai-compatible-fixture-openai", "oa-normal", [100, 20, 120, 3, 30, 30, 5])
        wait_committed(base, observed)
        # A settling check rejects delayed duplicate deliveries as well.
        time.sleep(0.3)
        wait_committed(base, observed)
        interval["to"] = now()
        summary, models = assert_queries(interval)
        auth_checks(base, interval)
    finally:
        stop(proc, log)
    check_database(database, observed)
    result = {"statistics": enabled, "committed_events_after_restart": observed,
              "large_integer_executor": "ClaudeExecutor nonstream", "summary": summary, "models": models}
    (directory / "results.json").write_text(json.dumps(result, indent=2))
    print(f"PASS: production statistics={enabled}: 19 mock requests, {observed} committed events, auth/privacy/grouping/>2^53/restart exact", flush=True)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", required=True, type=Path)
    parser.add_argument("--library", required=True, type=Path)
    args = parser.parse_args()
    directory = Path(tempfile.mkdtemp(prefix="token-usage-production-", dir="/tmp"))
    print("Production acceptance artifacts: " + str(directory), flush=True)
    servers = [ThreadingHTTPServer(("127.0.0.1", 0), kind) for kind in (spike.Mock, LargeMock)]
    threads = [threading.Thread(target=s.serve_forever, daemon=True) for s in servers]
    for thread in threads:
        thread.start()
    try:
        results = [run(args.cpa.resolve(), args.library.resolve(), directory / str(enabled).lower(),
                       *["http://127.0.0.1:" + str(s.server_port) for s in servers], enabled) for enabled in (False, True)]
        assert len(spike.Mock.seen) == 36 and len(LargeMock.seen) == 2, (spike.Mock.seen, LargeMock.seen)
        (directory / "results.json").write_text(json.dumps({"cpa_pin": spike.PIN, "results": results}, indent=2))
        print("PASS: production native acceptance; 38 synthetic HTTP executions, both statistics settings; no production/provider access")
    finally:
        for server in servers:
            server.shutdown()
            server.server_close()
        for thread in threads:
            thread.join()


if __name__ == "__main__":
    main()
