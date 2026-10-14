# Claude session warm-up

Warm-up sends one short prompt to an idle Claude subscription account so its
five-hour session window starts before you need it. It is off by default.
Quota Cache owns the scheduler; Quota Glance only displays its published status.
This does not advance weekly resets or spend banked resets.

## Set up the native runner

Install the unmodified [Claude Code CLI](https://code.claude.com/docs/en/setup),
version 2.1.293 or newer, **inside the machine or container running CPA**. A CLI
installed on your laptop is not available inside a remote CPA container. Keep
the executable and profiles across container replacements. The plugin does not
install or update Claude Code.

Each account needs its own native Claude Code sign-in. The plugin never copies
CPA OAuth tokens into another application. The default profile root is
`claude-profiles` beside `cache-path`, normally
`/CLIProxyAPI/plugins/data/quota-cache/claude-profiles` in Docker. Each profile
directory is named by the account's 16-character CPA auth index. Quota Cache's
warm-up card shows that index, including when the profile is missing.

Run the following as the CPA process user, replacing the example auth index
with the one shown for the account:

```sh
mkdir -p /CLIProxyAPI/plugins/data/quota-cache/claude-profiles/0123456789abcdef
chmod 700 /CLIProxyAPI/plugins/data/quota-cache/claude-profiles/0123456789abcdef
CLAUDE_CONFIG_DIR=/CLIProxyAPI/plugins/data/quota-cache/claude-profiles/0123456789abcdef claude auth login
```

Complete Anthropic's sign-in flow with that account's paid subscription. Repeat
for the other accounts. The runner checks the native profile's sign-in method,
provider, subscription type, email and configuration directory before sending
anything. A wrong account, missing profile or unrecognized status sends no
prompt. See [multiple-account authentication](https://code.claude.com/docs/en/authentication#log-in-with-multiple-accounts).

## Configure from the plugin settings page

Open **Plugins → Quota Cache → Edit config**. All of these fields appear in the
settings form; YAML edits are optional:

```yaml
warmup-enabled: true
warmup-model: claude-haiku-5-5
warmup-effort: low
warmup-hours: 00:00-24:00
warmup-timezone: UTC
warmup-cli-path: claude
# Optional; the default is claude-profiles beside cache-path:
# warmup-profile-root: /CLIProxyAPI/plugins/data/quota-cache/claude-profiles
```

The model is a free-text field so a future model needs no plugin release.
Effort accepts `low`, `medium`, `high`, `xhigh`, or `max`; the selected model must
support the chosen value. Haiku 5.5 in Claude Code supports low effort but not
thinking-off. Model, effort and schedule changes apply to subsequent attempts
without restarting the plugin. Disabling warm-up cancels an attempt in flight.
It does not change CPA's normal request routing or model selection.
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

One native CLI request at a time runs in an empty temporary directory with the
prompt `Reply only OK.`, tools and MCP disabled, customizations disabled, one
turn, no session persistence, and no inherited API keys or provider overrides.
No alternative model is configured. The CLI operation has a 60-second bound;
cancellation terminates its process group. Quota HTTP requests and CLI warm-ups
share a gate, so they do not run concurrently. Warm-up starts are at least
`request-spacing` apart; an in-flight runner can delay scheduled quota polls.

The page initially says **Waiting for quota confirmation**. A targeted poll of
that account is brought forward through the existing poller, respecting its
spacing, provider pauses and failure backoff. Only a fresh provider reset time
confirms an active session. Natural session expiry is eligible for another
warm-up after a successful observation of the new idle state. A spent weekly
must also be observed as available again first.
At session or weekly turnover, a targeted poll is brought forward rather than
waiting for the regular poll interval. It keeps the same spacing and backoff.

Before a request, the attempt and a five-hour duplicate guard are saved to
`snapshot.warmup.json` beside the snapshot. Timeouts, uncertain results and
request failures keep that guard across restarts and repeated button presses;
the provider's confirmed session reset replaces it when available. Setup
failures that prove no inference was attempted retry after 15 minutes. Multiple
CPA credentials for the same email share the attempt guard. No token, email,
CLI output or upstream error body is saved there. Keep this journal alongside
the other cache data. If it is malformed or unwritable, warm-up stops instead
of discarding the duplicate protection. Restore it or, after verifying no
attempt remains unresolved, move it aside to start a new journal.

Warm-up consumes subscription allowance. If usage credits are enabled, Claude
Code can spend them if another client exhausts the allowance between the quota
check and the prompt; there is no documented CLI switch preventing that race.
Disable usage credits in the Claude account's Usage settings if warm-up must
never incur credit spending. API-key pings do not warm subscription quota.

The authenticated status route includes a `warmup` object. The action is
`POST /v0/management/plugins/quota-cache/warmup`, protected by CPA's management
key. There is no public-resource action. Quota Glance reads the private journal
and shows model, effort and account status; older consumers ignore the file.

## Verification

`make ci` covers scheduler/runner tests, live configuration, identity checks,
durable attempts, cancellation and native compilation. The runner tests use a
fake local executable and make no inference requests. Page tests cover the
authenticated action and refusal handling. Live subscription priming is not
part of CI.
