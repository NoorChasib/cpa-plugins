#!/usr/bin/env python3
"""Load reset-priority into a disposable pinned CPA container and drive its API
credit router through CPA's scheduler.

Every router test calls Pick in process. Only real CPA shows that the plugin
config route declares and withdraws the scheduler capability, that CPA then
asks the plugin for each credential attempt, and that no answer turns into a
scheduler error, which CPA would hand the client as the request's failure.

No real accounts and no provider requests: Quota Cache is absent, and the
snapshot, meter and credit state it would write are written here. The credits
keys point at a closed port inside the container, so every attempt fails at
connect and CPA fails over.
"""
import gzip
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
IMAGE = 'eceasy/cli-proxy-api@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d'
MANAGEMENT_KEY = 'synthetic-local-smoke-key'
PLUGIN = 'reset-priority'

# Two fake Claude API keys in one v8 api-keys.claude group whose base URL is a
# closed port inside the container. The model is listed on the group, so it
# does not depend on the catalog CPA fetches at startup. The client key is
# CPA's own access key for /v1, and the organization is a placeholder.
CLIENT_KEY = 'synthetic-local-smoke-client-key'
CREDITS_PREFIX = 'smokecredits'
CREDITS_MODEL = 'claude-haiku-4-5-20251001'
CREDITS_ORG = '00000000-0000-4000-8000-00000000000a'
ACCOUNT = 'org-' + hashlib.sha256(CREDITS_ORG.encode()).hexdigest()[:12]

# Quota Cache publishes at least once a minute while it runs, and the router
# stops trusting a state older than three minutes, so the stand-in does too.
PUBLISH_EVERY = 60


def run(*args):
    return subprocess.check_output(args, text=True).strip()


def stamp(epoch):
    return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(epoch))


def commit(path, document):
    """Write the way quota-cache does: temporary file, then atomic rename."""
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(document))
    os.replace(temporary, path)


def header(headers, name):
    """One response header, by name in any case."""
    return next((value for key, value in headers.items() if key.lower() == name.lower()), None)


def answer_of(body, headers):
    """A JSON answer, or None when the body is not one."""
    if (header(headers, 'Content-Encoding') or '').lower() == 'gzip':
        body = gzip.decompress(body)
    try:
        return json.loads(body)
    except ValueError:
        return None


def snapshot(now):
    """Quota Cache's snapshot with the one organization on its page, keyed
    the way it keys it. The router does not read it; Reset Priority's
    OAuth engine would, and stays standalone here."""
    zero = '0001-01-01T00:00:00Z'
    return {'schema': 1, 'written_at': stamp(now), 'next_request': stamp(now + 600), 'provider_cooldown': {},
            'entries': {f'anthropic-api:{ACCOUNT}': {
                'provider': 'anthropic-api', 'auth_index': ACCOUNT, 'used_percent': 0,
                'reset_at': zero, 'observed_at': zero, 'last_attempt': zero, 'next_attempt': zero, 'failures': 0,
                'api_credit': {'label': 'smoke', 'position': 0, 'organization_id': CREDITS_ORG}}}}


def meter(started, now):
    """The meter as Quota Cache 0.2.0 saves it before any credits key has
    served a request: recording auth IDs since it started, nothing learned
    yet, and no stop."""
    return {'schema': 1, 'since': stamp(started), 'started_at': stamp(started), 'flushed_at': stamp(now),
            'received': 0, 'counted': 0, 'gaps': [], 'organizations': {CREDITS_ORG: {}}, 'unlinked': [],
            'auths': {}, 'auth_ids_since': stamp(started), 'auth_ids': {}}


def credit_state(now):
    """Quota Cache's published credit state: one linked Console organization,
    ok, with credit left, computed at now and good until its refill."""
    day = now - now % 86400
    renews = day + 10 * 86400
    return {
        'schema': 1, 'quota_cache_version': '0.2.0', 'computed_at': stamp(now), 'valid_until': stamp(renews),
        'inputs': {'snapshot_written_at': stamp(now), 'meter_flushed_at': stamp(now), 'meter_error': '',
                   'settings_revision': ''},
        'settings': {'state': 'ok', 'editable': True, 'revision': ''},
        'api_credits': {
            'title': 'Monthly API Credit', 'currency': 'USD',
            'meter': {'sinceEpoch': day, 'updatedAtEpoch': now, 'stale': False, 'gaps': []},
            'editing': {'available': True, 'reason': ''},
            'pool': {'hasEstimate': True, 'left': 37.5, 'leftText': '$37.50', 'accountCount': 1, 'countedCount': 1},
            'accounts': [{
                'id': ACCOUNT, 'label': 'smoke', 'order': 0, 'organizationId': CREDITS_ORG, 'state': 'ok',
                'counted': True, 'hasEstimate': True, 'monthlyCredit': 50, 'monthlyCreditText': '$50.00',
                'left': 37.5, 'leftText': '$37.50', 'estimateLeftText': '$37.50', 'level': 'ok',
                'renewsAtEpoch': renews, 'renewsInSeconds': renews - now, 'dataIssues': [], 'issue': '',
            }],
            'unlinked': [], 'orphans': [],
        },
        'renewals': {},
        'organizations': {'state': 'ok', 'revision': '1', 'config_items_ignored': 0},
    }


class Publisher:
    """Stands in for Quota Cache's publisher: writes the meter and the credit
    state now, and again every PUBLISH_EVERY seconds until stopped."""

    def __init__(self, data):
        self.data, self.started, self.stopped = data, int(time.time()), threading.Event()
        self.publish()
        self.thread = threading.Thread(target=self.loop, daemon=True)
        self.thread.start()

    def publish(self):
        now = int(time.time())
        commit(self.data / 'snapshot.meter.json', meter(self.started, now))
        commit(self.data / 'snapshot.credits.json', credit_state(now))

    def loop(self):
        while not self.stopped.wait(PUBLISH_EVERY):
            self.publish()

    def stop(self):
        self.stopped.set()
        self.thread.join()


def main():
    library = ROOT / 'plugins' / PLUGIN / f'{PLUGIN}.so'
    if not library.exists():
        raise SystemExit(f'build it first: make -C plugins/{PLUGIN} build')

    with tempfile.TemporaryDirectory(prefix='reset-priority-smoke-') as tmp:
        work = Path(tmp)
        plugins, data, auth = work / 'plugins', work / 'data', work / 'auth'
        for directory in (plugins, data, auth):
            directory.mkdir()
        shutil.copy2(library, plugins / f'{PLUGIN}.so')
        commit(data / 'snapshot.json', snapshot(int(time.time())))
        publisher = Publisher(data)

        # use-quota-cache: false keeps the OAuth engine standalone, and the
        # router still reads beside quota-cache-path: the two are independent.
        config = work / 'config.yaml'
        config.write_text(f'''host: 0.0.0.0
port: 8317
auth-dir: /work/auth
remote-management:
  allow-remote: true
  disable-control-panel: true
  secret-key: {MANAGEMENT_KEY}
access:
  api-keys:
    - {CLIENT_KEY}
api-keys:
  claude:
    - name: {CREDITS_PREFIX}
      prefix: {CREDITS_PREFIX}
      base-url: http://127.0.0.1:9
      models:
        - name: {CREDITS_MODEL}
      keys:
        - api-key: synthetic-smoke-credits-key-1
        - api-key: synthetic-smoke-credits-key-2
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    {PLUGIN}:
      enabled: true
      dry-run: true
      use-quota-cache: false
      quota-cache-path: /work/data/snapshot.json
''')

        container = run('docker', 'create', '--pull=never', '-p', '127.0.0.1::8317',
                        '--user', f'{os.getuid()}:{os.getgid()}',
                        '-v', f'{work}:/work',
                        '-v', f'{plugins}:/CLIProxyAPI/plugins',
                        '-v', f'{config}:/CLIProxyAPI/config.yaml',
                        IMAGE, './CLIProxyAPI', '--config', '/CLIProxyAPI/config.yaml')
        try:
            run('docker', 'start', container)
            checks(container)
        except Exception:
            # Synthetic configuration and an empty auth directory only.
            print(run('docker', 'logs', container)[-6000:])
            raise
        finally:
            publisher.stop()
            subprocess.run(['docker', 'rm', '-f', container],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def checks(container):
    base = 'http://' + run('docker', 'port', container, '8317/tcp').splitlines()[0]

    def request(path, token=None, management=False, headers=None, timeout=5, method=None, body=None):
        req = urllib.request.Request(base + path, data=body, method=method)
        if management:
            req.add_header('Authorization', 'Bearer ' + MANAGEMENT_KEY)
        elif token:
            req.add_header('Authorization', 'Bearer ' + token)
        for name, value in (headers or {}).items():
            req.add_header(name, value)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as response:
                return response.status, response.read(), dict(response.headers)
        except urllib.error.HTTPError as error:
            return error.code, error.read(), dict(error.headers)

    json_header = {'Content-Type': 'application/json'}

    # 1. The library loads, CPA accepts the ABI handshake, and the panel
    #    offers the router's switch.
    deadline = time.monotonic() + 60
    registered = None
    while time.monotonic() < deadline:
        try:
            status, body, headers = request('/v0/management/plugins', management=True)
            if status == 200:
                records = {p['id']: p for p in answer_of(body, headers)['plugins']}
                if PLUGIN in records:
                    registered = records[PLUGIN]
                    break
        except Exception:
            pass
        time.sleep(0.2)
    assert registered, 'reset-priority never registered; the library did not load'
    assert registered['registered'] and registered['effective_enabled'], f'inactive: {registered}'
    fields = {f['name']: f['type'] for f in registered.get('config_fields') or []}
    assert fields.get('route-api-credits') == 'boolean', fields
    print(f'  registered    version={registered["metadata"].get("version")}; route-api-credits is a panel toggle')

    def status_document():
        status, raw, headers = request(f'/v0/management/plugins/{PLUGIN}/status', management=True)
        assert status == 200, f'status: {status}'
        return answer_of(raw, headers)

    def router_status():
        return status_document()['router']

    def wait_router(ready, failure):
        deadline = time.monotonic() + 75
        state = None
        while time.monotonic() < deadline:
            try:
                state = router_status()
                if ready(state):
                    return state
            except AssertionError:
                pass  # 503 until the first register lands
            time.sleep(0.25)
        raise AssertionError(f'{failure}: {json.dumps(state)}')

    def route_api_credits(on):
        status, body, _ = request(f'/v0/management/plugins/{PLUGIN}/config', management=True, method='PATCH',
                                  body=json.dumps({'route-api-credits': on}).encode(), headers=json_header)
        assert status == 200, f'plugin config PATCH: {status} {body[:200]!r}'

    message = json.dumps({'model': f'{CREDITS_PREFIX}/{CREDITS_MODEL}', 'max_tokens': 1,
                          'messages': [{'role': 'user', 'content': 'smoke'}]}).encode()

    def send():
        status, raw, headers = request('/v1/messages', token=CLIENT_KEY, method='POST', body=message,
                                       headers=json_header, timeout=60)
        if (header(headers, 'Content-Encoding') or '').lower() == 'gzip':
            raw = gzip.decompress(raw)
        assert status >= 400, f'a request to a closed port succeeded: {status}'
        # The failure is the attempt's own: CPA's executor reached the closed
        # port. A scheduler error fails the request before any attempt, and
        # names the scheduler or the plugin.
        text = raw.decode(errors='replace').lower()
        assert 'scheduler' not in text and 'plugin' not in text, f'scheduler error: {status} {raw[:300]!r}'
        assert '127.0.0.1:9' in text, f'not the closed port\'s failure: {status} {raw[:300]!r}'
        return status

    # 2. Off, nothing is declared, and the table is still built from the
    #    published state, so the status says whether turning routing on would
    #    route: the operator's check before turning it on.
    state = wait_router(lambda s: s['table_reason'] == '' and not s['enabled'],
                        'the routing table never became ready with routing off')
    assert (state['declared'], state['reason'], state['active']) == (False, 'disabled', False), state
    print('  status        router off and undeclared; table ready from the published state')

    # 3. On through CPA's plugin-config route, as the panel saves it. declared
    #    is Reset Priority's own intent. CPA stores its new snapshot after the
    #    reload that follows the PATCH, so neither field proves CPA is asking
    #    yet, and the requests below are resent until it is.
    route_api_credits(True)
    wait_router(lambda s: s['declared'] and s['active'] and s['reason'] == '',
                'routing never became active after the config route turned it on')

    # 4. CPA asks the router for each attempt at the credits keys. Nothing is
    #    mapped yet, so the router explores an unproven key, and the client
    #    sees the closed port's failure, never a scheduler error.
    for _ in range(10):
        refused = send()
        state = router_status()
        if state['counters']['explored'] >= 1:
            break
        time.sleep(1)
    counters = state['counters']
    assert counters['explored'] >= 1, f'CPA never had the router explore a key (last {refused}): {counters}'
    document = json.dumps(status_document())
    status, page, _ = request(f'/v0/management/plugins/{PLUGIN}/status/html', management=True)
    assert status == 200 and b'Claude API credit routing' in page, f'status page: {status}'
    for shown, what in ((document, 'the status'), (page.decode(errors='replace'), 'the status page')):
        assert 'claude:apikey:' not in shown, f'{what} carries an auth ID'
        assert CREDITS_ORG not in shown, f'{what} carries an organization id'
    # The failed attempts left both keys on offer, so CPA asks again for the
    # next request. Without that, the check below that it stops asking once
    # routing is off would pass for the wrong reason.
    send()
    asked = router_status()['counters']['picks']
    assert asked > counters['picks'], f'CPA did not ask again while routing was on: {asked}'
    print(f'  routing       on through the config route; {counters["picks"]} picks explored '
          f'{counters["explored"]} unproven keys and the client saw {refused}, not a scheduler error')

    # 5. Off again, CPA stops asking.
    route_api_credits(False)
    wait_router(lambda s: not s['declared'], 'routing was never withdrawn after the config route turned it off')
    unchanged = 0
    for _ in range(10):
        before = router_status()['counters']['picks']
        send()
        unchanged = unchanged + 1 if router_status()['counters']['picks'] == before else 0
        if unchanged == 2:
            break
        time.sleep(1)
    assert unchanged == 2, 'CPA kept asking the router to pick after the config route turned routing off'
    logs = run('docker', 'logs', container)
    for line in ('reset-priority API credit routing on', 'reset-priority API credit router active',
                 'reset-priority API credit routing off'):
        assert line in logs, f'missing log line: {line}'
    print('  routing       off through the config route; CPA stopped asking')

    # 6. Unload. The router loop is stopped and drained before the bridge, so
    #    a wedged loop would hold CPA's shutdown here.
    started = time.monotonic()
    subprocess.run(['docker', 'stop', '-t', '30', container], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    elapsed = time.monotonic() - started
    assert elapsed < 25, f'shutdown took {elapsed:.1f}s; the plugin is blocking CPA unload'
    print(f'  shutdown      clean in {elapsed:.1f}s')
    print('PASS: reset-priority routes Claude API credits through real CPA, fails open and unloads cleanly')


if __name__ == '__main__':
    main()
