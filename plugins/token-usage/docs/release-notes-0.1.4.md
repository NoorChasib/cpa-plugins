# Token Usage 0.1.4

The sidebar signs in again with the session that official Management Center v1.27.0 and later remember. Plugin ID, routes, configuration, database paths, and collection behavior are unchanged.

## Why

Management Center v1.27.0 changed how it stores its remembered `cli-proxy-auth` session. New sessions are written as `enc::v2::`, keyed by the page's host alone. Earlier consoles wrote `enc::v1::`, keyed by the host and the browser's user agent. The console also discards an `enc::v1::` session when it loads. Token Usage 0.1.3 read only `enc::v1::`, so on v1.27.0 or later the sidebar found no remembered session even after signing in with **Remember password**.

## Changes

- **Reads `enc::v2::`.** The sidebar decodes the current form with the host-only key, and still decodes `enc::v1::` with the host and user agent, so older consoles keep working. Any other `enc::` version gives no session.
- **Nothing else about the session changed.** It is still read from the same storage entries, with the same strict UTF-8 decoding, `rememberPassword`, API base, and key checks, and the same handling of a rejected key.
- **Audited console codecs:** Management Center v1.27.2 (`f154fa9380825c4fe94bc577a9abaa7e505f8875`) for `enc::v2::` and v1.22.15 (`ed5f1c48e11ba7335f1e8f676f228c280196af85`) for `enc::v1::`. Browser tests write sessions with both codecs. A full console sign-in was not part of this verification.

Install or update Token Usage from the [combined catalog](../../../README.md#get-started). This release targets Linux amd64.
