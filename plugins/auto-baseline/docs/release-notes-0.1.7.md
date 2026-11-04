# Auto Baseline 0.1.7

The sidebar status page upgrades to the full view again with the session that official Management Center v1.27.0 and later remember. Plugin ID, routes, settings, and the state and backup paths are unchanged.

## Why

The redacted status page upgrades itself by reading the management key the console remembered in this origin's localStorage (`cli-proxy-auth`, with **Remember password** on). Management Center v1.27.0 changed that entry: new sessions are written as `enc::v2::`, keyed by the page's host alone, instead of `enc::v1::`, keyed by the host and the browser's user agent, and the console discards an `enc::v1::` entry when it loads. 0.1.6 read only `enc::v1::`, so on v1.27.0 or later the page stayed redacted.

## Changes

- **Reads `enc::v2::`.** The page decodes the current form with the host-only key and still decodes `enc::v1::` with the host and user agent, so older consoles keep working.
- **Nothing else changed.** The page reads the same storage entries, falls back to the legacy `managementKey` entry as before, and handles a rejected key the same way. Config writes, learning, and promotions are untouched.

Install or update Auto Baseline from the [combined catalog](../../../README.md#get-started). This release targets Linux amd64.
