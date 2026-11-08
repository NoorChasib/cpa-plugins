import base64
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
import zipfile

ROOT = Path(__file__).resolve().parents[2]
REPO = ROOT.parents[1]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


release = load("release", ROOT / "scripts/release.py")
candidate = load("check_candidate", ROOT / "scripts/check-candidate.py")
sys.path.insert(0, str(ROOT / "scripts/native"))
canaries = load("canaries", ROOT / "scripts/native/canaries.py")

ELF = b"\x7fELF\x02\x01" + b"\0" * 10 + b"\x03\x00\x3e\x00" + b"\0" * 44 + b"build\t-buildmode=c-shared\n"


class ReleaseTests(unittest.TestCase):
    def test_candidate_bindings_agree(self):
        self.assertEqual(candidate.validate(), release.candidate_version())

    def test_package_checksums_verify_round_trip(self):
        version = release.candidate_version()
        with tempfile.TemporaryDirectory() as tmp:
            dist = Path(tmp)
            (dist / "chat-archive.so").write_bytes(ELF + b"synthetic")
            release.package(dist, version)
            release.checksums(dist, version)
            release.verify(dist, version)
            with zipfile.ZipFile(dist / release.archive_name(version)) as bundle:
                self.assertEqual(set(bundle.namelist()), release.MEMBERS)
                modes = {i.filename: (i.external_attr >> 16) & 0o777 for i in bundle.infolist()}
                self.assertEqual(modes["chat-archive.so"], 0o755)
                self.assertEqual(modes["THIRD-PARTY-NOTICES.txt"], 0o644)
            (dist / "chat-archive.so").write_bytes(ELF + b"changed")
            with self.assertRaisesRegex(ValueError, "differs"):
                release.verify(dist, version)

    def test_rejects_nativefixture_wrong_arch_and_other_versions(self):
        with self.assertRaisesRegex(ValueError, "nativefixture"):
            release.validate_library(ELF + b"build\t-tags=nativefixture\n")
        arm = bytearray(ELF)
        arm[18] = 183
        with self.assertRaisesRegex(ValueError, "amd64"):
            release.validate_library(bytes(arm))
        with self.assertRaisesRegex(ValueError, "candidate"):
            release.archive_name("9.9.9")

    def test_notices_cover_every_linked_module(self):
        notices = (ROOT / "docs/third-party-notices.txt").read_text()
        for module in ("github.com/mattn/go-sqlite3 v1.14.52", "gopkg.in/yaml.v3 v3.0.1", "github.com/klauspost/compress v1.20.3",
                       "github.com/BobuSumisu/aho-corasick v1.0.3", "gitleaks v8.30.1", "Go runtime and standard library"):
            self.assertIn(module, notices)


class WorkflowTests(unittest.TestCase):
    """The root chat-archive CI mirrors token-usage's: tested bytes, no publication."""

    def setUp(self):
        self.text = (REPO / ".github/workflows/chat-archive-ci.yml").read_text()

    def test_read_only_and_no_publication(self):
        self.assertIn("permissions:\n  contents: read", self.text)
        for forbidden in ("contents: write", "upload-artifact@", "workflow_dispatch:", "secrets.", "gh release"):
            self.assertNotIn(forbidden, self.text)

    def test_required_gates(self):
        for gate in ('test "$(go env GOVERSION)" = go1.27.1', "make ci", "make browser", "make smoke", "make fuse-smoke",
                     "make store-smoke", "make package-tested checksums verify-release", "make importer-dist",
                     "cmp \"$CPA_SMOKE_WORK/chat-archive-production.so\" dist/chat-archive.so",
                     "scripts/verify-cpa-package.py --cpa-source \"$CPA_SMOKE_WORK/source\"",
                     "eceasy/cli-proxy-api@sha256:873ec3df3b673a82f7d956d4636c7736b0c961dc6ee28f9fb403238b2898dc0d"):
            self.assertIn(gate, self.text)


class CanaryEncodingTests(unittest.TestCase):
    def test_every_encoding_is_found_where_it_is_embedded(self):
        value = "ca-canary-" + "x" * 40 + "/+?&="
        for name, needle in canaries.variants(value).items():
            for pad in (b"", b"a", b"ab"):
                self.assertTrue(needle in pad + needle + b"tail", name)
        for k in range(3):
            blob = base64.b64encode(b"z" * k + value.encode() + b"tail")
            self.assertTrue(canaries.contains_any(blob, value), k)
            blob = base64.urlsafe_b64encode(b"z" * k + value.encode())
            self.assertTrue(canaries.contains_any(blob, value), k)
        self.assertTrue(canaries.contains_any(json.dumps({"v": value}).encode(), value))
        self.assertFalse(canaries.contains_any(b"unrelated bytes", value))

    def test_generated_canaries_are_distinct_per_run(self):
        a, b = canaries.Canaries(), canaries.Canaries()
        self.assertNotEqual(a.client_key, b.client_key)
        self.assertFalse(set(a.all_secrets().values()) & set(b.all_secrets().values()))


if __name__ == "__main__":
    unittest.main()
