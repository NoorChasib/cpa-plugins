#!/usr/bin/env python3
"""Fail before acceptance if active build/release version bindings disagree.

Historical documentation and the explicit immutable v0.1.0 red control are not
candidate bindings and are intentionally excluded. This does not relax the
single-version publication allowlist in release.py or the tag workflow.
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
    "scripts/native/run-smoke.sh": [r'internal/plugin.Version=' + SEMVER],
    "scripts/native/production.py": [r'status\["version"\] == "' + SEMVER + '"'],
    "scripts/native/abi_probe.py": [r'"version": "' + SEMVER + '"'],
    "scripts/native/store_smoke.py": [r'^VERSION = "' + SEMVER + '"'],
    "scripts/release.py": [r'if version != "' + SEMVER + '"', r'--version", default="' + SEMVER + '"',
                           r'only the scoped ' + SEMVER + ' release'],
    "scripts/verify-cpa-package.py": [r'/releases/download/v' + SEMVER, r'const archiveName = "token-usage_' + SEMVER,
        r'TagName: "v' + SEMVER, r'plugin.Version != "' + SEMVER, r'installed.Version != "' + SEMVER,
        r'case apiBase \+ "latest", apiBase \+ "tags/v' + SEMVER,
        r'client.InstallVersion\(ctx, plugin, "v' + SEMVER, r'release.verify\(args.dist, "' + SEMVER],
    ".github/workflows/ci.yml": [r'VERSION=' + SEMVER],
    ".github/workflows/release.yml": [r'^name: Publish verified Token Usage ' + SEMVER,
        r'tags: \[v' + SEMVER, r'group: token-usage-release-v' + SEMVER,
        r'github.ref == \'refs/tags/v' + SEMVER, r'RELEASE_TAG: v' + SEMVER,
        r'RELEASE_VERSION: ' + SEMVER, r'\["version"\] == "' + SEMVER,
        r'docs/release-notes-' + SEMVER, r'\["tagName"\] == "v' + SEMVER,
        r'token-usage_' + SEMVER + '_linux_amd64.zip'],
}
JSON_FILES = ("registry.json", "scripts/browser/package.json", "scripts/browser/package-lock.json")


def validate(root=ROOT):
    registry = json.loads((root / "registry.json").read_text())
    assert len(registry["plugins"]) == 1 and registry["plugins"][0]["id"] == "token-usage"
    version = registry["plugins"][0]["version"]
    assert re.fullmatch(SEMVER, version), "invalid canonical candidate version"
    for path, patterns in PATTERNS.items():
        text = (root / path).read_text()
        for pattern in patterns:
            values = re.findall(pattern, text, re.M)
            assert values, f"candidate version binding missing in {path}: {pattern}"
            assert set(values) == {version}, f"candidate version mismatch in {path}: {values} != {version}"
    package = json.loads((root / "scripts/browser/package.json").read_text())
    lock = json.loads((root / "scripts/browser/package-lock.json").read_text())
    assert package["version"] == lock["version"] == lock["packages"][""]["version"] == version, "browser manifest candidate version mismatch"
    return version


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=ROOT)
    args = parser.parse_args()
    print("PASS: all active candidate version bindings agree: " + validate(args.root))
