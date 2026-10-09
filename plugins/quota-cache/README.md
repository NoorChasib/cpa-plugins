# Quota Cache

One scheduled poller for Claude, Codex, and Grok quota windows, credit balances, banked Codex and Claude rate-limit resets, plans and renewal dates, and availability flags, and optionally your OpenRouter account balance and your Claude API credit spend. Account Health and Reset Priority can read its saved observations instead of each contacting the providers.

**Linux amd64:** Quota Cache 0.1.5 normalizes every provider's quota windows into one canonical vocabulary and records the subscription plan beside each credential, so a consumer never pattern-matches a provider string. 0.1.4's extended fields are still there — expand **All cached quota fields** in the sidebar to inspect them. Quota Cache 0.1.10 adds Claude's banked rate-limit resets, each account's plan from Claude's and Grok's account endpoints, Codex's subscription renewal date, and the Claude subscription start and billing period that a consumer can estimate a renewal from ([account details](docs/cache-format.md#account-details)). Everything is additive and the snapshot stays on schema 1: Account Health and Reset Priority keep their regular weekly/pool behavior and need no rebuild. Quota Cache 0.1.11 keeps saved observations, failure backoff, and account details through a CPA restart instead of clearing them while CPA is still loading its credentials. Quota Cache 0.1.12 stops one rate-limited or dead credential from pausing its provider for hours, stops a 429 on Codex's optional reset-credit request from discarding a good reading, sends polls at the configured spacing instead of often twice it, and logs every failed poll to CPA's log ([polling schedule and failures](docs/cache-format.md#polling-schedule-and-failures)). Quota Cache 0.1.13 adds [Claude API credits](#claude-api-credits-optional): each Claude Console organization's spend against its Max or Team plan's monthly API credit, with an `anthropic-api` entry per organization ([format](docs/cache-format.md#claude-api-credits)). See the [cache format and reader API](docs/cache-format.md) for building another consumer.

## Install and start

Add `https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json` as your store source, then install **Quota Cache**. If you already use `preview/registry.json`, keep that source and select **Update** for this plugin. Both catalogs advance together. Follow any disable/restart instruction CPA gives for a loaded native library.

### Build from source

Target: Linux amd64, native ABI 1 / schema 4+, including CPA v7.2.155 and v8.0.4. The writer uses Linux file locking. You need Go 1.26+ and a C toolchain; verification uses Go 1.27.1.

From the repository root:

```sh
make -C plugins/quota-cache ci
make -C plugins/account-health-pushover c-shared
make -C plugins/reset-priority build
```

Outputs are `plugins/quota-cache/dist/quota-cache.so`, `plugins/account-health-pushover/dist/account-health-pushover.so`, and `plugins/reset-priority/reset-priority.so`. These are local builds; the preview release provides checksum-verified artifacts of the tested binaries. Follow the [migration guide](../../docs/migration.md) before replacing installed libraries, especially versioned store-managed files.

Merge the following options into your existing plugin configuration; keep all other options and data paths:

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

## OpenRouter balance (optional)

Quota Cache 0.1.9 can also read how much money is left on your OpenRouter account, which [Quota Glance](../quota-glance/README.md#openrouter-balance) shows as a card of its own. OpenRouter reports the account balance only to a **management key**. The inference keys CPA routes with are refused, so this needs a key of its own:

1. Create one on OpenRouter's [Management API Keys page](https://openrouter.ai/settings/management-keys). **Give it an expiry.** A management key cannot make model requests, but it can create, edit, and delete your API keys, OpenRouter offers no read-only scope, and the expiry is fixed when the key is created.
2. Add it to Quota Cache's settings in the CPA plugin panel, or to the config file:

   ```yaml
   quota-cache:
     openrouter-management-key: sk-or-v1-...
   ```

It takes effect at the next scan, with no restart. Quota Cache then makes one `GET https://openrouter.ai/api/v1/credits` per `poll-interval`, with the same spacing, failure backoff, and 429 cooldown as every other account. Clear the setting and the account is dropped at the next scan.

The snapshot keeps OpenRouter's purchased and spent totals and never the key. The account is recorded as `openrouter:key-<fingerprint>`, a hash of the key, so a rotated or corrected key starts fresh rather than waiting out the previous key's failure backoff. Like every plugin setting, the key is stored in plain text in CPA's configuration file and is readable by anyone who holds the CPA management key.

## Claude API credits (optional)

Max and Team plans include a monthly Claude API credit, deposited into one Claude Console organization per plan and shared by every API key in it. Quota Cache can read what each organization has spent this cycle from Anthropic's cost report, so [Quota Glance](../quota-glance/README.md) can show what is left. Anthropic reports neither the credit's amount nor its renewal date, so you enter both.

**Update Quota Cache to 0.1.13 before adding `claude-api-credits`.** Older builds reject the key, which fails the whole configuration and stops all polling.

Configure one item per organization. Two Team seats share one organization and one credit, so they are a single item: four personal Max organizations and one Team organization make five items.

1. In each organization, create an Admin API key in Console under **Settings > Admin keys**. Only a member with the admin role can. **Give it an expiry.** An Admin key has no scopes: it can also manage the organization's members, workspaces, and API keys. A personal or service-account key that is not scoped to a workspace (`sk-ant-api...`) is also accepted by Anthropic's cost report.
2. Check each key once before you configure it. These are your own requests to Anthropic; Quota Cache's tests never make them:

   ```sh
   KEY=sk-ant-admin01-...
   curl -sS https://api.anthropic.com/v1/organizations/me \
     -H "x-api-key: $KEY" -H "anthropic-version: 2023-06-01"
   curl -sS "https://api.anthropic.com/v1/organizations/cost_report?starting_at=$(date -u +%F)T00:00:00Z&ending_at=$(date -u -d tomorrow +%F)T00:00:00Z&bucket_width=1d" \
     -H "x-api-key: $KEY" -H "anthropic-version: 2023-06-01"
   ```

   The first must return the organization's `id`. The second should return one bucket for today. A 401 or 403 means that key will not work. (On macOS, replace `date -u -d tomorrow +%F` with `date -u -v+1d +%F`.)
3. Add the items to Quota Cache's configuration in CPA's config file. The plugin panel cannot edit this list, so use YAML. Quote `monthly-usd` and `renews`:

   ```yaml
   quota-cache:
     claude-api-credits:
       - label: siphorchannel
         admin-key: sk-ant-admin01-...
         monthly-usd: "200"
         renews: "2026-10-29"
       - label: agency-team
         admin-key: sk-ant-admin01-...
         monthly-usd: "260"
         renews: "2026-10-03"
   ```

- `label` is your unique name for the organization.
- `monthly-usd` is the exact monthly credit in dollars. Team pools differ, so enter what Console shows.
- `renews` is the day the next credit is deposited: the date the linked claude.ai plan renews (claude.ai **Settings > Billing**, or **Organization settings > Billing** on Team), as a UTC date. Console's expiry date for the current credit (**Settings > Billing > Promotional credits**) may be the day before; if so, use the claude.ai date. Only the day of the month is used, and it repeats monthly.

Changes take effect at the next scan with no restart. An item with a mistake is shown in the sidebar as **Not polled** with the reason, and every other item keeps polling. Saving any Quota Cache setting in the plugin panel rewrites the list through JSON; unquoted values come back in a form Quota Cache still accepts.

The snapshot keeps each day's spend as Anthropic reported it, the values you configured, and a fingerprint of the key, never the key itself. The spend is the organization's gross spend, paid from the credit or from purchased credit alike; Priority Tier usage is not in Anthropic's cost report. Like every plugin setting, the keys are stored in plain text in CPA's configuration file and are readable by anyone who holds the CPA management key. Details are in [Claude API credits](docs/cache-format.md#claude-api-credits).

## Polling behavior

- At most one provider request runs at a time in the cache plugin. Polls are spaced 10 seconds apart by default; the follow-up requests a poll can make (Codex's reset inventory, account details) run straight after it.
- Each credential is polled at most once per 15-minute interval by default. The initial accounts are staggered by request spacing. If you shorten `poll-interval`, keep your number of credentials times `request-spacing` well below it, so every credential fits into each interval.
- Plans and subscription renewal dates are read from separate account endpoints at most once per credential every six hours, and a failure there, like a failure of Codex's reset inventory, never fails the poll, backs it off, or pauses the provider. See [account details and requests per credential](docs/cache-format.md#account-details).
- Each Claude API credit organization costs one cost-report request per `poll-interval` (96 a day at the default), and is polled again as soon as its cycle renews. Each organization has its own Admin API key and its own Anthropic rate limits, so a 429 on any of its requests backs off that organization alone, for at most an hour (or one interval, if that is longer), or a longer `Retry-After`. It never pauses another organization or Claude subscription polling. Each failed poll is logged like any other (below), and a failing organization backs off up to six hours, so a wrong key does not log every interval.
- On 429 from Claude, Codex, Grok, or OpenRouter, all accounts for that provider pause for one poll interval, and the rate-limited credential retries after at most an hour, or one interval if that is longer; a longer `Retry-After` from the provider extends either. The previous observation is kept, and other failures back off up to six hours. See [polling schedule and failures](docs/cache-format.md#polling-schedule-and-failures).
- Every failed poll writes one line to CPA's log: a warning for a 429, info for any other failure. It names the provider, the credential's auth index, the HTTP status, and when polling resumes, and never an email, token, file name, or response body.
- Schedules and cooldowns persist before calls; restarts retain them. A second writer for the same cache path is rejected.
- While CPA is still loading credentials at startup, Quota Cache waits for the complete list instead of treating every account as removed, so saved observations, failure backoff, and account details survive restarts. The sidebar shows **Starting** until the next scan after CPA finishes loading, and CPA's log gets a warning if the wait lasts more than five minutes.
- Consumers perform file reads only in cache mode. Missing, failed, stale (>30 minutes), future-dated, or expired-window data does not cause direct provider fallback.
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

Schedule edits (`poll-interval` and `request-spacing`) apply live after any in-flight request finishes. Successful accounts adopt the new interval; existing failure backoff, provider cooldowns, history, and the single writer are preserved. Moving `cache-path` still requires a native restart.
