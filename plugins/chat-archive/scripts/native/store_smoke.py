#!/usr/bin/env python3
"""Official CPA v8.0.60 image: Plugin Store install -> registration beside every published peer
-> configure -> capture to the fake archive -> default data path and modes.

The unmodified image runs on an --internal Docker network. Published peer libraries are
downloaded from the root registry.json with sha256 checks before that network exists. A
disposable Python fixture shares only CPA's network namespace and the plugins volume; it serves
the synthetic store, the mock upstream and the fake archive on loopback and drives the checks.
No operator config or environment is read; every value is synthetic.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1] if len(HERE.parents) > 1 else HERE
REPO = ROOT.parents[1] if len(ROOT.parents) > 1 else ROOT  # unused inside the fixture container
IMAGE = "eceasy/cli-proxy-api@sha256:873ec3df3b673a82f7d956d4636c7736b0c961dc6ee28f9fb403238b2898dc0d"
PYTHON_IMAGE = "python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285"
VERSION = "0.1.0"
STORE_URL = "http://127.0.0.1:8318/registry.json"
BAD_URL = "http://127.0.0.1:8318/bad-registry.json"
MANAGEMENT_KEY = "ca-store-smoke-management-" + os.urandom(8).hex()


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def docker(*args, timeout=120):
    result = subprocess.run(["docker", *map(str, args)], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError("docker " + " ".join(map(str, args[:2])) + " failed: " + result.stdout[-400:].replace(MANAGEMENT_KEY, "<key>"))
    return result.stdout.strip()


PEER_CONFIG = {
    "quota-cache": {"enabled": True, "request-spacing": "1s"},
    "account-health-pushover": {"enabled": True, "quota-alerts": True, "use-quota-cache": True},
    "reset-priority": {"enabled": True, "dry-run": True, "use-quota-cache": True},
    "auto-baseline": {"enabled": True, "dry-run": True, "config-path": "/fixture-config/config.yaml"},
    "token-usage": {"enabled": True},
    "quota-glance": {"enabled": True, "cache-path": "/work/quota-cache-snapshot.json", "data-dir": "/work/quota-glance"},
    "codex-catalog-filter": {"enabled": True, "models": {"claude-fable-5-1": False}},
}


def host_main(args):
    release = load("release", ROOT / "scripts/release.py")
    sys.path.insert(0, str(REPO / "scripts"))
    plugin_release = load("plugin_release", REPO / "scripts/plugin_release.py")
    release.validate_library(args.library.read_bytes())
    for image in (IMAGE, PYTHON_IMAGE):
        docker("image", "inspect", image)  # pinned images must already be pulled; no mutable fallback
    work = Path(tempfile.mkdtemp(prefix="chat-archive-store-"))
    print("Official-image store acceptance artifacts: " + str(work), flush=True)
    package, peers, cfgdir, share = work / "package", work / "peers", work / "config", work / "share"
    for d in (package, peers, cfgdir, share):
        d.mkdir()
    shutil.copyfile(args.library, package / "chat-archive.so")
    zipped = release.package(package, VERSION)
    shutil.move(zipped, package / "chat-archive.zip")
    archive = package / "chat-archive.zip"
    registry = release.source_registry(VERSION)
    registry["plugins"][0]["install"] = {"type": "direct", "artifacts": [{"goos": "linux", "goarch": "amd64",
        "url": "http://127.0.0.1:8318/chat-archive.zip", "sha256": release.digest(archive), "size": archive.stat().st_size}]}
    (package / "registry.json").write_text(json.dumps(registry))
    registry["plugins"][0]["install"]["artifacts"][0]["sha256"] = "0" * 64
    (package / "bad-registry.json").write_text(json.dumps(registry))
    catalog = plugin_release.load_catalog()
    published = [p for p in catalog["plugins"] if p["id"] != "chat-archive"]
    if args.skip_peers:
        # Local diagnosis only (for example when GitHub release downloads fail); never in CI.
        print("WARNING: --skip-peers: published peers NOT loaded; coexistence is unverified", flush=True)
        published = []
    for entry in published:
        # GitHub release downloads answered 504 intermittently on 2026-11-08: three bounded
        # attempts per peer; the catalog size and sha256 are checked on every attempt.
        for attempt in range(3):
            try:
                (peers / (entry["id"] + ".so")).write_bytes(plugin_release.released_library(entry))
                break
            except OSError:
                if attempt == 2:
                    raise
                time.sleep(15)
    print("Downloaded and sha256-verified %d published peers" % len(published), flush=True)
    configs = {p["id"]: PEER_CONFIG.get(p["id"], {"enabled": True}) for p in published}
    config = {"server": {"host": "127.0.0.1", "port": 8317},
              "management": {"allow-remote": False, "secret-key": MANAGEMENT_KEY, "disable-control-panel": True,
                             "disable-auto-update-panel": True},
              "access": {"api-keys": ["ca-store-client-" + os.urandom(8).hex()]},
              "oauth": {"auth-dir": "/work/auth"},
              "api-keys": {"openai-compatibility": [{"name": "fixture-openai", "base-url": "http://127.0.0.1:8319/v1",
                  "keys": [{"api-key": "ca-store-upstream", "proxy-url": "direct"}], "models": [{"name": "ca-openai"}]}]},
              "plugins": {"enabled": True, "dir": "plugins", "store-sources": [STORE_URL, BAD_URL], "configs": configs,
                          "store-auth": [{"match": "http://127.0.0.1:8318/", "type": "none", "allow-insecure": True}]}}
    (cfgdir / "config.yaml").write_text(json.dumps(config))
    (cfgdir / "config.yaml").chmod(0o644)
    (share / "quota-cache-snapshot.json").write_text(json.dumps({"schema": 1, "written_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "next_request": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "provider_cooldown": {}, "entries": {}}))
    (share / "auth").mkdir()
    suffix = "ca-test-store-" + os.urandom(4).hex()
    network, volume, cpa, fixture, seed = suffix + "-net", suffix + "-plugins", suffix + "-cpa", suffix + "-fixture", suffix + "-seed"
    made = []
    code = 1
    try:
        docker("network", "create", "--internal", network)
        made.append(("network", network))
        docker("volume", "create", volume)
        made.append(("volume", volume))
        if published:
            docker("run", "--rm", "--name", seed, "--network", "none", "--cpus", "1", "--memory", "256m",
                   "--mount", f"type=volume,src={volume},dst=/plugins", "--mount", f"type=bind,src={peers},dst=/peers,readonly",
                   PYTHON_IMAGE, "sh", "-ec", "cp /peers/*.so /plugins/ && chmod 0644 /plugins/*.so")
        docker("run", "-d", "--name", cpa, "--platform", "linux/amd64", "--network", network, "--cpus", "2", "--memory", "2g",
               "--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "CHOWN", "--cap-add", "FOWNER",
               "--security-opt", "no-new-privileges", "--pids-limit", "512",
               "-e", "HTTP_PROXY=http://127.0.0.1:1", "-e", "HTTPS_PROXY=http://127.0.0.1:1", "-e", "NO_PROXY=127.0.0.1,localhost",
               "-e", "CPA_PUSHOVER_APP_TOKEN=" + "A" * 30, "-e", "CPA_PUSHOVER_USER_KEY=" + "B" * 30,
               "--mount", f"type=volume,src={volume},dst=/CLIProxyAPI/plugins",
               "--mount", f"type=bind,src={cfgdir},dst=/fixture-config", "--mount", f"type=bind,src={share},dst=/work",
               IMAGE, "./CLIProxyAPI", "-config", "/fixture-config/config.yaml", "-local-model")
        made.append(("container", cpa))
        deadline = time.monotonic() + 30
        while "API server started successfully" not in docker("logs", cpa):
            if docker("inspect", "--format", "{{.State.Running}}", cpa) != "true":
                raise RuntimeError("CPA exited at start")
            if time.monotonic() > deadline:
                raise RuntimeError("CPA readiness timeout")
            time.sleep(0.2)
        docker("run", "-d", "--name", fixture, "--network", "container:" + cpa, "--read-only", "--cap-drop", "ALL",
               "--security-opt", "no-new-privileges", "--cpus", "1", "--memory", "1g", "-e", "PYTHONDONTWRITEBYTECODE=1",
               "-e", "CA_MANAGEMENT_KEY=" + MANAGEMENT_KEY, "-e", "CA_PEERS=" + ",".join(p["id"] for p in published),
               "-e", "CA_SKIP_PEERS=" + ("1" if args.skip_peers else ""),
               "-e", "CA_CLIENT_KEY=" + config["access"]["api-keys"][0],
               "--mount", f"type=volume,src={volume},dst=/CLIProxyAPI/plugins",
               "--mount", f"type=bind,src={HERE},dst=/fixture/scripts,readonly",
               "--mount", f"type=bind,src={package},dst=/fixture/package,readonly",
               "--mount", f"type=bind,src={args.fakearchive},dst=/fixture/fakearchive,readonly",
               "--tmpfs", "/tmp:rw,size=256m",
               PYTHON_IMAGE, "python", "/fixture/scripts/store_smoke.py", "--drive")
        made.append(("container", fixture))
        result = docker("wait", fixture, timeout=600)
        out = docker("logs", fixture).replace(MANAGEMENT_KEY, "<key>")
        (work / "fixture.log").write_text(out)
        print(out, flush=True)
        if result.strip() != "0":
            raise RuntimeError("store smoke drive failed")
        docker("stop", "--time", "20", cpa)
        exit_code = docker("inspect", "--format", "{{.State.ExitCode}}", cpa)
        if exit_code != "0":
            raise RuntimeError("CPA exit code " + exit_code)
        code = 0
        print("PASS: official v8.0.60 image: store install, peers coexist, configure, capture, default volume, clean exit", flush=True)
    finally:
        for kind, target in reversed(made):
            if kind == "container":
                try:
                    (work / (target.rsplit("-", 1)[1] + ".log")).write_text(docker("logs", target).replace(MANAGEMENT_KEY, "<key>"))
                except RuntimeError:
                    pass
                subprocess.run(["docker", "rm", "--force", target], capture_output=True)
            else:
                subprocess.run(["docker", kind, "rm", target], capture_output=True)
        (cfgdir / "config.yaml").unlink(missing_ok=True)
    sys.exit(code)


def drive():
    """Runs inside the fixture container (CPA's network namespace)."""
    import hashlib
    import threading
    from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
    sys.path.insert(0, "/fixture/scripts")
    import canaries as canarylib
    import harness as H
    import http_contract
    import mockup

    key = os.environ["CA_MANAGEMENT_KEY"]
    peers = [p for p in os.environ["CA_PEERS"].split(",") if p]
    base = "http://127.0.0.1:8317"

    class Store(SimpleHTTPRequestHandler):
        def __init__(self, *a, **kw):
            super().__init__(*a, directory="/fixture/package", **kw)

        def log_message(self, *a):
            pass
    store = ThreadingHTTPServer(("127.0.0.1", 8318), Store)
    threading.Thread(target=store.serve_forever, daemon=True).start()
    c = canarylib.Canaries()
    upstream = ThreadingHTTPServer(("127.0.0.1", 8319), mockup.make_handler(mockup.State(c)))
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    work = Path("/tmp/drive")
    work.mkdir()
    fake = H.Fake(Path("/fixture/fakearchive"), work)

    def mgmt(path, method="GET", body=None):
        return H.request(base, "/v0/management/" + path, key=key, body=body, method=method)

    def inventory():
        code, _, raw = mgmt("plugins")
        assert code == 200, code
        return {p["id"]: p for p in json.loads(raw)["plugins"]}

    def source_id(url):
        return "source-" + hashlib.sha256(url.encode()).hexdigest()[:12]
    install = "plugin-store/chat-archive/install?source="
    code, _, raw = mgmt(install + source_id(BAD_URL), "POST", {})
    assert code == 502 and json.loads(raw)["error"] == "plugin_install_failed", ("bad sha install", code)
    assert "chat-archive" not in inventory()
    code, _, raw = mgmt(install + source_id(STORE_URL), "POST", {})
    assert code == 200, ("install", code)
    installed = json.loads(raw)
    assert installed["status"] == "installed" and installed["version"] == VERSION, installed
    copied = Path("/CLIProxyAPI") / installed["path"]
    assert copied.read_bytes() == Path("/fixture/package/chat-archive.so").read_bytes(), "installed bytes differ"
    item = H.wait_until(lambda: (lambda p: p if p and p.get("registered") else None)(inventory().get("chat-archive")), 30, "registration")
    assert item["effective_enabled"] and item["metadata"]["version"] == VERSION, item
    assert [m["menu"] for m in item["menus"]] == ["Chat Archive"] and item["menus"][0]["path"] == H.PUBLIC, item["menus"]
    rows = inventory()
    inactive = [p for p in peers if not (rows.get(p, {}).get("registered") and rows[p].get("effective_enabled"))]
    assert not inactive, ("peers not registered beside chat-archive", inactive)
    code, _, raw = mgmt("plugins/chat-archive/config")
    generated = json.loads(raw)
    assert code == 200 and set(generated) == {"enabled", "store"}, sorted(generated)
    code, _, raw = mgmt("plugins/chat-archive/status")
    status = json.loads(raw)
    assert code == 200 and status["state"] == "unconfigured", status.get("state")
    for path in ("plugins/token-usage/status", "plugins/quota-cache/status"):
        if path.split("/")[1] in peers:
            assert mgmt(path)[0] == 200, ("peer route collides or vanished", path)
    code, headers, shell = H.request(base, H.PUBLIC)
    assert code == 200 and shell.startswith(b"<!doctype html>")
    http_contract.assert_public_headers(headers, shell)
    for unauth in (None, "ca-wrong"):
        assert mgmt("plugins")[0] == 200
        assert H.request(base, H.STATUS, key=unauth)[0] in (401, 403)
    # Configure through the management API, the way an operator saves the plugin config page.
    secrets = Path("/CLIProxyAPI/plugins/data/ca-secrets")
    secrets.mkdir(parents=True, mode=0o700)
    (secrets / "token").write_text(fake.token)
    (secrets / "hmac").write_text(c.hmac_key)
    for f in secrets.iterdir():
        f.chmod(0o600)
    c.source_token = fake.token
    patch = {"archive-url": fake.url, "source-token-file": str(secrets / "token"), "hmac-key-file": str(secrets / "hmac")}
    code, _, raw = mgmt("plugins/chat-archive/config", "PATCH", patch)
    assert code == 200, ("config patch", code)
    H.wait_until(lambda: json.loads(mgmt("plugins/chat-archive/status")[2])["state"] in ("running", "degraded"), 30, "running")
    client = os.environ["CA_CLIENT_KEY"]
    body = {"model": "ca-openai", "messages": [{"role": "user", "content": c.markers["request"]}]}
    code, _, _ = H.request(base, "/v1/chat/completions", body=body, headers={"Authorization": "Bearer " + client})
    assert code == 200, ("capture request", code)

    def captured():
        kinds = {env["kind"] for _, _, env in fake.envelopes()}
        return kinds if {"exchange.begin", "exchange.response", "exchange.completion"} <= kinds else None
    kinds = H.wait_until(captured, 30, "envelopes at the fake archive")
    data = Path("/CLIProxyAPI/plugins/data/chat-archive")
    modes = [(p.name, p.stat().st_mode & 0o777, p.is_dir()) for p in [data, *data.rglob("*")]]
    bad = [(n, oct(m)) for n, m, d in modes if (d and m != 0o700) or (not d and m != 0o600)]
    assert not bad, ("default data path modes", bad)
    assert (data / "outbox.sqlite").exists(), "outbox not at the default data path"
    print("PASS drive: store install, %d peers coexist, generated config enabled+store, unconfigured status, public shell CSP, "
          "configured via PATCH, captured %s, default data path 0700/0600" % (len(peers), ",".join(sorted(kinds))), flush=True)
    upstream.shutdown()
    fake.close()
    store.shutdown()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--library", type=Path)
    parser.add_argument("--fakearchive", type=Path)
    parser.add_argument("--drive", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--skip-peers", action="store_true", help="local diagnosis only: do not download and load published peers")
    ns = parser.parse_args()
    if ns.drive:
        drive()
    else:
        if not ns.library or not ns.fakearchive:
            parser.error("--library and --fakearchive are required")
        host_main(ns)
