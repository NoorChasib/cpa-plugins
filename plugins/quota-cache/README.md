# Quota Cache

One scheduled poller for Claude, Codex, and Grok quota windows, credit balances, banked Codex and Claude rate-limit resets, plans and renewal dates, and availability flags, and optionally your OpenRouter account balance and your Claude API credit spend. Account Health and Reset Priority can read its saved observations instead of each contacting the providers.

**Linux amd64:** Quota Cache 0.1.5 normalizes every provider's quota windows into one canonical vocabulary and records the subscription plan beside each credential, so a consumer never pattern-matches a provider string. 0.1.4's extended fields are still there — expand **All cached quota fields** in the sidebar to inspect them. Quota Cache 0.1.10 adds Claude's banked rate-limit resets, each account's plan from Claude's and Grok's account endpoints, Codex's subscription renewal date, and the Claude subscription start and billing period that a consumer can estimate a renewal from ([account details](docs/cache-format.md#account-details)). Everything is additive and the snapshot stays on schema 1: Account Health and Reset Priority keep their regular weekly/pool behavior and need no rebuild. Quota Cache 0.1.11 keeps saved observations, failure backoff, and account details through a CPA restart instead of clearing them while CPA is still loading its credentials. Quota Cache 0.1.12 stops one rate-limited or dead credential from pausing its provider for hours, stops a 429 on Codex's optional reset-credit request from discarding a good reading, sends polls at the configured spacing instead of often twice it, and logs every failed poll to CPA's log ([polling schedule and failures](docs/cache-format.md#polling-schedule-and-failures)). Quota Cache 0.1.13 adds [Claude API credits](#claude-api-credits-optional): each Claude Console organization's spend against its Max or Team plan's monthly API credit, with an `anthropic-api` entry per organization ([format](docs/cache-format.md#claude-api-credits)). Quota Cache 0.1.14 stops reading Anthropic's cost report for them: it counts each organization's Claude API-key traffic as CPA serves it, keyed by the organization's id, holds no Anthropic key, and sends Anthropic nothing. Quota Cache 0.2.0 keeps the organizations, monthly credits, refill dates, Console readings and Claude renewal dates you enter, on its own page in CPA's sidebar, and publishes each organization's remaining credit beside its snapshot ([credit settings](docs/credit-settings.md)). Quota Glance 0.8.0 and Reset Priority 0.2.0 read what this version publishes. Quota Cache 0.2.1 answers a method it does not handle with CPA's `unknown_method` error code instead of a generic plugin failure; the message is still the fixed `quota cache request failed`. See the [cache format and reader API](docs/cache-format.md) for building another consumer.

## Install and start

Add `https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json` as your store source, then install **Quota Cache**. If you already use `preview/registry.json`, keep that source and select **Update** for this plugin. Both catalogs advance together. Follow any disable/restart instruction CPA gives for a loaded native library.

### Build from source

Target: Linux amd64, native ABI 1 / schema 4+, including CPA v7.2.155 and v8.0.4; Claude warm-up needs CPA v8.0.4 or newer. The writer uses Linux file locking. You need Go 1.26+ and a C toolchain; verification uses Go 1.27.1.

From the repository root:

```sh
make -C plugins/quota-cache ci
make -C plugins/account-health-pushover c-shared
make -C plugins/reset-priority build
```

Outputs are `plugins/quota-cache/dist/quota-cache.so`, `plugins/account-health-pushover/dist/account-health-pushover.so`, and `plugins/reset-priority/reset-priority.so`. These are local builds; the preview release provides checksum-verified artifacts of the tested binaries. Follow the [migration guide](../../docs/migration.md) before replacing installed libraries, especially versioned store-managed files.

Set these on CPA's Plugins page: open Quota Cache's **Edit config**, which shows them under **Config fields** in the **Configure quota-cache** dialog, and set each consumer's through its own **Edit config**. The YAML below is the same configuration, for reference: if you edit `config.yaml` instead, merge it into your existing plugin configuration and keep all other options and data paths:

```yaml
plugins:
  enabled: true
  configs:
    quota-cache:
      enabled: true
      cache-path: /CLIProxyAPI/plugins/data/quota-cache/snapshot.json
      poll-interval: 15m
      request-spacing: 10s
    account-health-pushover:
      use-quota-cache: true
      quota-cache-path: /CLIProxyAPI/plugins/data/quota-cache/snapshot.json
    reset-priority:
      use-quota-cache: true
      quota-cache-path: /CLIProxyAPI/plugins/data/quota-cache/snapshot.json
```

Quota Cache is optional. Consumers default to standalone polling; enable `use-quota-cache` to opt in. Explicit false overrides a saved path and restores standalone polling. With the toggle on, missing or failed data waits without provider fallback.

Install/start the cache first and wait for observations, then enable cache mode in the updated consumers. Preserve Account Health's existing `quota-alerts` preference; the cache does not enable notifications itself. Keep Reset Priority's existing dry-run setting.

In the standard Coolify/Docker layout the cache is inside the existing `/CLIProxyAPI/plugins` volume. No new network port or management key is needed between plugins. The cache directory must be private (`0700`) and owned by the CPA process user; snapshots are written with `0600`. Custom paths must be identical across the three plugins and remain on a local filesystem supporting atomic rename and file locking.

Open **Quota Cache** in the CPA sidebar. It shows fresh versus stale observations, last/next polling times, account quotas, provider-wide cooldowns, request endpoints, HTTP results, durations, and the last 100 completed polls. History and cumulative totals survive restarts. Existing snapshots load unchanged; newly added history begins with subsequent polls.

**Refresh view** and the optional 30-second view refresh only read cached status. They never request a provider poll. Status-read counts cover this authenticated endpoint; consumer file reads cannot be counted. The page follows the other plugins' light/dark styling and uses your browser's time zone.

The sidebar resource is a static public shell; operational information comes only from authenticated `GET /v0/management/plugins/quota-cache/status` using your same-origin CPA session. No new login, key, or port is needed.

The page uses that session only if you signed in to the CPA console on the same address with **Remember password** ticked; without one it says so and sends nothing. CPA locks an address out of its management API for 30 minutes after five failed sign-ins, so since 0.1.10, when CPA refuses the session the page stops asking: automatic updates pause, and it does not present that key again, even after a reload, until you select **Refresh view**. Automatic updates also pause when a status request gets no answer at all, since CPA may have refused and counted it; select **Refresh view** to resume.

If version 0.1.0 becomes unregistered after changing the relative default to its equivalent absolute path, restore `cache-path: plugins/data/quota-cache/snapshot.json` until you update. Version 0.1.1 normalizes these paths before comparing configuration, so an unchanged location does not require a restart. Actual schedule or location changes still require a native restart; safe failure reasons now appear in CPA logs.

## Claude session warm-up (optional)

Warm idle Claude accounts with a tiny Haiku 5.5 prompt, automatically during
your chosen hours or with **Warm all now** on the Quota Cache page. It is off by
default. CPA sends each warm-up through the account it already holds, so there
is no second sign-in; it needs CPA v8.0.4 or newer. The model and effort are
editable in **Plugins → Quota Cache → Edit config**, with no restart. See
[warm-up requirements and behavior](docs/claude-warmup.md).

## OpenRouter balance (optional)

Quota Cache 0.1.9 can also read how much money is left on your OpenRouter account, which [Quota Glance](../quota-glance/README.md#openrouter-balance) shows as a card of its own. OpenRouter reports the account balance only to a **management key**. The inference keys CPA routes with are refused, so this needs a key of its own:

1. Create one on OpenRouter's [Management API Keys page](https://openrouter.ai/settings/management-keys). **Give it an expiry.** A management key cannot make model requests, but it can create, edit, and delete your API keys, OpenRouter offers no read-only scope, and the expiry is fixed when the key is created.
2. Set it on CPA's Plugins page: open Quota Cache's **Edit config** and enter it in **openrouter-management-key** under **Config fields**. Or add it to `config.yaml`:

   ```yaml
   quota-cache:
     openrouter-management-key: sk-or-v1-...
   ```

It takes effect at the next scan, with no restart. Quota Cache then makes one `GET https://openrouter.ai/api/v1/credits` per `poll-interval`, with the same spacing, failure backoff, and 429 cooldown as every other account. Clear the setting and the account is dropped at the next scan.

The snapshot keeps OpenRouter's purchased and spent totals and never the key. The account is recorded as `openrouter:key-<fingerprint>`, a hash of the key, so a rotated or corrected key starts fresh rather than waiting out the previous key's failure backoff. Like every plugin setting, the key is stored in plain text in CPA's configuration file and is readable by anyone who holds the CPA management key.

## Claude API credits (optional)

Max and Team plans include a monthly Claude API credit, deposited into one Claude Console organization per plan and shared by every API key in it. Quota Cache can count what each organization's API keys spend through CPA and estimate what is left. Nothing is asked of Anthropic: Quota Cache registers with CPA as a usage plugin, counts the tokens of every Claude API-key request CPA serves, per Console organization, and saves the count to a meter file beside its snapshot (`snapshot.meter.json`). Quota Cache prices the tokens itself at Anthropic's list prices and publishes the estimate beside its snapshot (`snapshot.credits.json`), which [Quota Glance](../quota-glance/README.md) shows and Reset Priority can route on. Every figure is an estimate; from time to time you type the balance Console shows on this plugin's page, which anchors it.

Requirements:

- **CPA v8.0.4 or newer (verified on v8.0.22).** An older CPA can hand a retried request the earlier attempt's response headers, which attributes its spend to the wrong organization.
- Every organization's API traffic goes through CPA as `claude-api-key` entries. Traffic that does not pass through CPA (the Console Workbench, other tools) is not counted. A personal key that is not scoped to a workspace needs an `anthropic-workspace-id` header for inference; a `claude-api-key` entry's `headers:` map can carry it. A key created inside a Console workspace is already scoped and needs no header, which is simpler.
- Quota Glance 0.8.0 and Reset Priority 0.2.0 read what Quota Cache 0.2.0 publishes. An older Quota Glance keeps its own editor and its own file ([updating from Quota Glance 0.7](#updating-from-quota-glance-07)).

Everything is entered on this plugin's page, not in its **Edit config** on CPA's Plugins page: open **Quota Cache** in the CPA sidebar, signed in to the console as described above. Add one organization per Console organization. Two Team seats share one organization and one credit, so they are a single organization: four personal Max organizations and one Team organization make five.

1. Under **Organizations**, select **Add organization**, or, once organizations are listed, **Edit organizations** and fill in the **Add an organization** row. Enter a label, your unique name for it, and its **Organization ID**, copied from Console under **Settings > Organization**. It is not a secret, and no key of any kind is entered.
2. Under **Claude API credits**, select **Set credits and dates** (**Edit credits and dates** once a credit is set) and set each organization's **Monthly credit, USD**, in dollars (Team pools differ, so enter what Console shows), and **Refills on, UTC**: the date the linked claude.ai plan renews (claude.ai **Settings > Billing**, or **Organization settings > Billing** on Team). Console's expiry date for the current credit (**Settings > Billing > Promotional credits**) may be the day before; if so, use the claude.ai date. Only the day of the month is used, and it repeats monthly.
3. From time to time, add a **Console reading**: the balance Console shows, with when you read it. It anchors the estimate and corrects any spend that did not pass through CPA.
4. Under **Claude renewal dates**, **Set renewal dates** enters the day each Claude subscription renews, which Anthropic does not report. Without one, Quota Glance shows an estimate.

A change takes effect at once, with no restart: an organization added on the page is counted from its next request. Removing one keeps the amounts and readings entered for it as an orphan you can clear, and adding the same Organization ID again brings them back. The page needs a console session, so a phone, a bookmark or the menu bar can only read. Quota Glance shows the values read-only, with a link to this page when you are signed in to the console.

Quota Cache 0.1.13 and 0.1.14 read the organizations, with their `monthly-usd` and `renews`, from `claude-api-credits` in their configuration. 0.2.0 does not: it lists only the organizations added on its page, and nothing moves there from the configuration. A configuration that still has `claude-api-credits`, `admin-key` from 0.1.13 included, loads as before and is ignored: CPA's log says so once per load, and the page shows how many organizations it still lists. You can leave it; only an older Quota Cache reads it, which keeps a [rollback](docs/credit-settings.md#rollback) simple.

The snapshot keeps each organization's label and Organization ID under `api_credit` and nothing else: no key, and no spend. The meter file keeps, per organization, the tokens of each UTC day (today and the 40 before) and hour (this hour and the 72 before) per model, low-credit refusals, the organizations CPA served that nothing lists, and a map of CPA's own auth indexes to organizations. Quota Cache 0.2.0 also remembers which CPA auth ID each Claude API key from CPA's configuration uses (an ID such as `claude:apikey:` followed by 12 hex digits, never a credential's name) and whether its latest attempts succeeded, so Reset Priority 0.2.0 can route by organization. It never holds a key, a request or response body, a session id or a client identity. Counting stops while Quota Cache is off, reloading or disabled, and every stop is recorded as a gap; the published estimate is marked incomplete after a stop of five minutes or more until you enter a Console reading, and shorter ones are ignored. Details are in [Claude API credits](docs/cache-format.md#claude-api-credits).

### Updating from Quota Glance 0.7

**Before you start**, write down from Quota Glance 0.7.2 each organization's monthly credit and refill date, plus its Console reading if you use them, and each Claude account's renewal date. Quota Cache 0.2.0 doesn't read them, and Quota Glance 0.7.2's **Monthly API Credit** card is gone until the organizations are added again.

1. Update Quota Cache first.
2. Open Quota Cache's page. Under **Organizations**, add each Claude Console organization: a label and its Organization ID (Console → Settings → Organization). Use the same Organization IDs as before, so counting carries on.
3. Under **Claude API credits**, enter each organization's monthly credit and refill date, plus a Console reading if you use them. Enter each Claude account's renewal date where you had set one.
4. Then update Quota Glance, then Reset Priority.

Until Quota Glance is updated, save nothing in its editor: Quota Cache doesn't read those saves, and while only some organizations are added, Quota Glance 0.7.2 offers **Remove** for the others' saved values.

**Counting across the update.** Claude API spend is not counted from the update until the first organization is added. From then on the meter keeps the traffic of every organization Quota Cache 0.1.14 counted, so organizations added later lose nothing more. Add the organizations right after updating. If the gap matters, take a Console reading for each organization once it is added and enter it: the reading covers the gap.

`claude-api-credits` can stay in `config.yaml`: Quota Cache 0.2.0 ignores it and an older Quota Cache reads it, which keeps a rollback simple. Quota Glance's `settings.json` stays where it is, untouched and unread, and rolling Quota Glance back to 0.7.2 shows its old values again. [Rollback](docs/credit-settings.md#rollback) says how to carry the newer values back.

## Polling behavior

- At most one provider request runs at a time in the cache plugin. Polls are spaced 10 seconds apart by default; the follow-up requests a poll can make (Codex's reset inventory, account details) run straight after it.
- Each credential is polled at most once per 15-minute interval by default, plus once more in each refresh after a banked reset (below). The initial accounts are staggered by request spacing. If you shorten `poll-interval`, keep your number of credentials times `request-spacing` well below it, so every credential fits into each interval.
- Plans and subscription renewal dates are read from separate account endpoints at most once per credential every six hours, and a failure there, like a failure of Codex's reset inventory, never fails the poll, backs it off, or pauses the provider. See [account details and requests per credential](docs/cache-format.md#account-details).
- Claude Console organizations are never polled. Their spend is counted from the usage records CPA hands Quota Cache ([Claude API credits](#claude-api-credits-optional)); no request to Anthropic is made for them, and they take no place in the polling schedule.
- On 429 from Claude, Codex, Grok, or OpenRouter, all accounts for that provider pause for one poll interval, and the rate-limited credential retries after at most an hour, or one interval if that is longer; a longer `Retry-After` from the provider extends either. The previous observation is kept, and other failures back off up to six hours. See [polling schedule and failures](docs/cache-format.md#polling-schedule-and-failures).
- After you spend a banked reset in Quota Glance 0.8.0 or newer, Quota Cache polls that account at its next free slot, then every other account that is not backing off after a failure, still one request per spacing and still respecting provider pauses. The sidebar shows **Refreshing after a banked reset: N left** until the round is done. Quota Glance asks through a small file beside the snapshot; see [refresh after a banked reset](docs/cache-format.md#refresh-after-a-banked-reset).
- Every failed poll writes one line to CPA's log: a warning for a 429, info for any other failure. It names the provider, the credential's auth index, the HTTP status, and when polling resumes, and never an email, token, file name, or response body.
- Schedules and cooldowns persist before calls; restarts retain them. A second writer for the same cache path is rejected.
- While CPA is still loading credentials at startup, Quota Cache waits for the complete list instead of treating every account as removed, so saved observations, failure backoff, and account details survive restarts. The sidebar shows **Starting** until the next scan after CPA finishes loading, and CPA's log gets a warning if the wait lasts more than five minutes.
- Account Health and Reset Priority read quota observations from the snapshot only in cache mode. Missing, failed, stale (>30 minutes), future-dated, or expired-window data does not cause direct provider fallback. Reset Priority 0.2.0 also reads the credit state and meter (`snapshot.credits.json` and `snapshot.meter.json`) beside its `quota-cache-path`, or beside `plugins/data/quota-cache/snapshot.json` when that is unset, once a minute while it is enabled, to build its API credit routing table. It does so with `route-api-credits` on or off and whatever `use-quota-cache` says. These reads are local files only and never reach a provider.
- Reset Priority retains its existing deadline/failure handling and requires an observation after recovery before promoting a recovering account. A refresh failure does not turn expired data into a new reset window.
- No tokens, auth documents, upstream bodies, or arbitrary upstream error messages are saved in the snapshot.

A cache outage can temporarily delay quota notifications and fresh reset confirmation. It deliberately does not trigger a burst of fallback requests. Use the default 15-minute interval with the consumers' 30-minute freshness limit.

## Scope and dashboard behavior

The initial snapshot contains regular weekly used percentage, reset time, observation time, and scheduling/error metadata. It does not yet normalize every five-hour, model-specific, subscription, or billing field shown by the CPA dashboard.

On **CPA v7.2.155**, the stock quota dashboard still makes independent requests. The inspected live management bundle sends both Claude usage and profile requests through CPA’s `/api-call` route for each quota refresh. The newer quota-provider interface found in a later local CPA checkout is absent from the v7.2.155 tag. Dashboard integration therefore needs a separately verified host/console change. This plugin reduces the participating plugins' requests; it cannot guarantee that all dashboard 429s disappear.

A future push-notification plugin can read the same version-1 snapshot or authenticated status route. Reading does not request a refresh. For the same Go monorepo, `client.ReadFresh` provides the read-only interface. Snapshot consumers must respect timestamps/errors rather than treating saved values as current.

## Verification

```sh
make -C plugins/quota-cache ci
python3 plugins/quota-cache/scripts/native-probe.py plugins/quota-cache/dist/quota-cache.so
python3 plugins/quota-cache/scripts/sidebar-smoke.py
# After building all five plugins:
python3 scripts/quota-cache-smoke.py
```

Browser acceptance requires `agent-browser` and Chrome. CI and releases reuse the pinned test-only toolchain through `scripts/verify-quota-sidebar.sh`; no browser dependencies are included in the plugin. See [preview verification evidence](../../docs/quota-cache-verification.md). No live provider credentials or production changes are required by these tests.

Schedule edits (`poll-interval` and `request-spacing`) apply live after any in-flight request finishes. Successful accounts adopt the new interval; existing failure backoff, provider cooldowns, history, and the single writer are preserved. Moving `cache-path` still requires a native restart, and the files beside the snapshot, which hold the organizations and values entered on the page, must move with it ([operator data beside the snapshot](docs/cache-format.md#operator-data-beside-the-snapshot)).
