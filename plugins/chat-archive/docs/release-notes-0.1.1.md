# Chat Archive 0.1.1

Fixes the capture of non-stream replies that CPA had already decoded, and ships `chat-archive-import` 0.1.1.

## What changed since 0.1.0

- **Non-stream replies with a `Content-Encoding` are stored again.** CPA decodes a compressed upstream reply itself but still hands the plugin's hooks the upstream `Content-Encoding` header. 0.1.0 tried to decode that plain JSON a second time and dropped it as `dropped: parse_error`, so every non-stream Anthropic `/v1/messages` reply was lost (streams were not affected). A labelled body that parses as a JSON object is now stored as it is, masked like any other body, with its response id, and carries the new flag `encoding_ignored`. A body labelled gzip that really is a gzip stream is still decoded (flag `gunzipped`). The same rule covers a request body CPA decoded whose inbound `Content-Encoding` the hook still sees. See [REFERENCE.md](../REFERENCE.md) and the `dropped.parse_error` row in [operations](operations.md).
- **The importer reads Codex rollouts that Codex compressed.** Codex compresses a rollout it has not written to for about a week (`rollout-*.jsonl.zst`) and restores it when the thread is resumed. The importer now reads the compressed form as the same file, so that history is imported and a cursor carries over between the two forms without re-uploading. When both forms exist, the plain file is read. See [importer](importer.md).
- **The importer fits a record that is over the archive's envelope limit.** A record such as a Codex tool call holding about 2 MB of short strings in nested objects used to become a `rejected_too_large` dead letter. The importer now moves the heaviest part of it into a separate piece and sends it; run and requeue summaries count `fitted=N`. Records under the limit are unchanged. A plain requeue rebuilds a row that 0.1.0 dead-lettered and sends it under its fitted key.
- **Docs on mask growth.** A secret's placeholder can be longer than the secret, so a text piece right at its limit can mask to a few bytes over it. The importer docs now say so and give the requeue sequence after the archive raises a limit.

## Upgrading

In CPA's Plugin Store, refresh the catalog and select **Update** for Chat Archive. Your configuration and local outbox are kept. Replies that 0.1.0 dropped as `parse_error` were never stored and are not recovered; only replies captured after the update are kept. The importer is not part of the plugin package: rebuild `chat-archive-import` from this release's source ([build](importer.md#build)) to get its changes.

## Known limits

- A labelled body that is really compressed with br, zstd or deflate, or that is not a JSON object, is still dropped as `parse_error` by design: the plugin adds no decoder, so a body still in its coding never becomes a piece.
- A non-stream reply that really is gzip-compressed is decoded and stored, but gets no `response_id`.
- The 0.1.0 limits still apply: requeue `full=1` and `split=1` are not supported, binary attachments are stored raw, and only Linux amd64 is supported.
