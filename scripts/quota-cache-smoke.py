#!/usr/bin/env python3
"""Load the suite in a disposable pinned CPA v8.0.4 container with no real accounts."""
import json
import hashlib
import os
from pathlib import Path
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

    # One Claude API credit item with a fake organization, so quota-cache
    # registers as a CPA usage plugin and opens its API meter beside the
    # snapshot. Nothing is ever sent to Anthropic: an item is listed, never
    # polled, and the meter only counts records CPA delivers, of which this
    # empty roster produces none. The published quota-cache (0.1.12) rejects
    # claude-api-credits outright, so the item is configured only when the
    # local build is the one loaded.
    ORG = '00000000-0000-4000-8000-00000000000a'
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
        if metered:
            # The item is listed as an anthropic-api entry keyed by its
            # organization, and never polled.
            credit_key = 'anthropic-api:org-' + hashlib.sha256(ORG.encode()).hexdigest()[:12]
            wait_until(lambda: get('plugins/quota-cache/status')['entries'].get(credit_key), 15, 'credit item was not listed')
            for _ in range(25):
                entries = get('plugins/quota-cache/status')['entries']
                assert set(entries) == {credit_key}, entries
                assert entries[credit_key]['api_credit']['organization_id'] == ORG, entries
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
            assert set(meter['organizations']) == {ORG}, meter
            assert 'quota-cache API meter counting Claude API-key traffic' in run('docker','logs',container)
        else:
            assert not meter_file.exists()
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
        print('PASS: pinned CPA v8.0.4 loads every native plugin; authenticated status routes, cache reads, default-volume SQLite/cache, restart' + (', and the API meter file with its shutdown gap' if metered else '') + ' verified with an empty synthetic roster')
    except Exception:
        # This container uses only synthetic configuration and an empty auth directory.
        print(run('docker','logs',container)[-6000:])
        raise
    finally:
        subprocess.run(['docker','rm','-f',container],check=True,stdout=subprocess.DEVNULL)
