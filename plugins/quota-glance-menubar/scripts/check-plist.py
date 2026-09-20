#!/usr/bin/env python3
"""Validate bundle metadata even on hosts without Apple's build tools."""
import base64
import pathlib
import plistlib

root = pathlib.Path(__file__).resolve().parent.parent
with (root / "Resources/Info.plist").open("rb") as source:
    info = plistlib.load(source)
assert info["CFBundleExecutable"] == "QuotaGlance"
assert info["CFBundleIdentifier"] == "com.noorchasib.quota-glance-menubar"
assert info["LSUIElement"] is True
assert info["LSMinimumSystemVersion"] == "13.0"
assert info["NSAppTransportSecurity"]["NSAllowsArbitraryLoadsInWebContent"] is True
assert "NSAllowsArbitraryLoads" not in info["NSAppTransportSecurity"]
print("Bundle metadata is valid.")

assert info["SUFeedURL"] == "https://raw.githubusercontent.com/NoorChasib/cpa-plugins/quota-glance-updates/appcast.xml"
assert len(base64.b64decode(info["SUPublicEDKey"], validate=True)) == 32
assert info["SUVerifyUpdateBeforeExtraction"] is True
assert info["SURequireSignedFeed"] is True
assert info["SUEnableAutomaticChecks"] is True
assert info["SUAutomaticallyUpdate"] is False
