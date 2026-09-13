#!/usr/bin/env python3
"""Prepare/verify public release metadata locally; never publishes or uses the network."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import stat
import zipfile

ROOT = Path(__file__).resolve().parents[1]
PROJECT = "https://github.com/NoorChasib/cpa-plugins"
SOURCE_URL = "https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json"
MEMBERS = {"token-usage.so", "LICENSE", "THIRD-PARTY-NOTICES.txt"}


def archive_name(version):
    if version != "0.1.2":
        raise ValueError("only the scoped 0.1.2 release is currently supported")
    return f"token-usage_{version}_linux_amd64.zip"


def release_base(version):
    archive_name(version)
    return f"{PROJECT}/releases/download/v{version}"


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def validate_library(raw):
    # ELF64, little endian, ET_DYN, x86-64. ABI/version behavior is tested by native smoke.
    if len(raw) < 64 or raw[:6] != b"\x7fELF\x02\x01" or raw[16:20] != b"\x03\x00\x3e\x00":
        raise ValueError("library must be an ELF64 Linux amd64 shared object")
    if b"build\t-buildmode=c-shared\n" not in raw:
        raise ValueError("Go c-shared build metadata is required")
    tags = re.findall(rb"build\t-tags=([^\n]+)", raw)
    if any(b"nativefixture" in entry for entry in tags):
        raise ValueError("nativefixture libraries must never be packaged")


def license_contents(root):
    return {"LICENSE": (root / "LICENSE").read_bytes(),
            "THIRD-PARTY-NOTICES.txt": (root / "docs/third-party-notices.txt").read_bytes()}


def package(dist, version, root=ROOT):
    path = dist / archive_name(version)
    raw = (dist / "token-usage.so").read_bytes()
    validate_library(raw)
    contents = {"token-usage.so": raw, **license_contents(root)}
    # Reproducible for identical inputs, not across compiler/libc versions.
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as out:
        for name in sorted(contents):
            info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | (0o755 if name.endswith(".so") else 0o644)) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            out.writestr(info, contents[name], compresslevel=9)
    return path


def source_registry(version):
    archive_name(version)
    return {"schema_version": 2, "plugins": [{"id": "token-usage", "name": "Token Usage",
        "description": "Persistent CPA-reported raw token usage by provider/model. Linux amd64 only; not billing or lossless accounting.",
        "author": "NoorChasib", "version": version, "repository": PROJECT, "license": "MIT",
        "install": {"type": "github-release"}}]}


def expected_registry(archive, version):
    registry = source_registry(version)
    registry["plugins"][0]["install"] = {"type": "direct", "artifacts": [
        {"goos": "linux", "goarch": "amd64", "url": release_base(version) + "/" + archive.name,
         "sha256": digest(archive), "size": archive.stat().st_size}]}
    return registry


def require_single_archive(dist, version):
    expected = dist / archive_name(version)
    if set(dist.glob("*.zip")) != {expected}:
        raise ValueError("release directory must contain exactly the claimed Linux amd64 archive")
    return expected


def checksum_text(archive, registry_path):
    return f"{digest(archive)}  {archive.name}\n{digest(registry_path)}  registry.json\n"


def checksums(dist, version):
    archive = require_single_archive(dist, version)
    registry_path = dist / "registry.json"
    if registry_path.resolve() == (ROOT / "registry.json").resolve():
        raise ValueError("generated metadata must not overwrite the committed source registry")
    registry_path.write_text(json.dumps(expected_registry(archive, version), indent=2) + "\n")
    (dist / "checksums.txt").write_text(checksum_text(archive, registry_path))


def verify(dist, version, root=ROOT):
    archive = require_single_archive(dist, version)
    registry_path = dist / "registry.json"
    if json.loads((root / "registry.json").read_text()) != source_registry(version):
        raise ValueError("committed source registry must use the canonical GitHub-release contract")
    if json.loads(registry_path.read_text()) != expected_registry(archive, version):
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
        validate_library(bundle.read("token-usage.so"))
        if bundle.read("token-usage.so") != (dist / "token-usage.so").read_bytes():
            raise ValueError("packaged library differs from current production build")
        for name, raw in license_contents(root).items():
            if bundle.read(name) != raw:
                raise ValueError("packaged license notices differ from source")
        if bundle.testzip():
            raise ValueError("archive CRC failure")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("package", "checksums", "verify"))
    parser.add_argument("--version", default="0.1.2")
    parser.add_argument("--dist", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    try:
        archive_name(args.version)
        if args.command == "package":
            package(args.dist, args.version)
        elif args.command == "checksums":
            checksums(args.dist, args.version)
        else:
            verify(args.dist, args.version)
    except (OSError, ValueError, zipfile.BadZipFile) as error:
        parser.exit(1, str(error) + "\n")
    print(f"PASS: {args.command} ({args.version}, linux/amd64; local validation, no publication)")


if __name__ == "__main__":
    main()
