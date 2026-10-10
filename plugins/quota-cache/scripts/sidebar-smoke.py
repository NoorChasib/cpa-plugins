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
import time

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
state = {'mode':'populated', 'reads':0, 'keyless':0}
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
    entry['quota'] = {'schema':1, 'observed_at':entry['observed_at'], 'windows':{'five_hour':{'used_percent':0,'resets_at':entry['reset_at']}}, 'limits':{'regular':{'allowed':False}}, 'balances':{'credits':{'unit':'credits','remaining':'0.00000000000000000001','unlimited':False}}}
    fixture['entries'][provider+':'+identifier] = entry
    fixture['history'].append({'provider':provider,'auth_index':identifier,'started_at':entry['last_attempt'],
                               'finished_at':entry['last_attempt'],'duration_ms':125,'request_sent':True,'http_status':status,
                               'outcome':'success' if status==200 else 'rate_limited' if status==429 else 'failed','error':entry['last_error']})
fixture['totals'] = {'attempts':3,'requests':3,'successes':1,'failures':2,'rate_limits':1}
# Claude API credit entries and the meter file, served only in the 'credits'
# mode: one metered organization and one misconfigured item, which is listed
# but never counted. Neither is ever polled. The organization ids are the
# spec's fakes.
midnight = now.replace(hour=0, minute=0, second=0, microsecond=0)
this_hour = now.replace(minute=0, second=0, microsecond=0)
fake_org = '00000000-0000-4000-8000-00000000000a'
credits = {
    'anthropic-api:org-1ad35d608dd7': {'provider':'anthropic-api','auth_index':'org-1ad35d608dd7','used_percent':0,
        'reset_at':'0001-01-01T00:00:00Z','observed_at':'0001-01-01T00:00:00Z','last_attempt':'0001-01-01T00:00:00Z',
        'next_attempt':'0001-01-01T00:00:00Z','failures':0,
        'api_credit':{'label':'siphorchannel','position':0,'monthly_usd':'200','renews':'2026-10-29','organization_id':fake_org,'admin_key_ignored':True}},
    'anthropic-api:item-2': {'provider':'anthropic-api','auth_index':'item-2','used_percent':0,'reset_at':'0001-01-01T00:00:00Z',
        'observed_at':'0001-01-01T00:00:00Z','last_attempt':'0001-01-01T00:00:00Z','next_attempt':'0001-01-01T00:00:00Z','failures':0,
        'api_credit':{'label':'','position':1,'monthly_usd':'500','problem':'organization_id_missing'}},
}
usage = [{'model':'claude-sonnet-5-5','requests':3,'failed':1,'input':1200,'output':800,'cache_read':50,'cache_write':10},
         {'model':'claude-haiku-5-5','prompt':'over_100k','requests':1,'input':150000}]
meter = {'schema':1,'since':iso(now-timedelta(days=3)),'started_at':iso(now-timedelta(hours=2)),'flushed_at':iso(now-timedelta(minutes=2)),
    'restarts':1,'received':120,'counted':100,'foreign':3,'rejected':1,'last_rejected_at':iso(now-timedelta(hours=1)),'unattributed':2,
    'last_unattributed_at':iso(now-timedelta(minutes=30)),
    'gaps':[{'from':iso(now-timedelta(hours=3)),'to':iso(now-timedelta(hours=2)),'reason':'shutdown'}],
    'organizations':{fake_org:{'since':iso(now-timedelta(days=3)),'last_seen_at':iso(now-timedelta(minutes=5)),'last_success_at':iso(now-timedelta(minutes=5)),
        'refusals':1,'last_refusal_at':iso(now-timedelta(hours=1)),
        'days':[{'start':iso(midnight-timedelta(days=1)),'usage':[{'model':'claude-sonnet-5-5','requests':9,'input':9000,'output':900}]},{'start':iso(midnight),'usage':usage}],
        'hours':[{'start':iso(this_hour),'usage':usage}]}},
    'unlinked':[{'organization_id':'00000000-0000-4000-8000-00000000000e','first_seen_at':iso(now-timedelta(days=1)),'last_seen_at':iso(now-timedelta(hours=2)),'requests':37}],
    'auths':{'0123456789abcdef':{'organization_id':fake_org,'seen_at':iso(now-timedelta(minutes=5))}}}
fixture['activity'] = {'last_scan':iso(now),'accounts':3}

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *unused):
        pass

    def do_GET(self):
        # /a and /b stand for two CPAs behind this one origin under different
        # prefixes; both answer exactly as the unprefixed one does.
        path = self.path
        for prefix in ('/a/', '/b/'):
            if path.startswith(prefix + 'v0/'):
                path = path[len(prefix)-1:]
        if path == '/v0/resource/plugins/quota-cache/status':
            self.send_response(200)
            for name,values in shell['Headers'].items():
                for value in values: self.send_header(name,value)
            self.end_headers(); self.wfile.write(html)
        elif path == '/v0/management/plugins/quota-cache/status':
            # Refusals carry CPA's own messages (AuthenticateManagementKey),
            # which the page uses to explain why it stopped asking.
            state['reads'] += 1
            if state['mode'] == 'drop':
                # The link drops after the request went out: no status at all.
                self.close_connection = True
                return
            if state['mode'] == 'hang':
                # CPA refuses at once, but its answer arrives only after the
                # page has given up waiting.
                time.sleep(11)
                try:
                    raw = json.dumps({'error':'invalid management key'}).encode()
                    self.send_response(401); self.send_header('Content-Type','application/json'); self.end_headers(); self.wfile.write(raw)
                except OSError:
                    pass
                return
            key = self.headers.get('Authorization')
            if key is None:
                state['keyless'] += 1
                code,data = 401,{'error':'missing management key'}
            elif key != 'Bearer synthetic-sidebar-key' or state['mode']=='unauthorized':
                code,data = 401,{'error':'invalid management key'}
            elif state['mode']=='banned':
                code,data = 403,{'error':'IP banned due to too many failed attempts. Try again in 29m40s'}
            elif state['mode']=='unavailable':
                code,data = 503,{'error':'cache_unavailable'}
            else:
                code,data = 200,copy.deepcopy(fixture)
                if state['mode']=='empty':
                    data.update(entries={},history=[],provider_cooldown={})
                if state['mode']=='credits':
                    data['entries'].update(copy.deepcopy(credits))
                    data['api_meter'] = copy.deepcopy(meter)
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

# Holds the page's 30-second timer so the test can fire its ticks on demand
# instead of waiting for them. Other timers run as normal.
hook = Path(tmp.name) / 'ticks.js'
hook.write_text('''
window.__quotaCacheTicks = [];
const setIntervalNative = window.setInterval;
window.setInterval = function (callback, delay, ...rest) {
  if (delay === 30000) { window.__quotaCacheTicks.push(callback); return 0; }
  return setIntervalNative.call(this, callback, delay, ...rest);
};
''')
REFUSED = 'quota-cache.console-refused'

def browser(*command):
    result = subprocess.run([binary,'--session',session,'--json',*command],text=True,capture_output=True,timeout=45)
    if result.returncode:
        raise AssertionError(result.stdout + result.stderr)
    data = json.loads(result.stdout)
    if data.get('success') is False: raise AssertionError(data)
    return data.get('data',{})

def check(expression):
    browser('wait','--fn',expression)

def until(predicate, message):
    deadline = time.monotonic() + 10
    while not predicate():
        if time.monotonic() > deadline: raise AssertionError(message)
        time.sleep(.05)

def ticks(count):
    # Fire the page's automatic update count times, then give anything it
    # sent time to arrive.
    browser('eval', 'for (let i = 0; i < %d; i++) window.__quotaCacheTicks.forEach(tick => tick())' % count)
    time.sleep(.5)

def stored(value, name='cli-proxy-auth'):
    return "localStorage.setItem('%s', JSON.stringify(%s))" % (name, value)

def remembered(key, base='location.origin', extra=''):
    return stored("{version:0, state:{managementKey:'%s', rememberPassword:true, apiBase:%s%s}}" % (key, base, extra))

# The console's obfuscated form of a remembered session, as it stores it.
OBFUSCATED = '''(() => {
  const plain = new TextEncoder().encode(JSON.stringify({version:0, state:{managementKey:'synthetic-sidebar-key', rememberPassword:true, apiBase:location.origin + '/v8/management'}}));
  const salt = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + '|' + navigator.userAgent);
  let raw = '';
  plain.forEach((byte, i) => { raw += String.fromCharCode(byte ^ salt[i % salt.length]); });
  localStorage.setItem('cli-proxy-auth', 'enc::v1::' + btoa(raw));
})()'''

def load(*seeds):
    browser('eval', 'localStorage.removeItem("cli-proxy-auth"); localStorage.removeItem("managementKey"); localStorage.removeItem("apiBase"); localStorage.removeItem("isLoggedIn");' + ';'.join(seeds))
    browser('reload')

try:
    if args.serve_only:
        print('Synthetic sidebar preview: '+origin+'/v0/resource/plugins/quota-cache/status',flush=True)
        print("Sign it in from the browser console: localStorage.setItem('cli-proxy-auth', JSON.stringify({version:0, state:{managementKey:'synthetic-sidebar-key', rememberPassword:true, apiBase:location.origin}}))",flush=True)
        threading.Event().wait()
    browser('--init-script',str(hook),'open',origin+'/v0/resource/plugins/quota-cache/status')
    signed_out = "document.querySelector('#error').textContent.includes('Sign in to CPA') && document.querySelector('#content').hidden"
    check(signed_out)
    ticks(3)
    assert state['reads'] == 0, 'the page asked CPA without a remembered session'
    # None of these is a session the console remembered for this address.
    # 0.1.9 presented most of them, and sent no key at all for the rest, on
    # every 30-second update; CPA counts each refusal toward its lockout.
    for seeds in (
        [stored("{state:{managementKey:'synthetic-sidebar-key'}}")],
        [remembered('synthetic-sidebar-key', extra=', isAuthenticated:false')],
        [stored("{version:0, state:{managementKey:'synthetic-sidebar-key', rememberPassword:false, apiBase:location.origin}}")],
        [remembered('synthetic-sidebar-key', base="'http://elsewhere.invalid:8317'")],
        [remembered('')],
        ["localStorage.setItem('cli-proxy-auth', 'enc::v1::' + btoa('not the console'))"],
        [stored("'synthetic-sidebar-key'", 'managementKey'), stored('location.origin', 'apiBase')],
    ):
        load(*seeds)
        check(signed_out)
        ticks(2)
        assert state['reads'] == 0, 'the page presented a key the console did not remember: %s' % seeds
    # A stale remembered key is presented once. After CPA refuses it, neither
    # the timer nor a reload presents it again; Refresh view presents it once.
    load(remembered('stale-sidebar-key'))
    refused = "document.querySelector('#error').textContent.includes('CPA refused the console session') && document.querySelector('#content').hidden"
    check(refused)
    assert state['reads'] == 1
    ticks(5)
    browser('reload')
    check(refused)
    ticks(5)
    assert state['reads'] == 1, 'a refused key was presented again without Refresh view'
    check("JSON.parse(localStorage.getItem('%s')).reason === 'refused'" % REFUSED)
    browser('click','#refresh')
    until(lambda: state['reads'] == 2, 'Refresh view did not try the refused key once')
    check(refused)
    ticks(3)
    assert state['reads'] == 2
    # After a refusal the timer stays stopped even when the console saves a
    # different key; only Refresh view or a reload uses it.
    browser('eval', remembered('synthetic-sidebar-key'))
    ticks(3)
    assert state['reads'] == 2, 'the automatic update resumed after a refusal'
    # A different key replaces the refused one. During a lockout Refresh view is
    # disabled and nothing is sent until it ends.
    state['mode'] = 'banned'
    load(OBFUSCATED)
    locked = "document.querySelector('#error').textContent.includes('until about') && document.querySelector('#refresh').disabled"
    check(locked)
    assert state['reads'] == 3
    browser('eval', "document.querySelector('#refresh').click()")
    ticks(3)
    browser('reload')
    check(locked)
    ticks(3)
    assert state['reads'] == 3, 'the page asked CPA during its lockout'
    browser('eval', "const r = JSON.parse(localStorage.getItem('%s')); r.until = Math.floor(Date.now() / 1000) - 1; localStorage.setItem('%s', JSON.stringify(r))" % (REFUSED, REFUSED))
    state['mode'] = 'populated'
    browser('reload')
    check("document.querySelector('#error').textContent.includes('should be over') && !document.querySelector('#refresh').disabled")
    ticks(3)
    assert state['reads'] == 3
    browser('click','#refresh')
    check("document.querySelectorAll('#accounts tr').length === 3 && document.querySelectorAll('#history tr').length === 3")
    check("localStorage.getItem('%s') === null" % REFUSED)
    before = state['reads']
    ticks(1)
    assert state['reads'] == before+1, 'the automatic update did not run with an accepted session'
    browser('snapshot','-i')
    browser('set','viewport','1280','900')
    browser('screenshot',str(args.artifacts/'light.png'),'--full')
    audit = browser('a11y','--tags','wcag2a,wcag2aa')
    (args.artifacts/'light-a11y.json').write_text(json.dumps(audit,indent=2))
    assert audit.get('violations') == [], audit
    check("document.body.innerText.includes('HTTP 429') && document.body.innerText.includes('backend-api/wham/usage')")
    browser('select','#provider','codex')
    check("document.querySelectorAll('#accounts tr').length === 1 && document.querySelectorAll('#history tr').length === 1")
    browser('click','#accounts summary')
    check("document.querySelector('#accounts').innerText.includes('0.00000000000000000001 credits') && document.querySelector('#accounts').innerText.includes('allowed: false') && document.querySelector('#accounts').innerText.includes('0% used')")
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
    # Claude API credits: named by label, metered rather than polled, a
    # misconfigured item says so, and the meter's counts are printed as
    # counted, never priced or added up.
    state['mode']='credits';browser('click','#refresh')
    browser('select','#provider','anthropic-api')
    check("document.querySelectorAll('#accounts tr').length === 2 && document.querySelector('#accounts').innerText.includes('siphorchannel') && document.querySelector('#accounts').innerText.includes('Metered') && document.querySelector('#accounts').innerText.includes('Not polled') && document.querySelector('#accounts').innerText.includes('organization_id_missing')")
    # Neither credit item has a next poll, and neither pulls the header's
    # next eligible poll forward to now: every polled entry is due twelve
    # minutes or more from now.
    check("[...document.querySelectorAll('#accounts tr')].find(r => r.textContent.includes('organization_id_missing')).cells[4].textContent === 'Not polled'")
    check("[...document.querySelectorAll('#accounts tr')].find(r => r.textContent.includes('siphorchannel')).cells[4].textContent === '—'")
    check("Date.parse(document.querySelector('#next-call time').dateTime) - Date.now() > 5 * 60000")
    check("(t => t.includes('Organization: 00000000-0000-4000-8000-00000000000a') && t.includes('Monthly credit: 200 USD (configured)') && t.includes('Renews: 2026-10-29 (configured)') && t.includes('admin-key is no longer used') && t.includes('Low-credit refusals: 1') && t.includes('claude-sonnet-5-5: 3 ok, 1 failed, in 1200, out 800, cache read 50, cache write 10 tokens') && t.includes('claude-haiku-5-5 over_100k: 1 ok, 0 failed, in 150000, out 0, cache read 0, cache write 0 tokens') && !t.includes('9000') && t.includes('Configuration problem: organization_id_missing') && !t.includes('key-'))(document.querySelector('#accounts').textContent)")
    # The meter panel: counters, the gap and the organization no item names.
    check("!document.querySelector('#meter-section').hidden && (t => t.includes('Saved within 30 minutes') && t.includes('Records received') && t.includes('120') && t.includes('Counting') && t.includes('shutdown') && t.includes('00000000-0000-4000-8000-00000000000e') && t.includes('37 requests'))(document.querySelector('#meter-section').textContent)")
    audit = browser('a11y','--tags','wcag2a,wcag2aa')
    assert audit.get('violations') == [], audit
    browser('select','#provider','all')
    state['mode']='empty';browser('click','#refresh')
    check("!document.querySelector('#accounts-empty').hidden && !document.querySelector('#history-empty').hidden && document.querySelector('#meter-section').hidden")
    state['mode']='unavailable';browser('click','#refresh')
    check("document.querySelector('#content').hidden && document.querySelector('#error').textContent.includes('503')")
    state['mode']='unauthorized';browser('click','#refresh')
    check(refused)
    state['mode']='populated'
    before = state['reads']
    ticks(3)
    assert state['reads'] == before, 'the automatic update resumed after a refusal'
    browser('click','#refresh')
    check("!document.querySelector('#content').hidden && document.querySelector('#error').hidden")
    # A request that gets no status back may still have been refused and
    # counted by CPA, so the timer stops as it does after a refusal. Nothing is
    # remembered, since the key may be fine, and Refresh view asks once more.
    no_answer = "document.querySelector('#error').textContent.includes('stopped updating on its own') && document.querySelector('#content').hidden"
    for mode in ('drop', 'hang'):
        state['mode'] = mode
        before = state['reads']
        ticks(1)
        # The request has ended once the button is back and the view is down.
        check("document.querySelector('#content').hidden && !document.querySelector('#refresh').disabled")
        ticks(5)
        assert state['reads'] == before+1, 'the automatic update presented the key again after a request got no answer (%s)' % mode
        check(no_answer)
        check("localStorage.getItem('%s') === null && sessionStorage.getItem('%s') === null" % (REFUSED, REFUSED))
        state['mode'] = 'populated'
        browser('click','#refresh')
        check("!document.querySelector('#content').hidden && document.querySelector('#error').hidden")
        ticks(1)
        assert state['reads'] == before+3, 'the automatic update did not resume after Refresh view (%s)' % mode
    # Two CPAs behind one origin under different prefixes share this storage.
    # The page for one finds no session of its own while the console is signed
    # in to the other, and must leave that one's refusal record alone.
    page = lambda prefix: origin + prefix + '/v0/resource/plugins/quota-cache/status'
    before = state['reads']
    browser('eval', 'localStorage.removeItem("cli-proxy-auth");' + remembered('stale-prefix-key', base="location.origin + '/a'"))
    browser('open', page('/a'))
    check(refused)
    assert state['reads'] == before+1
    browser('open', page('/b'))
    check(signed_out)
    ticks(3)
    browser('open', page('/a'))
    check(refused)
    ticks(3)
    assert state['reads'] == before+1, "a page for another prefix dropped this one's refusal, and its refused key was presented again"
    check("JSON.parse(localStorage.getItem('%s')).root === location.origin + '/a'" % REFUSED)
    browser('eval', "localStorage.removeItem('%s')" % REFUSED)
    browser('open', page(''))
    # The origin's storage is shared with the console and every other plugin
    # page, so it can be full. A refusal it cannot take is kept in this tab's
    # storage, and a reload still finds it.
    FILL = "(() => { let i = 0; for (const size of [1 << 20, 1 << 10, 1]) { try { for (;;) localStorage.setItem('quota-cache-smoke.filler.' + i++, 'x'.repeat(size)); } catch {} } })()"
    EMPTY = "Object.keys(localStorage).filter(k => k.startsWith('quota-cache-smoke.filler.')).forEach(k => localStorage.removeItem(k))"
    before = state['reads']
    load(remembered('stale-full-key'), FILL)
    check(refused)
    assert state['reads'] == before+1
    browser('reload')
    check(refused)
    ticks(3)
    assert state['reads'] == before+1, 'a reload presented a refused key the page could not remember in localStorage'
    check("localStorage.getItem('%s') === null && JSON.parse(sessionStorage.getItem('%s')).reason === 'refused'" % (REFUSED, REFUSED))
    browser('eval', EMPTY + "; sessionStorage.removeItem('%s')" % REFUSED)
    load(OBFUSCATED)
    check("!document.querySelector('#content').hidden && document.querySelector('#error').hidden")
    assert state['keyless'] == 0, 'the page sent a management request without a key'
    print('PASS: native sidebar, strict session reading, no keyless requests, refusal and lockout stop with a persistent record (kept per CPA prefix, and in the tab when the origin storage is full), no automatic retry after a request gets no answer, provider filters, Claude API credits, cooldown/history, empty/error states, safe text rendering, light/dark/mobile, cache-only view refresh')
finally:
    if not args.serve_only:
        subprocess.run([binary,'--session',session,'close'],stdout=subprocess.DEVNULL,check=False,timeout=30)
    server.shutdown()
    native['api'] = api
    api.shutdown()
    tmp.cleanup()
