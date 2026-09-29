#!/usr/bin/env python3
"""Load codex-catalog-filter into a disposable pinned CPA v8.0.4 container.

The unit tests call Handle() in process against a stand-in for CPA. Only this
proves that the official image loads the library, registers its resource
route, lets the plugin fetch CPA's own Codex catalog over loopback with the
caller's key, and serves the filtered result at the URL Codex is pointed at,
while CPA's own model lists stay byte-identical.

No real accounts or provider requests: models come from synthetic static
config. Both keys are synthetic keys for this throwaway container only.
"""
import argparse
import fnmatch
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

IMAGE = 'eceasy/cli-proxy-api@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d'
CPA_BANNER = 'CLIProxyAPI Version: v8.0.4, Commit: d33f63f'
PLUGIN = 'codex-catalog-filter'
CLIENT_KEY = 'synthetic-local-client-key'
MANAGEMENT_KEY = 'synthetic-local-smoke-key'
CATALOG = '/v0/resource/plugins/codex-catalog-filter/models?client_version=0.159.0'
CODEX_QUERY = '/v1/models?client_version=0.159.0'
INCLUDE = ['gpt-[0-9]*', 'codex-*']
# Codex's current picker set, which CPA v8.0.4 registers for a Codex API key.
TARGET = ['gpt-6-astra', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna', 'gpt-5.5']
FIXTURE = TARGET + ['codex-auto-review', 'claude-fable-5-1', 'cpa-sonnet', 'or-kimi-k2', 'grok-4.7']
UNWANTED = ('claude-', 'cpa-', 'or-', 'grok-')

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
oauth:
  auth-dir: /work/auth
api-keys:
  codex:
    - name: codex-smoke
      base-url: http://127.0.0.1:9
      keys:
        - api-key: synthetic-codex-key
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


def allowed(slug, include, exclude):
    return any(fnmatch.fnmatchcase(slug, p) for p in include) and not any(fnmatch.fnmatchcase(slug, p) for p in exclude)


def expected(baseline, include, exclude, action):
    kept = []
    for slug, _, raw in split_entries(baseline):
        if allowed(slug, include, exclude):
            kept.append(raw)
        elif action == 'hide':
            assert raw.count(b'"visibility":') == 1, slug + ': visibility is not a single member'
            kept.append(raw.replace(b'"visibility":"list"', b'"visibility":"hide"'))
    return b'{"models":[' + b','.join(kept) + b']}'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--library', type=Path, required=True)
    parser.add_argument('--version', required=True)
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
        config.write_text(CONFIG.format(management_key=MANAGEMENT_KEY, client_key=CLIENT_KEY, plugin=PLUGIN))
        container = run('docker', 'create', '--pull=never', '-p', '127.0.0.1::8317',
                        '--user', f'{os.getuid()}:{os.getgid()}',
                        '-v', f'{work}:/work', '-v', f'{work / "plugins"}:/CLIProxyAPI/plugins',
                        '-v', f'{config}:/CLIProxyAPI/config.yaml',
                        IMAGE, './CLIProxyAPI', '--config', '/CLIProxyAPI/config.yaml')
        origin = ''
        try:
            run('docker', 'start', container)
            origin = 'http://' + run('docker', 'port', container, '8317/tcp').splitlines()[0]

            def request(path, key=CLIENT_KEY, method='GET', data=None, headers=None):
                """Return (status, headers, body); HTTP errors are results, not exceptions."""
                req = urllib.request.Request(origin + path, method=method, data=data)
                if key is not None:
                    req.add_header('Authorization', 'Bearer ' + key)
                for name, value in (headers or {}).items():
                    req.add_header(name, value)
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

            def wait_until(fetch, ready, what):
                deadline = time.monotonic() + 45
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

            def catalog():
                return request(CATALOG)

            def await_served(want, what):
                """Wait until the plugin URL serves want(CPA's own catalog).

                CPA can refresh model metadata in the background at any time,
                so each attempt compares against a catalog read in the same
                attempt rather than one captured earlier.
                """
                def attempt():
                    return request(CODEX_QUERY), catalog()
                def ready(pair):
                    direct, served = pair
                    return direct[0] == 200 and served[0] == 200 and served[2] == want(direct[2])
                direct, served = wait_until(attempt, ready, what)
                return direct[2], served

            def reconfigure(changes, want, what):
                management(f'/plugins/{PLUGIN}/config', 'PATCH', changes)
                return await_served(want, what)

            def complete(body):
                return set(FIXTURE) <= {s for s, _, _ in split_entries(body)}

            # CPA answers before it has registered every configured model.
            wait_until(lambda: request(CODEX_QUERY), lambda r: r[0] == 200 and complete(r[2]), 'CPA models')
            logs = subprocess.run(['docker', 'logs', container], capture_output=True, text=True, check=True)
            assert CPA_BANNER in logs.stdout + logs.stderr, 'unexpected CPA runtime version'
            record = {p['id']: p for p in management('/plugins')['plugins']}[PLUGIN]
            assert record['registered'] and record['effective_enabled'], 'plugin inactive'
            assert record['metadata']['version'] == args.version, 'unexpected plugin version'
            assert not record.get('menus'), 'the catalog endpoint must not add a sidebar entry'

            # Idle (no include): the URL serves CPA's catalog byte for byte.
            codex, (_, headers, _) = await_served(lambda direct: direct, 'idle catalog')
            assert headers.get('Content-Type') == 'application/json; charset=utf-8'
            print(f'Idle: the plugin URL serves CPA\'s {len(split_entries(codex))}-entry catalog unchanged ({len(codex)} bytes)')

            codex, (_, _, body) = reconfigure({'include': INCLUDE}, lambda d: expected(d, INCLUDE, [], 'remove'), 'remove')
            entries = split_entries(body)
            listed = [s for s, v, _ in entries if v == 'list']
            assert set(TARGET) <= set(listed) and not any(s.startswith(UNWANTED) for s in listed)
            assert ('codex-auto-review', 'hide') in [(s, v) for s, v, _ in entries], 'codex-auto-review must stay hidden'
            # No interceptor: CPA's own lists still carry every model.
            assert complete(codex), 'CPA\'s own Codex catalog was filtered'
            plain = {m['id'] for m in json.loads(request('/v1/models')[2])['data']}
            assert set(FIXTURE) <= plain, 'CPA\'s own model list was filtered'
            print(f'Remove (default): {len(entries)} entries ({len(body)} bytes), byte-exact; listed {listed}')
            print('Unchanged: CPA\'s own /v1/models, with and without client_version, still lists every model')

            _, (_, _, body) = reconfigure({'action': 'hide'}, lambda d: expected(d, INCLUDE, [], 'hide'), 'hide')
            assert {s for s, v, _ in split_entries(body) if v == 'list'} == set(listed)
            print(f'Hide: all {len(split_entries(body))} entries kept, same listed set')
            reconfigure({'action': 'remove', 'exclude': ['gpt-5.5']}, lambda d: expected(d, INCLUDE, ['gpt-5.5'], 'remove'), 'exclude')
            print('Exclude: gpt-5.5 removed as configured')

            # The URL needs no key of its own, but CPA still checks the forwarded
            # one. Failed client keys never count toward the management-key ban.
            for key in [None] * 6 + ['wrong-key']:
                status, _, body = request(CATALOG, key)
                assert status == 401, f'key {key!r}: HTTP {status} {body[:80]!r}'
            management('/plugins')
            print('Keyless and wrong-key requests get CPA\'s 401; the management API still answers afterwards')
            for path, method in [(CATALOG.split('?')[0] + '/extra', 'GET'), (CATALOG, 'POST'), ('/v0/resource/plugins/codex-catalog-filter/status', 'GET')]:
                status = request(path, method=method, data=b'' if method == 'POST' else None)[0]
                assert status in (404, 405), f'{method} {path}: HTTP {status}'

            # A rejected configuration deactivates the plugin, so the URL is gone
            # and Codex keeps its cached catalog; fixing it brings the URL back.
            management(f'/plugins/{PLUGIN}/config', 'PATCH', {'action': 'drop'})
            wait_until(catalog, lambda r: r[0] == 404, 'route removal')
            reconfigure({'action': 'remove', 'exclude': []}, lambda d: expected(d, INCLUDE, [], 'remove'), 'recovery')
            print('Invalid action: the URL returns 404 until the configuration is fixed, then serves again without a restart')

            run('docker', 'restart', container)
            origin = 'http://' + run('docker', 'port', container, '8317/tcp').splitlines()[0]
            await_served(lambda d: expected(d, INCLUDE, [], 'remove'), 'restart')
            print('Restart: the persisted configuration serves the filtered catalog from startup')
            print('PASS: pinned CPA v8.0.4 serves the filtered Codex catalog at the plugin URL; CPA\'s own lists are unchanged')
        except Exception:
            # Synthetic configuration only; logs hold no real credentials.
            logs = subprocess.run(['docker', 'logs', container], capture_output=True, text=True)
            print((logs.stdout + logs.stderr)[-6000:])
            raise
        finally:
            subprocess.run(['docker', 'rm', '-f', container], check=True, stdout=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
