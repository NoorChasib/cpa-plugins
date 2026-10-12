#!/usr/bin/env python3
"""Load the suite in a disposable pinned CPA v8.0.4 container with no real accounts."""
import gzip
import json
import hashlib
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import tempfile
import time
import urllib.request
import argparse
from plugin_release import load_catalog, released_library

ROOT = Path(__file__).resolve().parents[1]
IMAGE = 'eceasy/cli-proxy-api@sha256:72205ea2dff7e3e3ef23b03de4e17b169ff7449c02b12f2924a3d4d3eee68b7d'
PLUGINS = {
    'quota-cache': 'dist/quota-cache.so',
    'account-health-pushover': 'dist/account-health-pushover.so',
    'reset-priority': 'reset-priority.so',
    'auto-baseline': 'auto-baseline.so',
    'token-usage': 'dist/token-usage.so',
    'quota-glance': 'dist/quota-glance.so',
    'codex-catalog-filter': 'dist/codex-catalog-filter.so',
}

def run(*args):
    return subprocess.check_output(args, text=True).strip()

def utc(epoch):
    return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(epoch))

# Placeholder Claude Console organizations, as quota-cache keys them: "org-"
# and 12 hex digits of the SHA-256 of the organization id. Both are added on
# Quota Cache's page. The second label carries the two characters CPA
# HTML-escapes in a plugin's management JSON below schema 6.
ORG = '00000000-0000-4000-8000-00000000000a'
ORG_B = '00000000-0000-4000-8000-00000000000b'
LABEL_B = "O'Brien & Co"

def account_of(org):
    return 'org-' + hashlib.sha256(org.encode()).hexdigest()[:12]

# Placeholder Claude credentials for the refresh leg. Their files carry no
# token, so each poll fails before it makes a request: nothing reaches a
# provider.
SYNTHETIC = ('refresh-one', 'refresh-two', 'refresh-three')

def seconds_of(duration):
    """A Go duration as the status prints it, such as 10s or 1m0s."""
    match = re.fullmatch(r'(?:(\d+)m)?(\d+)s', duration)
    assert match, duration
    return int(match[1] or 0) * 60 + int(match[2])

def commit(path, document):
    """Write a JSON file by rename, as the plugins do, so a reader never sees
    half of it."""
    temporary = path.with_name('.' + path.name + '-smoke')
    temporary.write_text(json.dumps(document))
    temporary.chmod(0o600)
    temporary.replace(path)

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--candidate', choices=PLUGINS, help='Test one local build with the other published plugins')
args = parser.parse_args()
published = {p['id']: p for p in load_catalog()['plugins']} if args.candidate else {}
# A peer that has never been released has no download yet. Skip it rather than
# fail every other plugin's release until its first publication lands.
LOADED = [plugin for plugin in PLUGINS if not args.candidate or plugin == args.candidate or plugin in published]

with tempfile.TemporaryDirectory(prefix='cpa-suite-smoke-') as tmp:
    work = Path(tmp)
    plugins, auth = work/'plugins', work/'auth'
    plugins.mkdir(); auth.mkdir()
    for plugin in LOADED:
        library = PLUGINS[plugin]
        if args.candidate and plugin != args.candidate:
            (plugins/(plugin+'.so')).write_bytes(released_library(published[plugin]))
        else:
            shutil.copy2(ROOT/'plugins'/plugin/library, plugins/(plugin+'.so'))
    # quota-glance refuses to start if it cannot read a snapshot, so seed a
    # valid empty one — the state a real install reaches as soon as quota-cache
    # has polled once. It is a standalone file under /work rather than the one
    # quota-cache writes: this test is about six plugins coexisting in one
    # process, and quota-cache's own cache-path is reconfigured further down.
    # The data path between the two is covered by quota-glance-smoke.py.
    # /work, not the plugins mount: CPA scans that directory for shared
    # libraries and a stray subdirectory stops every plugin loading.
    snapshot = work/'quota-cache-snapshot.json'
    stamp = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
    snapshot.write_text(json.dumps({'schema': 1, 'written_at': stamp, 'next_request': stamp,
                                    'provider_cooldown': {}, 'entries': {}}))

    # A Claude API credit item as quota-cache 0.1.x read it, which this one
    # accepts and ignores: the organizations are added on its page below,
    # after which quota-cache opens its API meter beside the snapshot. Nothing
    # is ever sent to Anthropic: an organization is listed, never polled, and
    # the meter only counts records CPA delivers, of which this empty roster
    # produces none. The item is configured, and the organizations added,
    # only when the local build is the one loaded, so a peer smoke runs the
    # published quota-cache as it is.
    METERED = not args.candidate or args.candidate == 'quota-cache'
    credits = '''      claude-api-credits:
        - label: smoke
          organization-id: ''' + ORG + '''
          monthly-usd: "200"
          renews: "2026-10-29"
''' if METERED else ''
    config = work/'config.yaml'
    config.write_text('''host: 0.0.0.0
port: 8317
auth-dir: /work/auth
remote-management:
  allow-remote: true
  disable-control-panel: true
  secret-key: synthetic-local-smoke-key
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    quota-cache:
      enabled: true
      request-spacing: 1s
''' + credits + '''    account-health-pushover:
      enabled: true
      quota-alerts: true
      use-quota-cache: true
    reset-priority:
      enabled: true
      dry-run: true
      use-quota-cache: true
    auto-baseline:
      enabled: true
      dry-run: true
      config-path: /CLIProxyAPI/config.yaml
    token-usage:
      enabled: true
    quota-glance:
      enabled: true
      cache-path: /work/quota-cache-snapshot.json
      data-dir: /work/quota-glance
    codex-catalog-filter:
      enabled: true
      models:
        claude-fable-5-1: false
''')
    container = run('docker','create','--pull=never','-p','127.0.0.1::8317',
                    '--user',str(os.getuid())+':'+str(os.getgid()),
                    '-v',str(work)+':/work','-v',str(plugins)+':/CLIProxyAPI/plugins','-v',str(config)+':/CLIProxyAPI/config.yaml',
                    '-e','CPA_PUSHOVER_APP_TOKEN='+'A'*30,'-e','CPA_PUSHOVER_USER_KEY='+'B'*30,
                    IMAGE,'./CLIProxyAPI','--config','/CLIProxyAPI/config.yaml')
    try:
        run('docker','start',container)
        address = run('docker','port',container,'8317/tcp').splitlines()[0]
        origin = 'http://'+address
        def get(path, authenticated=True):
            req = urllib.request.Request(origin+'/v0/management/'+path)
            if authenticated: req.add_header('Authorization','Bearer synthetic-local-smoke-key')
            with urllib.request.urlopen(req,timeout=5) as response:
                return json.load(response)
        def request(path, method='GET', body=None, headers=None):
            # Always with the management key: an answer of any status, as
            # (status, body, headers), never a retry.
            req = urllib.request.Request(origin+'/v0/management/'+path, data=body, method=method,
                                         headers={'Authorization':'Bearer synthetic-local-smoke-key', **(headers or {})})
            try:
                with urllib.request.urlopen(req,timeout=10) as response:
                    status, raw, got = response.status, response.read(), response.headers
            except urllib.error.HTTPError as err:
                status, raw, got = err.code, err.read(), err.headers
            if (got.get('Content-Encoding') or '').lower() == 'gzip':
                raw = gzip.decompress(raw)
            return status, raw, got
        def save(document, content_type='application/json', headers=None):
            status, raw, _ = request('plugins/quota-cache/settings', 'POST', json.dumps(document).encode(),
                                     {'Content-Type': content_type, **(headers or {})})
            return status, json.loads(raw) if raw else None
        def ready():
            deadline=time.monotonic()+45
            while time.monotonic()<deadline:
                try:
                    data=get('plugins/quota-cache/status')
                    if data['schema']==1: return data
                except Exception: time.sleep(.1)
            raise AssertionError('quota-cache did not become available')
        def wait_until(check, seconds, what):
            # check answers a value once the condition holds, else None or raises.
            deadline=time.monotonic()+seconds
            while True:
                try:
                    value=check()
                    if value is not None: return value
                except Exception: pass
                if time.monotonic()>deadline: raise AssertionError(what)
                time.sleep(.5)
        ready()
        registered = get('plugins')
        records = {entry['id']:entry for entry in registered['plugins']}
        # CPA lists no per-plugin capabilities. Its acceptance of the
        # registration (registered, effective_enabled), together with the
        # meter running below, which quota-cache starts only on the path that
        # answers usage_plugin: true, is the evidence that CPA holds
        # quota-cache as a usage plugin.
        metered = METERED and tuple(map(int, records['quota-cache']['metadata']['version'].split('.'))) >= (0, 1, 14)
        for plugin in LOADED:
            assert records[plugin]['registered'] and records[plugin]['effective_enabled'], 'plugin inactive: '+plugin
            assert records[plugin]['metadata']['github_repository'] == 'https://github.com/NoorChasib/cpa-plugins', 'legacy repository metadata: '+plugin
            if args.candidate and plugin != args.candidate:
                assert records[plugin]['metadata']['version'] == published[plugin]['version'], 'published peer version mismatch: '+plugin
        for plugin in ('account-health-pushover','reset-priority','auto-baseline','token-usage'):
            get('plugins/'+plugin+'/status')
        # The deployed config editor rewrites the relative cache default as an
        # absolute path. That spelling change must preserve registration/data.
        if records['quota-cache']['metadata']['version'] != '0.1.0':
            req = urllib.request.Request(origin+'/v0/management/plugins/quota-cache/config',
                data=json.dumps({'cache-path':'/CLIProxyAPI/plugins/data/quota-cache/snapshot.json'}).encode(),
                method='PATCH',headers={'Authorization':'Bearer synthetic-local-smoke-key','Content-Type':'application/json'})
            with urllib.request.urlopen(req,timeout=5) as response: assert response.status == 200
            deadline=time.monotonic()+10
            while True:
                try:
                    rows={p['id']:p for p in get('plugins')['plugins']}
                    assert rows['quota-cache']['registered'] and rows['quota-cache']['effective_enabled']
                    assert get('plugins/quota-cache/status')['cache_path']=='/CLIProxyAPI/plugins/data/quota-cache/snapshot.json'
                    break
                except Exception:
                    if time.monotonic()>deadline: raise
                    time.sleep(.1)
            with urllib.request.urlopen(origin+'/v0/resource/plugins/quota-cache/status',timeout=5) as response:
                shell=response.read()
                assert response.headers.get('Content-Security-Policy')
                assert b'Recent polling activity' in shell and b'synthetic-local-smoke-key' not in shell
        if tuple(map(int, records['quota-cache']['metadata']['version'].split('.'))) >= (0, 1, 2):
            for interval, spacing in [('5m', '2s'), ('15m', '10s'), ('5m', '10s')]:
                req = urllib.request.Request(origin+'/v0/management/plugins/quota-cache/config',
                    data=json.dumps({'poll-interval':interval,'request-spacing':spacing}).encode(),
                    method='PATCH',headers={'Authorization':'Bearer synthetic-local-smoke-key','Content-Type':'application/json'})
                with urllib.request.urlopen(req,timeout=5) as response: assert response.status == 200
                deadline=time.monotonic()+10
                while True:
                    try:
                        rows={p['id']:p for p in get('plugins')['plugins']}
                        assert rows['quota-cache']['registered'] and rows['quota-cache']['effective_enabled']
                        data=get('plugins/quota-cache/status')
                        assert data['poll_interval']==interval+'0s' and data['request_spacing']==spacing
                        assert rows['quota-cache']['menus'], 'schedule edit removed sidebar'
                        break
                    except Exception:
                        if time.monotonic()>deadline: raise
                        time.sleep(.1)
        try:
            get('plugins/quota-cache/status',False)
            raise AssertionError('private cache route accepted unauthenticated request')
        except urllib.error.HTTPError as err:
            assert err.code in (401,403)
        data = plugins/'data/quota-cache'
        credits_file = data/'snapshot.credits.json'
        organizations_file = data/'snapshot.organizations.json'
        def published():
            return json.loads(credits_file.read_text()) if credits_file.is_file() else None
        def accounts_of(state):
            return {a['id']: a for a in ((state or {}).get('api_credits') or {}).get('accounts', [])}
        if metered:
            # claude-api-credits is accepted and ignored: no organizations
            # file is written from it, nothing is listed or metered, and the
            # published state counts the item, which the log says is ignored.
            state = wait_until(lambda: published() if (published() or {}).get('organizations') else None, 60, 'the credit state was never published')
            assert state['organizations'] == {'state': 'missing', 'revision': '', 'config_items_ignored': 1}, state['organizations']
            assert state['api_credits'] is None, state['api_credits']
            assert not organizations_file.exists(), 'the configuration wrote an organizations file'
            status = get('plugins/quota-cache/status')
            assert not [key for key in status['entries'] if key.startswith('anthropic-api:')], status['entries']
            assert 'api_meter' not in status and not (data/'snapshot.meter.json').exists(), 'a meter ran with no organization'
            assert 'quota-cache keeps Claude Console organizations on its page; claude-api-credits in its configuration is ignored' in run('docker','logs',container)
            editor = status['editor']['organizations']
            assert editor['available'] and editor['revision'] == '' and editor['items'] == [], editor
            # Two organizations added on the page: the file appears, and the
            # credit state lists exactly those two before the save answers.
            code, answer = save({'kind': 'organizations', 'baseRevision': '',
                                 'ops': [{'op': 'add', 'label': 'smoke', 'organizationId': ORG}, {'op': 'add', 'label': LABEL_B, 'organizationId': ORG_B}]})
            assert code == 200 and answer['ok'] and not answer['unchanged'], (code, answer)
            assert [o['organization_id'] for o in answer['organizations']] == [ORG, ORG_B], answer
            organizations = json.loads(organizations_file.read_text())
            assert [(o['label'], o['organization_id']) for o in organizations['organizations']] == [('smoke', ORG), (LABEL_B, ORG_B)], organizations
            assert set(organizations) == {'schema', 'revision', 'organizations'}, organizations
            state = published()
            assert set(accounts_of(state)) == {account_of(ORG), account_of(ORG_B)}, accounts_of(state)
            assert state['organizations'] == {'state': 'ok', 'revision': answer['revision'], 'config_items_ignored': 1}, state['organizations']
            # Each organization is listed as an anthropic-api entry keyed by
            # it at the next scan, and never polled.
            credit_keys = {'anthropic-api:'+account_of(org): org for org in (ORG, ORG_B)}
            wait_until(lambda: set(get('plugins/quota-cache/status')['entries']) == set(credit_keys) or None, 15, 'the organizations were not listed')
            for _ in range(25):
                entries = get('plugins/quota-cache/status')['entries']
                assert set(entries) == set(credit_keys), entries
                for credit_key, org in credit_keys.items():
                    assert entries[credit_key]['api_credit']['organization_id'] == org, entries
                    assert entries[credit_key]['last_attempt'].startswith('0001-'), 'credit item was polled'
        else:
            for _ in range(25): assert get('plugins/quota-cache/status')['entries']=={}
        assert (plugins/'data/quota-cache/snapshot.json').is_file()
        assert (plugins/'data/token-usage/usage.sqlite').is_file()
        meter_file = plugins/'data/quota-cache/snapshot.meter.json'
        if metered:
            # The meter writes its own file beside the snapshot at its first
            # flush, a minute after it opened, and the status route carries
            # it from then on. It is counting: the organization is linked,
            # and there is no stop, gap or restart yet.
            meter = wait_until(lambda: get('plugins/quota-cache/status').get('api_meter'), 90, 'meter file was not written')
            assert meter_file.is_file()
            assert 'stopped_at' not in meter and meter['gaps'] == [] and meter.get('restarts', 0) == 0, meter
            assert set(meter['organizations']) == {ORG, ORG_B}, meter
            assert 'quota-cache API meter counting Claude API-key traffic' in run('docker','logs',container)
        else:
            assert not meter_file.exists()
        # Quota Cache 0.2.0 keeps the operator's API credit settings and Console
        # organizations, and publishes each organization's credit. Only a
        # loaded 0.2.0 or newer is asked, so a peer smoke with an older
        # published quota-cache still passes. Every request carries the key, so
        # none of these counts toward CPA's management-key ban.
        publishing = metered and tuple(map(int, records['quota-cache']['metadata']['version'].split('.'))) >= (0, 2, 0)
        if publishing:
            # Nothing is saved on the page yet: the settings are empty and
            # editable, with no settings file until the first save, and no
            # organization has a credit or a refill date, since none comes
            # from the configuration.
            state = published()
            assert state['settings'] == {'state': 'ok', 'editable': True, 'revision': ''}, state['settings']
            assert not (data/'snapshot.settings.json').exists(), 'a settings file was written before any save'
            first = accounts_of(state)[account_of(ORG)]
            assert (first['monthlyCreditSource'], first['renewsSource'], first['settings']['monthlyUsd'], first['settings']['configMonthlyUsd']) == ('none', 'none', '', ''), first
            assert state['renewals'] == {}, state['renewals']
            status = get('plugins/quota-cache/status')
            logs = run('docker','logs',container)
            assert 'quota-cache organizations changed' in logs
            assert 'quota-cache moved its Claude Console organizations' not in logs, 'something moved from the configuration'

            # A save through CPA's management route, published before
            # the answer; and the route's refusals that CPA must pass through.
            second = accounts_of(status['credit_state'])[account_of(ORG_B)]['settings']
            batch = {'kind': 'apiCredits', 'items': [{'id': account_of(ORG_B), 'baseRevision': second['revision'],
                                                      'monthlyUsd': '175', 'renews': second['renews'] or None, 'reading': None}]}
            code, answer = save(batch, 'text/plain')
            assert (code, answer) == (415, {'error': 'unsupported_media_type'}), (code, answer)
            code, answer = save(batch, headers={'Sec-Fetch-Site': 'cross-site'})
            assert (code, answer) == (403, {'error': 'cross_site'}), (code, answer)
            code, answer = save({'kind': 'importLegacy', 'baseRevision': ''})
            assert (code, answer) == (400, {'error': 'invalid_request'}), (code, answer)
            code, answer = save(batch)
            assert code == 200 and answer['ok'] and answer['settings'][account_of(ORG_B)]['monthlyUsd'] == '175', (code, answer)
            saved = accounts_of(published())[account_of(ORG_B)]
            assert (saved['monthlyCreditText'], saved['monthlyCreditSource']) == ('$175.00', 'dashboard'), saved
            code, raw, _ = request('plugins/quota-cache/settings')
            assert (code, raw) == (404, b''), f'GET on the save route: {code} {raw[:200]!r} (expected CPA\'s empty 404)'

            # Quota Cache registers schema 6 with a CPA that offers it,
            # so CPA passes its management JSON through unescaped: a label
            # with an apostrophe and an ampersand, and every sentence with an
            # apostrophe, arrive exactly as published.
            code, raw, _ = request('plugins/quota-cache/status')
            assert code == 200, code
            for entity in (b'&#39;', b'&amp;', b'&#34;', b'&lt;', b'&gt;'):
                assert entity not in raw, f'the status arrived HTML-escaped ({entity!r})'
            status = json.loads(raw)
            assert status['strings_escaped'] is False, status['strings_escaped']
            listed = accounts_of(status['credit_state'])
            assert listed[account_of(ORG_B)]['label'] == LABEL_B, listed[account_of(ORG_B)]
            assert status['entries']['anthropic-api:'+account_of(ORG_B)]['api_credit']['label'] == LABEL_B
            assert LABEL_B in [o['label'] for o in status['editor']['organizations']['items']]
            sentences = {id: a['issue'] for id, a in listed.items() if "'" in a['issue']}
            written = accounts_of(published())
            assert sentences, 'no published sentence carries an apostrophe: ' + json.dumps({id: a['issue'] for id, a in listed.items()})
            for id, issue in sentences.items():
                assert issue == written[id]['issue'], (issue, written[id]['issue'])
            print(f'  credits       claude-api-credits ignored and two organizations added on the page; a save published before its answer; '
                  f'415, 403, 400 for an unknown kind and CPA\'s empty 404 as specified; '
                  f'{LABEL_B!r} and {len(sentences)} sentence(s) arrived unescaped')
        # Quota Cache 0.2.0 refreshes every account after Quota Glance spends a
        # banked reset. Placeholder Claude credentials give it accounts to
        # poll: CPA picks the files up from its auth directory, and Quota
        # Cache's next scan lists them and polls each once. Every poll fails
        # before a request, since no file holds a token.
        refreshing = tuple(map(int, records['quota-cache']['metadata']['version'].split('.'))) >= (0, 2, 0)
        snapshot_file = plugins/'data/quota-cache/snapshot.json'
        if refreshing:
            for name in SYNTHETIC:
                (auth/f'claude-{name}.json').write_text(json.dumps({'type': 'claude', 'email': f'{name}@example.com'}))
            def synthetic_polled():
                entries = get('plugins/quota-cache/status')['entries']
                polled = {key: e for key, e in entries.items() if e['provider'] == 'claude' and not e['last_attempt'].startswith('0001-')}
                return polled if len(polled) == len(SYNTHETIC) else None
            synthetic = sorted(wait_until(synthetic_polled, 120, 'the placeholder credentials were not polled'))
            # Restarted with each placeholder as if its last poll had
            # answered: no failure backoff, and not due for an hour. Only
            # then can every other account follow the one a reset was spent
            # on, since a credential backing off after a failure keeps its
            # backoff.
            run('docker','stop',container)
            seeded = json.loads(snapshot_file.read_text())
            for key in synthetic:
                entry = seeded['entries'][key]
                entry.pop('last_error', None)
                entry.update(failures=0, next_attempt=utc(int(time.time()) + 3600))
            commit(snapshot_file, seeded)
            run('docker','start',container)
        else:
            run('docker','restart',container)
        origin = 'http://'+run('docker','port',container,'8317/tcp').splitlines()[0]
        ready()
        for plugin in ('account-health-pushover','reset-priority','auto-baseline','token-usage'):
            get('plugins/'+plugin+'/status')
        logs=run('docker','logs',container)
        assert '8.0.4' in logs, 'unexpected CPA runtime version'
        if metered:
            # CPA shuts its plugins down on a graceful stop, and the meter's
            # last save recorded it. The restarted meter reopened from that
            # file but writes it only at its own first flush, so the route
            # still shows the stop for the first minute.
            meter = get('plugins/quota-cache/status')['api_meter']
            assert meter['stop_reason'] == 'shutdown' and meter['stopped_at'], meter
            stopped_at = meter['stopped_at']
        if refreshing:
            # A placeholder request, as Quota Glance writes it after a banked
            # reset, for the placeholder last in the usual order. It is polled
            # within two spacings, and then the other two follow, one per
            # spacing, in the usual order.
            status = get('plugins/quota-cache/status')
            assert status['refresh'] == {'requested_at': None, 'remaining': 0}, status['refresh']
            assert set(synthetic) <= set(status['entries']), status['entries'].keys()
            for key in synthetic:
                entry = status['entries'][key]
                assert not entry.get('last_error') and entry['next_attempt'] > utc(int(time.time()) + 1800), entry
            spacing = seconds_of(status['request_spacing'])
            before = {key: status['entries'][key]['last_attempt'] for key in synthetic}
            target, others = synthetic[-1], synthetic[:-1]
            at = utc(int(time.time()))
            commit(data/'snapshot.refresh.json', {'schema': 1, 'requests': [{
                'id': secrets.token_hex(8), 'provider': 'claude', 'auth_index': status['entries'][target]['auth_index'],
                'reason': 'reset', 'at': at}]})
            written = time.monotonic()
            def moved(*keys):
                entries = get('plugins/quota-cache/status')['entries']
                return entries if all(entries[key]['last_attempt'] != before[key] for key in keys) else None
            leading = wait_until(lambda: moved(target), 2 * spacing + 2, f'the requested credential was not polled within two spacings ({2 * spacing}s)')
            waited = time.monotonic() - written
            assert all(leading[key]['last_attempt'] == before[key] for key in others), 'another credential was polled before the requested one'
            during = get('plugins/quota-cache/status')['refresh']
            assert during['requested_at'] == at and during['remaining'] >= 1, during
            wait_until(lambda: moved(*others), (len(others) + 1) * spacing + 2, 'the other credentials did not follow')
            status = get('plugins/quota-cache/status')
            polls = [poll for poll in status['history'] if poll['provider'] == 'claude'][-len(synthetic):]
            assert [poll['provider'] + ':' + poll['auth_index'] for poll in polls] == [target, *others], polls
            assert not any(poll['request_sent'] for poll in status['history'] if poll['provider'] == 'claude'), 'a placeholder poll sent a request'
            assert status['refresh'] == {'requested_at': at, 'remaining': 0}, status['refresh']
            assert 'quota-cache refreshing every account after a banked reset' in run('docker','logs',container)
            # The rest of the run is on the empty roster again.
            for name in SYNTHETIC:
                (auth/f'claude-{name}.json').unlink()
            wait_until(lambda: not any(e['provider'] == 'claude' for e in get('plugins/quota-cache/status')['entries'].values()) or None,
                       60, 'the placeholder credentials were not dropped')
            print(f'  refresh       the requested credential polled {waited:.1f}s after the request ({spacing}s spacing), '
                  f'then the other {len(others)} in order; no request sent')
        evidence = {'image':IMAGE,'code_commit':run('git','-C',str(ROOT),'rev-parse','HEAD'),'libraries':{plugin:{'sha256':hashlib.sha256((plugins/(plugin+'.so')).read_bytes()).hexdigest(),'version':records[plugin]['metadata']['version']} for plugin in LOADED}}
        # Prove the other four register without the cache library or snapshot.
        # Test both opted-in waiting and explicitly standalone configuration.
        run('docker','stop',container)
        if metered:
            # The graceful stop made the restarted meter save: the earlier
            # stop is now a gap, from when it was recorded to the restart,
            # and this stop is recorded in turn.
            meter = json.loads(meter_file.read_text())
            assert meter['restarts'] == 1 and meter['stop_reason'] == 'shutdown' and meter['stopped_at'], meter
            assert len(meter['gaps']) == 1 and meter['gaps'][0]['reason'] == 'shutdown' and meter['gaps'][0]['from'] == stopped_at, meter
            assert meter['gaps'][0]['to'] <= meter['stopped_at'], meter
        if publishing:
            # The publisher's last write came after the meter's final save,
            # so readers see the stop while Quota Cache is down.
            stopped = published()['api_credits']['meter']
            assert stopped['stoppedAtEpoch'] and stopped['stopReason'] == 'shutdown', stopped
        (plugins/'quota-cache.so').unlink()
        shutil.rmtree(plugins/'data/quota-cache')
        for mode in (True, False):
            config.write_text(config.read_text().replace('use-quota-cache: true', 'use-quota-cache: '+str(mode).lower()))
            run('docker','start',container)
            origin = 'http://'+run('docker','port',container,'8317/tcp').splitlines()[0]
            deadline=time.monotonic()+45
            while True:
                try:
                    absent = {p['id']:p for p in get('plugins')['plugins']}
                    assert not absent.get('quota-cache', {}).get('registered', False), 'missing library unexpectedly registered'
                    for plugin in ('account-health-pushover','reset-priority','auto-baseline','token-usage'):
                        assert absent[plugin]['registered'] and absent[plugin]['effective_enabled']
                        get('plugins/'+plugin+'/status')
                    break
                except Exception:
                    if time.monotonic()>deadline: raise
                    time.sleep(.1)
            run('docker','stop',container)
        evidence['optional_cache_modes'] = ['missing-cache-wait', 'standalone']
        if args.candidate:
            evidence['candidate'] = args.candidate
        (ROOT/'dist').mkdir(exist_ok=True)
        (ROOT/'dist'/'quota-preview-evidence.json').write_text(json.dumps(evidence,indent=2)+'\n')
        evidence['api_meter'] = 'file, shutdown stop and gap verified' if metered else 'not configured'
        print('PASS: pinned CPA v8.0.4 loads every native plugin; authenticated status routes, cache reads, default-volume SQLite/cache, restart' + (', the API meter file with its shutdown gap' if metered else '') + (', organizations, save route and published credit state' if publishing else '') + ' verified with an empty synthetic roster' + ('; the refresh after a banked reset with three tokenless placeholder credentials' if refreshing else ''))
    except Exception:
        # This container uses only synthetic configuration and an empty auth directory.
        print(run('docker','logs',container)[-6000:])
        raise
    finally:
        subprocess.run(['docker','rm','-f',container],check=True,stdout=subprocess.DEVNULL)
