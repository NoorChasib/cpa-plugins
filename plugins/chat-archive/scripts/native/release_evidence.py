#!/usr/bin/env python3
"""Write dist/chat-archive-evidence.json for the root packager (scripts/plugin_release.py).

Records the official CPA image the store smoke ran, the code commit, the candidate and the
sha256 and version of the smoke-tested library, plus the published peers that store smoke loaded
beside it (from the root catalog). Local only; reads no secret.
"""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
REPO = ROOT.parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--library", type=Path, default=ROOT / "dist/chat-archive.so")
    parser.add_argument("--output", type=Path, default=ROOT / "dist/chat-archive-evidence.json")
    args = parser.parse_args()
    spec = importlib.util.spec_from_file_location("store_smoke", HERE / "store_smoke.py")
    store = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(store)
    spec = importlib.util.spec_from_file_location("release", ROOT / "scripts/release.py")
    release = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(release)
    raw = args.library.read_bytes()
    release.validate_library(raw)
    catalog = json.loads((REPO / "registry.json").read_text())
    peers = {p["id"]: {"version": p["version"], "sha256": p["install"]["artifacts"][0]["sha256"]}
             for p in catalog["plugins"] if p["id"] != "chat-archive"}
    commit = subprocess.check_output(["git", "-C", str(REPO), "rev-parse", "HEAD"], text=True).strip()
    evidence = {"image": store.IMAGE, "code_commit": commit, "candidate": "chat-archive",
                "libraries": {"chat-archive": {"sha256": hashlib.sha256(raw).hexdigest(), "version": release.candidate_version()}},
                "peers": peers}
    args.output.write_text(json.dumps(evidence, indent=2) + "\n")
    print("Wrote " + str(args.output))


if __name__ == "__main__":
    main()
