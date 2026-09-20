#!/usr/bin/env python3
"""Publish the already signed feed byte-for-byte, after its release is public."""
import argparse
import base64
import json
import pathlib
import re
import subprocess
import xml.etree.ElementTree as ET

REPO = "NoorChasib/cpa-plugins"
BRANCH = "quota-glance-updates"
SPARKLE = "{http://www.andymatuschak.org/xml-namespaces/sparkle}"


def version_tuple(version):
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("Expected a three-component release version")
    return tuple(map(int, version.split(".")))


def validate_feed(data, version=None, assets=None):
    root = ET.fromstring(data)
    items = root.findall("./channel/item")
    if root.tag != "rss" or len(items) != 1:
        raise ValueError("Expected exactly one complete update")
    item = items[0]
    actual = item.findtext(SPARKLE + "version")
    version_tuple(actual or "")
    if version is not None and version != actual:
        raise ValueError("Feed version differs from the release")
    name = f"Quota-Glance-{actual}-macOS.dmg"
    enclosure = item.find("enclosure")
    expected = f"https://github.com/{REPO}/releases/download/quota-glance-menubar/v{actual}/{name}"
    if enclosure is None or enclosure.get("url") != expected:
        raise ValueError("Update must use this release's DMG")
    signature = base64.b64decode(enclosure.get(SPARKLE + "edSignature", ""), validate=True)
    if len(signature) != 64:
        raise ValueError("Missing Ed25519 archive signature")
    length = int(enclosure.get("length", "0"))
    if length <= 0 or (assets and (assets / name).stat().st_size != length):
        raise ValueError("Archive length differs from the signed feed")
    if b"<!-- sparkle-signatures:" not in data:
        raise ValueError("Missing signed-feed block")
    return actual


def should_publish(current, candidate):
    old_version, new_version = validate_feed(current), validate_feed(candidate)
    if version_tuple(old_version) > version_tuple(new_version):
        return False  # A slower, older release must never move the feed backward.
    if old_version == new_version:
        if current != candidate:
            raise ValueError("Refusing to replace an existing version with different signed bytes")
        return False
    return True


def github(endpoint, payload=None, allow_missing=False):
    command = ["gh", "api", f"repos/{REPO}/{endpoint}"]
    if payload is not None:
        command += ["--method", "PUT", "--input", "-"]
    result = subprocess.run(command, input=json.dumps(payload) if payload is not None else None,
                            text=True, capture_output=True)
    if result.returncode:
        if allow_missing and "HTTP 404" in result.stderr:
            return None
        raise RuntimeError(result.stderr.strip())
    return json.loads(result.stdout)


def publish(data, version):
    # Require the immutable download to be public before clients can discover it.
    release = github(f"releases/tags/quota-glance-menubar%2Fv{version}")
    if release["draft"] or release["prerelease"]:
        raise ValueError("The update release must be public and stable")
    for attempt in range(3):
        current = github(f"contents/appcast.xml?ref={BRANCH}", allow_missing=True)
        if current and not should_publish(base64.b64decode(current["content"]), data):
            print("Feed already contains this or a newer version.")
            return
        payload = {"message": f"Publish Quota Glance v{version} update feed", "branch": BRANCH,
                   "content": base64.b64encode(data).decode()}
        if current:
            payload["sha"] = current["sha"]
        try:
            github("contents/appcast.xml", payload)
            print(f"Published the signed v{version} update feed.")
            return
        except RuntimeError as error:
            if "HTTP 409" not in str(error) or attempt == 2:
                raise


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--validate", type=pathlib.Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--assets", type=pathlib.Path, required=True)
    parser.add_argument("--publish", action="store_true")
    args = parser.parse_args()
    data = args.validate.read_bytes()
    validate_feed(data, args.version, args.assets)
    if args.publish:
        publish(data, args.version)
