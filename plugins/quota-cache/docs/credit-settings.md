# Credit settings

From 0.2.0, Quota Cache keeps everything you enter about Claude API credits and Claude subscriptions: the Claude Console organizations, each organization's monthly credit, refill date and Console readings, and each Claude subscription's renewal date. You enter them on Quota Cache's page in CPA's sidebar. Quota Cache prices each organization's metered tokens at Anthropic's list prices and publishes the result beside its snapshot, in the [credit state file](cache-format.md#credit-state-file), so Quota Glance shows the same figures and Reset Priority routes on them. Quota Glance 0.8.0 shows them read-only, with a link to this page.

This page covers the editor, the save route, coming from Quota Glance 0.7, what happens when a file cannot be read, and rollback. The files themselves are described in [the cache format](cache-format.md#operator-data-beside-the-snapshot).

## The page

Open **Quota Cache** in CPA's sidebar while signed in to CPA's console on the same address, with **Remember password** ticked. None of these values is in Quota Cache's **Edit config** on CPA's Plugins page (the **Configure quota-cache** dialog), whose **Config fields** hold only where the cache lives, how it polls and the OpenRouter key. The page uses the console's remembered management key; without one it says so and sends nothing. It saves only when you press a button, and after CPA refuses the key (401 or 403) it never sends that key again until you select **Refresh view**, because CPA locks an address out of its management API for 30 minutes after five failed attempts. While a card is open, **Refresh view** is off, so a refused save is handled on the card: for a key CPA itself refused, and during a lockout, **Save** stays off until the console holds another key or the lockout ends; after any other refusal, such as one from a proxy in front of CPA or a lockout that has ended, **Save** stays on and pressing it is the one try. Your changes are kept either way until you reload. A phone, a bookmark or Quota Glance's menu bar can read the figures in Quota Glance but cannot edit them.

The page has three cards under the status:

- **Organizations**: one row per Claude Console organization, with its label and a shortened Organization ID. While the list is empty, **Add organization** takes a label and the **Organization ID** from Console under **Settings > Organization**, in any case and with spaces trimmed. **Edit organizations** renames or removes them, and its **Add an organization** row adds one. While the organizations file cannot be read, the card lists none and says what to do.
- **Claude API credits**: one row per organization, with its monthly credit, refill date, Console reading, what was used and what is left. **Set credits and dates**, or **Edit credits and dates** once a credit is set, sets the **Monthly credit, USD** (1 to 7 digits, up to 2 decimals, `0` allowed), **Refills on, UTC** (a date from 2000 to 2099) and a **Console reading** (the amount Console shows and when you read it, from 48 hours ago to 5 minutes ahead). A value entered here carries a mark, and **Clear** removes it.
- **Claude renewal dates**: one row per Claude credential on CPA's roster, disabled ones included, with the renewal in effect and where it comes from. **Set renewal dates** enters the day a subscription renews, which Anthropic does not report; **Use estimate** goes back to the estimate from when the subscription began. A date saved for a credential CPA no longer lists is shown by a shortened auth index and can be removed.

The values entered here are the only ones: Quota Cache never takes a monthly credit or a refill date from its configuration. A reading taken on a refill day, a reading above the monthly credit, and one too old to use are flagged on the row.

### Organizations

- An organization's ID never changes: a different ID is a different organization, since its account is a hash of the ID. To change one, remove it and add the new one.
- Removing an organization keeps the amounts and readings entered for it in the settings file, listed under the API credit card as an orphan you can clear, the last organization's too. Adding the same Organization ID again brings them back. There is no reorder; removing and adding an organization again puts it last.
- An organization is counted from the first request after it is added, with no restart. Removing the last one stops the meter, which records the stop; adding one starts it again from its file, with the gap.
- At most 16 organizations, and labels are unique ignoring case.
- A save is checked against the list its renames and removals were made on, even when the view updated while the card was open. If another device changed the organizations since, nothing is saved: the card shows the current list, names the renames and removals it dropped, and keeps a filled-in **Add an organization** row.

**`claude-api-credits` in the configuration.** Quota Cache 0.1.13 and 0.1.14 read the organizations, with their `monthly-usd` and `renews`, from `claude-api-credits`. 0.2.0 reads none of it and moves nothing: the organizations file, which only a save on this page writes, is the only list. A configuration that still has the key, `admin-key` from 0.1.13 included, loads as before, and a value that is not a list still rejects it. Its items are ignored: no organization, value or problem is taken from them, and nothing is logged about any one of them. While it lists any:

- The page shows a note you can dismiss, saying how many it lists.
- The credit state counts them as `organizations.config_items_ignored`.
- CPA's log says once per load: `quota-cache keeps Claude Console organizations on its page; claude-api-credits in its configuration is ignored`.

You can leave the key; only an older Quota Cache reads it. Nothing on the page is written back to it ([rollback](#rollback)).

## The save route

The page saves through one management route, `POST /v0/management/plugins/quota-cache/settings`. CPA checks the management key, which it takes only from a header, before Quota Cache sees the request. There is no route on the public resource tree. Every answer is JSON with `Cache-Control: no-store`, and a saved change is published to the credit state file before the answer.

The checks run in this order, and the first that fails answers:

| # | Check | Answer |
| --- | --- | --- |
| 1 | The cache is not running, including `enabled: false` | `503 {"error":"cache_stopped"}` |
| 2 | `Sec-Fetch-Site` is sent and is not `same-origin` (a request with none, from curl or a script, passes) | `403 {"error":"cross_site"}` |
| 3 | The body is not `application/json` | `415 {"error":"unsupported_media_type"}` |
| 4 | The body is over 4096 bytes | `400 {"error":"invalid_request"}` |
| 5 | An organizations batch while the organizations file cannot be read | `503 {"error":"organizations_unavailable"}` |
| 5 | Any other request while the settings file cannot be read | `503 {"error":"settings_unavailable"}` |
| 6 | The request's own rules, below | |

The request's `kind` picks one of three bodies. Each row of a batch carries the `baseRevision` the page read, `""` for a row with nothing saved.

- **`apiCredits`**: `{"kind":"apiCredits","items":[{"id":"org-1a2b3c4d5e6f","baseRevision":"12","monthlyUsd":"200"|null,"renews":"2026-10-29"|null,"reading":{"remainingUsd":"143.20","at":"2026-10-12T09:20:00Z"}|null}]}`, 1 to 16 rows. Every key is required; `null` means not set here.
- **`renewals`**: `{"kind":"renewals","items":[{"id":"<16 hex auth index>","baseRevision":"3","date":"2026-09-10"|null}]}`, 1 to 32 rows.
- **`organizations`**: `{"kind":"organizations","baseRevision":"<the organizations file's revision, \"\" without one>","ops":[…]}`, 1 to 16 operations, applied in order and committed whole or not at all: `{"op":"add","label":"team","organizationId":"<uuid>"}`, `{"op":"rename","id":"org-1a2b3c4d5e6f","label":"team-two"}` and `{"op":"remove","id":"org-1a2b3c4d5e6f"}`.

The answers after the checks:

| Answer | When |
| --- | --- |
| `200 {"ok":true,"unchanged":false,"revision":"13","settings":{"<id>":<block>}}` | A saved batch. Each block is the row as now published: an account's `settings` block, a credential's `{date, revision, updatedAtEpoch}`, or `null` for a cleared orphan or a cleared date |
| `200 {"ok":true,"unchanged":true,"revision":"12"}` | A batch whose rows already hold what was sent; nothing is written |
| `200 {"ok":true,"unchanged":bool,"revision":"5","organizations":[{"id","label","organization_id"}]}` | An organizations batch, with the list now in the file |
| `400 {"error":"invalid_request"}` | Anything that is not exactly one of the bodies above |
| `400 {"error":<code>,"id","field"}` | A value the rules refuse: `invalid_monthly_usd`, `invalid_renews`, `invalid_date`, `invalid_reading_amount`, `invalid_reading_time` (outside 48 hours back to 5 minutes ahead) or `reading_before_refill` (before the cycle the row's own `renews` starts; with `renews` null nothing says when the credit last refilled) |
| `400 {"error":<code>,"op","field"}` | An organizations operation the rules refuse, `op` being its index: `invalid_label`, `duplicate_label`, `invalid_organization_id`, `duplicate_organization`, `too_many_organizations` or `unknown_id` |
| `409 {"error":"not_editable","id","ids"}` | A row the published state does not offer: an account whose `settings.editable` is false, or a credential whose index cannot key a date. A stored row for an organization or credential no longer listed (an orphan) may only be cleared |
| `409 {"error":"conflict","revision","conflicts":[ids],"current":{"<id>":<block>}}` | A batch row saved elsewhere since the page read it, with what it holds now. Sending what is already saved is not a conflict |
| `409 {"error":"conflict","revision","organizations":[…]}` | An organizations batch whose `baseRevision` is not the file's, with the list now in it |
| `409 {"error":"settings_full"}` | The settings file would pass a bound even after dropping the oldest renewal dates of credentials CPA no longer lists, or the write needs a revision past 18446744073709551615, the highest there is, which only a file edited by hand holds |
| `429 {"error":"too_many_writes"}`, `Retry-After: 60` | More than 30 committed saves in a minute. The settings file and the organizations file each have their own limit |
| `503 {"error":"organizations_unavailable"}` | An `apiCredits` row while the organizations file cannot be read: no organization is listed then, and the row may belong to one in the file, so it is neither set nor cleared until the file can be read |
| `503 {"error":"settings_unwritable"}` | The file could not be written; nothing changed |

While CPA offers a schema below 6, it HTML-escapes every string in management JSON responses (`'` arrives as `&#39;`). Quota Cache registers the schema CPA offers, at most 6, which every supported CPA offers, and its status says `strings_escaped` so the page decodes the entities only when CPA escaped them.

## Coming from Quota Glance 0.7

Quota Glance 0.7 and older kept these values in its own `settings.json`. Quota Cache never reads that file, nor the organizations in its own `claude-api-credits` (above): after updating, add the organizations and enter the values again on this page, as the [README](../README.md#updating-from-quota-glance-07) lists. Quota Glance's `settings.json` stays where it is, untouched. The settings file has exactly its format, schema 1, so a rollback can copy one over the other ([rollback](#rollback)).

Claude API spend is not counted from the update until the first organization is added. Until then no meter runs, and the page shows the meter stopped with the reason Quota Cache 0.1.14 wrote on its way out: `quiesce` after an update through CPA's Plugin Store, or `shutdown` if CPA was stopped for it. The first add starts the meter from its file, with that window as a gap. From then on the meter keeps the traffic of every organization its file knows, added or not, so organizations added later with the same Organization ID lose nothing more. Add the organizations right after updating. If the gap matters, take a Console reading for each organization once it is added and enter it: the reading covers the gap.

## When a file cannot be read

A settings or organizations file that fails any of its rules is never written over. Each tick (every 10 seconds) checks it again, so the remedy is to move it aside; the page picks that up within a few seconds, with no configure. A save reads its file again first when it changed since the last tick, so it never writes over a good copy put back, a deleted file or one that fails its rules.

- **The settings file.** Nothing saved applies, the page cannot save, and CPA's log says once per load: `quota-cache cannot read its saved API credit settings; none apply until the file is moved aside`. Put back a good copy of `snapshot.settings.json`, which keeps the values saved here, or move it aside: Quota Cache then starts empty and editable, and every value saved here has to be entered again. A page still open on the moved file cannot save over what is entered since: every save writes a revision past each one the store has read, and at least the time in seconds, so a row from the moved file never matches a new one and its save meets a conflict, also when the file was moved while CPA was stopped, once a minute has passed since the last save.
- **The organizations file.** No organization is listed or counted, and the page cannot change organizations. A running meter stops with `no_items`, as when the last organization is removed. A Quota Cache that starts with the file unreadable opens no meter, so the meter file keeps the reason its last run stopped with, such as `shutdown` after a CPA restart. `claude-api-credits` stays ignored. Every value saved for an organization is kept, shown on the API credit card as kept, and cannot be cleared meanwhile: it may belong to an organization in the file. Fix the file's owner or permissions, or put back a good copy: it applies at the next tick. Or move `snapshot.organizations.json` aside and add the organizations again: the list starts empty, and the values saved for an organization apply again once it is added with the same Organization ID.
- **A file Quota Cache could not open or read**, such as one restored as another user, counts as unreadable too, but is read again at every tick: fixing its owner or permissions is enough, with no need to move it aside.

## Rollback

Every value entered on the page survives a rollback. An older Quota Cache knows only the organizations listed in its configuration, though, which the page never writes to (step 3). CPA's Plugin Store installs only the version this repository's catalog lists, so each downgrade below is done by hand, as [downgrading a plugin](#downgrading-a-plugin) describes. Roll back in this order, so that an editor always exists and nothing routes on figures nobody maintains:

1. **Reset Priority** first: turn `route-api-credits` off in Reset Priority's **Edit config** on CPA's Plugins page, wait until two consecutive requests leave the status's `router.counters.picks` unchanged, then downgrade. CPA calls the scheduler on every request, and a request that reaches Reset Priority while CPA replaces it fails. See [Updating or removing Reset Priority](../../reset-priority/REFERENCE.md#updating-or-removing-reset-priority).
2. **Quota Glance** to 0.7.2. It shows the values in its own `settings.json` again, as they were before the update. To carry the values entered on Quota Cache's page back instead, first keep a copy of Quota Glance's `settings.json`. Then, while CPA is stopped, for example for the downgrade, copy Quota Cache's `snapshot.settings.json` over Quota Glance's `settings.json`: the formats are identical. The copy is a one-off: Quota Cache never reads Quota Glance's file, so a value changed afterwards on either side stays where it was changed.
3. **Quota Cache** to 0.1.14 last. 0.1.14 still reads `claude-api-credits`, which 0.2.0 ignores, so first make it list the page's **Organizations**: one item per organization, with the `label` and `organization-id` the page shows. Editing it is safe while 0.2.0 runs; **Edit config** does not offer the list, so edit `config.yaml`. A list left as it was before the update lacks an organization added on the page under a new Organization ID, has an old label for one renamed, and still has one removed on the page, which 0.1.14 counts again. An item's `monthly-usd` and `renews` are what Quota Glance 0.7.2 shows for an organization it has no value saved for, so give them the monthly credit and refill date the page shows unless step 2 copied the settings file. An organization the list lacks is not counted by 0.1.14: its traffic is still recorded for 40 days, so its history is there after a return to 0.2.0, but Quota Glance 0.7.2 shows its saved values as an orphan, and with no item at all 0.1.14 stops its meter. Then downgrade. 0.1.14 ignores the three new files (organizations, settings and credit state) and never deletes them. Do not go further back with the list in place: 0.1.13 has no meter and marks every item that has `organization-id` as `unknown_field`, so nothing is counted, and 0.1.12 and older reject `claude-api-credits` altogether and stop polling, so Account Health and Reset Priority, where they read the cache, wait for data that never comes. To go back past 0.1.14, delete the list first. Quota Glance 0.8.0, if still installed, shows the last credit state as stale until it sees that no Quota Cache publishes it any more, then shows **no figures** with a note; moving `snapshot.credits.json` aside makes that immediate.

Back on 0.2.0, the organizations file is the list again, and `claude-api-credits` is ignored, with every change made to it meanwhile: make the same changes on the page. Values entered on the page are kept by organization, so they apply again to every organization still listed.

### Downgrading a plugin

On CPA v8.0.22, the Plugin Store installs a plugin only at the version the catalog lists, and this repository's catalog lists only each plugin's newest release. To go back to an older `<version>` of a plugin `<id>` (`reset-priority`, `quota-glance` or `quota-cache`):

1. From the `<id>/v<version>` release on the repository's [GitHub Releases](https://github.com/NoorChasib/cpa-plugins/releases), download `<id>_<version>_linux_amd64.zip` and `checksums.txt`, and check the ZIP with `sha256sum --check --ignore-missing checksums.txt`. A published release is never replaced, so every older one stays there.
2. Stop CPA.
3. Copy `<id>.so` from the ZIP into `linux/amd64/` under CPA's plugin directory (`/CLIProxyAPI/plugins` in the official image) as `<id>-v<version>.so`, the name the Plugin Store gives a library it installs. Leave the newer library where it is.
4. In `config.yaml`, set `version` in the `store` block under `plugins.configs.<id>` to `<version>`, and change nothing else in that block. CPA loads the library whose version it names: without this edit CPA goes on loading the newer library, and deletes the one you placed.
5. Start CPA. Once it has loaded the older library, CPA deletes the newer one, and the Plugin Store offers **Update** again, which is how to return.

## Privacy

- The settings, organizations and credit state files hold labels, Organization IDs, amounts, dates and token counts: what Quota Glance's own file and document held. No key, auth ID, email or credential name is in any of them.
- The renewal card names each Claude credential by the address in its auth index, or the address or label CPA reports. Those names are held in memory only, never written to a file or a log, and returned only by the authenticated status route.
- Log lines name kinds, ids, field names and counts, never a value, amount, path, label, Organization ID or name. The lines are: the warning above, `quota-cache settings changed` with `kind`, `ids` and `fields`, `quota-cache organizations changed` with `added`, `renamed` and `removed`, the configuration-ignored line, and `quota-cache could not publish its credit state` with a `reason`, at most every 10 minutes.
