# Claude session warm-up

Warm-up sends one short prompt to an idle Claude subscription account so its
five-hour session window starts before you need it. It is off by default.
Quota Cache owns the scheduler; Quota Glance only displays its published status.
This does not advance weekly resets or spend banked resets.

## Requirements

Warm-up uses the Claude accounts CPA already holds. There is no second sign-in
and nothing to install: CPA sends each warm-up itself, through the same
executor, token refresh and proxy settings as any other request it serves.
A warm-up never touches the account's token; CPA adds it itself.

- **CPA v8.0.4 or newer.** Each warm-up is pinned to one CPA credential.
  CPA before v7.3.3, including v7.2.155, silently ignores the pin and could
  send the request through another account. Quota Cache therefore checks
  first that CPA knows the `host.http.cancel` callback, which CPA added in
  v7.3.16, after the pin. A host without it gets no warm-up and shows
  `host_unsupported`. Releases from v7.3.16 to v8.0.3 pass this check but are
  untested.
- **A Claude OAuth sign-in with a paid plan.** CPA must list the credential
  as a Claude OAuth account, enabled, and not held back by a cooldown that is
  still running. The cache must also have
  recorded a Pro, Max, Team or Enterprise plan for it, which it reads from
  Claude's account endpoints when it first polls the account. Claude API
  keys are never warmed.
- **The model in CPA's model list for that account.** `claude-haiku-5-5` is
  built into CPA from v8.0.28. On earlier releases CPA has it only once its
  model catalog download succeeds, at startup and every three hours, from
  GitHub or CPA's own mirror. A host that can reach neither fails every
  warm-up with `request_failed` until another model is chosen.

## Configure from the plugin settings page

Open **Plugins → Quota Cache → Edit config**. All of these fields appear in the
settings form; YAML edits are optional:

```yaml
warmup-enabled: true
warmup-model: claude-haiku-5-5
warmup-effort: low
warmup-hours: 00:00-24:00
warmup-timezone: UTC
```

The model is a free-text field so a future model needs no plugin release.
Effort accepts `low`, `medium`, `high`, `xhigh`, or `max`. The selected model
must support the chosen value; Haiku 5.5 supports all five. Model, effort and
schedule changes apply to the next attempt without restarting the plugin.
Warm-up does not change CPA's normal request routing or model selection.
An invalid plugin configuration disables warm-up until a valid configuration
is saved again.

Hours are a daily `HH:MM-HH:MM` range in the configured IANA timezone, such as
`09:00-17:00` with `Australia/Sydney`. Overnight ranges such as `22:00-06:00`
are supported. The default enables automatic warming throughout the day once
`warmup-enabled` is switched on. Startup and session/weekly turnover become
eligible as soon as fresh quota data and the schedule allow.

On **Quota Cache** in the sidebar, **Warm all now** queues eligible accounts
outside automatic working hours too. It still skips active windows, disabled
accounts, spent weekly allowances, unknown or stale observations, provider
cooldowns, and attempts waiting out the duplicate-request guard. Reading either
dashboard never triggers a warm-up.

## Requests, confirmation and recovery

One warm-up runs at a time. It is a non-streaming CPA model request for the
`claude` provider, pinned to the account's CPA credential. The body is the
prompt `Reply only OK.` as one user turn, with `max_tokens` 32 and the
configured effort. There is no system prompt, no tools, no `thinking` field
and no alternative model. CPA never moves the request to another account: a
missing, disabled or cooling-down account fails instead. Within one warm-up,
CPA can retry the same account after a 429 with a short `Retry-After` or a
network error, up to its `request-retry` setting. No per-request switch turns
this off.

Before each request, Quota Cache checks the credential again: same auth
index, a Claude OAuth account, enabled and past any cooldown, with a paid
plan recorded. A failed check sends nothing.

Quota Cache waits at most 60 seconds. CPA's model callback has no deadline and
cannot be stopped once sent. So after 60 seconds, when warm-up is disabled, or
when the cache stops, Quota Cache stops waiting and records `outcome_unknown`
with the five-hour guard; the request may still finish upstream. No other
warm-up from the same loaded plugin starts until it returns. Quota HTTP
requests share a gate with warm-ups only while Quota Cache is waiting, so a
stalled request delays scheduled quota polls by at most a minute. Warm-up
starts are at least `request-spacing` apart.

Unloading the plugin's library waits until CPA returns from that request: at
CPA shutdown, and when the plugin is deleted or switched off in CPA. Until
then CPA applies no other configuration change and cannot load the plugin
again. A plugin update does not wait. CPA retires the old version, whose
request may still be running when the new version starts its first warm-up,
on another account: the five-hour guard in the journal still covers the
first one.

The page initially says **Waiting for quota confirmation**. A targeted poll of
that account is brought forward through the existing poller, respecting its
spacing, provider pauses and failure backoff. Only a fresh provider reset time
confirms an active session. Natural session expiry is eligible for another
warm-up after a successful observation of the new idle state. A spent weekly
must also be observed as available again first.
At session or weekly turnover, a targeted poll is brought forward rather than
waiting for the regular poll interval. It keeps the same spacing and backoff.

If CPA answered a warm-up but no observation in the five hours since showed
an active session, the account shows `unconfirmed` and is not warmed again
automatically. Anthropic may have counted the request against extra usage
instead, and every retry would do the same. **Warm all now** tries it once
more. Quota polls that fail for the whole five hours also end there.

Before a request, the attempt and a five-hour duplicate guard are saved to
`snapshot.warmup.json` beside the snapshot. Timeouts, uncertain results and
request failures keep that guard across restarts and repeated button presses;
the provider's confirmed session reset replaces it when available. Failures
that prove no request was sent retry after 15 minutes. Multiple CPA
credentials for the same email share the attempt guard. No token, credential
ID, email, model response or upstream error is saved there. Keep this journal
alongside the other cache data. If it is malformed or unwritable, warm-up
stops instead of discarding the duplicate protection. Restore it or, after
verifying no attempt remains unresolved, move it aside to start a new journal.

| Account error | Meaning | Next attempt |
| --- | --- | --- |
| `host_unsupported` | This CPA cannot pin a request to one account | 15 minutes |
| `subscription_login_required` | Not a Claude OAuth sign-in, or no paid plan recorded yet | 15 minutes |
| `account_unavailable` | CPA reports the account disabled, unavailable or not listed | 15 minutes |
| `identity_unknown` | CPA reported a different credential for the auth index | 15 minutes |
| `runner_busy` | An earlier request is still running in CPA | 15 minutes |
| `runner_unavailable` | The warm-up was stopped, or waited out its minute behind a quota poll, before sending | 15 minutes |
| `outcome_unknown` | Quota Cache stopped waiting; the request may have succeeded | five-hour guard |
| `request_failed` | CPA or Anthropic refused the request, or it did not answer with a message | five-hour guard |
| `unconfirmed` | CPA answered, but no session appeared within five hours | Warm all now only |

## Spending and provider policy

Warm-up consumes subscription allowance. CPA presents subscription traffic to
Anthropic as Claude Code, as it does for every request it sends with that
account; the warm-up adds one small request per account per session window.
It is not unmodified Claude Code. Anthropic has billed traffic it treats as a
third-party app against an account's extra usage instead of its plan, and has
refused such requests with "Third-party apps now draw from your extra usage".
Warm-up that is billed this way would spend extra usage without starting the
session. Turn extra usage off in the Claude account's Usage settings if
warm-up must never spend it. A refused warm-up then fails with
`request_failed` and keeps the five-hour guard. If extra usage stays on and
a warm-up is billed to it, the account stops at `unconfirmed` after one such
request. Narrower working hours send fewer requests. API-key requests do not warm subscription quota.

The authenticated status route includes a `warmup` object. The action is
`POST /v0/management/plugins/quota-cache/warmup`, protected by CPA's management
key. There is no public-resource action. Quota Glance reads the private journal
and shows model, effort and account status; older consumers ignore the file.

## Updating from 0.3.0

Quota Cache 0.3.0 ran a separately signed-in Claude Code profile for each
account. 0.4.0 sends warm-ups through CPA instead, so there is nothing to set
up. The journal, settings and routes carry over unchanged. `warmup-cli-path`
and `warmup-profile-root` are still accepted, so the same configuration loads
on either version, and ignored: Edit config no longer offers them, and CPA's
log says on each configure that they are ignored. Quota Cache 0.4.0 leaves
the `claude-profiles` directory alone. Rolling back to 0.3.0 needs it, so
delete it only when you no longer plan to; it holds Claude sign-ins.

## Verification

`make ci` covers scheduler and runner tests, live configuration, credential
checks, durable attempts, the bounded wait and native compilation. The runner
tests use a fake host whose model callback ignores cancellation, as CPA's
does. `scripts/native-probe.py` drives one synthetic warm-up through the real
shared library and checks the pinned request, and that a host without
`host.http.cancel` gets none. None of this sends a model request. Page tests
cover the authenticated action and refusal handling. Live subscription
priming is not part of CI.
