#!/usr/bin/env python3
"""Fail before acceptance if active build/release version bindings disagree.

The canonical candidate version is the single entry of the plugin-local registry.json. Every
listed file must spell exactly that version wherever the pattern matches. The reload library of
the native acceptance (version 9.9.9) is a test-only higher version and is not a binding.
"""
import argparse
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]
SEMVER = r"(\d+\.\d+\.\d+)"
PATTERNS = {
    "internal/plugin/plugin.go": [r'var Version = "' + SEMVER + '"'],
    "Makefile": [r'^VERSION \?= ' + SEMVER + '$'],
    "scripts/native/run-smoke.sh": [r'\$LIBFLAGS=' + SEMVER + r'" -o "\$WORK/chat-archive-production\.so"'],
    "scripts/native/fuse-smoke.sh": [r'internal/plugin\.Version=' + SEMVER + "'"],
    "scripts/native/harness.py": [r'^VERSION = "' + SEMVER + '"'],
    "scripts/native/abi_probe.py": [r'meta\["Version"\] == "' + SEMVER + '"', r'"version": "' + SEMVER + '"'],
    "scripts/native/store_smoke.py": [r'^VERSION = "' + SEMVER + '"'],
    "scripts/verify-cpa-package.py": [r'/releases/download/chat-archive/v' + SEMVER, r'const archiveName = "chat-archive_' + SEMVER,
        r'TagName: "chat-archive/v' + SEMVER, r'plugin.Version != "' + SEMVER, r'installed.Version != "' + SEMVER,
        r'client.InstallVersion\(ctx, plugin, "chat-archive/v' + SEMVER],
}


def validate(root=ROOT):
    registry = json.loads((root / "registry.json").read_text())
    assert len(registry["plugins"]) == 1 and registry["plugins"][0]["id"] == "chat-archive"
    version = registry["plugins"][0]["version"]
    assert re.fullmatch(SEMVER, version), "invalid canonical candidate version"
    for path, patterns in PATTERNS.items():
        text = (root / path).read_text()
        for pattern in patterns:
            values = re.findall(pattern, text, re.M)
            assert values, f"candidate version binding missing in {path}: {pattern}"
            assert set(values) == {version}, f"candidate version mismatch in {path}: {values} != {version}"
    return version


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=ROOT)
    args = parser.parse_args()
    print("PASS: all active candidate version bindings agree: " + validate(args.root))
