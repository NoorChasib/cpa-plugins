#!/usr/bin/env python3
"""Load quota-glance into a disposable pinned CPA container and drive its routes.

Everything else in this plugin's suite calls Handle() in process. That cannot
tell us the shared library actually loads, that CPA accepts the ABI handshake,
that the routes are registered where we think, or that the plugin unloads
without hanging. Those are the failures that make a first install look broken,
and they are the only ones no unit test can reach.

No real accounts and no provider requests: quota-cache is absent and the
snapshot is written directly, which is the whole point of the split.
"""
import gzip
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import re
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
IMAGE = 'eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b'
MANAGEMENT_KEY = 'synthetic-local-smoke-key'
PLUGIN = 'quota-glance'
WEB_TOKEN = 'synthetic-local-smoke-web-token'

# The committed fixture's shape, anchored to the current instant so the document
# is live rather than stale. Offsets match testdata/snapshots/seven-credentials
# so the acceptance values are the ones the design specifies.
#
# SLACK covers the gap between writing the file here and the plugin building its
# document inside the container. Without it a 4500-second countdown is 4499 by
# the time it is rendered, which floors to "1h 14m" — a real property of the
# code, and a flaky thing to assert on. The exact wording is pinned
# deterministically by the unit tests against a frozen clock; this asserts that
# the pipeline is wired, not what a clock reads.
SLACK = 20
CLAUDE = [
    ('siphorchannel@example.com', 'Max', 31, 4500, 24, 93600, 100),
    ('agency@example.com', 'Team', 0, 10800, 10, 158400, 12),
    ('chasibnoor@example.com', 'Max', 0, 14400, 76, 259200, 55),
    ('noor@example.com', 'Team', 0, 18000, 40, 399600, 30),
    ('noorchasib@example.com', 'Max', 0, 21600, 55, 442800, 90),
]


def run(*args):
    return subprocess.check_output(args, text=True).strip()


def stamp(epoch):
    return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(epoch))


def window(key, title, used, reset, now):
    item = {'key': key, 'title': title, 'used_percent': used}
    if reset is not None:
        item['reset_at'] = stamp(now + reset)
    item['observed_at'] = stamp(now - 60)
    return item


def snapshot(now, indices, nudge=0):
    """Key entries by the auth index CPA actually reported.

    CPA derives a stable index that is not necessarily the file name, and
    quota-cache keys its snapshot by whatever host.auth.list returns. Assuming
    the file name here would test a shape the deployment may never produce.
    """
    entries = {}
    for email, plan, session_used, session_reset, weekly_used, weekly_reset, fable in CLAUDE:
        auth = indices[email]
        entries[f'claude:{auth}'] = {
            'provider': 'claude', 'auth_index': auth,
            'used_percent': weekly_used, 'reset_at': stamp(now + weekly_reset),
            'observed_at': stamp(now - 60), 'last_attempt': stamp(now - 60),
            'next_attempt': stamp(now + 600), 'failures': 0, 'plan': plan,
            'windows': [
                window('session', 'Session', session_used + nudge, session_reset + SLACK, now),
                window('weekly', 'Weekly', weekly_used, weekly_reset, now),
                window('weekly_fable', 'Weekly (Fable)', fable, weekly_reset - 60, now),
            ],
        }
    return {'schema': 1, 'written_at': stamp(now - 60), 'next_request': stamp(now + 600),
            'provider_cooldown': {}, 'entries': entries}


def commit(path, document):
    """Write the way quota-cache does: temporary file, then atomic rename."""
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(document))
    os.replace(temporary, path)


def main():
    library = ROOT / 'plugins' / PLUGIN / 'dist' / f'{PLUGIN}.so'
    if not library.exists():
        raise SystemExit(f'build it first: make -C plugins/{PLUGIN} build')

    with tempfile.TemporaryDirectory(prefix='quota-glance-smoke-') as tmp:
        work = Path(tmp)
        plugins, data, auth = work / 'plugins', work / 'data', work / 'auth'
        for directory in (plugins, data, auth):
            directory.mkdir()
        shutil.copy2(library, plugins / f'{PLUGIN}.so')

        # Synthetic credential files so host.auth.list has a roster to report.
        # They carry no tokens: quota-glance only reads identity from them, and
        # nothing in this smoke may make a provider request.
        for email, *_ in CLAUDE:
            (auth / f'claude-{email}.json').write_text(
                json.dumps({'type': 'claude', 'email': email}))

        cache_path = data / 'snapshot.json'
        now = int(time.time())
        # Start with a valid but empty snapshot: the plugin refuses to start
        # without a readable cache-path, and the roster is not known yet.
        commit(cache_path, {'schema': 1, 'written_at': stamp(now), 'next_request': stamp(now + 600),
                            'provider_cooldown': {}, 'entries': {}})

        config = work / 'config.yaml'
        config.write_text(f'''host: 0.0.0.0
port: 8317
auth-dir: /work/auth
remote-management:
  allow-remote: true
  disable-control-panel: true
  secret-key: {MANAGEMENT_KEY}
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    {PLUGIN}:
      enabled: true
      cache-path: /work/data/snapshot.json
      data-dir: /work/data/{PLUGIN}
      web-token: {WEB_TOKEN}
      stale-after: 45m
''')

        container = run('docker', 'create', '--pull=never', '-p', '127.0.0.1::8317',
                        '--user', f'{os.getuid()}:{os.getgid()}',
                        '-v', f'{work}:/work',
                        '-v', f'{plugins}:/CLIProxyAPI/plugins',
                        '-v', f'{config}:/CLIProxyAPI/config.yaml',
                        IMAGE, './CLIProxyAPI', '--config', '/CLIProxyAPI/config.yaml')
        try:
            run('docker', 'start', container)
            checks(container, cache_path, now)
        finally:
            subprocess.run(['docker', 'rm', '-f', container],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def checks(container, cache_path, now):
    def origin():
        return 'http://' + run('docker', 'port', container, '8317/tcp').splitlines()[0]

    base = origin()

    def request(path, token=None, management=False, headers=None, timeout=5):
        req = urllib.request.Request(base + path)
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

    # 1. The library loads and CPA accepts the ABI handshake.
    deadline = time.monotonic() + 60
    registered = None
    while time.monotonic() < deadline:
        try:
            status, body, _ = request('/v0/management/plugins', management=True)
            if status == 200:
                records = {p['id']: p for p in json.loads(body)['plugins']}
                if PLUGIN in records:
                    registered = records[PLUGIN]
                    break
        except Exception:
            pass
        time.sleep(0.2)
    assert registered, 'quota-glance never registered; the library did not load'
    assert registered['registered'] and registered['effective_enabled'], f'inactive: {registered}'
    print(f'  registered    version={registered["metadata"].get("version")}')

    # 2. The unauthenticated shell, which must carry no data.
    status, body, headers = request(f'/v0/resource/plugins/{PLUGIN}/app')
    assert status == 200, f'app route: {status} (CPA home page enabled?)'
    assert 'text/html' in headers.get('Content-Type', ''), headers
    shell = body.decode()
    # Values, not vocabulary. The dashboard necessarily contains the words the
    # summary document is made of — it reads remainingFraction off the response
    # — so what is checked for is a token, an address, or a serialized document.
    # web/web_test.go holds the same line at build time, in more detail.
    for secret in (MANAGEMENT_KEY, 'siphorchannel', '@example.com'):
        assert secret not in shell, f'the public shell leaked {secret!r}'
    for serialized in ('"remainingFraction":', '"credentialId":', '"schemaVersion":'):
        assert serialized not in shell, f'the public shell carries a document ({serialized})'
    # And no second request: the page is one self-contained document, because a
    # third-party fetch would run with the console's key in scope.
    assert 'src="http' not in shell and 'href="http' not in shell, 'the shell fetches something external'
    print(f'  app           200 html, {len(body)} bytes, data-free, self-contained')

    # 3. Two ways to the same document, each gated by whoever owns that tree.
    #    CPA refuses an absent or wrong management key on its own path. The
    #    public fallback path is CPA-authenticated by nobody, so the plugin
    #    checks its own token there and refuses bare.
    for label, key in (('no key', None), ('wrong key', 'nope')):
        status, _, _ = request(f'/v0/management/plugins/{PLUGIN}/summary', token=key)
        assert status in (401, 403), f'management, {label}: {status}'
    for label, tok in (('no token', None), ('wrong token', 'nope')):
        status, body, _ = request(f'/v0/resource/plugins/{PLUGIN}/summary', token=tok)
        assert status == 401, f'fallback, {label}: {status}'
        assert not body, f'fallback {label} returned a body'
    # Both must serve the same document. Compared as parsed documents rather
    # than wire bytes: CPA compresses the management response and does not
    # compress the resource one, so identical documents arrive as different
    # byte counts. The ETag is the plugin's own and must match either way.
    def document(response):
        status, raw, headers = response
        assert status == 200, status
        if (headers.get('Content-Encoding') or '').lower() == 'gzip':
            raw = gzip.decompress(raw)
        return headers.get('Etag'), json.loads(raw)

    cpaTag, cpaDoc = document(request(f'/v0/management/plugins/{PLUGIN}/summary', management=True))
    tokTag, tokDoc = document(request(f'/v0/resource/plugins/{PLUGIN}/summary', token=WEB_TOKEN))
    assert cpaTag == tokTag, f'the two paths disagree on the ETag: {cpaTag} vs {tokTag}'
    assert cpaDoc == tokDoc, 'the two paths served different documents'
    print('  summary       401 bare on both paths; CPA key and web token each serve it')

    # 4. Learn the roster CPA actually reports, then write a snapshot keyed by
    #    it — which is precisely what quota-cache does.
    status, body, _ = request(f'/v0/management/plugins/{PLUGIN}/summary', management=True)
    assert status == 200, status
    catalog = json.loads(body)['credentials']
    assert len(catalog) == len(CLAUDE), f'roster = {catalog}'
    indices = {}
    for credential in catalog:
        local = credential['email'].split('@')[0]
        match = next((email for email, *_ in CLAUDE if email.split('@')[0] == local), None)
        assert match, f'could not identify credential {credential}'
        indices[match] = credential['id']
    shape = 'file names' if any('.json' in i for i in indices.values()) else 'opaque stable ids'
    print(f'  roster        {len(indices)} credentials; CPA auth indices are {shape}')

    commit(cache_path, snapshot(now, indices))
    deadline = time.monotonic() + 75
    document = None
    while time.monotonic() < deadline:
        status, body, headers = request(f'/v0/management/plugins/{PLUGIN}/summary', management=True)
        if status == 200:
            candidate = json.loads(body)
            # Wait for actual rows: a provider group with no rows means the
            # snapshot has not been picked up yet.
            if any(provider['rows'] for provider in candidate['providers']):
                document = candidate
                break
        time.sleep(0.25)
    assert document, 'the snapshot never reached the served document'

    # The acceptance case, over real HTTP, through CPA's own dispatch.
    session = next((row for provider in document['providers'] if provider['id'] == 'claude'
                    for row in provider['rows'] if row['rowId'] == 'session'), None)
    if session is None:
        raise AssertionError('no claude session row: ' + json.dumps(document['credentials'], indent=2))
    assert session['aggregate']['remainingPercent'] == 94, session['aggregate']
    subtext = session['aggregate']['subtext']
    assert re.fullmatch(r'\+6% when siphorchannel resets in 1h 1\dm', subtext), subtext
    assert session['aggregate']['projectedGainPercent'] == 6, session['aggregate']
    assert len(session['entries']) == 5, len(session['entries'])
    weekly = {e['credentialId']: e['resetAtEpoch'] for provider in document['providers']
              for row in provider['rows'] if row['rowId'] == 'weekly' for e in row['entries']}
    order = [weekly[e['credentialId']] for e in session['entries']]
    assert order == sorted(order), 'entries are not in weekly-reset order'
    assert not document['stale'], document['staleReason']
    plans = sorted({c['plan'] for c in document['credentials'] if c['plan']})
    print(f'  summary       200 · Session {session["aggregate"]["remainingPercent"]}% · '
          f'{session["aggregate"]["subtext"]!r} · {len(session["entries"])} entries in weekly order')
    print(f'  identity      emails resolved, plans {plans}')

    # Routing activity, which is the one field that comes from CPA's roster
    # rather than from the snapshot. Every other test in this plugin supplies it
    # from a fake host, so this is the only place the real ABI is asked whether
    # it carries recent_requests at all — a rename upstream would leave every
    # unit test passing and the strip permanently absent.
    activity = [c['activity'] for c in document['credentials']]
    assert all(a is not None for a in activity), (
        'CPA reported no recent_requests for at least one credential: '
        + json.dumps(document['credentials'], indent=2))
    ring = activity[0]
    assert ring['bucketSeconds'] > 0 and ring['windowSeconds'] > 0, ring
    assert len(ring['buckets']) == ring['windowSeconds'] // ring['bucketSeconds'], ring
    # Nothing is proxied through this container, so the ring is real and empty.
    # Empty is not null, and the difference is what the page draws.
    assert all(b['success'] == 0 and b['failed'] == 0 for b in ring['buckets']), ring
    assert ring['success'] == 0 and ring['failed'] == 0, ring
    assert ring['lastRequestAtEpoch'] is None and ring['live'] is False, ring
    print(f'  activity      {len(ring["buckets"])} x {ring["bucketSeconds"]}s ring from the real '
          f'roster; idle reads as empty, not absent')

    # 5. Conditional request.
    etag = headers.get('Etag') or headers.get('ETag')
    assert etag, headers
    status, _, _ = request(f'/v0/management/plugins/{PLUGIN}/summary', management=True,
                           headers={'If-None-Match': etag})
    assert status == 304, status
    print('  summary       304 on If-None-Match')

    # 6. Management diagnostics, behind CPA's own key.
    status, body, _ = request(f'/v0/management/plugins/{PLUGIN}/health', management=True)
    assert status == 200, status
    health = json.loads(body)
    assert health['watcher']['watching'], health['watcher']
    status, windows_body, _ = request(f'/v0/management/plugins/{PLUGIN}/windows', management=True)
    assert status == 200, status
    print(f'  health        200 · watching={health["watcher"]["watching"]} '
          f'dir={health["watcher"]["directory"]} · windows {len(json.loads(windows_body)["windows"])}')

    # 7. The update path, through whatever filesystem the deployment actually
    #    uses. A bind mount is exactly where inotify delivery gets unreliable,
    #    so this also reports whether the event or the stat backstop won.
    before = json.loads(request(f'/v0/management/plugins/{PLUGIN}/health', management=True)[1])
    commit(cache_path, snapshot(now, indices, nudge=10))
    deadline = time.monotonic() + 75
    updated = None
    while time.monotonic() < deadline:
        status, body, _ = request(f'/v0/management/plugins/{PLUGIN}/summary', management=True)
        if status == 200:
            document = json.loads(body)
            row = next(r for p in document['providers'] if p['id'] == 'claude'
                       for r in p['rows'] if r['rowId'] == 'session')
            if row['aggregate']['remainingPercent'] != 94:
                updated = time.monotonic()
                break
        time.sleep(0.25)
    assert updated, 'an atomic-rename update never reached the served document'
    after = json.loads(request(f'/v0/management/plugins/{PLUGIN}/health', management=True)[1])
    via = 'stat backstop' if after['watcher']['backstops'] > before['watcher']['backstops'] else 'fsnotify event'
    print(f'  reload        picked up via {via}')

    # 8. Restart. The plugin must come back without intervention.
    run('docker', 'restart', container)
    base = origin()  # the mapped port can change across a restart
    deadline = time.monotonic() + 60
    ok = False
    while time.monotonic() < deadline:
        try:
            status, _, _ = request(f'/v0/management/plugins/{PLUGIN}/summary', management=True)
            if status == 200:
                ok = True
                break
        except Exception:
            pass
        time.sleep(0.25)
    assert ok, 'the plugin did not serve again after a restart'
    print('  restart       served again without intervention')

    # 9. Unload. A blocking file operation on the watcher goroutine used to wedge
    #    this path, and a wedged shutdown means CPA cannot unload the plugin.
    started = time.monotonic()
    subprocess.run(['docker', 'stop', '-t', '30', container], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    elapsed = time.monotonic() - started
    assert elapsed < 25, f'shutdown took {elapsed:.1f}s; the plugin is blocking CPA unload'
    print(f'  shutdown      clean in {elapsed:.1f}s')
    print('PASS: quota-glance loads, serves, reloads, restarts and unloads inside real CPA')


if __name__ == '__main__':
    main()
