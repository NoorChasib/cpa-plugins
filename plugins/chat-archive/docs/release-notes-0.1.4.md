# Chat Archive 0.1.4

Stops storing model-list and model-detail polls as empty exchanges under CPA v8.0.70 and later.

## What changed since 0.1.3

- **Model lists and model details are not archived again under CPA v8.0.70 and later.** From v8.0.70, CPA sends the caller's identity with the response and completion of every `/v1/models`, `/v1/models/{id}` and `/v1beta/models` call from a client with a key. Earlier versions sent no metadata at all, and 0.1.3 partly relied on that missing metadata to recognize a model list. So under CPA v8.0.70 and later, each list or detail a keyed client fetched was stored as an exchange with a response and a completion but no begin, no endpoint and no model. The response held the model catalog. 0.1.4 recognizes these calls by the absence of `requested_model`, which CPA sets on every model execution and never on a model list. The polls are ignored again under every CPA version and counted in `ignored_model_list` on the status route (2 per call, one for each hook). See [REFERENCE.md](../REFERENCE.md#what-is-captured).
- **No conversation data was lost.** The polls were extra exchanges stored beside the real ones. Conversations were captured as before and are captured the same way now. Executions that reach the plugin without a begin, such as one whose before-auth went to an earlier plugin instance, still carry `requested_model` and are still stored.
- **Native acceptance runs on CPA v8.0.73.** The required native smoke builds CPA v8.0.73 from pinned source (it was v8.0.60). It now also sends keyed model-list and model-detail calls and requires them to leave no envelope.

## Upgrading

In CPA's Plugin Store, refresh the catalog and select **Update** for Chat Archive. Nothing changes in the configuration or in the outbox: your settings and local outbox are kept as they are. The update runs 0.1.3's stop, which keeps capturing the exchanges already open, so no quiet switch is needed. Going back to 0.1.3 works the same way, but under CPA v8.0.70 and later 0.1.3 stores the polls again.

Exchanges already stored from these polls are not removed by the update.

## Known limits

- The plugin recognizes a model list because its metadata has no `requested_model`. If a later CPA added that key to model-list calls, the polls would be stored again as exchanges without a begin.
- A model execution with no model, no requested model, no `requested_model` in its metadata and no before-auth seen by the plugin would be ignored as a model list. CPA sets `requested_model` on every execution.
- The 0.1.3 limits still apply.
