"""Shared plumbing for chat-archive native acceptance: CPA processes, the fake archive, HTTP
helpers and canary scans. Loopback only; runs inside a --network none container."""
import base64
import http.client
import json
import os
from pathlib import Path
import signal
import socket
import sqlite3
import subprocess
import time
from urllib.error import HTTPError, URLError
from urllib.request import ProxyHandler, Request, build_opener

import canaries as canarylib

OPENER = build_opener(ProxyHandler({}))
STATUS = "/v0/management/plugins/chat-archive/status"
REQUEUE = "/v0/management/plugins/chat-archive/requeue"
PUBLIC = "/v0/resource/plugins/chat-archive/status"
VERSION = "0.1.0"
ZSTD_MAGIC = b"\x28\xb5\x2f\xfd"


class Failure(AssertionError):
    pass


def check(condition, *what):
    if not condition:
        raise Failure(" ".join(str(w) for w in what))


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def request(base, path, key=None, body=None, headers=None, method=None, timeout=60, raw_body=None):
    hdrs = dict(headers or {})
    if key:
        hdrs.setdefault("Authorization", "Bearer " + key)
    data = raw_body
    if body is not None:
        hdrs.setdefault("Content-Type", "application/json")
        data = json.dumps(body).encode()
    req = Request(base + path, data=data, headers=hdrs, method=method)
    try:
        with OPENER.open(req, timeout=timeout) as response:
            return response.status, response.headers, response.read()
    except HTTPError as error:
        with error:
            return error.code, error.headers, error.read()


def wait_until(fn, timeout, what, interval=0.1):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = fn()
            if last:
                return last
        except (URLError, OSError, ValueError, KeyError):
            pass
        time.sleep(interval)
    raise Failure("timeout waiting for " + what)


class Fake:
    """The clienttest fake archive in its own process (scripts/native/fakearchive)."""

    def __init__(self, binary, directory):
        self.binary = binary
        state = directory / "fakearchive.json"
        self.log = (directory / "fakearchive.log").open("wb")
        self.proc = subprocess.Popen([str(binary), "serve", "-state", str(state)], stdout=self.log, stderr=subprocess.STDOUT)
        wait_until(lambda: state.exists(), 20, "fake archive")
        info = json.loads(state.read_text())
        self.url, self.control, self.source_id, self.token = info["url"], info["control"], info["source_id"], info["token"]

    def get(self, path):
        with OPENER.open(self.control + path, timeout=60) as response:
            return json.loads(response.read())

    def post(self, path, value, raw=False):
        data = value if raw else json.dumps(value).encode()
        with OPENER.open(Request(self.control + path, data=data, method="POST"), timeout=120) as response:
            out = response.read()
            return out if raw else json.loads(out)

    def envelopes(self):
        out = []
        for row in self.get("/envelopes") or []:
            env = row["envelope"]
            env["body"] = env.get("body")
            out.append((row["status"], row.get("reason", ""), env))
        return out

    def bodies_of(self, kind):
        return [env["body"] for status, _, env in self.envelopes() if env["kind"] == kind]

    def unzstd(self, raw):
        return self.post("/unzstd", raw, raw=True)

    def scan(self, named):
        """{id: value} -> {id: {"bodies":n,"media_bodies":n,"pieces":n,"heartbeats":n}} summed over encodings."""
        result = self.post("/scan", {"needles": canarylib.needles(named)})
        out = {}
        for key, hits in result.items():
            name = key.rsplit("/", 1)[0]
            agg = out.setdefault(name, {"bodies": 0, "media_bodies": 0, "pieces": 0, "heartbeats": 0, "encodings": []})
            for k in ("bodies", "media_bodies", "pieces", "heartbeats"):
                agg[k] += hits[k]
            if any(hits[k] for k in ("bodies", "media_bodies", "pieces", "heartbeats")):
                agg["encodings"].append(key.rsplit("/", 1)[1])
        return out

    def close(self):
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
        self.log.close()


def cpa_config(directory, port, mock, c, fake_url, extra_plugin=None, plugin_overrides=None, steering=False, plugins=True):
    models = lambda names: [{"name": n} for n in names]
    plugin = {"enabled": True, "priority": 100, "archive-url": fake_url, "source-token-env": "CA_TEST_SOURCE_TOKEN",
              "hmac-key-id": "k1", "hmac-key-env": "CA_TEST_HMAC_KEY", "workers": 2}
    plugin.update(plugin_overrides or {})
    configs = {"chat-archive": plugin}
    if extra_plugin:
        configs.update(extra_plugin)
    userinfo = mock.replace("http://", "http://ca-user:" + c.secret["U10_userinfo_password"] + "@") + "/v1?ca_q=" + c.secret["U10_query_secret"]
    return {
        "server": {"host": "127.0.0.1", "port": port},
        "management": {"allow-remote": False, "secret-key": c.management_key, "disable-control-panel": True,
                       "disable-auto-update-panel": True},
        "access": {"api-keys": [c.client_key, c.client_key_2]},
        "oauth": {"auth-dir": str(directory / "auth")},
        "routing": {"retry": {"request-retry": 1, "max-retry-interval": 0}, "cooldown": {"disable-cooling": True}},
        "requests": {"streaming": {"bootstrap-retries": 1}},
        "upstream": {"claude": {"disable-claude-cloak-mode": True},
                     "codex": {"websocket-compression": False, "response-steering": steering, "disable-codex-cloaking": True,
                               "stream-bootstrap-buffering": True}},
        "observability": {"logs": {"debug": False, "logging-to-file": False, "request-log": False},
                          "usage": {"usage-statistics-enabled": False}},
        "api-keys": {
            "claude": [{"name": "ca-claude", "base-url": mock, "proxy-url": "direct",
                        "models": models(["ca-claude", "ca-claude-failover", "ca-claude-midstream", "ca-claude-slow",
                                          "ca-claude-bootstrap", "ca-claude-hold", "ca-claude-latency", "reject-fixture",
                                          "ca-claude-big"]),
                        "keys": [{"api-key": c.claude_key_a, "priority": 10}, {"api-key": c.claude_key_b, "priority": 0}]}],
            "codex": [{"name": "ca-codex-http", "base-url": mock + "/codex", "proxy-url": "direct",
                       "models": models(["ca-codex", "ca-codex-native", "ca-codex-merged", "ca-codex-bootstrap"]), "keys": [{"api-key": c.codex_key}]},
                      {"name": "ca-codex-ws", "base-url": mock + "/codexws", "proxy-url": "direct",
                       "models": models(["ca-codex-ws"]), "keys": [{"api-key": c.codex_key, "websockets": True}]}],
            "gemini": [{"name": "ca-gemini", "base-url": mock, "proxy-url": "direct", "models": models(["ca-gemini"]),
                        "keys": [{"api-key": c.gemini_key}]}],
            "vertex": [{"name": "ca-vertex", "base-url": mock + "/vertex", "proxy-url": "direct", "models": models(["ca-vertex"]),
                        "keys": [{"api-key": c.vertex_key}]}],
            "interactions": [{"name": "ca-interactions", "base-url": mock, "proxy-url": "direct", "models": models(["ca-interactions"]),
                              "keys": [{"api-key": c.gemini_key}]}],
            "xai": [{"name": "ca-xai", "base-url": mock + "/xai/v1", "proxy-url": "direct",
                     "models": models(["grok-imagine-image", "grok-imagine-video", "grok-tts"]), "keys": [{"api-key": c.xai_key}]}],
            "openai-compatibility": [
                {"name": "fixture-openai", "base-url": mock + "/v1", "keys": [{"api-key": c.openai_key, "proxy-url": "direct"}],
                 "models": models(["ca-openai"])},
                {"name": "fixture-userinfo", "base-url": userinfo, "keys": [{"api-key": c.openai_key, "proxy-url": "direct"}],
                 "models": models(["ca-openai-userinfo"])}],
        },
        "plugins": {"enabled": plugins, "dir": str(directory / "plugins"), "configs": configs if plugins else {}},
    }


class CPA:
    def __init__(self, binary, directory, config, c, env_extra=None):
        self.binary, self.directory, self.config, self.c = binary, directory, config, c
        self.base = "http://127.0.0.1:" + str(config["server"]["port"])
        self.env_extra = env_extra or {}
        self.generation = 0
        self.proc = None

    def start(self, wait_plugin=True):
        self.generation += 1
        path = self.directory / "config.yaml"
        path.write_text(json.dumps(self.config))
        path.chmod(0o600)
        env = {"PATH": os.environ["PATH"], "HOME": str(self.directory), "XDG_CACHE_HOME": str(self.directory / "cache"),
               "HTTP_PROXY": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1", "NO_PROXY": "127.0.0.1,localhost",
               "CA_TEST_SOURCE_TOKEN": self.c.source_token or "", "CA_TEST_HMAC_KEY": self.c.hmac_key, **self.env_extra}
        self.log_path = self.directory / f"cpa-{self.generation}.log"
        self.log = self.log_path.open("wb")
        self.proc = subprocess.Popen([str(self.binary), "-config", str(path), "-local-model"], cwd=self.directory, env=env,
                                     stdout=self.log, stderr=subprocess.STDOUT)
        try:
            if wait_plugin:
                wait_until(self.ready, 60, "CPA and chat-archive ready (" + str(self.log_path) + ")", 0.2)
            else:
                wait_until(lambda: request(self.base, "/healthz")[0] == 200, 60, "CPA healthz", 0.2)
        except BaseException:
            self.stop(check_exit=False)
            raise
        return self

    def ready(self):
        if self.proc.poll() is not None:
            raise Failure("CPA exited during start: " + str(self.log_path))
        code, _, raw = request(self.base, STATUS, key=self.c.management_key, timeout=5)
        if code != 200:
            return None
        status = json.loads(raw)
        return status if status.get("state") in ("running", "degraded") else None

    def status(self):
        code, _, raw = request(self.base, STATUS, key=self.c.management_key, timeout=20)
        check(code == 200, "status", code)
        return json.loads(raw), raw

    def stop(self, check_exit=True, timeout=40):
        if self.proc is None:
            return None
        try:
            if self.proc.poll() is None:
                self.proc.send_signal(signal.SIGTERM)
            try:
                code = self.proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
                raise Failure("CPA did not stop within %d s" % timeout)
            if check_exit:
                check(code == 0, "CPA exit code", code)
            return code
        finally:
            self.log.close()
            self.proc = None

    def logs(self):
        return b"".join(p.read_bytes() for p in sorted(self.directory.glob("cpa-*.log")))


def client_headers(c, key_header="authorization", extra=None):
    """The request header canaries H1-H8 every capture request carries."""
    h = {"Proxy-Authorization": c.secret["H4_proxy_authorization"], "Cookie": c.secret["H5_cookie"],
         "X-Custom-Token": c.secret["H6_custom_token"], "X-Claude-Code-Prompt-Id": c.body["H7_prompt_id"],
         "X-Archive-Machine": c.body["H8_archive_machine"], "X-Archive-Foo": c.secret["H8_archive_foo"],
         "X-Archive-Origin": "native-acceptance", "Anthropic-Version": "2023-06-01"}
    if key_header == "x-api-key":
        h["X-Api-Key"] = c.client_key_2
    elif key_header == "x-goog-api-key":
        h["X-Goog-Api-Key"] = c.client_key
    else:
        h["Authorization"] = "Bearer " + c.client_key
    h.update(extra or {})
    return h


def stream_open(base, path, body, headers, timeout=60):
    """POST and return (connection, response) without reading the body."""
    host, port = base.split("//", 1)[1].split(":")
    conn = http.client.HTTPConnection(host, int(port), timeout=timeout)
    raw = json.dumps(body).encode()
    conn.request("POST", path, body=raw, headers={**headers, "Content-Type": "application/json", "Content-Length": str(len(raw))})
    return conn, conn.getresponse()


def decoded_outbox(directory, fake):
    """Raw bytes of every outbox file plus every zstd value decoded (rows and blob files)."""
    raw_parts, decoded_parts, modes = [], [], []
    for path in sorted(directory.rglob("*")):
        st = path.lstat()
        modes.append((str(path.relative_to(directory)), st.st_mode & 0o777, path.is_dir()))
        if path.is_file():
            data = path.read_bytes()
            raw_parts.append(data)
            if data.startswith(ZSTD_MAGIC):
                decoded_parts.append(fake.unzstd(data))
    db = directory / "outbox.sqlite"
    integrity = None
    if db.exists():
        with sqlite3.connect(f"file:{db}?mode=ro", uri=True) as conn:
            integrity = conn.execute("PRAGMA integrity_check").fetchone()[0]
            tables = [r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")]
            for table in tables:
                for row in conn.execute(f'SELECT * FROM "{table}"'):
                    for value in row:
                        if isinstance(value, bytes):
                            decoded_parts.append(fake.unzstd(value) if value.startswith(ZSTD_MAGIC) else value)
                        elif isinstance(value, str):
                            decoded_parts.append(value.encode())
    return b"\n".join(raw_parts), b"\n".join(decoded_parts), modes, integrity


def body_text(env):
    """canon/v1 body of an envelope as compact JSON bytes (for structural checks only)."""
    return json.dumps(env.get("body"), separators=(",", ":")).encode()


def b64d(s):
    return base64.b64decode(s)
