#!/usr/bin/env python3
"""Load Model Picker (codex-catalog-filter) into a disposable CPA container.

The unit tests call Handle() in process against a stand-in for CPA. Only this
proves that a real CPA image loads the library and registers its routes; that
the plugin reads CPA's own lists over loopback with the caller's key and serves
the Codex catalog and the Antigravity (agy) catalog at the URLs those clients
are pointed at; that it rewrites CPA's Claude-format model list for Claude
Code through its response interceptor; that the Model Picker page and its
private state route work; and that switches saved through CPA's own config API
apply without a restart.

By default it runs the pinned official CPA v8.0.4 image that make smoke and CI
use. --image runs it against another local image instead, such as
eceasy/cli-proxy-api:latest. It never pulls.

No real accounts or provider requests: models come from synthetic static
config. Every key is a synthetic key for this throwaway container only.
"""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

IMAGE = 'eceasy/cli-proxy-api@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d'
CPA_BANNER = 'CLIProxyAPI Version: v8.0.4, Commit: d33f63f'
BANNER = re.compile(r'CLIProxyAPI Version: v(\d+)\.(\d+)\.(\d+), Commit: (\w+)')
# CPA runs response interceptors on its model-list responses (OpenAI, Codex,
# Claude, and Gemini formats) from v7.2.158: commit 5b278561, "expose model
# list responses to plugin interceptors" (WriteModelListResponse). Before it,
# the Claude Code rewrite cannot happen, so that part is skipped.
MODEL_LIST_INTERCEPTORS = (7, 2, 158)
PLUGIN = 'codex-catalog-filter'
NAME = 'Model Picker'
CLIENT_KEY = 'synthetic-local-client-key'
# A second valid client key that never asks for the agy list before CPA goes
# away, so it must not receive the last good list.
OTHER_CLIENT_KEY = 'synthetic-local-other-client-key'
MANAGEMENT_KEY = 'synthetic-local-smoke-key'
CATALOG = '/v0/resource/plugins/codex-catalog-filter/models?client_version=0.159.0'
AGY = '/v0/resource/plugins/codex-catalog-filter/agy/models'
SETTINGS = '/v0/resource/plugins/codex-catalog-filter/settings'
STATE = '/plugins/codex-catalog-filter/state'
CODEX_QUERY = '/v1/models?client_version=0.159.0'
GEMINI_LIST = '/v1beta/models'
CLAUDE_CODE_UA = 'claude-code/2.1.327'
ANTHROPIC = {'anthropic-version': '2023-06-01'}
# What the page saves to leave only GPT models listed in Codex, plus the
# synthetic models the other clients need. Every other id follows new-models.
OFF = {'claude-fable-5-1': False, 'cpa-sonnet': False, 'or-kimi-k2': False, 'grok-4.7': False, 'grok-imagine-video': False}
# Codex models CPA v8.0.4 registers for a Codex API key, both from its built-in
# catalog and after its startup refresh from router-for-me/models, which can add
# or drop models at any time (it dropped gpt-5.5 in November 2026).
TARGET = ['gpt-6-astra', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna']
FIXTURE = TARGET + ['codex-auto-review', 'claude-fable-5-1', 'cpa-sonnet', 'or-kimi-k2', 'grok-4.7']
UNWANTED = ('claude-', 'cpa-', 'or-', 'grok-')

# Antigravity fixtures. Synthetic aliases carry no limits and are present in
# every CPA image: gemini-4-neon-xhigh is an effort id agy sends under its base
# name, and mp-slot-01 .. mp-slot-40 exceed the 32 slots.
SLOTS = [f'mp-slot-{n:02d}' for n in range(1, 41)]
AGY_REQUIRED = ['gemini-4-neon-xhigh', 'grok-imagine-video'] + SLOTS
# The Vertex API key registers CPA's catalog of Vertex models, whose advertised
# limits drive the token checks:
#   gemini-4-argon         agy built-in; CPA advertises output == input (1048576)
#   gemini-2.5-pro         agy built-in with ordinary limits
#   gemini-2.5-flash-lite  agy built-in; contains flash-lite
#   gemini-3.5-flash-lite  needs a placeholder slot; limits passed through
#   gemini-omni-1.2-flash  needs a slot; output (260640) above half the input (131072)
# CPA v8.0.4's embedded catalog lacks gemini-4-argon and gemini-omni-1.2-flash;
# CPA adds them only with its startup refresh from router-for-me/models, which
# needs network access and can change at any time. So these are waited for
# briefly, and the checks that need a missing one are skipped with a message.
AGY_CATALOG = ['gemini-4-argon', 'gemini-2.5-pro', 'gemini-2.5-flash-lite', 'gemini-3.5-flash-lite', 'gemini-omni-1.2-flash']
# Ids agy 1.5.12 has built-in metadata for (REFERENCE.md): no token fields.
AGY_BUILTIN = {'gemini-4-argon', 'gemini-2.5-pro', 'gemini-2.5-flash-lite'}
AGY_KEYS = {'modelId', 'displayName', 'maxTokens', 'maxOutputTokens', 'supportsThinking', 'thinkingBudget'}
OUTPUT_CLAMP = 65536
NON_TEXT_WORDS = ('tts', 'voice', 'image', 'imagine', 'imagen', 'dall', 'video', 'veo', 'embedding', 'embed',
                  'audio', 'whisper', 'moderation')
CLOAK = 'claude-fable-5-dd-'

CONFIG = '''config-version: 8
server:
  host: ""
  port: 8317
management:
  allow-remote: true
  secret-key: {management_key}
  disable-control-panel: true
access:
  api-keys:
    - {client_key}
    - {other_client_key}
oauth:
  auth-dir: /work/auth
api-keys:
  codex:
    - name: codex-smoke
      base-url: http://127.0.0.1:9
      keys:
        - api-key: synthetic-codex-key
  vertex:
    - name: vertex-smoke
      base-url: http://127.0.0.1:9
      keys:
        - api-key: synthetic-vertex-key
  claude:
    - name: claude-smoke
      base-url: http://127.0.0.1:9
      models:
        - name: claude-fable-5-1
          alias: claude-fable-5-1
        - name: claude-sonnet-5-5
          alias: cpa-sonnet
      keys:
        - api-key: synthetic-claude-key
  xai:
    - name: xai-smoke
      base-url: http://127.0.0.1:9/v1
      models:
        - name: grok-4.7
          alias: grok-4.7
        - name: grok-imagine-video
          alias: grok-imagine-video
      keys:
        - api-key: synthetic-xai-key
  openai-compatibility:
    - name: openrouter
      base-url: http://127.0.0.1:9/v1
      keys:
        - api-key: synthetic-openrouter-key
      models:
        - name: moonshotai/kimi-k2
          alias: or-kimi-k2
        - name: gemini-4-neon
          alias: gemini-4-neon-xhigh
    - name: slots
      base-url: http://127.0.0.1:9/v1
      keys:
        - api-key: synthetic-slots-key
      models:
{slots}
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    {plugin}:
      enabled: true
'''


def run(*args):
    return subprocess.check_output(args, text=True).strip()


def split_entries(body):
    """Return the exact bytes of each models[] entry in a compact Codex catalog."""
    prefix, suffix = b'{"models":[', b']}'
    assert body.startswith(prefix) and body.endswith(suffix), 'unexpected catalog framing'
    text, decoder, entries, index = body.decode(), json.JSONDecoder(), [], len(prefix)
    while text[index] != ']':
        value, end = decoder.raw_decode(text, index)
        entries.append((value['slug'], value.get('visibility'), text[index:end].encode()))
        index = end + (1 if text[end] == ',' else 0)
    assert index == len(text) - len(suffix), 'trailing catalog bytes'
    return entries


def expected(baseline, switches, enable_new=True, action='remove'):
    kept = []
    for slug, _, raw in split_entries(baseline):
        if switches.get(slug, enable_new):
            kept.append(raw)
        elif action == 'hide':
            assert raw.count(b'"visibility":') == 1, slug + ': visibility is not a single member'
            kept.append(raw.replace(b'"visibility":"list"', b'"visibility":"hide"'))
    return b'{"models":[' + b','.join(kept) + b']}'


def gemini_models(body):
    """CPA's /v1beta/models as an ordered {id: entry} map."""
    return {m['name'].removeprefix('models/'): m for m in json.loads(body)['models']}


def non_text(model_id, entry):
    """The documented non-text rule: output modalities without text, or a name token."""
    modalities = entry.get('supportedOutputModalities') or []
    if modalities and not any(m.lower() == 'text' for m in modalities):
        return True
    tokens = [t for t in re.split(r'[^a-z0-9]+', model_id.lower()) if t]
    return any(t == w or (len(w) >= 4 and t.startswith(w)) for t in tokens for w in NON_TEXT_WORDS)


def token_fields(model_id, entry):
    """The documented token rule for one agy entry, from CPA's advertised limits."""
    if model_id in AGY_BUILTIN:
        return {}, 'builtin'
    limit_in, limit_out = entry.get('inputTokenLimit') or 0, entry.get('outputTokenLimit') or 0
    if limit_in <= 0 and limit_out <= 0:
        return {}, 'none'
    fields, kind = {}, 'passed'
    if limit_in > 0:
        fields['maxTokens'] = limit_in
    if limit_out > 0:
        fields['maxOutputTokens'] = limit_out
        if (limit_in > 0 and limit_out > limit_in // 2) or (limit_in <= 0 and limit_out > OUTPUT_CLAMP):
            cap = OUTPUT_CLAMP if limit_in <= 0 else min(OUTPUT_CLAMP, limit_in // 2)
            fields['maxOutputTokens'], kind = min(limit_out, cap), 'clamped'
    return fields, kind


def agy_order(cpa_ids, selected, default='', title=''):
    """Selected ids by id (CPA's own order changes on every reload), default
    first, title ahead of other flash-lite ids. Python compares str by code
    point, as Go compares strings."""
    ids = sorted(i for i in cpa_ids if i in selected)
    if default in ids:
        ids.remove(default)
        ids.insert(0, default)
    if 'flash-lite' in title and title in ids and ids[0] != title:
        at = ids.index(title)
        first = next((j for j in range(1, at) if 'flash-lite' in ids[j]), None)
        if first is not None:
            ids.pop(at)
            ids.insert(first, title)
    return ids


def check_agy_shape(status, headers, body):
    """Return the parsed agy catalog after checking its framing, or raise."""
    assert status == 200, f'agy list: HTTP {status} {body[:200]!r}'
    assert headers.get('Content-Type') == 'application/json; charset=utf-8', headers.get('Content-Type')
    assert headers.get('Cache-Control') == 'no-store', headers.get('Cache-Control')
    doc = json.loads(body)
    assert set(doc) == {'defaultModelId', 'list'} and doc['list'], 'agy list must be {defaultModelId, list} and non-empty'
    labels = [e.get('displayName') for e in doc['list']]
    assert all(isinstance(label, str) and label for label in labels), 'every entry needs a displayName'
    assert len(set(labels)) == len(labels), 'displayNames must be unique'
    for e in doc['list']:
        assert set(e) <= AGY_KEYS and isinstance(e.get('modelId'), str) and e['modelId'], e
        # agy's ACP lists an entry labelled with its own id under a shifting
        # MODEL_PLACEHOLDER id instead.
        assert e['displayName'].strip() != e['modelId'], f'{e["modelId"]}: the label equals the id'
    return doc


def decloak(model_id):
    if model_id.startswith(CLOAK) and len(model_id) > len(CLOAK):
        return model_id[len(CLOAK):][::-1]
    return model_id


def claude_expected(baseline, allows):
    """CPA's Claude-format list with only allowed (real, de-cloaked) ids kept."""
    data = [e for e in baseline['data'] if allows(decloak(e['id']))]
    if not data or len(data) == len(baseline['data']):
        return baseline
    return dict(baseline, data=data, first_id=data[0]['id'], last_id=data[-1]['id'])


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--library', type=Path, required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--image', default=IMAGE, help='local CPA image to run (default: pinned v8.0.4); never pulled')
    parser.add_argument('--container-prefix', default='codex-catalog-filter-smoke-', help='name prefix for the throwaway container')
    args = parser.parse_args()
    if not args.library.is_file():
        raise SystemExit(f'build it first: missing {args.library}')

    with tempfile.TemporaryDirectory(prefix='codex-catalog-filter-smoke-') as tmp:
        work = Path(tmp)
        # The Plugin Store's layout and file name, as a production install has.
        library_dir, auth = work / 'plugins' / 'linux' / 'amd64', work / 'auth'
        library_dir.mkdir(parents=True)
        auth.mkdir()
        shutil.copy2(args.library, library_dir / f'{PLUGIN}-v{args.version}.so')
        config = work / 'config.yaml'
        slots = '\n'.join(f'        - name: upstream-{slot}\n          alias: {slot}' for slot in SLOTS)
        config.write_text(CONFIG.format(management_key=MANAGEMENT_KEY, client_key=CLIENT_KEY,
                                        other_client_key=OTHER_CLIENT_KEY, slots=slots, plugin=PLUGIN))
        name = args.container_prefix + os.urandom(4).hex()
        container = run('docker', 'create', '--pull=never', '--name', name, '-p', '127.0.0.1::8317',
                        '--user', f'{os.getuid()}:{os.getgid()}',
                        '-v', f'{work}:/work', '-v', f'{work / "plugins"}:/CLIProxyAPI/plugins',
                        '-v', f'{config}:/CLIProxyAPI/config.yaml',
                        args.image, './CLIProxyAPI', '--config', '/CLIProxyAPI/config.yaml')
        origin = ''
        try:
            run('docker', 'start', container)
            origin = 'http://' + run('docker', 'port', container, '8317/tcp').splitlines()[0]

            def request(path, key=CLIENT_KEY, method='GET', data=None, headers=None):
                """Return (status, headers, body); HTTP errors are results, not exceptions."""
                req = urllib.request.Request(origin + path, method=method, data=data)
                if key is not None:
                    req.add_header('Authorization', 'Bearer ' + key)
                for header, value in (headers or {}).items():
                    req.add_header(header, value)
                try:
                    with urllib.request.urlopen(req, timeout=10) as response:
                        return response.status, response.headers, response.read()
                except urllib.error.HTTPError as err:
                    return err.code, err.headers, err.read()

            def management(path, method='GET', body=None):
                data = json.dumps(body).encode() if body is not None else None
                status, _, raw = request('/v0/management' + path, MANAGEMENT_KEY, method, data,
                                         {'Content-Type': 'application/json'} if data else None)
                # Never retry a rejected management key: CPA bans the IP after five.
                assert status == 200, f'management {path}: HTTP {status}'
                return json.loads(raw)

            def patch(changes):
                management(f'/plugins/{PLUGIN}/config', 'PATCH', changes)

            def wait_until(fetch, ready, what, timeout=45):
                deadline = time.monotonic() + timeout
                while True:
                    try:
                        result = fetch()
                        if ready(result):
                            return result
                    except (urllib.error.URLError, ConnectionError):
                        pass
                    if time.monotonic() > deadline:
                        raise AssertionError('timed out waiting for ' + what)
                    time.sleep(0.25)

            def settle(fetch, check, what):
                """Retry fetch until check(result) returns None (no mismatch), then return result.

                CPA reloads configuration asynchronously and can refresh model
                metadata in the background, so each attempt compares results read
                in the same attempt. The last mismatch is reported on timeout.
                """
                deadline, problem = time.monotonic() + 45, 'no attempt'
                while True:
                    try:
                        result = fetch()
                        problem = check(result)
                        if problem is None:
                            return result
                    except (urllib.error.URLError, ConnectionError) as err:
                        problem = repr(err)
                    if time.monotonic() > deadline:
                        raise AssertionError(f'{what}: {problem}')
                    time.sleep(0.25)

            def catalog():
                return request(CATALOG)

            def await_served(want, what):
                """Wait until the plugin URL serves want(CPA's own catalog)."""
                def attempt():
                    return request(CODEX_QUERY), catalog()
                def ready(pair):
                    direct, served = pair
                    return direct[0] == 200 and served[0] == 200 and served[2] == want(direct[2])
                direct, served = wait_until(attempt, ready, what)
                return direct[2], served

            def reconfigure(changes, want, what):
                patch(changes)
                return await_served(want, what)

            def complete(body):
                return set(FIXTURE) <= {s for s, _, _ in split_entries(body)}

            # CPA answers before it has registered every configured model.
            wait_until(lambda: request(CODEX_QUERY), lambda r: r[0] == 200 and complete(r[2]), 'CPA models')
            wait_until(lambda: request(GEMINI_LIST), lambda r: r[0] == 200 and set(AGY_REQUIRED) <= set(gemini_models(r[2])),
                       'CPA Gemini-format models')
            try:
                wait_until(lambda: request(GEMINI_LIST), lambda r: r[0] == 200 and set(AGY_CATALOG) <= set(gemini_models(r[2])),
                           'the Vertex models from CPA\'s model catalog', timeout=30)
            except AssertionError:
                pass
            agy_catalog = [i for i in AGY_CATALOG if i in gemini_models(request(GEMINI_LIST)[2])]
            if len(agy_catalog) < len(AGY_CATALOG):
                print(f'NOTE: CPA\'s model catalog lists no {sorted(set(AGY_CATALOG) - set(agy_catalog))}; '
                      'the agy checks that need them are skipped (CPA v8.0.4\'s embedded catalog lacks some, '
                      'and its startup refresh from router-for-me/models needs network access)')
            logs = subprocess.run(['docker', 'logs', container], capture_output=True, text=True, check=True)
            banner = BANNER.search(logs.stdout + logs.stderr)
            assert banner, 'no CPA version banner'
            cpa_version = tuple(int(part) for part in banner.group(1, 2, 3))
            cpa = 'v' + '.'.join(map(str, cpa_version))
            if args.image == IMAGE:
                assert CPA_BANNER in logs.stdout + logs.stderr, 'unexpected CPA runtime version'
            print(f'CPA {cpa} (commit {banner.group(4)}) from {args.image}')
            record = {p['id']: p for p in management('/plugins')['plugins']}[PLUGIN]
            assert record['registered'] and record['effective_enabled'], 'plugin inactive'
            assert record['metadata']['version'] == args.version, 'unexpected plugin version'
            assert record['metadata']['name'] == NAME, record['metadata']['name']
            assert record.get('menus') == [{'path': SETTINGS, 'menu': NAME, 'description': 'Choose which models each client shows.'}], record.get('menus')

            # Before any client has fetched, the page has no list to show.
            state = management(STATE)
            assert state['seen_at'] is None and state['models'] == [] and state['switches'] == {}, state
            assert (state['new_models'], state['action'], state['version']) == ('enabled', 'remove', args.version), state
            agy_state, claude_state = state['antigravity'], state['claude_code']
            assert (agy_state['config_key'], agy_state['configured'], agy_state['error'], agy_state['new_models']) == ('antigravity', False, '', 'disabled'), agy_state
            assert agy_state['preview'] is None and agy_state['path'] == AGY, agy_state
            assert (claude_state['config_key'], claude_state['filter'], claude_state['new_models'], claude_state['error']) == ('claude-code', True, 'enabled', ''), claude_state

            # ---- Codex: the catalog URL ----
            # No switches: the URL serves CPA's catalog byte for byte.
            codex, (_, headers, _) = await_served(lambda direct: direct, 'idle catalog')
            assert headers.get('Content-Type') == 'application/json; charset=utf-8'
            print(f'Codex idle: the plugin URL serves CPA\'s {len(split_entries(codex))}-entry catalog unchanged ({len(codex)} bytes)')

            # The page's private route now lists what Codex was just offered.
            state = management(STATE)
            assert [m['slug'] for m in state['models']] == [s for s, _, _ in split_entries(codex)], 'state list differs from the catalog'
            assert all(m['enabled'] and not m['switched'] for m in state['models']) and state['seen_at'], state
            print(f'Codex state: the page lists all {len(state["models"])} models, every one on by default')

            codex, (_, _, body) = reconfigure({'models': OFF}, lambda d: expected(d, OFF), 'switches')
            entries = split_entries(body)
            listed = [s for s, v, _ in entries if v == 'list']
            assert set(TARGET) <= set(listed) and not any(s.startswith(UNWANTED) for s in listed)
            assert ('codex-auto-review', 'hide') in [(s, v) for s, v, _ in entries], 'codex-auto-review must stay hidden'
            state = {m['slug']: m for m in management(STATE)['models']}
            assert all(not state[s]['enabled'] and state[s]['switched'] for s in OFF) and state['gpt-6-sol']['enabled'] and not state['gpt-6-sol']['switched']
            # The interceptor rewrites only Claude Code's list: CPA's own lists
            # for every other caller still carry every model.
            assert complete(codex), 'CPA\'s own Codex catalog was filtered'
            plain = {m['id'] for m in json.loads(request('/v1/models')[2])['data']}
            assert set(FIXTURE) <= plain, 'CPA\'s own model list was filtered'
            print(f'Codex switches off {sorted(OFF)}: {len(entries)} entries ({len(body)} bytes), byte-exact')
            print('Unchanged: CPA\'s own /v1/models, with and without client_version, still lists every model')

            _, (_, _, body) = reconfigure({'action': 'hide'}, lambda d: expected(d, OFF, action='hide'), 'hide')
            assert {s for s, v, _ in split_entries(body) if v == 'list'} == set(listed)
            print(f'Codex hide: all {len(split_entries(body))} entries kept, same listed set')
            only = {'gpt-6-sol': True, 'codex-auto-review': True}
            _, (_, _, body) = reconfigure({'action': 'remove', 'new-models': 'disabled', 'models': only},
                                          lambda d: expected(d, only, enable_new=False), 'new models disabled')
            assert [(s, v) for s, v, _ in split_entries(body)] == [('gpt-6-sol', 'list'), ('codex-auto-review', 'hide')], body[:200]
            print('Codex new models disabled: only the models switched on remain')
            reconfigure({'new-models': 'enabled', 'models': OFF}, lambda d: expected(d, OFF), 'restore')

            # The URL needs no key of its own, but CPA still checks the forwarded
            # one. Failed client keys never count toward the management-key ban.
            for key in [None] * 6 + ['wrong-key']:
                status, _, body = request(CATALOG, key)
                assert status == 401, f'key {key!r}: HTTP {status} {body[:80]!r}'
            management('/plugins')
            print('Codex: keyless and wrong-key requests get CPA\'s 401; the management API still answers afterwards')
            for path, method in [(CATALOG.split('?')[0] + '/extra', 'GET'), (CATALOG, 'POST'), (AGY + '/', 'GET'),
                                 (AGY, 'POST'), ('/v0/resource/plugins/codex-catalog-filter/state', 'GET')]:
                status = request(path, method=method, data=b'' if method == 'POST' else None)[0]
                assert status in (404, 405), f'{method} {path}: HTTP {status}'

            # ---- Antigravity: the agy catalog URL ----
            def agy_attempt(key=CLIENT_KEY):
                return request(GEMINI_LIST), request(AGY, key)

            def agy_settings(sent):
                """Wait until /state reports the antigravity subtree just PATCHed."""
                def check(section):
                    want = (sent.get('models', {}), sent.get('new-models', 'disabled'), sent.get('default-model', ''), sent.get('title-model', ''), '')
                    got = (section['switches'], section['new_models'], section['default_model'], section['title_model'], section['error'])
                    return None if got == want else f'state reports {got}, sent {want}'
                settle(lambda: management(STATE)['antigravity'], check, 'antigravity settings')

            # No antigravity settings: nothing is switched on, so agy gets CPA's
            # Gemini text models instead of an empty list.
            def fallback_check(pair):
                (_, _, cpa_body), (status, headers, body) = pair
                cpa_models = gemini_models(cpa_body)
                doc = check_agy_shape(status, headers, body)
                want = sorted(i for i, m in cpa_models.items() if i.startswith('gemini-') and not non_text(i, m))
                got = [e['modelId'] for e in doc['list']]
                if got != want or doc['defaultModelId'] != want[0]:
                    return f'fallback served {got}, want {want}'
                return None
            _, (_, _, body) = settle(agy_attempt, fallback_check, 'agy fallback')
            preview = management(STATE)['antigravity']['preview']
            assert preview and preview['fallback'] and preview['warnings'], preview
            print(f'agy defaults: nothing switched on, so CPA\'s {len(json.loads(body)["list"])} Gemini text models are served, never an empty list')
            for key in [None, 'wrong-key']:
                status, _, body = request(AGY, key)
                assert status == 401, f'agy key {key!r}: HTTP {status} {body[:80]!r}'
            print('agy: keyless and wrong-key requests get CPA\'s 401')

            # Explicit switches, a default, and a title model: order, labels,
            # token limits, and thinking.
            chosen = {i: True for i in ['gemini-2.5-pro', 'gemini-2.5-flash-lite', 'gemini-3.5-flash-lite', 'gemini-omni-1.2-flash',
                                        'gemini-4-neon-xhigh', 'grok-4.7', 'grok-imagine-video', 'mp-slot-01']}
            # An agy built-in from CPA's catalog as the default, when there is one.
            default = next((i for i in ['gemini-4-argon', 'gemini-2.5-pro'] if i in agy_catalog), 'mp-slot-01')
            title = 'gemini-3.5-flash-lite'
            sent = {'models': chosen, 'new-models': 'disabled', 'default-model': default, 'title-model': title}
            patch({'antigravity': sent})
            agy_settings(sent)
            kinds = {}

            def chosen_check(pair):
                (_, _, cpa_body), (status, headers, body) = pair
                cpa_models = gemini_models(cpa_body)
                doc = check_agy_shape(status, headers, body)
                want = agy_order(list(cpa_models), set(chosen) | {default}, default, title)
                got = [e['modelId'] for e in doc['list']]
                if got != want:
                    return f'served {got}, want {want}'
                if doc['defaultModelId'] != default:
                    return 'defaultModelId ' + doc['defaultModelId']
                if title in got and next(i for i in got if 'flash-lite' in i) != title:
                    return 'the title model is not the first flash-lite id'
                kinds.clear()
                for e in doc['list']:
                    fields, kind = token_fields(e['modelId'], cpa_models[e['modelId']])
                    tokens = {k: e[k] for k in ('maxTokens', 'maxOutputTokens') if k in e}
                    if tokens != fields:
                        return f'{e["modelId"]}: tokens {tokens}, want {fields} ({kind})'
                    kinds.setdefault(kind, []).append(e['modelId'])
                    thinking = {k: e[k] for k in ('supportsThinking', 'thinkingBudget') if k in e}
                    want_thinking = {'supportsThinking': True, 'thinkingBudget': 32768} if e['modelId'] == 'gemini-4-neon-xhigh' else {}
                    if thinking != want_thinking:
                        return f'{e["modelId"]}: thinking {thinking}, want {want_thinking}'
                return None
            _, (_, _, body) = settle(agy_attempt, chosen_check, 'agy switches')
            doc = json.loads(body)
            labels = {e['modelId']: e['displayName'] for e in doc['list']}
            # CPA names an alias without a display name after the alias itself;
            # the plugin puts it into words and keeps the effort.
            assert labels['gemini-4-neon-xhigh'].endswith('(XHigh)'), labels['gemini-4-neon-xhigh']
            # Each token case must occur whenever this image's catalog gives its
            # fixture the limits for it. Catalogs differ: pinned v8.0.4 lists
            # gemini-3.5-flash-lite and gemini-omni-1.2-flash without limits, so
            # only their 'none' case can occur there. Every served entry was
            # already checked against the token rule above. If CPA's catalog
            # drops a fixture's limits, pick another Vertex model with the case.
            cpa_now = gemini_models(request(GEMINI_LIST)[2])
            cases = {'none'} | ({'builtin'} if default != 'mp-slot-01' else set())
            for fixture, case in [('gemini-3.5-flash-lite', 'passed'), ('gemini-omni-1.2-flash', 'clamped')]:
                if fixture in cpa_now and token_fields(fixture, cpa_now[fixture])[1] == case:
                    cases.add(case)
            assert cases <= set(kinds), f'token cases not all exercised: {kinds}'
            if missing := {'builtin', 'passed', 'clamped'} - set(kinds):
                print(f'SKIP agy token cases {sorted(missing)}: this image\'s model catalog has no fixture with the limits for them')
            models = {m['id']: m for m in management(STATE)['antigravity']['models']}
            assert models['grok-imagine-video']['non_text'] and models['grok-imagine-video']['in_list'], 'a switched-on non-text model stays listed and flagged'
            for fixture in ['gemini-3.5-flash-lite', 'gemini-omni-1.2-flash']:
                if fixture in cpa_now:
                    want_kind = token_fields(fixture, cpa_now[fixture])[1]
                    assert models[fixture]['tokens'] == want_kind, (models[fixture], want_kind)
            if default != 'mp-slot-01':
                assert models[default]['tokens'] == 'builtin', models[default]
                limits = gemini_models(request(GEMINI_LIST)[2])[default]
                limits = (limits.get('inputTokenLimit'), limits.get('outputTokenLimit'))
            print(f'agy switches: default first, title model ahead of other flash-lite ids, by id otherwise: {list(labels)}')
            print('agy tokens: ' + '; '.join(f'{kind} {ids}' for kind, ids in sorted(kinds.items()))
                  + (f' (CPA advertises {limits} for {default}; none are sent)' if default != 'mp-slot-01' else ''))
            print(f'agy thinking: gemini-4-neon-xhigh is labelled {labels["gemini-4-neon-xhigh"]!r} with thinkingBudget 32768')

            # More ids without built-in metadata than agy's 32 placeholder slots.
            slot_switches = {slot: True for slot in SLOTS}
            sent = {'models': slot_switches, 'new-models': 'disabled', 'default-model': default}
            patch({'antigravity': sent})
            agy_settings(sent)
            cap = {}
            # A built-in default takes no slot; the fallback default is a slot id.
            lead = [default] if default != 'mp-slot-01' else []

            def slot_check(pair):
                (_, _, cpa_body), (status, headers, body) = pair
                order = sorted(i for i in gemini_models(cpa_body) if i in slot_switches)
                doc = check_agy_shape(status, headers, body)
                got = [e['modelId'] for e in doc['list']]
                if got != lead + order[:32]:
                    return f'served {got}, want {lead} and the first 32 slots by id {order[:32]}'
                cap['skipped'] = order[32:]
                return None
            settle(agy_attempt, slot_check, 'agy slot cap')
            preview = management(STATE)['antigravity']['preview']
            assert (preview['slots_used'], preview['slot_limit'], preview['skipped']) == (32, 32, cap['skipped']), preview
            print(f'agy slots: 40 switched-on ids without built-in metadata; the first 32 by id are served, {len(cap["skipped"])} reported as skipped')

            # New models enabled: every text model is offered, non-text ones are not.
            sent = {'new-models': 'enabled'}
            patch({'antigravity': sent})
            agy_settings(sent)

            def open_check(pair):
                (_, _, cpa_body), (status, headers, body) = pair
                cpa_models = gemini_models(cpa_body)
                doc = check_agy_shape(status, headers, body)
                got = [e['modelId'] for e in doc['list']]
                leaked = [i for i in got if non_text(i, cpa_models[i])]
                if leaked:
                    return f'non-text models served: {leaked}'
                text = [i for i in cpa_models if not non_text(i, cpa_models[i])]
                if sorted(i for i in text if i in got) != got:
                    return 'served ids are not in id order'
                return None
            _, (_, _, body) = settle(agy_attempt, open_check, 'agy new models enabled')
            last_agy = json.loads(body)
            preview = management(STATE)['antigravity']['preview']
            assert preview['slots_used'] == 32 and preview['skipped'], preview
            print(f'agy new models enabled: {len(last_agy["list"])} text models served, no image, video, or speech models; {len(preview["skipped"])} beyond the slots')

            # A bad antigravity subtree disables only that client's new settings.
            patch({'antigravity': {'models': {'gemini-4-argon': 'maybe'}}})
            section = settle(lambda: management(STATE)['antigravity'],
                             lambda s: None if s['error'] else 'no error reported yet', 'antigravity error')
            assert section['using'] == 'last-good' and section['new_models'] == 'enabled', section
            record = {p['id']: p for p in management('/plugins')['plugins']}[PLUGIN]
            assert record['registered'] and record['effective_enabled'], 'a bad client subtree must not deactivate the plugin'
            status = catalog()[0]
            assert status == 200, f'the Codex URL must keep serving: HTTP {status}'
            settle(agy_attempt, open_check, 'the agy URL must keep serving the last good settings')
            print(f'agy bad subtree: reported ({section["error"]!r}); the last good settings still apply and both catalog URLs keep serving')
            sent = {'new-models': 'enabled'}
            patch({'antigravity': sent})
            agy_settings(sent)

            # ---- Claude Code: the rewritten Claude-format list ----
            if cpa_version < MODEL_LIST_INTERCEPTORS:
                print(f'SKIP Claude Code: CPA {cpa} runs no response interceptors on model lists; they arrived in v7.2.158 (WriteModelListResponse)')
            else:
                def claude_lists(agent=CLAUDE_CODE_UA, headers=ANTHROPIC):
                    baseline = request('/v1/models', headers=dict(ANTHROPIC, **{'User-Agent': 'smoke-client/1.0'}))
                    served = request('/v1/models', headers=dict(headers, **{'User-Agent': agent}))
                    return baseline, served

                def claude_check(allows, agent=CLAUDE_CODE_UA, headers=ANTHROPIC):
                    def check(pair):
                        (status, _, raw), (served_status, _, served) = pair
                        if status != 200 or served_status != 200:
                            return f'HTTP {status}/{served_status}'
                        baseline = json.loads(raw)
                        if {'data', 'first_id', 'last_id', 'has_more'} - set(baseline):
                            return 'CPA did not answer in Claude format'
                        want = claude_expected(baseline, allows)
                        got = json.loads(served)
                        return None if got == want else f'{agent} got {len(got["data"])} entries, want {len(want["data"])}'
                    return settle(lambda: claude_lists(agent, headers), check, 'Claude Code list for ' + agent)

                # Defaults: filter on, new models enabled, no switches: unchanged.
                (_, _, raw), _ = claude_check(lambda _: True)
                baseline = json.loads(raw)
                cloaked = {e['id'] for e in baseline['data'] if e['id'].startswith(CLOAK)}
                assert CLOAK + '7.4-korg' in cloaked, 'CPA no longer cloaks grok-4.7 for Claude Code'
                section = management(STATE)['claude_code']
                seen = {m['id']: m for m in section['models']}
                assert section['lists'] >= 1 and section['rewritten'] == 0 and section['seen_at'], section
                assert seen['grok-4.7']['cloaked_id'] == CLOAK + '7.4-korg' and seen['grok-4.7']['enabled'], seen['grok-4.7']
                print(f'Claude Code defaults: CPA\'s {len(baseline["data"])}-entry list passes unchanged; the page lists real ids ({len(cloaked)} cloaked by CPA)')

                off = {'grok-4.7': False, 'gpt-6-sol': False, 'mp-slot-01': False}
                patch({'claude-code': {'filter': True, 'models': off, 'new-models': 'enabled'}})
                (_, _, raw), (_, _, served) = claude_check(lambda i: off.get(i, True))
                got, unfiltered = json.loads(served), json.loads(raw)
                assert len(got['data']) == len(unfiltered['data']) - len(off) and got['has_more'] == unfiltered['has_more']
                assert (got['first_id'], got['last_id']) == (got['data'][0]['id'], got['data'][-1]['id'])
                claude_check(lambda i: off.get(i, True), agent='claude-cli/2.1.327 (external, cli)', headers={})
                # Other callers keep CPA's lists: another agent in Claude format,
                # and Claude Code's agent without anthropic-version (OpenAI format).
                plain = request('/v1/models', headers={'User-Agent': CLAUDE_CODE_UA})
                assert set(FIXTURE) <= {m['id'] for m in json.loads(plain[2])['data']}, 'the OpenAI-format list was filtered'
                section = management(STATE)['claude_code']
                assert section['rewritten'] >= 1 and section['intercept_errors'] == 0, section
                print(f'Claude Code switches off {sorted(off)}: {len(got["data"])} entries, de-cloaked for matching, first_id/last_id recomputed; other callers unchanged')

                only = {'gpt-6-sol': True}
                patch({'claude-code': {'filter': True, 'models': only, 'new-models': 'disabled'}})
                _, (_, _, served) = claude_check(lambda i: only.get(i, False))
                assert [e['id'] for e in json.loads(served)['data']] == [CLOAK + 'los-6-tpg'], served[:200]
                print('Claude Code new models disabled: only gpt-6-sol remains, in CPA\'s cloaked form')

                patch({'claude-code': {'filter': False, 'models': off, 'new-models': 'enabled'}})
                settle(lambda: management(STATE)['claude_code'], lambda s: None if not s['filter'] else 'filter still on', 'filter off')
                claude_check(lambda _: True)
                print('Claude Code filter off: the interceptor is no longer declared and the list passes unchanged')
                patch({'claude-code': None})
                settle(lambda: management(STATE)['claude_code'],
                       lambda s: None if s['filter'] and not s['configured'] and s['switches'] == {} else 'not reset', 'claude-code reset')

            # ---- The page ----
            # Fixed bytes behind a hash-pinned CSP; its data comes only from the
            # management-key route above.
            status, headers, page = request(SETTINGS, None)
            csp = headers.get('Content-Security-Policy', '')
            assert status == 200 and headers.get('Content-Type') == 'text/html; charset=utf-8', status
            assert f'<title>{NAME}</title>'.encode() in page, 'page title is not ' + NAME
            assert "default-src 'none'" in csp and "frame-ancestors 'self'" in csp and "connect-src 'self'" in csp, csp
            assert b'gpt-6-sol' not in page and b'mp-slot-01' not in page, 'the public page must not carry model data'
            print(f'Page: the {NAME} sidebar page is served with its locked-down CSP and no data')

            # A rejected top-level value deactivates the plugin, so the URL is gone
            # and Codex keeps its cached catalog; fixing it brings the URL back.
            patch({'models': {'gpt-6-sol': 'maybe'}})
            wait_until(catalog, lambda r: r[0] == 404, 'route removal')
            codex, _ = reconfigure({'models': OFF}, lambda d: expected(d, OFF), 'recovery')
            print('Invalid Codex switch: the URLs return 404 until the configuration is fixed, then serve again without a restart')
            data_dir = work / 'plugins' / 'data' / PLUGIN
            saved = data_dir / 'catalog.json'
            assert saved.is_file() and b'base_instructions' not in saved.read_bytes(), 'model list not saved as expected'

            # ---- agy fail-safe: CPA unavailable ----
            # Only a key CPA accepted on the agy route gets the last good list.
            _, (_, _, body) = settle(agy_attempt, open_check, 'agy before CPA goes away')
            last_agy = json.loads(body)
            patch({'cpa-url': 'http://127.0.0.1:9'})
            wait_until(catalog, lambda r: r[0] == 502, 'cpa-url change')
            status, headers, body = request(AGY)
            assert check_agy_shape(status, headers, body) == last_agy, 'the last good list differs'
            for key in [OTHER_CLIENT_KEY, 'wrong-key', None]:
                status, _, body = request(AGY, key)
                assert status == 502 and json.loads(body) == {'error': 'cpa_unavailable'}, f'agy key {key!r} while CPA is away: HTTP {status}'
            section = management(STATE)['antigravity']
            assert (section['served_from'], section['last_error']) == ('last-good', 'cpa_unavailable'), section
            clients = data_dir / 'clients.json'
            raw = clients.read_bytes()
            assert stat.S_IMODE(clients.stat().st_mode) == 0o600, oct(clients.stat().st_mode)
            assert CLIENT_KEY.encode() not in raw and OTHER_CLIENT_KEY.encode() not in raw, 'clients.json holds a raw key'
            print('agy CPA away: the accepted key gets the last good list; another valid key, a wrong key, and no key get 502')
            print('Saved: clients.json is mode 0600 and holds key hashes only')

            run('docker', 'restart', container)
            origin = 'http://' + run('docker', 'port', container, '8317/tcp').splitlines()[0]
            # The page's lists survive the restart before any client fetches again.
            def state_after_restart():
                result = request('/v0/management' + STATE, MANAGEMENT_KEY)
                # Never retry a rejected management key.
                assert result[0] not in (401, 403), f'management key rejected: HTTP {result[0]}'
                return result
            state = json.loads(wait_until(state_after_restart, lambda r: r[0] == 200, 'state after restart')[2])
            assert [m['slug'] for m in state['models']] == [s for s, _, _ in split_entries(codex)], 'saved Codex list lost'
            assert state['antigravity']['fetched_at'] and state['antigravity']['preview'], 'saved agy list lost'
            status, headers, body = request(AGY)
            assert check_agy_shape(status, headers, body) == last_agy, 'the last good list did not survive the restart'
            assert request(AGY, None)[0] == 502
            print('Restart with CPA still away: the accepted key gets the saved list from disk; no key gets 502')
            patch({'cpa-url': None})
            await_served(lambda d: expected(d, OFF), 'restart')
            settle(agy_attempt, open_check, 'agy after restart')
            print('Restart: the saved lists and the switches apply from startup')
            print(f'PASS: CPA {cpa} serves the switched Codex and agy catalogs at the plugin URLs, '
                  + ('rewrites Claude Code\'s list, ' if cpa_version >= MODEL_LIST_INTERCEPTORS else '')
                  + f'and the {NAME} page; CPA\'s lists for other callers are unchanged')
        except Exception:
            # Synthetic configuration only; logs hold no real credentials.
            logs = subprocess.run(['docker', 'logs', container], capture_output=True, text=True)
            print((logs.stdout + logs.stderr)[-6000:])
            raise
        finally:
            subprocess.run(['docker', 'rm', '-f', container], check=True, stdout=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
