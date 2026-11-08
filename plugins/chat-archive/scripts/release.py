#!/usr/bin/env python3
"""Prepare/verify chat-archive release metadata locally; never publishes or uses the network."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import stat
import zipfile

ROOT = Path(__file__).resolve().parents[1]
PLUGIN = "chat-archive"
PROJECT = "https://github.com/NoorChasib/cpa-plugins"
MEMBERS = {"chat-archive.so", "LICENSE", "THIRD-PARTY-NOTICES.txt"}


def candidate_version(root=ROOT):
    """The one version this tree may package: the plugin-local registry entry (check-candidate.py
    binds every other copy to it)."""
    registry = json.loads((root / "registry.json").read_text())
    entries = registry["plugins"]
    if len(entries) != 1 or entries[0]["id"] != PLUGIN:
        raise ValueError("plugin-local registry must list chat-archive only")
    return entries[0]["version"]


def archive_name(version, root=ROOT):
    if version != candidate_version(root):
        raise ValueError("only the candidate version in registry.json can be packaged")
    return f"{PLUGIN}_{version}_linux_amd64.zip"


def release_base(version, root=ROOT):
    archive_name(version, root)
    return f"{PROJECT}/releases/download/{PLUGIN}/v{version}"


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def validate_library(raw):
    # ELF64, little endian, ET_DYN, x86-64. ABI/version behaviour is tested by native smoke.
    if len(raw) < 64 or raw[:6] != b"\x7fELF\x02\x01" or raw[16:20] != b"\x03\x00\x3e\x00":
        raise ValueError("library must be an ELF64 Linux amd64 shared object")
    if b"build\t-buildmode=c-shared\n" not in raw:
        raise ValueError("Go c-shared build metadata is required")
    tags = re.findall(rb"build\t-tags=([^\n]+)", raw)
    if any(b"nativefixture" in entry for entry in tags):
        raise ValueError("nativefixture libraries must never be packaged")


def license_contents(root=ROOT):
    return {"LICENSE": (root / "LICENSE").read_bytes(),
            "THIRD-PARTY-NOTICES.txt": (root / "docs/third-party-notices.txt").read_bytes()}


def package(dist, version, root=ROOT):
    path = dist / archive_name(version, root)
    raw = (dist / "chat-archive.so").read_bytes()
    validate_library(raw)
    contents = {"chat-archive.so": raw, **license_contents(root)}
    # Reproducible for identical inputs, not across compiler/libc versions.
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as out:
        for name in sorted(contents):
            info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | (0o755 if name.endswith(".so") else 0o644)) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            out.writestr(info, contents[name], compresslevel=9)
    return path


def source_registry(version, root=ROOT):
    archive_name(version, root)
    return {"schema_version": 2, "plugins": [{"id": PLUGIN, "name": "Chat Archive",
        "description": "Archives CPA chat exchanges, masked and canonical, to a private chat archive. Linux amd64 only.",
        "author": "NoorChasib", "version": version, "repository": PROJECT, "license": "MIT",
        "install": {"type": "github-release"}}]}


def expected_registry(archive, version, root=ROOT):
    registry = source_registry(version, root)
    registry["plugins"][0]["install"] = {"type": "direct", "artifacts": [
        {"goos": "linux", "goarch": "amd64", "url": release_base(version, root) + "/" + archive.name,
         "sha256": digest(archive), "size": archive.stat().st_size}]}
    return registry


def require_single_archive(dist, version, root=ROOT):
    expected = dist / archive_name(version, root)
    if set(dist.glob("*.zip")) != {expected}:
        raise ValueError("release directory must contain exactly the claimed Linux amd64 archive")
    return expected


def checksum_text(archive, registry_path):
    return f"{digest(archive)}  {archive.name}\n{digest(registry_path)}  registry.json\n"


def checksums(dist, version, root=ROOT):
    archive = require_single_archive(dist, version, root)
    registry_path = dist / "registry.json"
    if registry_path.resolve() == (root / "registry.json").resolve():
        raise ValueError("generated metadata must not overwrite the committed source registry")
    registry_path.write_text(json.dumps(expected_registry(archive, version, root), indent=2) + "\n")
    (dist / "checksums.txt").write_text(checksum_text(archive, registry_path))


def verify(dist, version, root=ROOT):
    archive = require_single_archive(dist, version, root)
    registry_path = dist / "registry.json"
    if json.loads((root / "registry.json").read_text()) != source_registry(version, root):
        raise ValueError("committed source registry must use the canonical GitHub-release contract")
    if json.loads(registry_path.read_text()) != expected_registry(archive, version, root):
        raise ValueError("release registry must exactly describe the public Linux amd64 artifact")
    if (dist / "checksums.txt").read_text() != checksum_text(archive, registry_path):
        raise ValueError("checksums.txt must contain exactly the current archive and registry checksums")
    with zipfile.ZipFile(archive) as bundle:
        if len(bundle.infolist()) != len(MEMBERS) or set(bundle.namelist()) != MEMBERS:
            raise ValueError("archive must contain one root library and the license notices only")
        for member in bundle.infolist():
            mode = member.external_attr >> 16
            expected_mode = 0o755 if member.filename.endswith(".so") else 0o644
            if not stat.S_ISREG(mode) or stat.S_IMODE(mode) != expected_mode:
                raise ValueError("archive entries must be regular files with safe exact modes")
        validate_library(bundle.read("chat-archive.so"))
        if bundle.read("chat-archive.so") != (dist / "chat-archive.so").read_bytes():
            raise ValueError("packaged library differs from current production build")
        for name, raw in license_contents(root).items():
            if bundle.read(name) != raw:
                raise ValueError("packaged license notices differ from source")
        if bundle.testzip():
            raise ValueError("archive CRC failure")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("package", "checksums", "verify", "validate-library"))
    parser.add_argument("--version", default=None)
    parser.add_argument("--dist", type=Path, default=ROOT / "dist")
    parser.add_argument("--library", type=Path)
    args = parser.parse_args()
    try:
        version = args.version or candidate_version()
        if args.command == "validate-library":
            validate_library((args.library or args.dist / "chat-archive.so").read_bytes())
        elif args.command == "package":
            package(args.dist, version)
        elif args.command == "checksums":
            checksums(args.dist, version)
        else:
            verify(args.dist, version)
    except (OSError, ValueError, KeyError, zipfile.BadZipFile) as error:
        parser.exit(1, str(error) + "\n")
    print(f"PASS: {args.command} ({version}, linux/amd64; local validation, no publication)")


if __name__ == "__main__":
    main()
