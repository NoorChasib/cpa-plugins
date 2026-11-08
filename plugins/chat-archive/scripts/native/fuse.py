#!/usr/bin/env python3
"""make fuse-smoke: CPA survives a host-side fuse of chat-archive (CPA disables a plugin whose hook
panics inside its recovered adapter).

The CPA under test is built from the pinned source with scripts/native/fuse-v8.0.60.patch, which
panics inside the recovered before-auth adapter when CPA_TEST_FUSE_PLUGIN names the plugin. Run 1
(variable unset) proves traffic moves calls.before_auth. Run 2 (same data directory, variable set
to chat-archive) asserts: CPA keeps serving, CPA logs the fuse, and the plugin keeps heartbeating
to the fake archive while calls.before_auth stays frozen. Synthetic only, loopback only.
"""
import argparse
import json
from pathlib import Path
import shutil
import tempfile
import time

import canaries as canarylib
import harness as H
import mockup


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=Path, required=True)
    parser.add_argument("--library", type=Path, required=True)
    parser.add_argument("--fakearchive", type=Path, required=True)
    args = parser.parse_args()
    c = canarylib.Canaries()
    state, server, _ = mockup.start(c)
    mock = "http://127.0.0.1:%d" % server.server_port
    directory = Path(tempfile.mkdtemp(prefix="ca-fuse-", dir="/tmp"))
    (directory / "plugins").mkdir()
    (directory / "auth").mkdir()
    shutil.copy2(args.library, directory / "plugins" / "chat-archive.so")
    fake = H.Fake(args.fakearchive, directory)
    c.source_token = fake.token
    config = H.cpa_config(directory, H.free_port(), mock, c, fake.url)
    body = {"model": "ca-claude", "max_tokens": 16, "messages": [{"role": "user", "content": "fuse " + c.markers["request"]}]}
    headers = {"X-Api-Key": c.client_key_2, "Anthropic-Version": "2023-06-01"}
    try:
        cpa = H.CPA(args.cpa, directory, config, c).start()
        for _ in range(3):
            assert H.request(cpa.base, "/v1/messages", body=body, headers=headers)[0] == 200
        status, _ = cpa.status()
        moved = status["calls"].get("before_auth", 0)
        assert moved >= 3, ("run 1 before_auth calls", moved)
        cpa.stop()
        boots_before = {h["boot_id"] for h in fake.get("/heartbeats")}

        fused = H.CPA(args.cpa, directory, config, c, env_extra={"CPA_TEST_FUSE_PLUGIN": "chat-archive"}).start()
        served = 0
        for _ in range(5):
            code, _, _ = H.request(fused.base, "/v1/messages", body=body, headers=headers)
            served += code == 200
        assert served == 5, ("CPA stopped serving after the fuse", served)
        log = fused.log_path.read_bytes()
        assert b"plugin panic recovered" in log and b"chat-archive" in log, "CPA did not log the fuse"

        def new_heartbeats():
            beats = [h for h in fake.get("/heartbeats") if h["boot_id"] not in boots_before]
            return beats if len(beats) >= 2 else None
        beats = H.wait_until(new_heartbeats, 150, "two heartbeats from the fused plugin", 2)
        frozen = {h["calls"].get("before_auth", 0) for h in beats}
        assert frozen == {0}, ("calls.before_auth moved while fused", sorted(frozen))
        for _ in range(3):
            assert H.request(fused.base, "/v1/messages", body=body, headers=headers)[0] == 200
        fused.stop()
        secrets = [n for n, v in c.all_secrets().items() if canarylib.contains_any(json.dumps(beats).encode(), v)]
        assert not secrets, ("heartbeat leaks", secrets)
        print("PASS: fuse-smoke: run 1 before_auth=%d; fused run served %d+3 requests, CPA logged the fuse, "
              "%d heartbeats arrived with calls.before_auth frozen at 0" % (moved, served, len(beats)), flush=True)
    finally:
        fake.close()
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    main()
