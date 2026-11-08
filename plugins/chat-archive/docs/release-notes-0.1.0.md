# Chat Archive 0.1.0

First release. Captures every chat exchange CPA v8.0.60 or newer serves (requests, attempts, responses including streams and Responses WebSocket turns, completions and usage records), masks them with `mask/v1`, splits them into content-addressed pieces and uploads them to the chat archive server from a local SQLite outbox. Adds the **Chat Archive** sidebar page (counters only) and the authenticated status and requeue routes.

Known limits of this version: requeue `full=1` and `split=1` are not supported; binary attachments are stored raw, because mask/v1 does not look inside binary formats; Linux amd64 only.
