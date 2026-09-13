#!/usr/bin/env python3
"""Load the suite in a disposable pinned CPA v7.2.155 container with no real accounts."""
import json
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
IMAGE = 'eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b'
PLUGINS = {
    'quota-cache': 'dist/quota-cache.so',
    'account-health-pushover': 'dist/account-health-pushover.so',
    'reset-priority': 'reset-priority.so',
    'auto-baseline': 'auto-baseline.so',
    'token-usage': 'dist/token-usage.so',
}

def run(*args):
    return subprocess.check_output(args, text=True).strip()

with tempfile.TemporaryDirectory(prefix='cpa-suite-smoke-') as tmp:
    work = Path(tmp)
    plugins, auth = work/'plugins', work/'auth'
    plugins.mkdir(); auth.mkdir()
    for plugin, library in PLUGINS.items():
        shutil.copy2(ROOT/'plugins'/plugin/library, plugins/(plugin+'.so'))
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
    account-health-pushover:
      enabled: true
      quota-alerts: true
      quota-cache-path: /CLIProxyAPI/plugins/data/quota-cache/snapshot.json
    reset-priority:
      enabled: true
      dry-run: true
      quota-cache-path: /CLIProxyAPI/plugins/data/quota-cache/snapshot.json
    auto-baseline:
      enabled: true
      dry-run: true
      config-path: /CLIProxyAPI/config.yaml
    token-usage:
      enabled: true
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
        ready()
        registered = get('plugins')
        records = {entry['id']:entry for entry in registered['plugins']}
        for plugin in PLUGINS:
            assert records[plugin]['registered'] and records[plugin]['effective_enabled'], 'plugin inactive: '+plugin
        for plugin in ('account-health-pushover','reset-priority','auto-baseline','token-usage'):
            get('plugins/'+plugin+'/status')
        try:
            get('plugins/quota-cache/status',False)
            raise AssertionError('private cache route accepted unauthenticated request')
        except urllib.error.HTTPError as err:
            assert err.code in (401,403)
        for _ in range(25): assert get('plugins/quota-cache/status')['entries']=={}
        assert (plugins/'data/quota-cache/snapshot.json').is_file()
        assert (plugins/'data/token-usage/usage.sqlite').is_file()
        run('docker','restart',container)
        origin = 'http://'+run('docker','port',container,'8317/tcp').splitlines()[0]
        ready()
        for plugin in ('account-health-pushover','reset-priority','auto-baseline','token-usage'):
            get('plugins/'+plugin+'/status')
        logs=run('docker','logs',container)
        assert '7.2.155' in logs, 'unexpected CPA runtime version'
        evidence = {'image':IMAGE,'code_commit':run('git','-C',str(ROOT),'rev-parse','HEAD'),'libraries':{plugin:{'sha256':hashlib.sha256((plugins/(plugin+'.so')).read_bytes()).hexdigest(),'version':records[plugin]['metadata']['version']} for plugin in PLUGINS}}
        (ROOT/'dist').mkdir(exist_ok=True)
        (ROOT/'dist'/'quota-preview-evidence.json').write_text(json.dumps(evidence,indent=2)+'\n')
        print('PASS: pinned CPA v7.2.155 loads all five native plugins; authenticated status routes, cache reads, default-volume SQLite/cache, and restart verified with an empty synthetic roster')
    except Exception:
        # This container uses only synthetic configuration and an empty auth directory.
        print(run('docker','logs',container)[-6000:])
        raise
    finally:
        subprocess.run(['docker','rm','-f',container],check=True,stdout=subprocess.DEVNULL)
