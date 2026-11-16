# Chat Archive 0.1.3

Keeps capturing the exchanges that are open while CPA replaces the plugin with a new version, stores the rest of each one under new keys in the new version, ends the whole stop within CPA's time limit, and reports skipped duplicate envelopes as drops.

## What changed since 0.1.2

- **A stop keeps capturing exchanges already begun.** CPA keeps sending every hook to the old instance until `plugin.quiesce` returns and the new version is loaded, but 0.1.2 closed admission as soon as the quiesce began. The replies and completions of exchanges in flight during an update or a config change were therefore dropped as `stopped`. A quiesce, and a reconfigure that keeps capture on, now keep capturing until the exchanges that were open when the stop began have finished, for at most 15 seconds. Exchanges that begin during this wait do not extend it, so steady traffic does not hold every update to the bound. After admission closes, the plugin processes what the hooks had queued instead of counting it `stopped`. A stream that is still open is written as a partial response (`terminal_status` `incomplete`, `incomplete_reason` `plugin_stopped`, `truncated`). Turning capture off and shutdown still stop at once.
- **Exchanges open across an update keep both halves.** The new instance gets the rest of every exchange still open, and in 0.1.2 it numbered those exchanges from zero again. The rest of a cut stream, and every later turn of a Codex Responses WebSocket left open across the update, therefore reused envelope keys the old instance had already written. The outbox skipped them as duplicates, and that content was not stored. The stopping instance now saves, in its outbox, each open exchange's `input_seq`, attempt `ordinal` and response `index`, and whether its begin was written and how many responses were. The new instance takes them at its start and continues from there. A cut stream is stored as the old version's partial response followed by the new version's next response, marked `truncated`. A WebSocket's turns keep `input_seq`, ordinals and response indexes in call order across both versions. The exchange's single completion counts every response and reports `begin_written` for the begin the old version wrote. See [REFERENCE.md](../REFERENCE.md#stops-and-upgrades).
- **A skipped duplicate envelope counts as a drop.** An envelope the outbox skips because its key is already queued is now reported in heartbeats and the status route as `dropped.duplicate_key`. Before, it was a local counter that never reached the archive, so its capture-drop alert could not see it.
- **A stop ends within 25.5 seconds.** The wait, the uploader's stop and final heartbeat, the capture drain and the final saves share one deadline 25 seconds after the stop began, under the 30 seconds CPA allows. Only the outbox close follows it, and it is cut short half a second past the deadline (0.1.2 gave it up to 5 seconds of its own); a close cut short leaves the run marked unclean, reported in `unclean_runs` at the next start. An upload cut short stays pending in the outbox and is sent after the next start.
- **A body dropped at a stop no longer takes its exchange's completion with it.** When the stop's drain bound passed with a body still queued, 0.1.2 counted the body `dropped.stopped` but never wrote the completion that was waiting for it, without counting that loss. The completion is now written, with `begin_written` false or without the dropped response in `responses`; if the writer cannot take it before the stop's deadline, it counts as `dropped.stopped` too.

## Upgrading

In CPA's Plugin Store, refresh the catalog and select **Update** for Chat Archive. Your configuration and local outbox are kept. At its first start, 0.1.3 adds a small `open_requests` table to the outbox; 0.1.2 ignores it if you go back.

The update from 0.1.2 runs 0.1.2's stop, so that one switch still behaves the old way. Replies and completions of exchanges in flight are dropped as `stopped`, and a long stream or an open Responses WebSocket loses its continuation. To avoid this, switch while CPA is quiet:

1. Let running requests finish and pause the clients that use CPA. Close Codex sessions too, so no Responses WebSocket stays open.
2. Select **Update** and wait until the Plugin Store shows Chat Archive 0.1.3 running.
3. Resume the clients.
4. Check that `dropped.stopped` and `dropped.duplicate_key` did not rise, in the plugin's status route or the archive's next heartbeat.

Going back to 0.1.2 needs the same quiet switch: 0.1.2 cannot take the numbering 0.1.3 saves, so an exchange open across the switch would reuse keys. Later updates from 0.1.3 behave as described above.

## Known limits

- Hooks that CPA sends after the old instance closed admission and before the new version is serving are still lost: counted `dropped.stopped` while the old instance is stopping, and uncounted once its stop has finished. After the wait, that window holds only the old instance's final flush, the outbox close and CPA's load of the new library.
- An exchange that stays open (a long stream, or a Responses WebSocket between turns) still holds the wait to its 15-second bound.
- A Responses WebSocket turn that the new version receives is marked `truncated` even when it is whole. CPA numbers a socket's stream chunks across its turns, and the new version did not see the first ones.
- The saved numbering is taken only by a start within 2 minutes of the stop. A new version that starts later numbers those exchanges from zero again, and any key it reuses counts as `dropped.duplicate_key`.
- The 0.1.2 limits still apply.
