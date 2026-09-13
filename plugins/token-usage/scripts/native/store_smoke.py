#!/usr/bin/env python3
"""Actual official CPA image: HTTP store install -> native -> default volume -> browser.

The unmodified image has no Python/browser runtime additions. A disposable Python
fixture shares only its network namespace; all registry and upstream traffic is
loopback. The Docker bridge is internal, with a fixed-destination host-loopback
reverse proxy providing browser ingress (Docker 29 ignores internal-bridge -p). No operator config/environment is read.
"""
import argparse
import hashlib
import importlib.util
import http.client
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import tempfile
import threading
import time
from http.server import ThreadingHTTPServer
from urllib.error import URLError
from urllib.request import Request
import uuid

from http_contract import assert_public_headers
import production
import spike

ROOT = Path(__file__).resolve().parents[2]
IMAGE = "eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b"
PYTHON_IMAGE = "python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285"
VERSION = "0.1.2"
PUBLIC = "/v0/resource/plugins/token-usage/status"
STORE_URL = "http://127.0.0.1:8318/registry.json"
BAD_URL = "http://127.0.0.1:8318/bad-registry.json"
DATABASE = "/CLIProxyAPI/plugins/data/token-usage/usage.sqlite"
OLD_SHA256 = "2d5ca4d6b5dfe9cb449020a848866288eacb14b011d7c92b6c507d282b17716d"
SPEC = importlib.util.spec_from_file_location("release", ROOT / "scripts/release.py")
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


def sanitize(text):
    for key in production.CANARIES:
        text = text.replace(key, "<REDACTED>")
    return text


def docker(*args, timeout=60):
    result = subprocess.run(["docker", *map(str, args)], stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(sanitize("docker " + str(args[:2]) + ": " + result.stdout))
    return result.stdout.strip()


def source_id(url):
    return "source-" + hashlib.sha256(url.encode()).hexdigest()[:12]


def fixture_server(directory):
    class Fixture(spike.Mock):
        def do_GET(self):
            paths = {"/registry.json": "registry.json", "/bad-registry.json": "bad-registry.json",
                     "/token-usage.zip": "token-usage.zip"}
            name = paths.get(self.path)
            assert self.headers.get("Authorization") is None, "store must not receive management/upstream credentials"
            if name is None:
                self.reply('{"error":"not_found"}', code=404)
                return
            raw = (directory / name).read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", "application/zip" if name.endswith("zip") else "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
        def do_POST(self):
            assert self.path == "/v1/chat/completions"
            assert self.headers.get("Authorization") == "Bearer " + spike.UPSTREAM_KEY
            super().do_POST()
    server = ThreadingHTTPServer(("127.0.0.1", 8318), Fixture)
    print("Synthetic registry/upstream ready", flush=True)
    server.serve_forever()


def request_json(base, path, **kwargs):
    code, raw = spike.request(base, path, **kwargs)
    assert code == 200, (path, code, sanitize(raw.decode()))
    return json.loads(raw)


def inventory(base):
    result = request_json(base, "/v0/management/plugins")
    return next((p for p in result["plugins"] if p["id"] == "token-usage"), None)


def wait_inventory(base, container, predicate, label):
    deadline = time.monotonic() + 25
    last = None
    while time.monotonic() < deadline:
        assert docker("inspect", "--format", "{{.State.Running}}", container) == "true", "CPA exited during " + label
        try:
            last = inventory(base)
            if predicate(last):
                return last
        except (URLError, ConnectionError):
            pass  # Only startup connection readiness is polled, not assertion failures.
        time.sleep(0.1)
    raise AssertionError((label, last))


def assert_discovery(item):
    for field in ("configured", "enabled", "registered", "effective_enabled"):
        assert item[field] is True, (field, item)
    assert item["metadata"]["version"] == VERSION, item
    fields = item["config_fields"]
    assert any(f["name"] == "database-path" for f in fields), fields
    assert len(item["menus"]) == 1, item["menus"]
    assert item["menus"][0]["menu"] == "Token Usage"
    assert item["menus"][0]["path"] == PUBLIC


def assert_generated_config(base, version):
    # Generic /config intentionally omits opaque YAML extras. This actual
    # management endpoint returns the preserved generated plug-in YAML object.
    item = request_json(base, "/v0/management/plugins/token-usage/config")
    assert item["enabled"] is True
    assert "database-path" not in item, "regression requires handler-generated configuration WITHOUT a database override"
    assert set(item) == {"enabled", "store"}, item.keys()
    store = item["store"]
    assert store["id"] == "token-usage" and store["version"] == version, store
    assert store["source-url"] == STORE_URL and store["install"]["type"] == "direct", store
    return item  # Contains only synthetic public store metadata, no CPA credentials.


def public_shell(base, expected=None):
    # Preserve and assert the headers from real CPA, not merely the ABI response.
    with spike.OPENER.open(Request(base + PUBLIC), timeout=10) as response:
        code, raw = response.status, response.read()
        assert_public_headers(response.headers, raw)
    assert code == 200 and raw.startswith(b"<!doctype html>"), code
    production.no_leak(raw)
    assert b"oa-normal" not in raw and b"fixture_records" not in raw
    if expected is not None:
        assert raw == expected, "public shell changed with collection/auth/storage state"
    for key in (None, spike.MANAGEMENT_KEY, "wrong-synthetic-key"):
        assert spike.request(base, PUBLIC + "?provider=ignored&managementKey=ignored", key=key) == (200, raw)
    for path in ("/v0/resource/plugins/token-usage/summary", "/v0/resource/plugins/token-usage/models",
                 "/v0/resource/token-usage/status", "/v0/management/v0/resource/plugins/token-usage/status"):
        assert spike.request(base, path, key=None)[0] in (401, 404), path
    return raw


def private_auth(base, unavailable=False):
    for route in ("status", "summary", "models"):
        # Reset CPA's IP failure counter between each pair of negative probes.
        request_json(base, "/v0/management/plugins")
        for key in (None, "wrong-synthetic-key"):
            code, raw = spike.request(base, production.BASE + route, key=key)
            assert code in (401, 403), (route, code)
            production.no_leak(raw)
            assert b"oa-normal" not in raw and b"coverage" not in raw
        if unavailable:
            code, raw = spike.request(base, production.BASE + route)
            assert code == 503, (route, code)
            value = json.loads(raw)
            assert value["error"] == "storage_unavailable", value
            assert "totals" not in value and "coverage" not in value


def browser(base, directory, expected, *, timeout=180):
    session = "token-usage-native-" + uuid.uuid4().hex
    environment = {**os.environ, "TOKEN_USAGE_NATIVE_BROWSER_SESSION": session}
    log = directory.parent / (directory.name + ".log")
    try:
        try:
            result = subprocess.run(["node", str(ROOT / "scripts/browser/native.cjs"), base, str(directory), str(expected)],
                                    env=environment, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=timeout)
        except subprocess.TimeoutExpired as error:
            output = error.stdout or b""
            if isinstance(output, bytes):
                output = output.decode("utf-8", errors="replace")
            log.write_text(sanitize(output))
            raise RuntimeError("native browser acceptance timed out") from error
        output = sanitize(result.stdout)
        log.write_text(output)
        print(output, end="", flush=True)
        assert result.returncode == 0, "native browser acceptance failed"
    except BaseException:
        # Node's finally cannot run after a parent timeout kills it. The parent
        # owns the exact unique session and closes ONLY that session, boundedly.
        try:
            closed = subprocess.run([environment["TOKEN_USAGE_AGENT_BROWSER"], "--session", session, "--json", "close"],
                                    env=environment, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=15)
            payload = json.loads(closed.stdout)
            if closed.returncode != 0 or not isinstance(payload, dict) or payload.get("success") is not True:
                raise RuntimeError(closed.stdout)
        except (OSError, subprocess.TimeoutExpired, ValueError, RuntimeError) as cleanup_error:
            print("Native browser cleanup failed: " + sanitize(str(cleanup_error)), flush=True)
        raise


def loopback_ingress(ip, port):
    # Docker 29 intentionally ignores -p for internal bridges. This test-only
    # fixed-destination reverse proxy gives the browser host-loopback ingress;
    # it does not add Docker egress or alter any CPA response/body/auth policy.
    from http.server import BaseHTTPRequestHandler
    class Ingress(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass
        def forward(self):
            assert self.path.startswith("/") and not self.path.startswith("//")
            length = int(self.headers.get("Content-Length", "0"))
            assert 0 <= length <= 1048576
            body = self.rfile.read(length) if length else None
            connection = http.client.HTTPConnection(ip, 8317, timeout=15)
            try:
                connection.request(self.command, self.path, body, dict(self.headers))
                response = connection.getresponse()
                raw = response.read()
                self.send_response_only(response.status)
                for key, value in response.getheaders():
                    if key.lower() not in ("connection", "transfer-encoding", "content-length"):
                        self.send_header(key, value)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
            except ConnectionRefusedError:
                # During an explicit CPA restart, propagate a closed connection
                # to the readiness poll without noisy handler-thread tracebacks.
                self.close_connection = True
            finally:
                connection.close()
        do_GET = forward
        do_POST = forward
    server = ThreadingHTTPServer(("127.0.0.1", port), Ingress)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread


def run_case(parent, library, version, *, unavailable=False, old=False, expected_shell=None):
    name = "old-release-red" if old else "storage-unavailable" if unavailable else "installed"
    directory = parent / name
    directory.mkdir()
    package = directory / "package"
    package.mkdir()
    shutil.copyfile(library, package / "token-usage.so")
    # The old release is an explicitly hash-verified local immutable artifact;
    # its negative fixture is never prepared by candidate publication tooling.
    if old:
        import zipfile
        with zipfile.ZipFile(package / "token-usage.zip", "w") as out:
            out.write(library, "token-usage.so")
    else:
        shutil.move(release.package(package, VERSION), package / "token-usage.zip")
    archive = package / "token-usage.zip"
    registry = release.source_registry(VERSION)
    registry["plugins"][0]["version"] = version
    registry["plugins"][0]["install"] = {"type": "direct", "artifacts": [{"goos": "linux", "goarch": "amd64",
        "url": "http://127.0.0.1:8318/token-usage.zip", "sha256": release.digest(archive), "size": archive.stat().st_size}]}
    (package / "registry.json").write_text(json.dumps(registry))
    bad = json.loads(json.dumps(registry))
    bad["plugins"][0]["install"]["artifacts"][0]["sha256"] = "0" * 64
    (package / "bad-registry.json").write_text(json.dumps(bad))
    suffix = parent.name + "-" + name
    network, container, fixture, volume = (suffix + ending for ending in ("-net", "-cpa", "-fixture", "-plugins"))
    port = spike.free_port()
    base = "http://127.0.0.1:" + str(port)
    cfg = spike.config(Path("/tmp/synthetic"), 8317, "http://127.0.0.1:8318", False)
    cfg["host"] = "0.0.0.0"  # Only the fixed host-loopback ingress can reach it.
    cfg["remote-management"]["allow-remote"] = True
    cfg["plugins"] = {"enabled": True, "dir": "plugins", "store-sources": [STORE_URL, BAD_URL], "configs": {},
        # CPA's existing policy, scoped ONLY to this locally controlled fixture;
        # no transport patch, broad allowlist, or production configuration change.
        "store-auth": [{"match": "http://127.0.0.1:8318/", "type": "none", "allow-insecure": True}]}
    cfg["claude-api-key"] = []
    config_dir = directory / "config"
    config_dir.mkdir()
    (config_dir / "config.yaml").write_text(json.dumps(cfg))
    (config_dir / "config.yaml").chmod(0o600)
    made = []
    ingress = None
    try:
        docker("network", "create", "--internal", network)
        made.append(("network", network))
        assert json.loads(docker("network", "inspect", network))[0]["Internal"] is True
        docker("volume", "create", volume)
        made.append(("volume", volume))
        docker("run", "-d", "--name", container, "--platform", "linux/amd64", "--network", network,
               "--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--security-opt", "no-new-privileges",
               "--pids-limit", "256", "--memory", "2g",
               "-e", "HTTP_PROXY=http://127.0.0.1:1", "-e", "HTTPS_PROXY=http://127.0.0.1:1", "-e", "NO_PROXY=127.0.0.1,localhost",
               "--mount", f"type=volume,src={volume},dst=/CLIProxyAPI/plugins",
               "--mount", f"type=bind,src={config_dir},dst=/fixture-config",
               IMAGE, "./CLIProxyAPI", "-config", "/fixture-config/config.yaml", "-local-model")
        made.append(("container", container))
        mounts = json.loads(docker("inspect", container))[0]["Mounts"]
        assert [(m["Destination"]) for m in mounts if m["Type"] == "volume"] == ["/CLIProxyAPI/plugins"]
        environment = docker("exec", container, "/bin/bash", "-ec", "pwd; getconf GNU_LIBC_VERSION")
        assert environment == "/CLIProxyAPI\nglibc 2.36", environment
        deadline = time.monotonic() + 15
        while "API server started successfully" not in docker("logs", container):
            assert docker("inspect", "--format", "{{.State.Running}}", container) == "true", "CPA exited"
            assert time.monotonic() < deadline, "CPA initial readiness timeout"
            time.sleep(0.1)
        ip = json.loads(docker("inspect", container))[0]["NetworkSettings"]["Networks"][network]["IPAddress"]
        ingress = loopback_ingress(ip, port)
        wait_inventory(base, container, lambda item: item is None, "empty initial inventory")
        docker("run", "-d", "--name", fixture, "--platform", "linux/amd64", "--network", "container:" + container,
               "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
               "--mount", f"type=bind,src={ROOT},dst={ROOT},readonly",
               "--mount", f"type=bind,src={package},dst=/fixture-package,readonly",
               PYTHON_IMAGE, "python", str(Path(__file__).resolve()), "--serve", "/fixture-package")
        made.append(("container", fixture))
        deadline = time.monotonic() + 15
        while "Synthetic registry/upstream ready" not in docker("logs", fixture):
            assert docker("inspect", "--format", "{{.State.Running}}", fixture) == "true", "fixture exited"
            assert time.monotonic() < deadline, "fixture readiness timeout"
            time.sleep(0.1)
        if unavailable:
            docker("exec", container, "/bin/bash", "-ec", "mkdir -p /CLIProxyAPI/plugins/data/token-usage/usage.sqlite")
        # Actual handler rejects bad SHA before enabling/writing a plug-in config.
        path = "/v0/management/plugin-store/token-usage/install"
        code, raw = spike.request(base, path + "?source=" + source_id(BAD_URL), body={})
        assert code == 502 and json.loads(raw)["error"] == "plugin_install_failed", (code, sanitize(raw.decode()))
        assert inventory(base) is None
        code, raw = spike.request(base, path + "?source=" + source_id(STORE_URL), body={})
        assert code == 200, (code, sanitize(raw.decode()))
        installed = json.loads(raw)
        assert installed["status"] == "installed" and installed["version"] == version, installed
        (directory / "handler-install.json").write_text(json.dumps(installed, indent=2))
        generated = assert_generated_config(base, version)
        (directory / "handler-generated-plugin-config.json").write_text(json.dumps(generated, indent=2))
        # The installed native bytes must be the exact production .so supplied.
        copied = directory / "installed.so"
        installed_path = str(Path("/CLIProxyAPI") / installed["path"])
        assert installed_path.startswith("/CLIProxyAPI/plugins/")
        docker("cp", container + ":" + installed_path, copied)
        assert copied.read_bytes() == library.read_bytes()
        if old:
            item = wait_inventory(base, container, lambda p: p is not None and p["configured"], "old release registration")
            # A successful inventory call is not registration success: the exact
            # prior bug reports enabled/configured but has no native registration.
            assert item["registered"] is False and item["effective_enabled"] is False, item
            assert spike.request(base, production.BASE + "status")[0] == 404
            (directory / "inventory.json").write_text(json.dumps(item, indent=2))
            print("PASS RED CONTROL: immutable local v0.1.0 installs through actual handler but fails native registration with generated store/no database-path", flush=True)
            return None
        item = wait_inventory(base, container, lambda p: p is not None and p["registered"], "native registration after store install")
        assert_discovery(item)
        (directory / "inventory.json").write_text(json.dumps(item, indent=2))
        shell = public_shell(base, expected_shell)
        private_auth(base, unavailable)
        if unavailable:
            production.send(base, "alias-oa-normal")
            code, raw = spike.request(base, production.BASE + "status")
            value = json.loads(raw)
            assert code == 503 and value["state"] == "unavailable" and "coverage" not in value
            public_shell(base, shell)
            print("PASS: store-generated config + unavailable default storage remain discoverable; private 503, no fake history, fixed public shell", flush=True)
            return shell
        # Same-artifact installation is idempotent at the ACTUAL handler too.
        again = request_json(base, path + "?source=" + source_id(STORE_URL), body={})
        assert again["path"] == installed["path"]
        assert_generated_config(base, VERSION)
        production.send(base, "alias-oa-normal")
        status = production.wait_committed(base, 1)
        interval = {"from": status["coverage"]["from"], "to": production.now()}
        summary = production.query(base, "summary", interval)
        assert summary["totals"]["observed_events"] == "1"
        browser(base, directory / "browser-before-restart", 1)
        public_shell(base, shell)
        # Restart the SAME container/one plugins volume. The fixture also rejoins
        # CPA's new loopback namespace; it never injects config or usage records.
        docker("stop", "--time", "5", fixture)
        docker("stop", "--time", "15", container)
        assert docker("inspect", "--format", "{{.State.ExitCode}}", container) == "0"
        docker("start", container)
        wait_inventory(base, container, lambda p: p is not None and p["registered"], "same-volume restart")
        assert production.query(base, "summary", interval)["totals"] == summary["totals"]
        docker("start", fixture)
        deadline = time.monotonic() + 15
        while docker("logs", fixture).count("Synthetic registry/upstream ready") != 2:
            assert docker("inspect", "--format", "{{.State.Running}}", fixture) == "true", "restarted fixture exited"
            assert time.monotonic() < deadline, "restarted fixture readiness timeout"
            time.sleep(0.1)
        production.send(base, "alias-oa-normal")
        production.wait_committed(base, 2)
        time.sleep(0.3)  # Explicit delayed-duplicate rejection, not success retry.
        production.wait_committed(base, 2)
        browser(base, directory / "browser-after-restart", 2)
        public_shell(base, shell)
        private_auth(base)
        # Verify modes while running, then use a quiescent copy after clean exit.
        permissions = docker("exec", container, "/bin/bash", "-ec",
            "stat -c '%a %u:%g %n' /CLIProxyAPI/plugins /CLIProxyAPI/plugins/data/token-usage /CLIProxyAPI/plugins/data/token-usage/*")
        (directory / "permissions.txt").write_text(permissions + "\n")
        rows = permissions.splitlines()
        assert rows[0].startswith("755 ") and rows[1].startswith("700 ") and all(row.startswith("600 ") for row in rows[2:]), rows
        docker("stop", "--time", "15", container)
        assert docker("inspect", "--format", "{{.State.ExitCode}}", container) == "0"
        target = directory / "database"
        target.mkdir()
        docker("cp", container + ":/CLIProxyAPI/plugins/data/token-usage/.", target)
        with sqlite3.connect(f"file:{target / 'usage.sqlite'}?mode=ro", uri=True) as db:
            assert db.execute("PRAGMA integrity_check").fetchone() == ("ok",)
            assert db.execute("SELECT COUNT(*),SUM(input_tokens),SUM(output_tokens) FROM usage_events").fetchone() == (2, 200, 40)
            assert db.execute("SELECT COUNT(*) FROM collection_runs WHERE clean=0").fetchone() == (0,)
            production.no_leak("\n".join(db.iterdump()).encode())
        (directory / "results.json").write_text(json.dumps({"image": IMAGE, "cpa_pin": spike.PIN,
            "version": VERSION, "production_library_sha256": release.digest(library), "database_path": DATABASE,
            "plugins_volumes": 1, "committed_events_after_restart": 2, "sqlite_integrity": "ok",
            "browser_seam": "native page + pinned official codec remembered session + real CPA JSON; not full console sign-in/navigation"}, indent=2))
        print("PASS: actual HTTP store handler -> generated enabled/store/no database-path -> native metadata/menu/auth -> executor -> default SQLite on ONE plugins volume -> restart +1 -> native browser", flush=True)
        return shell
    finally:
        if ingress:
            ingress[0].shutdown()
            ingress[0].server_close()
            ingress[1].join()
        for kind, target in reversed(made):
            if kind == "container":
                (directory / ("fixture.log" if target == fixture else "cpa.log")).write_text(sanitize(docker("logs", target)))
                docker("rm", "--force", target)
            else:
                docker(kind, "rm", target)
        # The config is synthetic, but retain only redacted logs/plugin metadata.
        for path in config_dir.iterdir():
            path.unlink()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--library", type=Path)
    parser.add_argument("--old-library", type=Path, help="optional immutable LOCAL v0.1.0 red control, exact released SHA required")
    parser.add_argument("--serve", type=Path, help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.serve:
        fixture_server(args.serve)
        return
    if not args.library:
        parser.error("--library is required")
    release.validate_library(args.library.read_bytes())
    assert os.environ.get("TOKEN_USAGE_AGENT_BROWSER"), "Use make store-smoke (mandatory pinned browser gate)"
    assert subprocess.check_output(["node", "--version"], text=True).strip() == "v26.8.1"
    assert subprocess.check_output([os.environ["TOKEN_USAGE_AGENT_BROWSER"], "--version"], text=True).strip() == "agent-browser 0.37.1"
    assert release.digest(Path(os.environ["TOKEN_USAGE_AGENT_BROWSER"])) == "f8e5f9294bd0da70dda61854f12004fd61c668cd682bfb600cdf6d0df73dea69"
    for image in (IMAGE, PYTHON_IMAGE):
        docker("image", "inspect", image)  # Missing pinned images fail; no mutable fallback.
    info = json.loads(docker("image", "inspect", IMAGE))[0]
    assert info["Os"] == "linux" and info["Architecture"] == "amd64" and info["Config"]["WorkingDir"] == "/CLIProxyAPI"
    directory = Path(tempfile.mkdtemp(prefix="token-usage-store-"))
    print("Actual store/image/browser acceptance artifacts: " + str(directory), flush=True)
    if args.old_library:
        assert release.digest(args.old_library) == OLD_SHA256, "not the immutable released v0.1.0 native library"
        run_case(directory, args.old_library.resolve(), "0.1.0", old=True)
    shell = run_case(directory, args.library.resolve(), VERSION)
    run_case(directory, args.library.resolve(), VERSION, unavailable=True, expected_shell=shell)
    print("PASS: disposable official-image store/native/browser acceptance; no production or provider access", flush=True)


if __name__ == "__main__":
    main()
