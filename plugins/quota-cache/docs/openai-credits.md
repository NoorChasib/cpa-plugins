# OpenAI API credits

ChatGPT Pro and Business plans include a monthly OpenAI API credit, granted to the one OpenAI API organization the subscription is linked to. From 0.6.0, Quota Cache counts what that organization spends through CPA's `openai-compatibility` entries and estimates what is left, the same way it does for [Claude API credits](cache-format.md#claude-api-credits). It holds no OpenAI key and sends OpenAI nothing: it counts the tokens of the usage records CPA hands it, names the organization from OpenAI's own response header, prices the tokens at OpenAI's list prices, and publishes the estimate beside its snapshot. Quota Glance 0.11.0 or newer shows it. Reset Priority does not route on it.

Every figure is an estimate. From time to time you enter what API Platform says is left, which anchors it. This page says what is counted and how, how to add the organization, what the prices are and what they leave out, and what is kept. The files are described in [the cache format](cache-format.md#openai-api-credits), and the page and its save route in [credit settings](credit-settings.md).

## What is counted

Quota Cache counts a usage record only when its `Provider` is `openai-compatibility` or `openai-compatible-` followed by an entry's name, and its `AuthType` is `apikey`, ignoring case: the traffic of CPA's `openai-compatibility` entries. Nothing else is counted: not `codex-api-key` entries, not OAuth credentials, and not Claude traffic, which the Claude API meter counts.

`codex-api-key` traffic is left out on purpose. OpenAI says "Codex usage through API keys is not supported by this benefit", and CPA sends that traffic as Codex: by default it sets `User-Agent: codex-tui/…` and `Originator: codex-tui` on every `codex-api-key` request, after the entry's own `headers:`, so they cannot override it. An `openai-compatibility` entry sends no Codex header of its own, but it passes on the client's `User-Agent`, so a Codex client routed through it still names itself. OpenAI does not say how it tells Codex usage apart; if you route a Codex client through the entry, give the entry a `User-Agent` of its own in its `headers:`.

The organization is counted only when the entry reaches OpenAI itself:

- `base-url: https://api.openai.com/v1`, with an API key of a project in the linked organization. OpenAI applies the credit only to requests made with that organization's project keys.
- A project key is bound to its organization, so the entry needs no `OpenAI-Organization` header.
- A `models:` list naming the OpenAI models the entry serves, as any `openai-compatibility` entry needs, under an `alias`, or with a `prefix` on the entry, that no Codex credential also serves. Otherwise a request for that model name can go to a ChatGPT sign-in or a `codex-api-key` entry instead, and is not counted.
- CPA v8.0.4 or newer, as for the Claude API meter. CPA gives each upstream attempt its own response headers from v7.2.142, and hands usage plugins the response's service tier from v7.3.11.

An entry pointed at a gateway that mixes upstreams on one address is not supported: its responses that carry OpenAI's header are counted as OpenAI's, and the others are ignored.

## How a request is attributed

OpenAI names the organization of every response in an `openai-organization` header. It carries the organization's name, not the `org-…` Organization ID in API Platform's settings: captured responses show `user-` and 24 letters and digits for a personal organization, and a name with a short suffix, such as `acme-x1wegt`, for one created with a name. OpenAI documents the header but not the form of its value. Quota Cache reads it trimmed, in its case, and only when it is 1 to 64 ASCII letters, digits, dots, underscores or dashes, the first a letter or digit (`client.NormalizeOpenAIOrganization`).

1. **With the header**: that organization. The CPA auth index of the request learns that it belongs to it.
2. **With a header Quota Cache cannot read**, one that does not have that form: the record is `rejected`, and every OpenAI organization's estimate is marked as a lower bound from then on. The form of a named organization's value is undocumented, so a value that does not fit is said rather than lost in silence; it cannot be counted until a Quota Cache release accepts its form.
3. **Without the header, a failed request on an auth index the meter has learned**: the organization that index belongs to. This is what lands a refusal on its organization if OpenAI sends it without the header.
4. **Without the header, anything else**: ignored. That includes every success without the header, even on a known auth index. An `openai-compatibility` entry also serves upstreams that never send the header, such as OpenRouter or a local server, so a record without it is no sign of OpenAI spend; CPA records the response headers on every success from OpenAI; and CPA keys an auth index by the base URL and key alone, so behind a gateway a success without the header is another upstream's and would be priced at OpenAI's rates. Unlike the Claude meter, it counts nothing as foreign or unattributed.

An organization not listed on the page is noted as seen, never counted: it goes to the meter's `unlinked` list, which the page offers to add. Two names that differ only in case are two organizations.

## Adding the organization

The OpenAI meter runs whenever Quota Cache's cache runs and its last configuration was accepted, whether or not an OpenAI organization is listed. Until one is, it watches. It keeps only:

- each organization a response named, at most 8, with how many requests named it and when it was first and last seen;
- the map from CPA auth index to the organization last seen on it.

It counts no tokens for an organization that is not listed, and it writes no file until it has seen one: `snapshot.openai-meter.json` appears with the first response that names an organization. A Quota Cache that never serves one writes no OpenAI file at all.

So the organization's name never has to be found by hand:

1. Send one request through the `openai-compatibility` entry that uses the linked organization's key.
2. Open **Quota Cache** in CPA's sidebar. Under **OpenAI organizations**, **Seen in traffic** lists the organization the response named, with its request count and when it was last seen.
3. Select **Add** beside it. The card opens with the organization filled in. Give it a label, your own name for it, and select **Save**.

You can also type it with **Add organization**. For a personal organization, OpenAI's responses have been seen to name it `user-` followed by 24 letters and digits. This appears to be your user ID, shown in API Platform's profile settings, but OpenAI does not document it. The page warns, without refusing, about a value shaped like an `org-…` Organization ID: OpenAI's responses name the organization differently, so its traffic would not match.

Spend is counted from when the organization is added; its traffic before that is not. Its row then shows that counting began after the cycle started: enter an API Platform reading to correct it. A wrong name typed shows no traffic, and the right one appears under **Seen in traffic** once a response names it.

**Checking the header yourself** is optional. With the project key in `OPENAI_API_KEY`, `curl -s -o /dev/null -D - -H "Authorization: Bearer $OPENAI_API_KEY" https://api.openai.com/v1/models | grep -i '^openai-organization'` prints it. A chat completion's response is known to carry the header; whether `/v1/models` does is unverified. Quota Cache never makes such a request.

## Tokens, tiers and long context

OpenAI's input tokens include the cached and written ones, and its output tokens include reasoning. So for each record the meter keeps:

- **input**: the input tokens less cache reads and cache writes, and 0 when a record reports more of those than input;
- **output**: as reported;
- **cache read** and **cache write**: as reported.

**Long context.** A request whose input tokens, cached and written included, are over 272,000 is `over_272k`, which OpenAI prices at 2x the input and cache rates and 1.5x output for the whole request. 272K is read as 272,000, not 272 × 1024, as Claude's 100K is.

**Service tier**, from the response's `service_tier`:

| Response | Priced as |
| --- | --- |
| `auto`, `default`, `scale` or none | Standard |
| `flex` | Flex |
| `priority`, `fast` | Fast (Fast mode, formerly Priority processing) |
| `ultrafast` | Ultrafast |
| anything else | `unknown`: never priced, so the estimate is a lower bound |

Scale Tier is reserved capacity a subscription's organization does not use, and pricing it as Standard can only overstate spend. A tier OpenAI adds later may cost more than Standard, so an unknown one is left unpriced rather than guessed.

**Model.** The model the response named, else the one requested, lower-cased and kept as `client.NormalizeMeterModel` keeps Claude's. A fine-tuned model `ft:<base>:<organization>:<suffix>:<id>` is kept as `ft:<base>`: the rest is the organization's name and a suffix someone chose. Fine-tuned models stay unpriced, since their rates differ from the base model's.

## Prices

Quota Cache prices the tokens itself, from a table of OpenAI's [pricing page](https://developers.openai.com/api/docs/pricing) as of 2026-11-18, in US dollars per million tokens: every model's Standard, Flex, Fast and Ultrafast rows, short and long context. The credit state names that date and page.

- **gpt-5.2-codex** is not on the pricing page. Its row comes from the comparison table on the [gpt-5.3-codex model page](https://developers.openai.com/api/docs/models/gpt-5.3-codex). That page says the model does not support Chat Completions, which CPA's executor sends, so this traffic is unlikely through CPA.
- **Cache writes** are priced at the 30-minute rate only. The 12-hour rate applies only to the Responses API, CPA's `openai-compatibility` executor sends Chat Completions except for `/responses/compact`, and CPA's usage record does not say which a write was. So no OpenAI account ever shows a cache-write difference.
- **A rate the page prints as "–"**: cached input and cache writes are billed at the input rate. GPT-5.5 and earlier have no cache-write charge, and a model without caching reports no cached tokens.
- **gpt-6-cyber's long-context writes** are not printed. They are taken as 2x its short-context ones, $50.00 and $100.00, by the page's long-context rule.
- **gpt-6-astra-law** prints no write rate at all, so its writes are priced at the input rate: understated, if OpenAI charges them.
- **No figures for long context.** Where the page prices a model by context length but prints no long-context figures for a tier, a long-context request in that tier is unpriced: it never falls back to the short-context row. A model the page prices at one rate whatever the length uses that rate. A tier with no row for the model is unpriced, with no fallback to Standard, as is gpt-5.6-sol's Ultrafast tier, which OpenAI's Responses API reference says is available but the pricing page does not price.
- **Not in the table**: Batch rows, since the Batch API does not pass through CPA, and image, audio, realtime, transcription and embedding models, whose usage is listed as unpriced.

Unpriced usage is listed on the account by model, tier and context, such as `gpt-5.5 fast over 272K`, and makes its estimate a lower bound.

The table needs a Quota Cache release to change. These prices are already due to:

- gpt-5.6-sol's promotion ends on 2026-11-30, and no list price is published for after it;
- GPT-6.2's Ultrafast rate goes from 3x to 6x on 2027-03-01;
- the Law and Rosalind promotions run at least through 2026-12-31.

## What the estimate leaves out

The meter sees only the token counts of each request that passes through CPA. So:

- **Not counted at all**: built-in tool calls (web search at $10 to $25 per thousand calls, file search at $2.50 per thousand), container sessions, storage, image-generation tool tokens, and anything that does not pass through CPA, such as the API Platform playground or other tools.
- **Overstated**: complimentary data-sharing tokens, background-window discounts, and Scale Tier, priced as Standard.
- **Understated**:
  - the 10% regional-processing uplift: the request's base URL is never read;
  - 12-hour cache writes, priced at the 30-minute rate;
  - gpt-6-astra-law's cache writes, priced at the input rate;
  - a stream the client cancels before its final usage chunk most likely records no tokens, though OpenAI bills what it processed;
  - a success without the `openai-organization` header, which is never counted;
  - a `credit_balance_exhausted` refusal without the header, on an auth index no request with the header has taught the meter: it is lost. Whether OpenAI sends the header on that refusal is unverified;
  - a record whose header Quota Cache cannot read: it is rejected and marks every OpenAI estimate as a lower bound, but cannot be counted.

An API Platform reading corrects all of these up to the time it was taken.

## Running out

OpenAI spends the subscription's credit before any credit you bought. When none is left, it refuses requests with HTTP 429 and the error code `credit_balance_exhausted`. Quota Cache counts that refusal on the organization, and the account shows the credit as spent (`out`) until a later request succeeds or you enter a reading. A 429 rate limit, or `insufficient_quota` without that code, is not such a refusal. If you have bought credits, requests go on succeeding once the monthly credit is spent, and the account shows spend past the monthly credit instead.

A refusal that arrives inside a stream that started with HTTP 200 loses its code on the way through CPA, so it is not recognised.

## Files

The OpenAI API credit keeps four files of its own beside the snapshot, with the same schemas as Claude's: `snapshot.openai-meter.json`, `snapshot.openai-organizations.json`, `snapshot.openai-settings.json` and `snapshot.openai-credits.json`. No Claude file changes, and `snapshot.json` lists no OpenAI organization. The organizations and settings files are operator data and must move with `cache-path`. [The cache format](cache-format.md#openai-api-credits) describes them.

## Rollback

Quota Cache 0.5.0 never opens the four OpenAI files and leaves them in place. Returning to 0.6.0, the OpenAI meter carries on from its file, with the time in between as a gap, and every organization and value entered on the page applies again.

## Privacy

The OpenAI meter reads a record's provider and auth type only to decide whether to count it, and keeps only:

- the organization's name from the response header;
- CPA's auth index;
- the model, with a fine-tuned model cut to its base;
- the service tier;
- the token counts;
- the times;
- whether a failure was a low-credit refusal or a client cancellation.

`Provider` and `AuthID` are never kept or turned into a string, since both carry the name the operator gave the entry. The failure body is looked at only for `credit_balance_exhausted`, and never kept. A named organization's name in the header contains the name it was created with, which the meter keeps as it keeps any organization it sees.

The OpenAI organizations, settings and credit state files hold labels, organization names, amounts, dates and token counts. No key, auth ID, email or credential name is in any of them.

The OpenAI meter writes these lines to CPA's log and no others, none naming an organization, model, label or amount:

- `info` `quota-cache OpenAI API meter counting openai-compatibility traffic`, with `organizations`, when it starts;
- `warn` `quota-cache could not read its OpenAI API meter file and starts counting again`;
- `warn` `quota-cache OpenAI API meter is dropping usage records; OpenAI API credit estimates will be low`, with `dropped`, at most once per 10 minutes;
- `warn` `quota-cache cannot save its OpenAI API meter; OpenAI API credit estimates may lose recent spend`, at most once per 10 minutes;
- `error` `quota-cache OpenAI API meter stopped after an internal error; OpenAI API credit estimates are incomplete until Quota Cache reloads`.

The lines about the page's saves and the files are listed in [credit settings](credit-settings.md#privacy).

## Evidence

No live response from OpenAI was captured for this. The meter's tests decode two records generated from CPA v8.0.77's own usage record type and JSON encoder, one success and one `credit_balance_exhausted` refusal, and check that every field read is in them and that nothing else they carry leaves the record. CPA's `openai-compatibility` executor records the upstream response headers for a success, an HTTP error, a stream, an image request and a compaction, and hands them to usage plugins unchanged. What OpenAI's header carries comes from responses others captured and published, and OpenAI's own pages for the prices, the credit and the refusal.
