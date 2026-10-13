# Reset Priority

Give higher priority to healthy Claude and Codex accounts whose regular weekly quota resets sooner. Provider groups are ranked independently.

## Install

1. Ensure CPA supports this plugin's native ABI/platform and persist its plugin directory. In the standard Docker image, mount your existing plugins volume at `/CLIProxyAPI/plugins`.
2. Before you install the plugin, merge the configuration below into your existing `config.yaml`: the store source goes in the existing `plugins.store-sources` list, and Reset Priority's entry keeps `dry-run: true`. Do not create a second `plugins:` mapping. Reset Priority saves credential priorities from its first start unless `dry-run` is already on, and CPA can load it as soon as it is installed, before its options appear in the console.
3. Install the plugin from CPA's Plugin Store and follow any restart prompt. To change its options later, on CPA's Plugins page, open Reset Priority's **Edit config**; the YAML below is the same configuration, for reference.

This source lists all five plugins from this repository, including the optional quota-cache preview. If this plugin is already installed from an old source, follow the [migration guide](../../docs/migration.md) before switching; CPA v7.2.155 will otherwise reject the source change.

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json"
  configs:
    reset-priority:
      enabled: true
      dry-run: true
      reconcile-interval: 1h
```

## First use and data

Inspect Reset Priority's status page after discovery. Confirm the expected account order, then set `dry-run: false` to allow credential-priority writes. Existing installations should preserve their current dry-run setting during migration.

CPA's `routing.strategy: fill-first` is recommended for this hard-priority policy. Keep your existing routing settings during migration; changing routing is a separate decision.

By default this version reads provider quota endpoints directly (see [Optional shared quota cache](#optional-shared-quota-cache)). Its regular reconciliation interval does not govern every read: startup, account changes, reset deadlines, recovery, and manual refresh can also cause activity. Avoid repeated manual refreshes while investigating 429s.

Preserve the auth directory: written priority/quarantine values live in the physical credential files. Runtime scheduling state is rebuilt after restart.

## More detail

- [Complete behavior and compatibility reference](REFERENCE.md)
- [Migration without losing settings or data](../../docs/migration.md)
- [All configuration options](config.example.yaml)

## Optional shared quota cache

Version 0.2.1 works independently by default. Quota Cache is **not required** for OAuth priorities; API credit routing needs Quota Cache 0.2.0.

To share quota observations, install Quota Cache. Then, on CPA's Plugins page, open Reset Priority's **Edit config** and turn on **use-quota-cache**:

```yaml
use-quota-cache: true
# Optional if Quota Cache uses its default path:
# quota-cache-path: plugins/data/quota-cache/snapshot.json
```

With the toggle on, missing, stale, or failed observations wait for fresh cache data; there is no direct-provider fallback. Right after a CPA restart, accounts may show `unknown` briefly; see [Restarting in quota-cache mode](REFERENCE.md#restarting-in-quota-cache-mode). Set `use-quota-cache: false` to restore standalone polling. A `quota-cache-path` without `use-quota-cache`, as preview 1 configured it, also opts in until the toggle is saved as false. The form on CPA's Plugins page shows the toggle off while it is unset, and saves only the fields you change, so switch it on and off again before you **Save**.

Auto Baseline and Token Usage do not use Quota Cache. See [shared quota setup](../../docs/quota-cache-preview.md).

## Spend the credit that refills soonest (optional)

With `route-api-credits: true`, Reset Priority also becomes CPA's scheduler for one kind of request attempt: the ones where every credential CPA offers is a Claude API key from CPA's own configuration. For those it picks a key of the Console organization whose monthly API credit refills soonest and still has credit left, so that credit is spent before it expires. Every other attempt, and every attempt it is unsure about, is left to CPA. It never refuses a request and never changes how OAuth priorities work. Off, CPA routes exactly as it does without it.

Reset Priority's status page shows the router in its `router` section: the `router` object of `GET /v0/management/plugins/reset-priority/status`, and **Claude API credit routing** on the HTML view.

To turn it on:

1. Install Quota Cache **0.2.0 or newer** and add your Console organizations, with their monthly credits and refill dates, on its page. Reset Priority routes on the figures Quota Cache publishes. If Quota Cache uses a non-default `cache-path`, set `quota-cache-path` here to the same path. If you do not also want OAuth priorities to come from Quota Cache, set `use-quota-cache: false` as well: the router still reads that path, while a bare `quota-cache-path` switches the OAuth engine to cache mode. In the form on CPA's Plugins page the toggle shows off until it is saved, so switch it on and off again before you **Save**, or the form leaves it unset.
2. Give the organizations' keys model names of their own. Put the keys in an `api-keys.claude` group with no `prefix`, and give them a `models:` list that maps each Claude model they should serve to an alias starting with `credits-claude-`, such as `credits-claude-sonnet-5-5`: the same aliases on every credits key, and on no other credential. Clients that should spend the credit ask for those names. No subscription serves them and the credits keys serve nothing else, so every attempt for them is Reset Priority's to route, and plain Claude model names never reach the credits keys. In the console that is AI Providers → Claude → the key's **Edit** → **Custom models** → **Add model**, with the **Model name** and an **Alias (optional)**. The alternative, a `prefix` such as `credits` with CPA's `routing.force-model-prefix: true`, is in [REFERENCE.md](REFERENCE.md#what-it-does). Settle the prefix now, before step 4: adding, changing or removing a key's prefix gives it a new auth ID.
3. Add the failover rule, so a request Anthropic refuses for low credit moves to the next key. In the console, on each credits key: AI Providers → Claude → the key's **Edit** → **Advanced runtime policy** → **Request error rules**: **Override**, then **Add rule** with **Status code** `400`, **Match type** **Contains text**, **Condition** `credit balance is too low` and **Action** **Continue and cool down**, and **Save**. Or, in `config.yaml`, put the `request-scoped-errors` rule at the end of [`config.example.yaml`](config.example.yaml) on the group, and every key inherits it. Add the two optional 400 rules there too if a key has no workspace ID or an organization has a spend limit.
4. With routing still off, let traffic flow until every Claude API key in CPA's configuration has served a request through CPA. The status shows that once `router.table_reason` is `""`, `router.unmapped_keys` and `router.failing_keys` are `0`, **and** `router.mapped_keys`, `router.foreign_keys` and `router.unlinked_keys` add up to the number of keys in all your `api-keys.claude` groups, whatever models they serve, not only the credits group. A key listed in two groups counts twice. The zeros alone are not enough: a key that has not served a request yet is in no count, and the counts cover only the last 7 days. CPA's `fill-first` strategy keeps choosing the first key, so either switch `routing.strategy` to `round-robin` until every key is counted, or expect Reset Priority to explore each remaining key once routing is on. Turning routing on before then still makes CPA consult Reset Priority for every request.

   A key that fails every request, such as a personal key sent without an `anthropic-workspace-id` header, can hold `router.unmapped_keys` or `router.failing_keys` above `0` for as long as CPA sends it requests. Fix it, or remove it from CPA's configuration; otherwise Reset Priority keeps exploring it once routing is on. After you change a key's API key, prefix, base URL, proxy or headers, or remove the key, its old auth ID keeps counting for 7 days after its last request, so the check above holds again only once that ID has moved to `router.stale_keys`. [New and unproven keys](REFERENCE.md#new-and-unproven-keys) has what exploring costs and what else moves these counts.
5. On CPA's Plugins page, open Reset Priority's **Edit config** and turn on `route-api-credits`. Or set `route-api-credits: true` in `config.yaml`.
6. Check that the status shows `router.active` as `true`.

Three things to know first:

- **Routing needs CPA v8.0.4 or newer. Before v8.0.22, turn routing off before turning Quota Cache off.** Those versions leave a plugin that is turned off running until CPA restarts, and only stop sending it usage records, so Quota Cache keeps publishing figures that no longer count spend, and Reset Priority keeps routing on them. After turning Quota Cache back on, enter a Console reading for each organization on its page, then turn routing back on.
- **Never point interactive Claude Code at the credits keys**, the `credits-claude-…` names. The monthly credit does not cover it, and with the rule above one Claude Code turn can cool every credits key for that model. For the same reason, give these keys no `cloak` block, or one with `mode: never`, and no `fingerprint-profile: claude-code-cli`. A `cloak` block in any other mode, or that profile, makes CPA send every request on them as Claude Code, whichever client sent it.
- **Turn routing off before updating, disabling or removing Reset Priority**, even for an update that is only about OAuth priorities, and wait until `router.counters.picks` stops moving. A request that reaches the plugin while CPA unloads it fails.

Reset Priority 0.1.8 and older ignore the key. [REFERENCE.md](REFERENCE.md#claude-api-credit-routing-optional) has how it chooses, when it steps aside, failover and Claude Code, what turning it on changes for the rest of CPA's traffic, and every status field.
