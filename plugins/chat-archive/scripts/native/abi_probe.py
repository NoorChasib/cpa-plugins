#!/usr/bin/env python3
"""Direct native ABI checks for chat-archive (ctypes; NOT a CPA HTTP test).

Init negatives, schema negotiation, strict config, management registration, hooks that always
answer OK (unconfigured, oversize, garbage), X-Archive-* clearing, sanitized errors, quiesce and
rollback, identical-config idempotence, shutdown and re-init in one process. Synthetic only;
prints check names and counts, never a value.
"""
import base64
import ctypes as c
import json
import os
from pathlib import Path
import stat
import sys
import tempfile


class Buffer(c.Structure):
    _fields_ = [("ptr", c.c_void_p), ("len", c.c_size_t)]


Call = c.CFUNCTYPE(c.c_int, c.c_char_p, c.c_void_p, c.c_size_t, c.POINTER(Buffer))
Free = c.CFUNCTYPE(None, c.c_void_p, c.c_size_t)
Shutdown = c.CFUNCTYPE(None)
HostCall = c.CFUNCTYPE(c.c_int, c.c_void_p, c.c_char_p, c.c_void_p, c.c_size_t, c.POINTER(Buffer))


class Host(c.Structure):
    _fields_ = [("abi_version", c.c_uint32), ("ctx", c.c_void_p), ("call", HostCall), ("free", c.c_void_p)]


class API(c.Structure):
    _fields_ = [("abi_version", c.c_uint32), ("call", Call), ("free", Free), ("shutdown", Shutdown)]


CANARY = "sk-ant-api03-" + "Q" * 93 + "AA"
OK = {"ok": True, "result": {}}


@HostCall
def host_call(ctx, method, req, length, resp):
    return 1  # like CPA's preflight stub: every host call fails


def main():
    library = c.CDLL(sys.argv[1])
    library.cliproxy_plugin_init.argtypes = [c.POINTER(Host), c.POINTER(API)]
    library.cliproxy_plugin_init.restype = c.c_int
    host, api = Host(1, None, host_call, None), API()
    assert library.cliproxy_plugin_init(None, c.byref(api)) != 0
    assert library.cliproxy_plugin_init(c.byref(Host(99, None, host_call, None)), c.byref(api)) != 0
    assert library.cliproxy_plugin_init(c.byref(host), c.byref(api)) == 0
    assert api.abi_version == 1
    assert library.cliproxy_plugin_init(c.byref(host), c.byref(API())) != 0, "second init while initialized"
    directory = Path(tempfile.mkdtemp(prefix="chat-archive-abi-", dir="/tmp"))
    data = directory / "outbox"
    token_file = directory / "token"
    token_file.write_text("cha_src_" + "A" * 43)
    token_file.chmod(0o600)
    key_file = directory / "hmac"
    key_file.write_text("probe-hmac-key-" + "k" * 40)
    key_file.chmod(0o600)
    checks = 0

    def call(method, raw=b"{}", length=None, null_request=False):
        buffer = Buffer()
        request = c.create_string_buffer(raw, len(raw)) if raw is not None else None
        rc = api.call(method, None if null_request else request, len(raw) if length is None else length, c.byref(buffer))
        try:
            return rc, json.loads(c.string_at(buffer.ptr, buffer.len)) if buffer.ptr else None
        finally:
            if buffer.ptr:
                api.free(buffer.ptr, buffer.len)

    def register(cfg, schema=10, method=b"plugin.register"):
        raw = json.dumps({"schema_version": schema, "config_yaml": base64.b64encode(json.dumps(cfg).encode()).decode()}).encode()
        return call(method, raw)

    def management(method, path, query=None):
        rc, result = call(b"management.handle", json.dumps({"Method": method, "Path": path, "Query": query or {},
                                                            "Headers": {"Authorization": ["Bearer " + CANARY]}}).encode())
        assert rc == 0, result
        response = result["result"]
        return response["StatusCode"], base64.b64decode(response["Body"]), response.get("Headers") or {}

    def status():
        code, body, _ = management("GET", "/v0/management/plugins/chat-archive/status")
        assert code == 200
        assert CANARY.encode() not in body
        return json.loads(body)

    configured = {"enabled": True, "priority": 0, "archive-url": "http://127.0.0.1:9", "source-token-file": str(token_file),
                  "hmac-key-file": str(key_file), "data-dir": str(data), "max-rpc-mib": 8,
                  "store": {"id": "chat-archive", "version": "0.1.0", "install": {"type": "direct"}}}
    try:
        # Strict, sanitized configuration and schema 7 floor.
        for bad in ({"unknown-key": True}, {"archive-url": "http://u:" + CANARY + "@h/"}, {"data-dir": "relative"},
                    {"workers": 9}, {"source-token-env": "a", "source-token-file": str(token_file)}):
            rc, result = register(bad)
            assert rc != 0 and result["error"]["code"] == "config_error", bad
            assert CANARY not in json.dumps(result)
            checks += 1
        assert register({}, schema=6)[0] != 0
        assert call(b"plugin.register", b'{"schema_version":10,"config_yaml":"!!"}')[0] != 0
        rc, reg = register({"enabled": True, "store": {"id": "chat-archive"}})
        assert rc == 0 and reg["result"]["schema_version"] == 7, reg
        caps = reg["result"]["capabilities"]
        for name in ("request_interceptor", "request_interceptor_omit_body_after", "response_interceptor",
                     "response_stream_interceptor", "request_lifecycle_plugin", "usage_plugin", "management_api"):
            assert caps.get(name) is True, name
        assert not caps.get("request_interceptor_omit_body_before") and not caps.get("websocket_response_observer")
        meta = reg["result"]["metadata"]
        assert meta["Name"] == "Chat Archive" and meta["Version"] == "0.1.0" and meta["Author"] == "NoorChasib", meta
        assert status()["state"] == "unconfigured"
        checks += 4
        rc, routes = call(b"management.register")
        assert rc == 0
        routes = routes["result"]
        assert [(r["Method"], r["Path"], r.get("Menu", "")) for r in routes["routes"]] == [
            ("GET", "/plugins/chat-archive/status", ""), ("POST", "/plugins/chat-archive/requeue", "")], routes
        assert [(r["Path"], r["Menu"]) for r in routes["resources"]] == [("/status", "Chat Archive")]
        code, shell, headers = management("GET", "/v0/resource/plugins/chat-archive/status")
        assert code == 200 and shell.startswith(b"<!doctype html>") and "Content-Security-Policy" in headers
        checks += 2
        # Hooks always answer OK, also unconfigured; X-Archive-* names are cleared.
        before = {"RequestID": "rid-probe-1", "Model": "m", "SourceFormat": "claude",
                  "Headers": {"X-Archive-Machine": ["probe"], "X-Archive-Unknown": ["x"], "Authorization": ["Bearer " + CANARY]},
                  "Body": base64.b64encode(b'{"model":"m"}').decode(), "Metadata": {}}
        rc, result = call(b"request.intercept_before", json.dumps(before).encode())
        assert rc == 0 and sorted(result["result"].get("ClearHeaders") or []) == ["X-Archive-Machine", "X-Archive-Unknown"], result
        for method in (b"request.intercept_after", b"response.intercept_after", b"response.intercept_stream_chunk",
                       b"request.complete", b"usage.handle"):
            for raw in (b"{}", b"not json", b'{"RequestID":5}'):
                assert call(method, raw) == (0, OK), method
        checks += 16
        # Invalid native calls fail closed without echoing input.
        for method, raw, length, null in [(None, b"{}", 2, False), (b"usage.handle", b"x", 1, True)]:
            rc, result = call(method, raw, length, null)
            assert rc != 0 and result["error"]["code"] == "invalid_request"
        rc, result = call(b"no.such.method")
        assert rc != 0 and result["error"]["code"] in ("plugin_error", "invalid_request")
        checks += 3
        # Configured: outbox opened 0700/0600, oversize calls pass through and count.
        rc, result = register(configured, method=b"plugin.reconfigure")
        assert rc == 0, result
        st = status()
        assert st["state"] in ("running", "degraded"), (st["state"], st.get("reason"))
        assert stat.S_IMODE(data.stat().st_mode) == 0o700
        assert all(stat.S_IMODE(p.stat().st_mode) == 0o600 for p in data.iterdir() if p.is_file())
        big = dict(before, Body=base64.b64encode(b"x" * (9 << 20)).decode())
        rc, result = call(b"request.intercept_before", json.dumps(big).encode())
        assert rc == 0 and result["result"].get("ClearHeaders"), "oversize before-auth still clears X-Archive-*"
        assert call(b"response.intercept_stream_chunk", json.dumps({"RequestID": "r", "ChunkIndex": 0, "Body": base64.b64encode(b"y" * (17 << 20)).decode()}).encode()) == (0, OK)
        assert call(b"usage.handle", json.dumps({"APIKey": CANARY, "Failure": {"Body": "z" * (5 << 20)}}).encode()) == (0, OK)
        code, body, _ = management("POST", "/v0/management/plugins/chat-archive/requeue", {"reason": ["x" * (2 << 20)]})
        assert code == 413 or code == 400
        st = status()
        assert st["dropped"]["oversize_rpc"] >= 3, st["dropped"]
        assert CANARY not in json.dumps(st)
        checks += 6
        # Identical config is idempotent; quiesce stops and closes; reconfigure rolls back.
        boot = st["boot_id"]
        assert register(configured, method=b"plugin.reconfigure")[0] == 0
        assert status()["boot_id"] == boot
        assert call(b"plugin.quiesce")[0] == 0
        assert status()["state"] == "stopped"
        assert call(b"request.intercept_before", json.dumps(before).encode())[0] == 0
        assert register(configured, method=b"plugin.reconfigure")[0] == 0
        assert status()["state"] in ("running", "degraded")
        checks += 4
        # Shutdown; hooks still answer OK; other methods report not_initialized; re-init works.
        api.shutdown()
        assert call(b"request.intercept_before", json.dumps(before).encode()) == (0, OK)
        assert call(b"management.register")[1]["error"]["code"] == "not_initialized"
        api.shutdown()
        assert library.cliproxy_plugin_init(c.byref(host), c.byref(api)) == 0
        assert register(configured)[0] == 0
        assert status()["state"] in ("running", "degraded")
        checks += 4
    finally:
        api.shutdown()
    for path in data.rglob("*"):
        if path.is_file():
            assert CANARY.encode() not in path.read_bytes()
    print("PASS: direct native ABI: %d checks (init negatives, schema 7 floor, strict sanitized config, routes, "
          "hooks always OK, X-Archive-* cleared, oversize pass-through counted, quiesce/rollback, idempotent "
          "reconfigure, shutdown and re-init); no CPA, no network" % checks)


if __name__ == "__main__":
    main()
