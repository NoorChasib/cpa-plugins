#!/usr/bin/env python3
"""Direct native lifecycle/large-integer checks; NOT a CPA HTTP/authentication test."""
import base64
import ctypes as c
from datetime import datetime, timezone
import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import time


class Buffer(c.Structure):
    _fields_ = [("ptr", c.c_void_p), ("len", c.c_size_t)]


Call = c.CFUNCTYPE(c.c_int, c.c_char_p, c.c_void_p, c.c_size_t, c.POINTER(Buffer))
Free = c.CFUNCTYPE(None, c.c_void_p, c.c_size_t)
Shutdown = c.CFUNCTYPE(None)


class Host(c.Structure):
    _fields_ = [("abi_version", c.c_uint32), ("ctx", c.c_void_p), ("call", c.c_void_p), ("free", c.c_void_p)]


class API(c.Structure):
    _fields_ = [("abi_version", c.c_uint32), ("call", Call), ("free", Free), ("shutdown", Shutdown)]


def timestamp():
    return datetime.now(timezone.utc).isoformat()


def main():
    library = c.CDLL(sys.argv[1])
    library.cliproxy_plugin_init.argtypes = [c.POINTER(Host), c.POINTER(API)]
    library.cliproxy_plugin_init.restype = c.c_int
    host, api = Host(1), API()
    assert library.cliproxy_plugin_init(None, c.byref(api)) != 0
    assert library.cliproxy_plugin_init(c.byref(Host(99)), c.byref(api)) != 0
    assert library.cliproxy_plugin_init(c.byref(host), c.byref(api)) == 0
    assert api.abi_version == 1
    assert library.cliproxy_plugin_init(c.byref(host), c.byref(API())) != 0
    directory = Path(tempfile.mkdtemp(prefix="token-usage-abi-", dir="/tmp"))
    database = directory / "data" / "usage.sqlite"

    def call(method, raw=b"{}", length=None, null_request=False):
        buffer = Buffer()
        request = c.create_string_buffer(raw)
        rc = api.call(method, None if null_request else request, len(raw) if length is None else length, c.byref(buffer))
        try:
            return rc, json.loads(c.string_at(buffer.ptr, buffer.len))
        finally:
            api.free(buffer.ptr, buffer.len)

    def register(path=database, schema=6, method=b"plugin.register"):
        cfg = {"database-path": str(path), "batch-size": 1, "flush-interval": "10ms"}
        raw = json.dumps({"schema_version": schema, "config_yaml": base64.b64encode(json.dumps(cfg).encode()).decode()}).encode()
        return call(method, raw)

    def management(route, query=None):
        rc, result = call(b"management.handle", json.dumps({"Method": "GET", "Path": "/v0/management/plugins/token-usage/" + route, "Query": query or {}}).encode())
        assert rc == 0, result
        response = result["result"]
        return response["StatusCode"], json.loads(base64.b64decode(response["Body"]))

    try:
        for raw in (b'{"schema_version":6,"config_yaml":""}', b'{"schema_version":6,"config_yaml":"invalid!"}'):
            assert call(b"plugin.register", raw)[0] != 0
        assert register("relative.sqlite")[0] != 0
        assert register(schema=5)[0] != 0
        rc, registration = register()
        assert rc == 0 and registration["result"]["schema_version"] == 6
        assert register()[0] == 0
        assert register(directory / "other.sqlite", method=b"plugin.reconfigure")[0] != 0
        for method, raw, length, null in [(None, b"{}", 2, False),
                                          (b"usage.handle", b"x", 2**32 + 1, False),
                                          (b"usage.handle", b"x", 1, True)]:
            rc, result = call(method, raw, length, null)
            assert rc != 0 and result["error"]["code"] == "invalid_request"
        rc, result = call(b"usage.handle", b'{"Provider":"secret-canary","Detail":{"InputTokens":-1}}')
        assert rc != 0 and "secret-canary" not in json.dumps(result)
        assert api.call(b"usage.handle", None, 0, None) != 0
        canary = "synthetic-direct-abi-sensitive-canary"
        event = {"Provider": "direct-fixture", "ExecutorType": "direct-ABI-only", "Model": "large-integer",
                 "Alias": "synthetic-alias", "RequestedAt": timestamp(), "Generate": True, "Failed": True,
                 "Failure": {"StatusCode": 500, "Body": canary}, "APIKey": canary, "Source": canary,
                 "Headers": {"X-Secret": canary}, "AuthIndex": "", "SessionID": canary,
                 "Detail": {"InputTokens": 9007199254740993, "OutputTokens": 20,
                            "TotalTokens": 9007199254741013, "ReasoningTokens": 3,
                            "CachedTokens": 0, "CacheReadTokens": 0, "CacheCreationTokens": 0}}
        for _ in range(2):
            assert call(b"usage.handle", json.dumps(event).encode())[0] == 0
        deadline = time.monotonic() + 5
        while True:
            code, status = management("status")
            assert code == 200
            if status["collection"]["diagnostics"]["committed_events"] == "2":
                break
            assert time.monotonic() < deadline, status
            time.sleep(0.02)
        query = {"from": [status["coverage"]["from"]], "to": [timestamp()]}
        code, summary = management("summary", query)
        assert code == 200, summary
        assert summary["totals"]["observed_events"] == "2"
        assert summary["totals"]["failed_events"] == "2"
        assert summary["totals"]["reported_tokens"]["input_tokens"] == "18014398509481986"
        assert canary not in json.dumps(summary)
        assert call(b"plugin.quiesce")[0] == 0
        code, stopped = management("status")
        assert code == 200 and stopped["state"] == "stopped", stopped
        assert stopped["collection"]["state"] == "stopped"
        before_late = stopped["collection"]["diagnostics"]
        assert before_late["committed_events"] == "2"
        for route in ("summary", "models"):
            assert management(route, query)[0] == 503, route
        # Both normal and oversized deliveries after close remain visible in
        # stopped RAM diagnostics, never admitted or written into closed SQLite.
        assert call(b"usage.handle", json.dumps(event).encode())[0] != 0
        rc, result = call(b"usage.handle", b"x", 2**32 + 1)
        assert rc != 0 and result["error"]["code"] == "invalid_request"
        code, late = management("status")
        assert code == 200 and late["state"] == "stopped", late
        diagnostics = late["collection"]["diagnostics"]
        for counter in ("observed_events", "dropped_stopped"):
            assert int(diagnostics[counter]) == int(before_late[counter]) + 2, diagnostics
        for counter in ("admitted_events", "committed_events", "rejected_events"):
            assert diagnostics[counter] == before_late[counter], diagnostics
        assert canary not in json.dumps(late)
        for route in ("summary", "models"):
            assert management(route, query)[0] == 503, route
        assert register(method=b"plugin.reconfigure")[0] == 0
        code, reopened = management("status")
        assert code == 200 and reopened["state"] != "stopped", reopened
        # These deliveries happened strictly after final save/close, so reopening
        # resumes the durable snapshot rather than inventing persistence for them.
        for counter in ("observed_events", "dropped_stopped", "committed_events"):
            assert reopened["collection"]["diagnostics"][counter] == before_late[counter], reopened
        assert management("summary", query)[1]["totals"] == summary["totals"]
        assert management("models", query)[0] == 200
        assert register()[0] == 0  # Identical register remains idempotent when open.
        api.shutdown()
        assert call(b"usage.handle")[1]["error"]["code"] == "not_initialized"
        api.shutdown()
        assert library.cliproxy_plugin_init(c.byref(host), c.byref(api)) == 0
        assert register()[0] == 0
        assert management("summary", query)[1]["totals"] == summary["totals"]
        assert management("status")[1]["collection"]["previous_unclean_runs"] == "0"
    finally:
        api.shutdown()
    with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as db:
        assert db.execute("SELECT input_tokens FROM usage_events").fetchall() == [(9007199254740993,)] * 2
        assert canary not in "\n".join(db.iterdump())
    for path in database.parent.iterdir():
        if path.is_file():
            assert canary.encode() not in path.read_bytes()
    print("PASS: direct native ABI lifecycle/config/bounds/sanitization; exact >2^53 SQLite totals; stopped status/closed queries/late RAM diagnostics; same-config reopen/reinit (not HTTP)")


if __name__ == "__main__":
    main()
