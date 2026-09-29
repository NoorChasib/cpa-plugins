#!/usr/bin/env python3
"""Load codex-catalog-filter into a disposable pinned CPA v8.0.4 container.

The unit tests call Handle() in process with JSON shaped like CPA's. Only this
proves that the official image loads the library, accepts the ABI handshake,
sends its Codex catalog through the interceptor, and serves the rewritten body,
while every other model list and an ordinary completion stay byte-identical.

No real accounts or provider requests: models come from synthetic static
config, and the one completion goes to a mock upstream that shares CPA's
network namespace, so it is reached on loopback whatever the host firewall.
The management key is a synthetic key for this throwaway container only.
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
# The official Python image already pinned by token-usage's store smoke.
MOCK_IMAGE = 'python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285'
MOCK_PORT = 18080
CPA_BANNER = 'CLIProxyAPI Version: v8.0.4, Commit: d33f63f'
PLUGIN = 'codex-catalog-filter'
CLIENT_KEY = 'synthetic-local-client-key'
MANAGEMENT_KEY = 'synthetic-local-smoke-key'
CODEX_QUERY = '/v1/models?client_version=0.159.0'
INCLUDE = ['gpt-[0-9]*', 'codex-*']
# Codex's current picker set, which CPA v8.0.4 registers for a Codex API key.
TARGET = ['gpt-6-astra', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna', 'gpt-5.5']
UNWANTED = ('claude-', 'cpa-', 'or-', 'grok-')
COMPLETION = {
    'id': 'chatcmpl-smoke', 'object': 'chat.completion', 'created': 1790000000, 'model': 'moonshotai/kimi-k2',
    'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'ok <b>&</b> "models"'}, 'finish_reason': 'stop'}],
    'usage': {'prompt_tokens': 3, 'completion_tokens': 1, 'total_tokens': 4},
}

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
      base-url: http://127.0.0.1:{mock_port}/v1
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


# Answers OpenAI-compatible chat completions with fixed bytes and logs each
# request's path and credential, one line per request, for the final check.
MOCK = '''
import http.server, sys
BODY = sys.argv[2].encode()
class Mock(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        print("request", self.path, self.headers.get("Authorization"), flush=True)
        if self.path != "/v1/chat/completions":
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(BODY)))
        self.end_headers()
        self.wfile.write(BODY)
    def log_message(self, *_):
        pass
server = http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Mock)
print("ready", flush=True)
server.serve_forever()
'''


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


def comparable(body):
    """Parse a non-Codex model list into a form that survives a config reload.

    Every PATCH below makes CPA reload its configuration and re-register
    clients. That reorders these lists, which CPA builds from a map, and
    re-stamps the registration time of statically configured models. Between
    reloads the bytes are identical (the idle check asserts that). The Codex
    catalog is sorted by priority and has no timestamps, so it is compared byte
    for byte across reloads instead.
    """
    doc = json.loads(body)
    for key in ('data', 'models'):
        if isinstance(doc.get(key), list):
            entries = [{k: v for k, v in entry.items() if k not in ('created', 'created_at')} for entry in doc[key]]
            doc[key] = sorted(entries, key=lambda entry: json.dumps(entry, sort_keys=True))
    return doc


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
        plugins, auth = work / 'plugins', work / 'auth'
        plugins.mkdir()
        auth.mkdir()
        shutil.copy2(args.library, plugins / f'{PLUGIN}.so')
        config = work / 'config.yaml'
        config.write_text(CONFIG.format(management_key=MANAGEMENT_KEY, client_key=CLIENT_KEY, plugin=PLUGIN,
                                        mock_port=MOCK_PORT))
        mock = None
        container = run('docker', 'create', '--pull=never', '-p', '127.0.0.1::8317',
                        '--user', f'{os.getuid()}:{os.getgid()}',
                        '-v', f'{work}:/work', '-v', f'{plugins}:/CLIProxyAPI/plugins',
                        '-v', f'{config}:/CLIProxyAPI/config.yaml',
                        IMAGE, './CLIProxyAPI', '--config', '/CLIProxyAPI/config.yaml')
        try:
            run('docker', 'start', container)
            origin = 'http://' + run('docker', 'port', container, '8317/tcp').splitlines()[0]
            mock = run('docker', 'run', '-d', '--pull=never', '--network', 'container:' + container,
                       '--user', f'{os.getuid()}:{os.getgid()}', MOCK_IMAGE,
                       'python', '-c', MOCK, str(MOCK_PORT), json.dumps(COMPLETION))
            deadline = time.monotonic() + 20
            while 'ready' not in run('docker', 'logs', mock):
                if time.monotonic() > deadline:
                    raise AssertionError('mock upstream did not start')
                time.sleep(0.1)

            def request(path, key=CLIENT_KEY, headers=None, method='GET', data=None):
                req = urllib.request.Request(origin + path, method=method, data=data)
                req.add_header('Authorization', 'Bearer ' + key)
                for name, value in (headers or {}).items():
                    req.add_header(name, value)
                with urllib.request.urlopen(req, timeout=10) as response:
                    return response.status, response.headers, response.read()

            def wait_for_catalog(ready):
                """Poll until the Codex catalog satisfies ready(body).

                CPA answers before it has registered every configured model,
                so the first 200 after a start is not yet the full catalog.
                """
                deadline = time.monotonic() + 45
                while True:
                    try:
                        _, _, body = request(CODEX_QUERY)
                        if ready(body):
                            return body
                    except urllib.error.HTTPError as err:
                        # A rejected key is a bug in this script; never retry it.
                        if err.code in (401, 403):
                            raise
                    except (urllib.error.URLError, ConnectionError):
                        pass
                    if time.monotonic() > deadline:
                        raise AssertionError('CPA did not serve the expected catalog')
                    time.sleep(0.25)

            def management(path, method='GET', body=None):
                data = json.dumps(body).encode() if body is not None else None
                status, _, raw = request('/v0/management' + path, MANAGEMENT_KEY,
                                         {'Content-Type': 'application/json'} if data else None, method, data)
                assert status == 200, path
                return json.loads(raw)

            def snapshot():
                completion = json.dumps({'model': 'or-kimi-k2', 'messages': [{'role': 'user', 'content': 'hi'}]}).encode()
                return {
                    'codex': request(CODEX_QUERY)[2],
                    'openai': request('/v1/models')[2],
                    'claude': request('/v1/models', headers={'Anthropic-Version': '2023-06-01'})[2],
                    'gemini': request('/v1beta/models')[2],
                    'completion': request('/v1/chat/completions', headers={'Content-Type': 'application/json'},
                                          method='POST', data=completion)[2],
                }

            def reconfigure(changes, previous):
                management(f'/plugins/{PLUGIN}/config', 'PATCH', changes)
                deadline = time.monotonic() + 15
                while True:
                    status, headers, body = request(CODEX_QUERY)
                    if body != previous:
                        return status, headers, body
                    if time.monotonic() > deadline:
                        raise AssertionError(f'catalog unchanged after {changes}')
                    time.sleep(0.25)

            fixture = TARGET + ['codex-auto-review', 'claude-fable-5-1', 'cpa-sonnet', 'or-kimi-k2', 'grok-4.7']
            wait_for_catalog(lambda body: set(fixture) <= {slug for slug, _, _ in split_entries(body)})
            logs = subprocess.run(['docker', 'logs', container], capture_output=True, text=True, check=True)
            assert CPA_BANNER in logs.stdout + logs.stderr, 'unexpected CPA runtime version'
            record = {p['id']: p for p in management('/plugins')['plugins']}[PLUGIN]
            assert record['registered'] and record['effective_enabled'], 'plugin inactive'
            assert record['metadata']['version'] == args.version, 'unexpected plugin version'

            # Idle: the plugin is loaded and enabled but has no include list.
            idle = snapshot()
            assert idle == snapshot(), 'CPA responses are not deterministic; comparisons would be meaningless'
            catalog = split_entries(idle['codex'])
            assert json.loads(idle['completion'])['choices'][0]['message']['content'] == COMPLETION['choices'][0]['message']['content']
            print(f'Idle: {len(catalog)} catalog entries ({len(idle["codex"])} bytes), all lists and completion recorded')

            status, headers, body = reconfigure({'include': INCLUDE}, idle['codex'])
            assert status == 200 and headers.get('ETag') is None, 'catalog response validators'
            assert body == expected(idle['codex'], INCLUDE, [], 'remove'), 'remove output is not the byte-exact filtered catalog'
            entries = split_entries(body)
            listed = [slug for slug, visibility, _ in entries if visibility == 'list']
            assert all(allowed(s, INCLUDE, []) for s, _, _ in entries) and not any(s.startswith(UNWANTED) for s in listed)
            assert set(TARGET) <= set(listed), 'target models missing'
            assert ('codex-auto-review', 'hide') in [(s, v) for s, v, _ in entries], 'codex-auto-review must stay hidden'
            active = snapshot()
            assert active['completion'] == idle['completion'], 'completion changed while the filter was active'
            for name in ('openai', 'claude', 'gemini'):
                assert comparable(active[name]) == comparable(idle[name]), name + ' model list changed while the filter was active'
            print(f'Remove (default): {len(entries)} entries ({len(body)} bytes); listed {listed}')
            print('Unchanged while active: OpenAI /v1/models, Claude /v1/models, Gemini /v1beta/models, non-streaming completion')

            status, _, body = reconfigure({'action': 'hide'}, body)
            assert body == expected(idle['codex'], INCLUDE, [], 'hide'), 'hide output is not the byte-exact rewritten catalog'
            entries = split_entries(body)
            assert len(entries) == len(catalog)
            hidden_listed = [s for s, v, _ in entries if v == 'list']
            assert set(hidden_listed) == set(listed), 'hide listed a different set'
            print(f'Hide: {len(entries)} entries kept, listed {hidden_listed}')

            exclude = ['gpt-5.5']
            status, _, body = reconfigure({'action': 'remove', 'exclude': exclude}, body)
            assert body == expected(idle['codex'], INCLUDE, exclude, 'remove'), 'exclude output is not the byte-exact filtered catalog'
            print('Exclude: gpt-5.5 removed as configured')

            # A rejected reconfiguration must never break the catalog: CPA
            # deactivates the plugin and serves its catalog unfiltered, then
            # re-registers it once the configuration is valid again.
            management(f'/plugins/{PLUGIN}/config', 'PATCH', {'action': 'drop'})
            deadline = time.monotonic() + 15
            while {p['id']: p for p in management('/plugins')['plugins']}[PLUGIN]['registered']:
                if time.monotonic() > deadline:
                    raise AssertionError('CPA kept a plugin that rejected its configuration')
                time.sleep(0.25)
            status, _, after = request(CODEX_QUERY)
            assert status == 200 and after == idle['codex'], 'invalid configuration did not fail open'
            status, _, body = reconfigure({'action': 'remove', 'exclude': []}, after)
            assert body == expected(idle['codex'], INCLUDE, [], 'remove'), 'filter did not resume after the configuration was fixed'
            print('Invalid action: CPA deactivates the plugin and serves the catalog unfiltered; fixing it resumes filtering without a restart')

            seen = [line.split(' ', 2)[1:] for line in run('docker', 'logs', mock).splitlines() if line.startswith('request ')]
            assert seen and all(s == ['/v1/chat/completions', 'Bearer synthetic-openrouter-key'] for s in seen), 'unexpected upstream requests'
            # The mock shares the namespace a restart replaces, so it goes first.
            subprocess.run(['docker', 'rm', '-f', mock], check=True, stdout=subprocess.DEVNULL)
            mock = None

            # The persisted configuration must apply from a cold start as well.
            run('docker', 'restart', container)
            origin = 'http://' + run('docker', 'port', container, '8317/tcp').splitlines()[0]
            wait_for_catalog(lambda body: body == expected(idle['codex'], INCLUDE, [], 'remove'))
            print('Restart: persisted configuration filters from startup')
            print(f'PASS: pinned CPA v8.0.4 serves the filtered Codex catalog; every other response is unchanged ({len(seen)} mock completions)')
        except Exception:
            # Synthetic configuration only; logs hold no real credentials.
            logs = subprocess.run(['docker', 'logs', container], capture_output=True, text=True)
            print((logs.stdout + logs.stderr)[-6000:])
            raise
        finally:
            for name in filter(None, (mock, container)):
                subprocess.run(['docker', 'rm', '-f', name], check=True, stdout=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
