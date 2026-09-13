#!/usr/bin/env python3
"""Real-browser acceptance of the native sidebar using synthetic account data."""
import argparse
import base64
import copy
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import threading

ROOT = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--library', default=str(ROOT / 'dist/quota-cache.so'))
parser.add_argument('--artifacts', type=Path, default=ROOT / 'dist/sidebar-evidence')
parser.add_argument('--serve-only', action='store_true')
args = parser.parse_args()
args.artifacts.mkdir(parents=True, exist_ok=True)
# Exercise native registration/reconfiguration, cooldown/restart, and buffer
# checks first, then reuse those exact bindings for the browser's public shell.
sys.argv = [str(ROOT / 'scripts/native-probe.py'), args.library]
native = runpy.run_path(sys.argv[0])
native['mode']['status'] = 200
tmp = tempfile.TemporaryDirectory(prefix='quota-sidebar-')
api = native['start'](Path(tmp.name) / 'cache/snapshot.json')
base = native['await_result'](api, '')
shell = native['call'](api, 'management.handle', {'Method':'GET','Path':'/v0/resource/plugins/quota-cache/status'})
html = base64.b64decode(shell['Body'])
now = datetime.now(timezone.utc)
iso = lambda d: d.isoformat().replace('+00:00','Z')
state = {'mode':'populated', 'reads':0}
fixture = copy.deepcopy(base)
fixture['provider_cooldown'] = {'claude':iso(now+timedelta(minutes=12))}
fixture['entries'] = {}
fixture['history'] = []
for index,(provider,status) in enumerate((('claude',429),('codex',200),('xai',503))):
    identifier = 'synthetic-' + provider
    entry = {'provider':provider,'auth_index':identifier,'used_percent':42+index*20,
             'observed_at':iso(now-timedelta(minutes=4)), 'reset_at':iso(now+timedelta(days=3)),
             'last_attempt':iso(now-timedelta(minutes=1)), 'next_attempt':iso(now+timedelta(minutes=12)),
             'failures':int(status!=200),'last_error':'' if status==200 else 'provider rate limited' if status==429 else 'quota fetch failed'}
    fixture['entries'][provider+':'+identifier] = entry
    fixture['history'].append({'provider':provider,'auth_index':identifier,'started_at':entry['last_attempt'],
                               'finished_at':entry['last_attempt'],'duration_ms':125,'request_sent':True,'http_status':status,
                               'outcome':'success' if status==200 else 'rate_limited' if status==429 else 'failed','error':entry['last_error']})
fixture['totals'] = {'attempts':3,'requests':3,'successes':1,'failures':2,'rate_limits':1}
fixture['activity'] = {'last_scan':iso(now),'accounts':3}

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *unused):
        pass

    def do_GET(self):
        if self.path == '/v0/resource/plugins/quota-cache/status':
            self.send_response(200)
            for name,values in shell['Headers'].items():
                for value in values: self.send_header(name,value)
            self.end_headers(); self.wfile.write(html)
        elif self.path == '/v0/management/plugins/quota-cache/status':
            state['reads'] += 1
            if self.headers.get('Authorization') != 'Bearer synthetic-sidebar-key' or state['mode']=='unauthorized':
                code,data = 401,{'error':'unauthorized'}
            elif state['mode']=='unavailable':
                code,data = 503,{'error':'cache_unavailable'}
            else:
                code,data = 200,copy.deepcopy(fixture)
                if state['mode']=='empty':
                    data.update(entries={},history=[],provider_cooldown={})
                if state['mode']=='untrusted':
                    data['entries']['codex:synthetic-codex']['last_error'] = '<img src=x onerror="window.injected=true">'
                data['status_reads'] = state['reads']
            raw=json.dumps(data).encode()
            self.send_response(code); self.send_header('Content-Type','application/json'); self.end_headers(); self.wfile.write(raw)
        else:
            self.send_response(404); self.end_headers()

server = ThreadingHTTPServer(('127.0.0.1',0), Handler)
threading.Thread(target=server.serve_forever,daemon=True).start()
origin = f'http://127.0.0.1:{server.server_port}'
session = 'quota-sidebar-' + str(os.getpid())
binary = os.environ.get('QUOTA_CACHE_AGENT_BROWSER','agent-browser')

def browser(*command):
    result = subprocess.run([binary,'--session',session,'--json',*command],text=True,capture_output=True,timeout=45)
    if result.returncode:
        raise AssertionError(result.stdout + result.stderr)
    data = json.loads(result.stdout)
    if data.get('success') is False: raise AssertionError(data)
    return data.get('data',{})

def check(expression):
    browser('wait','--fn',expression)

try:
    if args.serve_only:
        print('Synthetic sidebar preview: '+origin+'/v0/resource/plugins/quota-cache/status',flush=True)
        threading.Event().wait()
    browser('open',origin+'/v0/resource/plugins/quota-cache/status')
    check("document.querySelector('#error').textContent.includes('Sign in to CPA')")
    browser('eval',"localStorage.setItem('cli-proxy-auth',JSON.stringify({state:{managementKey:'synthetic-sidebar-key'}}))")
    browser('reload')
    check("document.querySelectorAll('#accounts tr').length === 3 && document.querySelectorAll('#history tr').length === 3")
    browser('snapshot','-i')
    browser('set','viewport','1280','900')
    browser('screenshot',str(args.artifacts/'light.png'),'--full')
    audit = browser('a11y','--tags','wcag2a,wcag2aa')
    (args.artifacts/'light-a11y.json').write_text(json.dumps(audit,indent=2))
    assert audit.get('violations') == [], audit
    check("document.body.innerText.includes('HTTP 429') && document.body.innerText.includes('backend-api/wham/usage')")
    browser('select','#provider','codex')
    check("document.querySelectorAll('#accounts tr').length === 1 && document.querySelectorAll('#history tr').length === 1")
    browser('select','#provider','all')
    count = native['counts']['http']
    for _ in range(3):
        before = state['reads']
        browser('click','#refresh')
        check("!document.querySelector('#refresh').disabled")
        assert state['reads']>before
    assert native['counts']['http']==count, 'view refresh sent provider requests'
    browser('set','media','dark')
    browser('screenshot',str(args.artifacts/'dark.png'),'--full')
    audit = browser('a11y','--tags','wcag2a,wcag2aa')
    (args.artifacts/'dark-a11y.json').write_text(json.dumps(audit,indent=2))
    assert audit.get('violations') == [], audit
    browser('set','viewport','390','844')
    check('document.documentElement.scrollWidth <= innerWidth')
    browser('screenshot',str(args.artifacts/'mobile.png'),'--full')
    state['mode']='untrusted';browser('click','#refresh')
    check("document.body.innerText.includes('<img src=x') && !window.injected && !document.querySelector('#accounts img')")
    state['mode']='empty';browser('click','#refresh')
    check("!document.querySelector('#accounts-empty').hidden && !document.querySelector('#history-empty').hidden")
    state['mode']='unavailable';browser('click','#refresh')
    check("document.querySelector('#content').hidden && document.querySelector('#error').textContent.includes('503')")
    state['mode']='unauthorized';browser('click','#refresh')
    check("document.querySelector('#content').hidden && document.querySelector('#error').textContent.includes('Sign in to CPA')")
    state['mode']='populated';browser('click','#refresh')
    check("!document.querySelector('#content').hidden && document.querySelector('#error').hidden")
    print('PASS: native sidebar, session handling, provider filters, cooldown/history, empty/error states, safe text rendering, light/dark/mobile, cache-only view refresh')
finally:
    if not args.serve_only:
        subprocess.run([binary,'--session',session,'close'],stdout=subprocess.DEVNULL,check=False,timeout=30)
    server.shutdown()
    native['api'] = api
    api.shutdown()
    tmp.cleanup()
