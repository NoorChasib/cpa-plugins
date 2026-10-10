import base64
import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("appcast", Path(__file__).parents[1] / "publish-appcast.py")
appcast = importlib.util.module_from_spec(spec)
spec.loader.exec_module(appcast)


def feed(version="0.3.0", hardware="arm64"):
    # Structural fixture only; real signature verification uses Sparkle on macOS.
    # hardware=None models an earlier universal release, such as 0.4.0.
    signature = base64.b64encode(bytes(64)).decode()
    requirement = "" if hardware is None else f"<sparkle:hardwareRequirements>{hardware}</sparkle:hardwareRequirements>"
    return f'''<rss xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle"><channel><item>
      <sparkle:version>{version}</sparkle:version>{requirement}
      <enclosure url="https://github.com/NoorChasib/cpa-plugins/releases/download/quota-glance-menubar/v{version}/Quota-Glance-{version}-macOS.dmg" length="4" sparkle:edSignature="{signature}"/>
    </item></channel></rss><!-- sparkle-signatures: fixture -->'''.encode()


class AppcastTests(unittest.TestCase):
    def test_only_correct_release_and_asset_length_are_accepted(self):
        with tempfile.TemporaryDirectory() as folder:
            assets = Path(folder)
            archive = assets / "Quota-Glance-0.3.0-macOS.dmg"
            archive.write_bytes(b"test")
            self.assertEqual(appcast.validate_feed(feed(), "0.3.0", assets), "0.3.0")
            for data in [feed().replace(b"github.com", b"untrusted.example"),
                         feed().replace(b'length="4"', b'length="5"'),
                         feed().replace(b"edSignature", b"unsigned"),
                         feed().replace(b"sparkle-signatures:", b"unsigned:")]:
                with self.assertRaises(ValueError):
                    appcast.validate_feed(data, "0.3.0", assets)
            with self.assertRaises(ValueError):
                appcast.validate_feed(feed(), "0.4.0", assets)

    def test_late_release_cannot_downgrade_feed(self):
        self.assertFalse(appcast.should_publish(feed("0.10.0"), feed("0.9.0")))
        self.assertTrue(appcast.should_publish(feed("0.9.0"), feed("0.10.0")))

    def test_retries_are_idempotent_but_versions_cannot_be_replaced(self):
        self.assertFalse(appcast.should_publish(feed(), feed()))
        with self.assertRaises(ValueError):
            appcast.should_publish(feed(), feed().replace(b"fixture", b"changed"))

    def test_updates_require_apple_silicon(self):
        # Sparkle hides an arm64-only item from Intel Macs running a universal build.
        self.assertEqual(appcast.validate_feed(feed()), "0.3.0")
        self.assertEqual(appcast.validate_feed(feed(hardware=" ARM64 ")), "0.3.0")
        for hardware in [None, "", "x86_64"]:
            with self.assertRaises(ValueError):
                appcast.validate_feed(feed(hardware=hardware))

    def test_universal_feed_can_be_replaced_only_by_an_arm64_update(self):
        universal = feed("0.4.0", hardware=None)
        self.assertTrue(appcast.should_publish(universal, feed("0.5.0")))
        with self.assertRaises(ValueError):
            appcast.should_publish(universal, feed("0.5.0", hardware=None))
