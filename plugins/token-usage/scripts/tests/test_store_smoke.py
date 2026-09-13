"""Fail-closed contracts supporting the real Docker/HTTP acceptance, not replacing it."""
import importlib.util
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/native"))
SPEC = importlib.util.spec_from_file_location("store_smoke", ROOT / "scripts/native/store_smoke.py")
smoke = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(smoke)


class StoreSmokeContracts(unittest.TestCase):
    def generated(self):
        return {"enabled": True, "store": {"id": "token-usage", "version": "0.1.1",
            "source-url": smoke.STORE_URL, "install": {"type": "direct"}}}

    def test_assertion_requires_actual_preserved_handler_config(self):
        with patch.object(smoke, "request_json", return_value=self.generated()) as request:
            self.assertEqual(smoke.assert_generated_config("http://127.0.0.1:1", "0.1.1"), self.generated())
        request.assert_called_once_with("http://127.0.0.1:1", "/v0/management/plugins/token-usage/config")

    def test_config_regression_cannot_pass_with_missing_store_or_manual_database_override(self):
        cases = [{"enabled": True}, {**self.generated(), "database-path": "/tmp/manual.sqlite"},
                 {**self.generated(), "enabled": False}]
        for item in cases:
            with self.subTest(item=item), patch.object(smoke, "request_json", return_value=item):
                with self.assertRaises((AssertionError, KeyError)):
                    smoke.assert_generated_config("http://127.0.0.1:1", "0.1.1")

    def test_inventory_does_not_confuse_configured_with_registered(self):
        item = {"configured": True, "enabled": True, "registered": False, "effective_enabled": False}
        with self.assertRaises(AssertionError):
            smoke.assert_discovery(item)

    def test_synthetic_logs_strip_all_credential_canaries(self):
        raw = " ".join(smoke.production.CANARIES)
        clean = smoke.sanitize(raw)
        self.assertTrue(all(key not in clean for key in smoke.production.CANARIES))

    def test_browser_manifest_lock_and_binary_are_exact_test_only_pins(self):
        directory = ROOT / "scripts/browser"
        package = json.loads((directory / "package.json").read_text())
        lock = json.loads((directory / "package-lock.json").read_text())
        self.assertTrue(package["private"])
        self.assertEqual(package["engines"], {"node": "26.8.1", "npm": "11.19.0"})
        self.assertEqual(package["devDependencies"], {"agent-browser": "0.37.1"})
        tool = lock["packages"]["node_modules/agent-browser"]
        self.assertEqual(tool["version"], "0.37.1")
        self.assertEqual(tool["integrity"], "sha512-NDojTSXrIq7zS090T0VwI1uzBryZWvewgxXvS0swj8/5GGnziBZLmyhEkfETO8n4n3DsJpZbZkq9F/MV2rIQKw==")
        self.assertIn("f8e5f9294bd0da70dda61854f12004fd61c668cd682bfb600cdf6d0df73dea69", (directory / "check-tools.sh").read_text())
        self.assertIn("167a098c4fdec156b58a9f678c90a84f9072d789f9c6e7b35496a6987b8b7ef8", (directory / "install-chrome.sh").read_text())
        runner = (directory / "run.sh").read_text()
        self.assertIn("export TOKEN_USAGE_BROWSER_TEST=1", runner)
        self.assertNotIn("exit 0", runner)
        self.assertNotIn("|| true", runner)

    def test_both_workflows_require_browser_and_actual_store_before_package(self):
        for name in ("ci.yml", "release.yml"):
            workflow = (ROOT / ".github/workflows" / name).read_text()
            steps = ["run: make browser-tools", "run: make browser\n", "make smoke | tee",
                     "make store-smoke CPA_SMOKE_WORK=", "make package-tested checksums verify-release"]
            positions = [workflow.index(step) for step in steps]
            with self.subTest(name=name):
                self.assertEqual(positions, sorted(positions))
                for unsafe in ("continue-on-error", "|| true", "package-current"):
                    self.assertNotIn(unsafe, workflow)
                self.assertIn(smoke.IMAGE, workflow)
                self.assertIn('docker pull --platform linux/amd64 ' + smoke.PYTHON_IMAGE, workflow)
