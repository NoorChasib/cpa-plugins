"""Release packaging/workflow contracts; no publishing and no network required."""
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
import zipfile

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("release", ROOT / "scripts/release.py")
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.dist = self.root / "dist"
        self.dist.mkdir()
        (self.root / "docs").mkdir()
        (self.root / "LICENSE").write_text("fixture license")
        (self.root / "docs/third-party-notices.txt").write_text("fixture dependency notices")
        (self.root / "registry.json").write_text(json.dumps(release.source_registry("0.1.1")))
        elf = bytearray(64)
        elf[:6] = b"\x7fELF\x02\x01"
        elf[16:20] = b"\x03\x00\x3e\x00"
        self.binary = bytes(elf) + b"build\t-buildmode=c-shared\n"
        (self.dist / "token-usage.so").write_bytes(self.binary)
        self.registry = self.dist / "registry.json"
        self.bundle = release.package(self.dist, "0.1.1", self.root)
        release.checksums(self.dist, "0.1.1")

    def verify(self):
        release.verify(self.dist, "0.1.1", self.root)

    def test_exact_bundle_and_public_registry(self):
        self.verify()
        data = json.loads(self.registry.read_text())
        artifact = data["plugins"][0]["install"]["artifacts"][0]
        self.assertEqual(data["schema_version"], 2)
        self.assertEqual(artifact["url"], "https://github.com/NoorChasib/cpa-plugin-token-usage/releases/download/v0.1.1/token-usage_0.1.1_linux_amd64.zip")
        self.assertEqual(artifact["sha256"], release.digest(self.bundle))
        self.assertEqual(artifact["size"], self.bundle.stat().st_size)
        with zipfile.ZipFile(self.bundle) as archive:
            self.assertEqual(set(archive.namelist()), release.MEMBERS)
            self.assertNotIn("token-usage.h", archive.namelist())

    def test_checksums_cover_archive_and_release_registry(self):
        self.assertEqual((self.dist / "checksums.txt").read_text(),
                         f"{release.digest(self.bundle)}  {self.bundle.name}\n{release.digest(self.registry)}  registry.json\n")
        subprocess.run(["sha256sum", "-c", "checksums.txt"], cwd=self.dist, check=True, capture_output=True)

    def test_checksums_never_rewrite_committed_source_registry(self):
        before = (self.root / "registry.json").read_bytes()
        release.checksums(self.dist, "0.1.1")
        self.assertEqual(before, (self.root / "registry.json").read_bytes())
        self.assertEqual(json.loads((ROOT / "registry.json").read_text()), release.source_registry("0.1.1"))

    def test_reproducible_zip_for_identical_inputs(self):
        first = self.bundle.read_bytes()
        release.package(self.dist, "0.1.1", self.root)
        self.assertEqual(first, self.bundle.read_bytes())

    def test_reject_unsupported_release(self):
        for version in ("0.1.0", "v0.1.1", "../0.1.1", "0.2.0", "0.1.1\n", ""):
            with self.subTest(version=version), self.assertRaises(ValueError):
                release.archive_name(version)
            with self.subTest(version=version), self.assertRaises(ValueError):
                release.source_registry(version)

    def test_reject_fixture_build(self):
        for tags in (b"nativefixture", b"other,nativefixture", b"nativefixture,other",
                     b'"other nativefixture"', b"nativefixture other"):
            (self.dist / "token-usage.so").write_bytes(self.binary + b"build\t-tags=" + tags + b"\n")
            with self.assertRaisesRegex(ValueError, "nativefixture"):
                release.package(self.dist, "0.1.1", self.root)

    def test_reject_wrong_platform_and_buildmode(self):
        for raw in (b"not ELF", self.binary.replace(b"\x3e\x00", b"\xb7\x00"), self.binary[:64]):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                release.validate_library(raw)

    def test_reject_modified_archive_checksum(self):
        self.bundle.write_bytes(self.bundle.read_bytes() + b"changed")
        with self.assertRaisesRegex(ValueError, "registry|checksum"):
            self.verify()

    def test_reject_modified_checksum_manifest(self):
        (self.dist / "checksums.txt").write_text("0" * 64 + "  " + self.bundle.name + "\n")
        with self.assertRaisesRegex(ValueError, "checksum"):
            self.verify()

    def test_reject_extra_platform_archive(self):
        (self.dist / "token-usage_0.1.1_linux_arm64.zip").write_bytes(b"unclaimed")
        with self.assertRaisesRegex(ValueError, "exactly"):
            self.verify()

    def test_reject_wrong_registry_urls_and_metadata(self):
        original = json.loads(self.registry.read_text())
        for key, value in (("url", "https://127.0.0.1:8765/unpublished.zip"),
                           ("url", "https://example.com/unpublished.zip"),
                           ("url", release.release_base("0.1.1").replace("v0.1.1", "v0.2.0") + "/" + self.bundle.name),
                           ("url", release.release_base("0.1.1").replace("https://", "http://") + "/" + self.bundle.name),
                           ("goarch", "arm64"), ("size", 1), ("sha256", "0" * 64)):
            registry = json.loads(json.dumps(original))
            registry["plugins"][0]["install"]["artifacts"][0][key] = value
            self.registry.write_text(json.dumps(registry))
            with self.subTest(key=key, value=value), self.assertRaisesRegex(ValueError, "registry"):
                self.verify()
        registry = original
        registry["plugins"][0]["version"] = "0.2.0"
        self.registry.write_text(json.dumps(registry))
        with self.assertRaisesRegex(ValueError, "registry"):
            self.verify()

    def test_reject_changed_committed_registry(self):
        registry = release.source_registry("0.1.1")
        registry["plugins"][0]["repository"] = "https://github.com/other/project"
        (self.root / "registry.json").write_text(json.dumps(registry))
        with self.assertRaisesRegex(ValueError, "committed source registry"):
            self.verify()

    def test_reject_changed_library_and_licenses(self):
        (self.dist / "token-usage.so").write_bytes(self.binary + b"new build")
        with self.assertRaisesRegex(ValueError, "differs"):
            self.verify()
        (self.dist / "token-usage.so").write_bytes(self.binary)
        (self.root / "LICENSE").write_text("changed")
        with self.assertRaisesRegex(ValueError, "license notices differ"):
            self.verify()

    def test_reject_additional_archive_members(self):
        with zipfile.ZipFile(self.bundle, "a") as archive:
            archive.writestr("../secret", "bad")
        release.checksums(self.dist, "0.1.1")
        with self.assertRaisesRegex(ValueError, "one root library"):
            self.verify()

    def test_reject_symlink_or_unsafe_mode_member(self):
        for mode in (0o120777, 0o104755):
            with zipfile.ZipFile(self.bundle, "w") as archive:
                for name in sorted(release.MEMBERS):
                    info = zipfile.ZipInfo(name)
                    info.create_system = 3
                    info.external_attr = mode << 16
                    archive.writestr(info, "target")
            release.checksums(self.dist, "0.1.1")
            with self.assertRaisesRegex(ValueError, "regular"):
                self.verify()


# These deliberately do not require a third-party YAML package in make ci.
# Full Actions schema validation is also performed with actionlint before publishing.
def run_blocks(workflow):
    lines = workflow.splitlines()
    blocks = []
    for index, line in enumerate(lines):
        if line.startswith("        run: "):
            value = line.removeprefix("        run: ")
            if value != "|":
                blocks.append(value + "\n")
                continue
            script = []
            for following in lines[index + 1:]:
                if following and not following.startswith("          "):
                    break
                script.append(following[10:])
            blocks.append("\n".join(script) + "\n")
    return blocks


class WorkflowTests(unittest.TestCase):
    def test_read_only_ci_and_required_smoke(self):
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        self.assertIn("contents: read", workflow)
        self.assertIn("persist-credentials: false", workflow)
        self.assertIn('make smoke | tee', workflow)
        self.assertIn('run: make browser', workflow)
        self.assertIn('make store-smoke CPA_SMOKE_WORK=', workflow)
        self.assertIn('make package-tested checksums verify-release', workflow)
        self.assertNotIn('package-current', workflow)
        for forbidden in ("pull_request_target:", "workflow_dispatch:", "secrets.", "contents: write", "gh release", "github.token"):
            self.assertNotIn(forbidden, workflow)
        runner = (ROOT / "scripts/native/run-smoke.sh").read_text()
        self.assertIn("--network none", runner)
        self.assertIn("python@sha256:", runner)
        self.assertIn("sha256sum -c -", runner)
        self.assertIn("7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974", runner)
        self.assertIn("python /fixture/scripts/production.py", runner)
        self.assertNotIn("exit 0", runner)
        self.assertNotIn("/tmp/token-usage-spike.", runner)
        self.assertIn('cp -- "$CPA_SOURCE_ARCHIVE" "$WORK/cpa.tar.gz"', runner)

    def test_pinned_actions_and_shell_syntax(self):
        for file in sorted((ROOT / ".github/workflows").glob("*.yml")):
            workflow = file.read_text()
            pins = re.findall(r"uses: (\S+)", workflow)
            self.assertEqual(pins, ["actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683",
                                    "actions/setup-go@d35c59abb061a4a6fb18e82ac0862c26744d6ab5"])
            self.assertIn("go-version: '1.27.1'", workflow)
            self.assertIn('test "$(go env GOVERSION)" = go1.27.1', workflow)
            self.assertIn("runs-on: ubuntu-24.04", workflow)
            self.assertIn("persist-credentials: false", workflow)
            for block in run_blocks(workflow):
                with self.subTest(file=file.name, block=block.splitlines()[0]):
                    subprocess.run(["bash", "-n"], input=block, text=True, check=True, capture_output=True)

    def test_release_is_guarded_and_ordered(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("tags: [v0.1.1]", workflow)
        self.assertIn("github.repository == 'NoorChasib/cpa-plugin-token-usage'", workflow)
        self.assertIn("github.event_name == 'push'", workflow)
        self.assertIn("github.ref == 'refs/tags/v0.1.1'", workflow)
        self.assertIn("github.event.deleted == false", workflow)
        self.assertIn("cancel-in-progress: false", workflow)
        self.assertEqual(workflow.count("contents: write"), 1)
        self.assertEqual(re.findall(r"^  ([a-z-]+):\n    if:", workflow, re.M), ["publish"])
        for forbidden in ("pull_request:", "pull_request_target:", "workflow_dispatch:", "secrets.", "continue-on-error", "always()", "--clobber", "package-current", "git push"):
            self.assertNotIn(forbidden, workflow)
        gates = ['bash scripts/browser/install-node.sh', 'run: make browser-tools',
                 'make ci VERSION=', 'run: make browser\n', 'make smoke | tee', 'shutil.copyfile(work / "token-usage-production.so"',
                 'make store-smoke CPA_SMOKE_WORK=', 'make package-tested checksums verify-release', 'python3 scripts/verify-cpa-package.py',
                 'gh release create', 'gh release download', 'python3 scripts/release.py verify',
                 'gh release edit']
        positions = [workflow.index(gate) for gate in gates]
        self.assertEqual(positions, sorted(positions))
        self.assertIn('--verify-tag --draft', workflow)
        self.assertIn('--verify-tag --draft=false --latest', workflow)
        makefile = (ROOT / "Makefile").read_text()
        self.assertIn("package-tested:\n\t$(PYTHON) scripts/release.py package", makefile)

    def test_release_guard_rejects_wrong_repository_event_ref_and_version(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        guard = run_blocks(workflow)[0]
        environment = {**os.environ, "GITHUB_REPOSITORY": "NoorChasib/cpa-plugin-token-usage",
                       "RELEASE_REPO": "NoorChasib/cpa-plugin-token-usage", "GITHUB_EVENT_NAME": "push",
                       "GITHUB_REF": "refs/tags/v0.1.1", "RELEASE_TAG": "v0.1.1", "RELEASE_VERSION": "0.1.1"}
        for key, value in (("GITHUB_REPOSITORY", "other/fork"), ("GITHUB_EVENT_NAME", "pull_request"),
                           ("GITHUB_REF", "refs/heads/main"), ("RELEASE_VERSION", "0.2.0")):
            result = subprocess.run(["bash", "-euo", "pipefail", "-c", guard],
                                    env={**environment, key: value}, capture_output=True)
            with self.subTest(key=key):
                self.assertNotEqual(result.returncode, 0)

    def test_existing_release_and_api_errors_fail_closed(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        guard = next(block for block in run_blocks(workflow) if "existing-release" in block)
        with tempfile.TemporaryDirectory() as directory:
            fake = Path(directory) / "gh"
            fake.write_text('#!/bin/bash\nprintf "%s" "$FAKE_RESULT"\nexit "$FAKE_STATUS"\n')
            fake.chmod(0o700)
            env = {**os.environ, "PATH": directory + ":" + os.environ["PATH"], "RUNNER_TEMP": directory,
                   "RELEASE_REPO": "NoorChasib/cpa-plugin-token-usage", "RELEASE_TAG": "v0.1.1"}
            for output, status, expected in (("", "0", 0), ("123", "0", 1), ("", "1", 1)):
                result = subprocess.run(["bash", "-euo", "pipefail", "-c", guard],
                                        env={**env, "FAKE_RESULT": output, "FAKE_STATUS": status}, capture_output=True)
                with self.subTest(output=output, status=status):
                    self.assertEqual(result.returncode, expected)

    def test_draft_asset_validation_rejects_published_or_partial_release(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        block = next(block for block in run_blocks(workflow) if "draft.json" in block)
        python = block.split("python3 - <<'PY'\n", 1)[1].split("\nPY\n", 1)[0]
        valid = {"isDraft": True, "tagName": "v0.1.1", "assets": [
            {"name": "checksums.txt"}, {"name": "registry.json"}, {"name": release.archive_name("0.1.1")} ]}
        cases = [(valid, 0), ({**valid, "isDraft": False}, 1), ({**valid, "tagName": "v0.2.0"}, 1),
                 ({**valid, "assets": valid["assets"][:-1]}, 1),
                 ({**valid, "assets": valid["assets"] + [{"name": "unexpected.so"}]}, 1)]
        with tempfile.TemporaryDirectory() as directory:
            for data, expected in cases:
                (Path(directory) / "draft.json").write_text(json.dumps(data))
                result = subprocess.run(["python3", "-c", python], env={**os.environ, "RUNNER_TEMP": directory}, capture_output=True)
                with self.subTest(data=data):
                    self.assertEqual(result.returncode, expected)


if __name__ == "__main__":
    unittest.main()
