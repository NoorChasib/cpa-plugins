#!/usr/bin/env python3
"""Real-browser acceptance of the native sidebar using synthetic account data."""
import argparse
import base64
import calendar
import copy
from datetime import datetime, timedelta, timezone
import hashlib
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
parser.add_argument('--mode', default='populated', help='the status --serve-only answers with: populated, credits or editor')
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
state = {'mode':'populated', 'reads':0, 'keyless':0, 'posts':[], 'answers':[], 'escaped':False}
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
# A round of polls Quota Glance asked for after a banked reset, two left.
fixture['refresh'] = {'requested_at':iso(now-timedelta(seconds=30)),'remaining':2}
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

# The editor's status (mode 'editor'): credit_state and editor as Quota Cache
# 0.2.0 publishes them, with placeholder organizations, addresses and auth
# indexes. One label and one sentence carry an apostrophe and an ampersand,
# which CPA escapes on a host below schema 6 (strings_escaped).
epoch = lambda d: int(d.timestamp())
def refill_in(year, month, day):
    year += (month - 1) // 12; month = (month - 1) % 12 + 1
    return datetime(year, month, min(day, calendar.monthrange(year, month)[1]), tzinfo=timezone.utc)
def cycle_of(day):
    this = refill_in(now.year, now.month, day)
    return (this, refill_in(now.year, now.month + 1, day)) if this <= now else (refill_in(now.year, now.month - 1, day), this)
org_id = lambda n: '00000000-0000-4000-8000-%012x' % n
org_account = lambda organization: 'org-' + hashlib.sha256(organization.encode()).hexdigest()[:12]
ESCAPED_LABEL = "o'neil & co"
ESCAPED_ISSUE = "Quota Cache's meter was full & dropped records, so this is a lower bound."
def account(order, label, organization, state_, monthly='', config_monthly='', renews='', config_renews='', reading=None, issue='', issues=(),
            editable=True, reason='', used='$40.00', left='$160.00', credit='$200.00', source='config', estimate=True, remaining=80, revision=''):
    effective = renews or config_renews
    start, end = cycle_of(int(effective[8:10])) if effective else (None, None)
    return {'id': org_account(organization) if organization else 'item-%d' % (order + 1), 'label': label, 'order': order, 'organizationId': organization,
        'state': state_, 'counted': estimate, 'hasEstimate': estimate, 'basis': 'reading' if reading else 'credit', 'lowerBound': False,
        'monthlyCredit': 0, 'monthlyCreditText': credit if (monthly or config_monthly) else '', 'monthlyCreditSource': source if (monthly or config_monthly) else 'none',
        'spent': 0, 'spentText': used, 'spentSinceEpoch': epoch(start) if start else None, 'used': 0, 'usedText': used, 'left': 0, 'leftText': left,
        'estimateLeftText': '', 'overage': 0, 'overageText': '$0.00', 'remainingFraction': remaining / 100, 'remainingPercent': remaining, 'level': 'ok',
        'cycleStartEpoch': epoch(start) if start else None, 'renewsAtEpoch': epoch(end) if end else None, 'renewsInSeconds': epoch(end) - epoch(now) if end else None,
        'renewsSource': ('dashboard' if renews else 'config') if effective else 'none', 'reading': None, 'cacheWriteExtra': 0, 'cacheWriteExtraText': '', 'unpriced': [],
        'refusals': {'total': 0, 'lastAtEpoch': None, 'claudeCodeTotal': 0, 'claudeCodeLastAtEpoch': None},
        'meterSinceEpoch': epoch(now - timedelta(days=3)), 'lastSeenEpoch': epoch(now - timedelta(minutes=5)), 'dataIssues': list(issues), 'issue': issue,
        'settings': {'editable': editable, 'notEditableReason': reason, 'revision': revision, 'monthlyUsd': monthly, 'configMonthlyUsd': config_monthly,
            'configMonthlyUsdInvalid': False, 'renews': renews, 'configRenews': config_renews, 'configRenewsInvalid': False, 'reading': reading,
            'readingUnusedReason': '', 'updatedAtEpoch': epoch(now - timedelta(days=1)) if revision else None}}
def editor_state():
    reading_at = epoch(now - timedelta(hours=20))
    accounts = [
        account(0, 'alpha', org_id(1), 'ok', config_monthly='200', config_renews='2026-09-29'),
        account(1, ESCAPED_LABEL, org_id(2), 'ok', monthly='260.50', config_monthly='200', renews='2026-09-20', source='dashboard', revision='12',
                reading={'remainingUsd': '180.00', 'atEpoch': reading_at, 'enteredAtEpoch': reading_at}, issue=ESCAPED_ISSUE, issues=['meterFull'],
                used='$88.50', left='$172.00', credit='$260.50', remaining=66),
        account(2, 'charlie', org_id(3), 'needsSettings', config_monthly='100', issue='Set the refill date, or enter a Console reading.', issues=['needsRefillDate'], estimate=False),
        account(3, 'foxtrot', '', 'misconfigured', editable=False, reason='noOrganization', issue='This item has no organization-id.', estimate=False),
    ]
    alpha_end, oneil_end = accounts[0]['renewsAtEpoch'], accounts[1]['renewsAtEpoch']
    counts = {'monthly_credits': 2, 'refill_dates': 2, 'readings': 2, 'renewal_dates': 3}
    legacy = {'state': 'imported', 'kind': 'automatic', 'source_path': '/CLIProxyAPI/plugins/data/quota-glance/settings.json', 'sha256': 'a' * 64,
              'settings_sha256': 'a' * 64, 'legacy_revision': '12', 'legacy_updated_at': iso(now - timedelta(days=1)), 'counts': counts,
              'imported_at': iso(now - timedelta(hours=1)), 'following': False, 'importable': True}
    renewal = {'date': '2026-09-10', 'revision': '3', 'updatedAtEpoch': epoch(now - timedelta(days=2))}
    orphan_renewal = {'id': '9c1e00000000a1a1', 'date': '2026-08-01', 'revision': '2', 'updatedAtEpoch': epoch(now - timedelta(days=9))}
    organizations = [{'id': org_account(org_id(n)), 'label': label, 'organization_id': org_id(n), 'has_default_monthly_usd': True, 'has_default_renews': n == 1}
                     for n, label in ((1, 'alpha'), (2, ESCAPED_LABEL), (3, 'charlie'))]
    return {
        'credit_state': {'schema': 1, 'quota_cache_version': '0.2.0', 'computed_at': iso(now), 'valid_until': None,
            'inputs': {'snapshot_written_at': iso(now), 'meter_flushed_at': iso(now - timedelta(minutes=2)), 'meter_error': '', 'settings_revision': '12'},
            'settings': {'state': 'ok', 'editable': True, 'revision': '12', 'import': legacy},
            'api_credits': {'title': 'Monthly API Credit', 'currency': 'USD', 'pricing': {'asOf': '2026-10-09', 'source': 'https://docs.anthropic.com/pricing', 'cacheWrites': '5m'},
                'meter': None, 'editing': {'available': True, 'reason': '', 'migration': ''},
                'pool': {'hasEstimate': True, 'lowerBound': False, 'monthlyCredit': 0, 'monthlyCreditText': '$460.50', 'used': 0, 'usedText': '$128.50', 'left': 0,
                    'leftText': '$332.00', 'overage': 0, 'overageText': '$0.00', 'remainingFraction': .72, 'remainingPercent': 72, 'level': 'ok',
                    'nextRefill': {'accountIds': [accounts[1]['id']], 'refillAtEpoch': oneil_end, 'refillInSeconds': 0, 'gain': 0, 'gainText': '$88.50', 'gainFraction': .2, 'gainPercent': 20},
                    'fullAtEpoch': max(alpha_end, oneil_end), 'fullInSeconds': 0, 'accountCount': 4, 'countedCount': 2, 'missingCount': 2},
                'accounts': accounts, 'unlinked': [],
                'orphans': [{'id': 'org-7c1e09a4d2b3', 'monthlyUsd': '50', 'renews': '', 'hasReading': False, 'revision': '3', 'updatedAtEpoch': epoch(now - timedelta(days=5))}]},
            'renewals': {'5f2b8c41d09e7a36': renewal, orphan_renewal['id']: {k: orphan_renewal[k] for k in ('date', 'revision', 'updatedAtEpoch')}},
            'organizations': {'state': 'ok', 'revision': '4', 'source': 'page', 'migration': {'at': iso(now - timedelta(hours=2)), 'moved': 3, 'skipped': 1}, 'config_items_ignored': 3}},
        'editor': {'available': True, 'reason': '', 'revision': '12', 'max_api_credit_items': 16, 'max_renewal_items': 32, 'max_batch_bytes': 4096,
            'legacy': dict(legacy, file_counts={'monthly_credits': 3, 'refill_dates': 1, 'readings': 0, 'renewal_dates': 4}),
            'credentials': [
                {'auth_index': '5f2b8c41d09e7a36', 'name': 'foxtrot@example.com', 'disabled': False, 'billing_period': 'monthly', 'renewal_editable': True, 'renewal': renewal,
                 'renewal_at_epoch': epoch(cycle_of(10)[1]), 'renewal_source': 'dashboard', 'renewal_estimate_at_epoch': epoch(cycle_of(12)[1])},
                {'auth_index': '0a1b2c3d4e5f6071', 'name': 'golf@example.com', 'disabled': False, 'billing_period': 'monthly', 'renewal_editable': True, 'renewal': None,
                 'renewal_at_epoch': epoch(now + timedelta(days=23)), 'renewal_source': 'estimated', 'renewal_estimate_at_epoch': epoch(now + timedelta(days=23))},
                {'auth_index': 'feedface00000001', 'name': 'india@example.com', 'disabled': True, 'billing_period': '', 'renewal_editable': True, 'renewal': None,
                 'renewal_at_epoch': None, 'renewal_source': '', 'renewal_estimate_at_epoch': None}],
            'renewal_orphans': [orphan_renewal], 'api_credit_orphans': [], 'held_api_credits': [],
            'organizations': {'available': True, 'reason': '', 'revision': '4', 'max_items': 16, 'max_ops': 16, 'items': organizations}},
    }
editor = editor_state()
plans = {'5f2b8c41d09e7a36': 'max_20x', '0a1b2c3d4e5f6071': 'team'}
def escape_values(value):
    # html.EscapeString, as CPA applies it to every string value (never a key).
    if isinstance(value, str):
        return value.replace('&', '&amp;').replace('<', '&lt;').replace('>', '&gt;').replace('"', '&#34;').replace("'", '&#39;')
    if isinstance(value, list):
        return [escape_values(item) for item in value]
    if isinstance(value, dict):
        return {key: escape_values(item) for key, item in value.items()}
    return value
def saved(raw):
    # What Quota Cache would answer a save that commits, applied to the
    # editor's status so the page's next read shows it.
    body = json.loads(raw)
    state_ = editor['credit_state']
    if body['kind'] == 'apiCredits':
        blocks = {}
        for item in body['items']:
            found = [a for a in state_['api_credits']['accounts'] if a['id'] == item['id']] if state_['api_credits'] else []
            if not found:
                if state_['api_credits']:
                    state_['api_credits']['orphans'] = [o for o in state_['api_credits']['orphans'] if o['id'] != item['id']]
                editor['editor']['api_credit_orphans'] = [o for o in editor['editor']['api_credit_orphans'] if o['id'] != item['id']]
                blocks[item['id']] = None
                continue
            s_ = found[0]['settings']
            s_['monthlyUsd'] = item['monthlyUsd'] or ''
            found[0]['monthlyCreditSource'] = 'dashboard' if item['monthlyUsd'] else 'config'
            s_['revision'] = '13'
            blocks[item['id']] = s_
        return 200, {'ok': True, 'unchanged': False, 'revision': '13', 'settings': blocks}
    if body['kind'] == 'renewals':
        blocks = {}
        for item in body['items']:
            blocks[item['id']] = None
            for c in editor['editor']['credentials']:
                if c['auth_index'] == item['id']:
                    c['renewal'] = {'date': item['date'], 'revision': '13', 'updatedAtEpoch': epoch(now)} if item['date'] else None
                    c['renewal_source'] = 'dashboard' if item['date'] else ('estimated' if c['renewal_estimate_at_epoch'] else '')
                    blocks[item['id']] = c['renewal']
            editor['editor']['renewal_orphans'] = [o for o in editor['editor']['renewal_orphans'] if o['id'] != item['id']]
        return 200, {'ok': True, 'unchanged': False, 'revision': '13', 'settings': blocks}
    if body['kind'] == 'importLegacy':
        # The plugin records the import before it answers, so the status read
        # that follows already says it was imported.
        for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
            block.update(state='imported', imported_at=iso(now))
        return 200, {'ok': True, 'unchanged': False, 'revision': '13', 'counts': {'monthly_credits': 2, 'refill_dates': 2, 'readings': 2, 'renewal_dates': 3}}
    organizations = editor['editor']['organizations']
    if body['baseRevision'] != organizations['revision']:
        return 409, {'error': 'conflict', 'revision': organizations['revision'], 'organizations': organizations['items']}
    for op in body['ops']:
        if op['op'] == 'remove':
            organizations['items'] = [i for i in organizations['items'] if i['id'] != op['id']]
        elif op['op'] == 'rename':
            for i in organizations['items']:
                if i['id'] == op['id']: i['label'] = op['label']
        else:
            organizations['items'].append({'id': org_account(op['organizationId']), 'label': op['label'], 'organization_id': op['organizationId'], 'has_default_monthly_usd': False, 'has_default_renews': False})
    organizations['revision'] = str(int(organizations['revision']) + 1)
    return 200, {'ok': True, 'unchanged': False, 'revision': organizations['revision'], 'organizations': organizations['items']}

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
            if state['mode'] in ('drop', 'slowdrop'):
                # The link drops after the request went out: no status at all;
                # slowdrop first waits long enough to open a card meanwhile.
                if state['mode'] == 'slowdrop':
                    time.sleep(4)
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
                    data.update(entries={},history=[],provider_cooldown={},refresh={'requested_at':None,'remaining':0})
                if state['mode']=='credits':
                    data['entries'].update(copy.deepcopy(credits))
                    data['api_meter'] = copy.deepcopy(meter)
                if state['mode']=='untrusted':
                    data['entries']['codex:synthetic-codex']['last_error'] = '<img src=x onerror="window.injected=true">'
                if state['mode']=='editor':
                    data.update(copy.deepcopy(editor))
                    for auth, plan in plans.items():
                        data['entries']['claude:' + auth] = dict(copy.deepcopy(data['entries']['claude:synthetic-claude']), auth_index=auth, plan=plan)
                data['status_reads'] = state['reads']
                data['strings_escaped'] = state['escaped']
                if state['escaped']:
                    data = escape_values(data)
            raw=json.dumps(data).encode()
            self.send_response(code); self.send_header('Content-Type','application/json'); self.end_headers(); self.wfile.write(raw)
        else:
            self.send_response(404); self.end_headers()

    def do_POST(self):
        # The save route. Every request is recorded with the headers the page
        # sent; the answer is the next queued one, else a committed save.
        path = self.path
        for prefix in ('/a/', '/b/'):
            if path.startswith(prefix + 'v0/'):
                path = path[len(prefix)-1:]
        raw = self.rfile.read(int(self.headers.get('Content-Length') or 0))
        if path != '/v0/management/plugins/quota-cache/settings':
            self.send_response(404); self.end_headers(); return
        state['posts'].append({'auth': self.headers.get('Authorization'), 'type': self.headers.get('Content-Type'), 'site': self.headers.get('Sec-Fetch-Site'), 'body': json.loads(raw)})
        headers = {}
        if self.headers.get('Authorization') is None:
            state['keyless'] += 1
            code, data = 401, {'error': 'missing management key'}
        elif state['answers'] and state['answers'][0] == 'drop':
            # The link drops after the save went out: no answer at all.
            state['answers'].pop(0)
            self.close_connection = True
            return
        elif state['answers']:
            code, data, headers = state['answers'].pop(0)
        else:
            code, data = saved(raw)
        if state['escaped']:
            data = escape_values(data)
        out = json.dumps(data).encode()
        self.send_response(code); self.send_header('Content-Type', 'application/json'); self.send_header('Cache-Control', 'no-store')
        for name, value in headers.items(): self.send_header(name, value)
        self.end_headers(); self.wfile.write(out)

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
    try:
        browser('wait','--fn',expression)
    except AssertionError:
        # Say what the page showed, which is all synthetic.
        shown = browser('eval', "document.querySelector('main').innerText.slice(0, 6000)")
        raise AssertionError('%s\n--- the page showed ---\n%s' % (expression, shown.get('result', shown)))

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

def press(container, text):
    # A button by its accessible name, inside one card: its aria-label when
    # it has one, else its text.
    browser('eval', "(() => { const b = [...document.querySelectorAll(%s)].find(b => (b.getAttribute('aria-label') || b.textContent.trim()) === %s); if (!b) throw new Error('no button ' + %s + ' among ' + JSON.stringify([...document.querySelectorAll(%s)].map(b => b.getAttribute('aria-label') || b.textContent.trim()))); b.click(); })()"
            % (json.dumps(container + ' button'), json.dumps(text), json.dumps(text), json.dumps(container + ' button')))

def pick_date(selector, value):
    # A date picked in the browser's date control. Typing cannot fill one
    # (its segments follow the locale), so the value is set and the control's
    # own input event dispatched, as a pick does.
    browser('eval', "(e => { e.focus(); e.value = %s; e.dispatchEvent(new Event('input', {bubbles: true})); e.dispatchEvent(new Event('change', {bubbles: true})); })(document.querySelector(%s))" % (json.dumps(value), json.dumps(selector)))

def text_of(selector):
    return "document.querySelector(%s).textContent" % json.dumps(selector)

def posted(count, message):
    until(lambda: len(state['posts']) == count, message)
    time.sleep(.3)
    assert len(state['posts']) == count, message
    return state['posts'][-1] if count else None

def refreshed():
    # Refresh view, then wait until the status it read has been drawn: the
    # request has been answered and the button is back.
    before = state['reads']
    browser('click', '#refresh')
    until(lambda: state['reads'] > before, 'Refresh view did not read the status')
    check("!document.querySelector('#refresh').disabled")

def tick_drawn():
    # Fire the page's automatic update once and wait until it is drawn: the
    # first organization's label is changed for that read alone, which only
    # the Organizations card shows.
    items = editor['editor']['organizations']['items']
    label = items[0]['label']
    items[0]['label'] = label + '-drawn'
    ticks(1)
    check("%s.includes(%s)" % (text_of('#organizations'), json.dumps(label + '-drawn')))
    items[0]['label'] = label

def editor_checks():
    """The editor's cards, against the editor status."""
    global editor
    browser('set', 'viewport', '1280', '900')
    browser('set', 'media', 'light')
    state.update(mode='editor', escaped=False, answers=[])
    refreshed()
    check("!document.querySelector('#organizations').hidden && !document.querySelector('#api-credits').hidden && !document.querySelector('#renewals').hidden")
    # At rest: the organizations, their notes, and the cards' rows with the
    # marks that say where each value comes from.
    check("(t => t.includes('alpha') && t.includes('00000000…0001') && t.includes('Edit organizations') && t.includes('Moved 3 organizations here from Quota Cache’s configuration') && t.includes('1 skipped because of a problem in the configuration') && t.includes('still lists organizations under claude-api-credits'))(%s)" % text_of('#organizations'))
    check("document.querySelector('#organizations span.org-id').title === '00000000-0000-4000-8000-000000000001'")
    check("document.querySelector('#api-credits tbody td.account .secondary').textContent === '00000000…0001'")
    check("(t => t.includes('$260.50') && t.includes('entered here · config $200.00') && t.includes('$180.00') && t.includes('in use') && t.includes('from config') && t.includes('every 29th') && t.includes('Needs refill date') && t.includes('Enter Console reading') && t.includes('org-7c1e09a4d2b3'))(%s)" % text_of('#api-credits'))
    check("(t => t.includes('foxtrot@example.com') && t.includes('Max 20x') && t.includes('entered here') && t.includes('estimated') && t.includes('not known yet') && t.includes('9c1e…a1a1'))(%s)" % text_of('#renewals'))
    check("(t => t.includes('Imported from Quota Glance on') && t.includes('2 monthly credits, 2 refill dates, 2 Console readings and 3 renewal dates') && t.includes('last changed there') && !t.includes('older than 0.8.0'))(%s)" % text_of('#import-note'))
    screenshot = lambda name: browser('screenshot', str(args.artifacts / name), '--full')
    def named_by_text(container):
        # Every button with an aria-label is named with the words it shows,
        # so speech input reaches it by them (WCAG 2.5.3, Label in Name, which
        # the audit's wcag2a and wcag2aa rules do not cover).
        found = browser('eval', "[...document.querySelectorAll(%s)].map(b => [b.textContent.replace(/\\s+/g, ' ').trim(), (b.getAttribute('aria-label') || '').replace(/\\s+/g, ' ').trim()]).filter(([text, label]) => label && !label.toLowerCase().includes(text.toLowerCase()))" % json.dumps(container + ' button'))
        assert found.get('result') == [], 'buttons named without the words they show: %r' % found.get('result', found)
    def audit(name):
        result = browser('a11y', '--tags', 'wcag2a,wcag2aa')
        (args.artifacts / (name + '-a11y.json')).write_text(json.dumps(result, indent=2))
        assert result.get('violations') == [], result
        named_by_text('main')
    screenshot('editor-rest.png')
    audit('editor-rest')
    assert browser('errors').get('errors', []) == [], 'the page threw'
    # A view update at rest keeps every node whose content did not change, so
    # focus stays on the control the reader is on; a card that changed puts
    # focus back on its control by id.
    browser('eval', "(b => { b.focus(); b.kept = true; })(document.querySelector('#qc-renewals-edit'))")
    tick_drawn()
    check("document.activeElement === document.querySelector('#qc-renewals-edit') && document.activeElement.kept === true")
    editor['credit_state']['api_credits']['pool']['leftText'] = '$331.00'
    browser('eval', "(b => { b.focus(); b.kept = true; })(document.querySelector('#qc-credits-edit'))")
    tick_drawn()
    check("%s.includes('$331.00 left') && document.activeElement.id === 'qc-credits-edit' && document.activeElement.kept !== true" % text_of('#api-credits .section-head'))
    editor['credit_state']['api_credits']['pool']['leftText'] = '$332.00'
    tick_drawn()
    # The import note is dismissed for good, per import. Its Dismiss goes
    # with it, so focus moves on to the next control, as Tab would.
    browser('focus', '#qc-import-dismiss')
    browser('press', 'Enter')
    check("document.querySelector('#import-note').hidden && document.activeElement === document.querySelector('#qc-orgs-edit')")
    refreshed()
    check("document.querySelector('#import-note').hidden && JSON.parse(localStorage.getItem('quota-cache.dismissed')).keys.some(k => k.startsWith('import:'))")
    # Quota Glance's Edit in Quota Cache links name a section, which the page
    # scrolls to once the first status has drawn it.
    browser('set', 'viewport', '1280', '600')
    for section in ('renewals', 'api-credits'):
        browser('eval', "scrollTo(0, 0); history.replaceState(null, '', location.pathname + '#%s')" % section)
        browser('reload')
        check("!document.querySelector('#content').hidden && scrollY > 0 && document.querySelector('#%s').getBoundingClientRect().top < innerHeight / 2" % section)
    browser('eval', "history.replaceState(null, '', location.pathname); scrollTo(0, 0)")
    browser('set', 'viewport', '1280', '900')
    reads = state['reads']

    # Edit, change a credit, save: exactly one POST, JSON, with the
    # remembered key, and the view read again with the new figures.
    press('#api-credits', 'Edit credits and dates')
    check("document.querySelector('#api-credits form') && document.querySelector('#view-status').textContent === 'View updates paused while you edit' && document.querySelector('#refresh').disabled")
    check("document.activeElement === document.querySelector('#qc-credit-0-monthly')")
    # A locked row says why, and where organizations are edited.
    check("(t => t.includes('no usable organization-id. Add the organization under Organizations above to edit it here.') && t.includes('Labels and Organization IDs are edited under Organizations.'))(%s)" % text_of('#api-credits'))
    # Every row's fields share their labels, so each is named by its
    # organization first.
    named = "(e => e.getAttribute('aria-labelledby').split(' ').map(id => document.getElementById(id).textContent).join(' '))(document.querySelector(%s))"
    check("%s === 'alpha Monthly credit, USD' && %s === \"o'neil & co Refills on, UTC\"" % (named % json.dumps('#qc-credit-0-monthly'), named % json.dumps('#qc-credit-1-refill')))
    # The foot stays pinned to the bottom of a short window while the rows
    # above it run past.
    browser('set', 'viewport', '1280', '400')
    browser('eval', "document.querySelector('#api-credits form').scrollIntoView({block: 'start'})")
    check("document.querySelector('#api-credits .edit-foot').getBoundingClientRect().bottom <= innerHeight + 1 && document.querySelector('#api-credits form').getBoundingClientRect().bottom > innerHeight + 40")
    # Keyboard focus is never hidden under the pinned foot (WCAG 2.4.11, Focus
    # Not Obscured): in a sidebar as narrow as the console's, each control Tab
    # reaches in a card is scrolled clear of the foot, its focus ring
    # included, and Tab onto the foot's own controls, always in view, scrolls
    # nothing. Real key presses, so the browser does the scrolling.
    browser('set', 'viewport', '420', '800')
    clear = "(e => { const form = e.closest('form'), foot = form && form.querySelector(':scope > .edit-foot'); return !foot || foot.contains(e) || (e.closest('.box') || e).getBoundingClientRect().bottom + 6 <= foot.getBoundingClientRect().top })(document.activeElement)"
    def tab_through(first, last):
        browser('eval', 'scrollTo(0, 0)')
        browser('focus', first)
        reached = []
        while last not in reached:
            assert len(reached) < 40, 'Tab never reached %s: %r' % (last, reached)
            before = browser('eval', 'scrollY').get('result')
            browser('press', 'Tab')
            check(clear)
            focused, after = browser('eval', '[document.activeElement.id, scrollY]').get('result')
            reached.append(focused)
        assert after == before, 'Tab onto %s scrolled the page from %s to %s' % (last, before, after)
        return reached
    assert 'qc-credit-1-monthly' in tab_through('#qc-credit-0-monthly', 'qc-credits-cancel')
    # One card is edited at a time.
    press('#api-credits .edit-foot', 'Cancel')
    check("!document.querySelector('#qc-renewals-edit').disabled")
    press('#renewals', 'Set renewal dates')
    check("document.activeElement === document.querySelector('#qc-renewal-0')")
    assert 'qc-renewal-2' in tab_through('#qc-renewal-0', 'qc-renewals-cancel')
    press('#renewals .edit-foot', 'Cancel')
    check("!document.querySelector('#qc-credits-edit').disabled")
    press('#api-credits', 'Edit credits and dates')
    check("document.activeElement === document.querySelector('#qc-credit-0-monthly')")
    browser('set', 'viewport', '1280', '900')
    browser('eval', 'scrollTo(0, 0)')
    ticks(3)
    assert state['reads'] == reads, 'the view updated while a card was being edited'
    # An edit still to fix is unsaved too: leaving asks first.
    browser('fill', '#qc-credit-0-monthly', '1,50')
    check("(t => t.includes('1 unsaved') && t.includes('1 to fix'))(%s) && (e => (dispatchEvent(e), e.defaultPrevented))(new Event('beforeunload', {cancelable: true}))" % text_of('#api-credits'))
    browser('fill', '#qc-credit-0-monthly', '275')
    check("(t => t.includes('changed') && t.includes('was $200.00') && t.includes('1 change'))(%s)" % text_of('#api-credits'))
    screenshot('editor-credits.png')
    audit('editor-credits')
    # Every press on a card says where focus goes next, since the card is
    # drawn afresh: Save from the keyboard keeps it through the save, and the
    # card at rest puts it on its Edit button.
    browser('focus', '#qc-credits-save')
    browser('press', 'Enter')
    post = posted(1, 'Save did not send exactly one request')
    assert post['type'] == 'application/json' and post['auth'] == 'Bearer synthetic-sidebar-key' and post['site'] == 'same-origin', post
    assert post['body'] == {'kind': 'apiCredits', 'items': [{'id': org_account(org_id(1)), 'baseRevision': '', 'monthlyUsd': '275', 'renews': None, 'reading': None}]}, post['body']
    check("!document.querySelector('#api-credits form') && %s.includes('Saved 1 change')" % text_of('#api-credits'))
    until(lambda: state['reads'] > reads, 'the view was not read again after a save')
    check("%s.includes('entered here · config $200.00') && !document.querySelector('#refresh').disabled" % text_of('#api-credits tbody tr'))
    check("document.activeElement === document.querySelector('#qc-credits-edit')")
    # The saved line goes after 20 seconds. That redraws only the cards at
    # rest: a card being edited keeps its nodes, so a date typed halfway,
    # which no draft can hold, and focus stay as they were.
    press('#renewals', 'Set renewal dates')
    check("document.activeElement === document.querySelector('#qc-renewal-0')")
    # Key by key, as a reader types: a date control takes no inserted text.
    browser('focus', '#qc-renewal-1')
    browser('press', '1')
    browser('press', '0')
    browser('eval', "window.__date = document.querySelector('#qc-renewal-1')")
    check("window.__date.validity.badInput && document.activeElement === window.__date")
    time.sleep(21)
    check("!%s.includes('Saved 1 change') && window.__date.isConnected && window.__date.validity.badInput && document.activeElement === window.__date" % text_of('#api-credits'))
    # So does the Accounts table's provider filter, which draws the same
    # status again.
    for provider in ('claude', 'all'):
        browser('eval', "(s => { s.value = %s; s.dispatchEvent(new Event('change', {bubbles: true})); })(document.querySelector('#provider'))" % json.dumps(provider))
        check("window.__date.isConnected && window.__date.validity.badInput && document.activeElement === window.__date")
    press('#renewals .edit-foot', 'Cancel')
    check("!document.querySelector('#renewals form') && document.activeElement === document.querySelector('#qc-renewals-edit')")

    # A caption's links keep their nodes while their card is open. Focus
    # leaving a refill date hides its hint, and the window taking focus again
    # brings the card up to date with the console's session; neither takes
    # away the link the reader is pressing or is on. Real clicks: one lands
    # only if the button pressed is still there when the mouse comes up.
    press('#api-credits', 'Edit credits and dates')
    use_config = lambda field: '#qc-credit-1-%s-caps button[aria-label^="Use config"]' % field
    browser('focus', '#qc-credit-1-refill')
    check("document.querySelector('#qc-credit-1-refill-caps .cap.help') !== null")
    browser('click', use_config('refill'))
    check("%s.includes('cleared') && document.activeElement === document.querySelector('#qc-credit-1-refill')" % text_of('#qc-credit-1-refill-caps'))
    press('#qc-credit-1-refill-caps', 'Undo')
    browser('focus', '#qc-credit-1-refill')
    browser('click', use_config('monthly'))
    check("%s.includes('back to config') && document.activeElement === document.querySelector('#qc-credit-1-monthly')" % text_of('#qc-credit-1-monthly-caps'))
    press('#qc-credit-1-monthly-caps', 'Undo')
    # From the keyboard: focus moves from the date to its Use config, then
    # the window takes focus again.
    def stays_on(link):
        browser('eval', "window.__link = document.querySelector(%s); window.__link.focus()" % json.dumps(link))
        check("document.activeElement === window.__link")
        browser('eval', "dispatchEvent(new Event('focus'))")
        check("document.activeElement === window.__link && window.__link.isConnected")
    browser('focus', '#qc-credit-1-refill')
    stays_on(use_config('refill'))
    check("%s === 'no changes'" % text_of('#api-credits .edit-foot .count'))
    press('#api-credits .edit-foot', 'Cancel')
    press('#renewals', 'Set renewal dates')
    stays_on('#qc-renewal-0-caps button[aria-label^="Use estimate"]')
    press('#renewals .edit-foot', 'Cancel')
    press('#organizations', 'Edit organizations')
    browser('fill', '#qc-org-0-label', 'alpha-two')
    stays_on('#qc-org-0-label-caps button')
    press('#organizations .edit-foot', 'Cancel')
    check("!document.querySelector('#api-credits form') && !document.querySelector('#renewals form') && !document.querySelector('#organizations form')")

    # Use config sends null; an orphan is cleared with every value null.
    press('#api-credits', 'Edit credits and dates')
    named_by_text('#api-credits')
    press('#api-credits', "Use config: monthly credit for o'neil & co")
    check("%s.includes('back to config')" % text_of('#api-credits'))
    press('#api-credits', 'Remove saved values for org-7c1e09a4d2b3')
    check("%s.includes('2 changes') && /^qc-credit-orphan-\\d+-undo$/.test(document.activeElement.id)" % text_of('#api-credits'))
    press('#api-credits', 'Save')
    post = posted(2, 'Use config did not send one request')
    assert post['body']['items'] == [{'id': org_account(org_id(2)), 'baseRevision': '12', 'monthlyUsd': None, 'renews': '2026-09-20', 'reading': {'remainingUsd': '180.00', 'at': iso(datetime.fromtimestamp(editor['credit_state']['api_credits']['accounts'][1]['settings']['reading']['atEpoch'], timezone.utc))}},
                                     {'id': 'org-7c1e09a4d2b3', 'baseRevision': '3', 'monthlyUsd': None, 'renews': None, 'reading': None}], post['body']
    check("!document.querySelector('#api-credits form')")

    # Values that cannot be sent are said under their field, and nothing goes.
    press('#api-credits', 'Edit credits and dates')
    browser('fill', '#qc-credit-0-monthly', '260,50')
    check("(t => t.includes('A comma is ambiguous. Did you mean 260.50 or 26,050.00?') && t.includes('1 to fix'))(%s) && document.querySelector('#api-credits button.save').disabled" % text_of('#api-credits'))
    press('#api-credits', 'Use 260.50')
    check("document.querySelector('#qc-credit-0-monthly').value === '260.50' && !document.querySelector('#api-credits button.save').disabled")
    # Without a session nothing is sent, even if the form is submitted anyway.
    browser('eval', "localStorage.removeItem('cli-proxy-auth')")
    browser('fill', '#qc-credit-0-monthly', '261')
    check("document.querySelector('#api-credits button.save').disabled && %s.includes('Sign in to CPA’s management console')" % text_of('#api-credits .edit-foot'))
    # A keystroke updates the foot in place: its alert and its count keep
    # their nodes, so a screen reader announces each once.
    browser('eval', "window.__foot = ['[role=alert]', '.count'].map(s => document.querySelector('#api-credits .edit-foot ' + s))")
    browser('fill', '#qc-credit-0-monthly', '2610')
    check("window.__foot.every(e => e.isConnected) && window.__foot[0] === document.querySelector('#api-credits .edit-foot [role=alert]') && window.__foot[1] === document.querySelector('#api-credits .edit-foot .count')")
    # So does a press, which renders the whole card afresh: only what changed
    # in them changes.
    browser('eval', "window.__readded = false; window.__watch = new MutationObserver(records => { window.__readded ||= records.some(r => [...r.addedNodes].some(n => window.__foot.includes(n))); }); window.__watch.observe(document.querySelector('#api-credits'), {childList: true, subtree: true})")
    press('#api-credits', 'Undo')
    check("window.__foot.every(e => e.isConnected) && window.__foot[0] === document.querySelector('#api-credits .edit-foot [role=alert]') && window.__foot[1] === document.querySelector('#api-credits .edit-foot .count') && window.__foot[1].textContent === 'no changes' && document.querySelector('#qc-credit-0-monthly').value !== '2610'")
    check("(() => { window.__watch.takeRecords().forEach(r => { window.__readded ||= [...r.addedNodes].some(n => window.__foot.includes(n)); }); window.__watch.disconnect(); return !window.__readded; })()")
    browser('eval', "document.querySelector('#api-credits form').requestSubmit()")
    time.sleep(.5)
    assert len(state['posts']) == 2, 'the editor saved without a remembered session'
    browser('eval', OBFUSCATED)
    browser('fill', '#qc-credit-0-monthly', '262')
    check("!document.querySelector('#api-credits button.save').disabled")

    # A refused key latches: one request, then Save stays off, nothing is
    # retried on a press or on the timer, and a reload does not present it.
    state['answers'] = [(401, {'error': 'invalid management key'}, {})]
    reads = state['reads']
    press('#api-credits', 'Save')
    posted(3, 'a refused save was not sent once')
    check("(t => t.includes('CPA refused the remembered management key, so nothing was saved'))(%s) && document.querySelector('#api-credits button.save').disabled" % text_of('#api-credits .edit-foot'))
    # Save cannot be pressed, so focus is on the foot's notice that says why.
    check("document.activeElement === document.querySelector('#qc-credits-notice')")
    check("JSON.parse(localStorage.getItem('%s')).reason === 'refused' && !document.querySelector('#error').hidden" % REFUSED)
    browser('eval', "document.querySelector('#api-credits form').requestSubmit()")
    ticks(3)
    assert len(state['posts']) == 3 and state['reads'] == reads, 'a refused key was presented again'
    press('#api-credits .edit-foot', 'Cancel')
    check("document.activeElement === document.querySelector('#qc-credits-edit')")
    check("(t => t.includes('then select Refresh view.') && !t.includes('save again'))(%s)" % text_of('#error'))
    ticks(3)
    assert state['reads'] == reads, 'the timer resumed after a refused save'
    browser('reload')
    check("document.querySelector('#error').textContent.includes('CPA refused the console session')")
    ticks(2)
    assert state['reads'] == reads and len(state['posts']) == 3, 'a reload presented the refused key'
    refreshed()
    check("!document.querySelector('#content').hidden && document.querySelector('#error').hidden && !document.querySelector('#api-credits').hidden")
    assert state['reads'] == reads + 1

    # A lost answer may or may not have saved: the page says so, stops its
    # timer, and sends again only on Try again.
    state['answers'] = ['drop']
    press('#api-credits', 'Edit credits and dates')
    browser('fill', '#qc-credit-0-monthly', '290')
    pick_date('#qc-credit-2-refill', '2026-10-15')
    press('#api-credits', 'Save')
    posted(4, 'a save whose answer was lost was not sent once')
    check("%s.includes('No answer came back, so this may or may not have saved.') && %s.includes('Try again') && document.activeElement === document.querySelector('#qc-credits-save')" % (text_of('#api-credits .edit-foot'), text_of('#api-credits button.save')))
    time.sleep(.5)
    assert len(state['posts']) == 4, 'a lost save was sent again on its own'
    state['answers'] = [(409, {'error': 'conflict', 'revision': '14', 'conflicts': [org_account(org_id(1))],
        'current': {org_account(org_id(1)): dict(editor['credit_state']['api_credits']['accounts'][0]['settings'], monthlyUsd='280', revision='14')}}, {})]
    press('#api-credits', 'Try again')
    post = posted(5, 'a conflicting save was not sent once')
    # The conflict takes the other device's value, says so under it, and
    # keeps the other row's change.
    check("(t => t.includes('Not saved. Another device saved first.') && t.includes('Changed from another device'))(%s) && document.querySelector('#qc-credit-0-monthly').value === '280' && document.querySelector('#qc-credit-2-refill').value === '2026-10-15'" % text_of('#api-credits'))
    check("%s.includes('Try again') && document.activeElement === document.querySelector('#qc-credits-save')" % text_of('#api-credits button.save'))
    reads = state['reads']
    press('#api-credits', 'Try again')
    post = posted(6, 'Try again did not send once')
    assert [item['id'] for item in post['body']['items']] == [org_account(org_id(3))], post['body']
    check("!document.querySelector('#api-credits form')")
    # The plugin answered, so the pause the lost answer put on the view ends.
    until(lambda: state['reads'] > reads, 'the view was not read again after a save that followed a lost answer')
    check("!document.querySelector('#view-status').textContent.includes('paused')")

    # Enter Console reading opens the editor on that row's reading.
    press('#api-credits', 'Enter Console reading for charlie')
    check("document.activeElement === document.querySelector('#qc-credit-2-amount')")
    browser('fill', '#qc-credit-2-amount', '31.80')
    check("%s.includes('Left in Console') && %s.includes('= ')" % (text_of('#api-credits'), text_of('#api-credits')))
    press('#api-credits .edit-foot', 'Cancel')

    # Quota Glance older than 0.8.0 is still followed: the note says so, and
    # the first save asks before it ends that.
    for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
        block.update(following=True, imported_at=iso(now - timedelta(minutes=30)))
    refreshed()
    check("(t => t.includes('Imported from Quota Glance') && t.includes('Quota Glance older than 0.8.0 still saves these values in its own file'))(%s)" % text_of('#import-note'))
    press('#api-credits', 'Edit credits and dates')
    browser('fill', '#qc-credit-0-monthly', '295')
    press('#api-credits', 'Save')
    check("%s.includes('Saving here stops picking up changes made in Quota Glance older than 0.8.0. Save anyway?')" % text_of('#api-credits'))
    # The question names its group and takes focus, so it is read out.
    check("document.activeElement === document.querySelector('#qc-credits-keep-editing') && document.getElementById(document.activeElement.closest('[role=group]').getAttribute('aria-labelledby')).textContent.endsWith('Save anyway?')")
    press('#api-credits', 'Keep editing')
    check("!document.querySelector('#api-credits .confirm') && document.activeElement === document.querySelector('#qc-credits-save')")
    press('#api-credits', 'Save')
    time.sleep(.3)
    assert len(state['posts']) == 6, 'the first save while following was sent before it was confirmed'
    press('#api-credits', 'Save anyway')
    posted(7, 'Save anyway did not send once')
    check("!document.querySelector('#api-credits form') && document.activeElement === document.querySelector('#qc-credits-edit')")
    for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
        block['following'] = False
    # That save ended follow mode, and importing again on the same page brings
    # it back: Save anyway answered that one save, so the next one asks again.
    refreshed()
    for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
        block['following'] = True
    refreshed()
    press('#api-credits', 'Edit credits and dates')
    browser('fill', '#qc-credit-0-monthly', '296')
    press('#api-credits', 'Save')
    check("%s.includes('Saving here stops picking up changes made in Quota Glance older than 0.8.0. Save anyway?')" % text_of('#api-credits'))
    time.sleep(.3)
    assert len(state['posts']) == 7, 'a save after follow mode came back was sent without asking'
    press('#api-credits', 'Keep editing')
    press('#api-credits .edit-foot', 'Cancel')
    check("!document.querySelector('#api-credits form')")
    for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
        block['following'] = False
    # A drop-in is a copy Quota Glance never saves to: the note says its later
    # saves are not picked up, never that this page follows them, and a copy
    # that changed is said to be the copy.
    own_file = editor['editor']['legacy']['source_path']
    for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
        block['source_path'] = '/CLIProxyAPI/plugins/data/quota-cache/quota-glance-settings.json'
    refreshed()
    check("(t => t.includes('These values came from a copy of Quota Glance’s settings.json') && t.includes('are not picked up') && !t.includes('picks up its changes'))(%s)" % text_of('#import-note'))
    for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
        block['state'] = 'changedSinceImport'
    refreshed()
    check("(t => t.includes('The copy of Quota Glance’s settings.json beside Quota Cache’s snapshot changed after your last save here.') && !t.includes('still edits it'))(%s)" % text_of('#import-note'))
    for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
        block.update(state='imported', source_path=own_file)

    # Every other import state: its line, its button and its confirmation,
    # which names what is saved here, which it replaces, and what Quota
    # Glance's file holds now, not what the last import brought; keeping sends
    # nothing.
    for legacy_state, line, button in (('notImported', 'has values that were not imported here', 'Import Quota Glance’s settings'),
                                       ('changedSinceImport', 'changed after your last save here', 'Import Quota Glance’s settings again'),
                                       ('settingsMissing', 'saved settings are gone', 'Import Quota Glance’s settings again')):
        for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
            block['state'] = legacy_state
        refreshed()
        check("(t => t.includes(%s))(%s)" % (json.dumps(line), text_of('#import-note')))
        press('#import-note', button)
        check("(t => t.includes('This replaces what is saved here:') && t.includes(%s) && t.includes('Import Quota Glance’s settings.json now?'))(%s)"
              % (json.dumps('It brings what Quota Glance’s settings.json holds now: 3 monthly credits, 1 refill date and 4 renewal dates.'), text_of('#import-note')))
        screenshot('editor-import-%s.png' % legacy_state)
        press('#import-note', 'Keep what is here')
        check("!%s.includes('This replaces')" % text_of('#import-note'))
    assert len(state['posts']) == 7, 'an import was sent without being confirmed'
    for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
        block['state'] = 'notImported'
    refreshed()
    check("%s.includes('has values that were not imported here')" % text_of('#import-note'))
    press('#import-note', 'Import Quota Glance’s settings')
    # The confirmation does not hold the view, and an update keeps it and
    # focus on it.
    browser('eval', "document.querySelector('#qc-import-go').focus()")
    tick_drawn()
    check("%s.includes('Import Quota Glance’s settings.json now?') && document.activeElement === document.querySelector('#qc-import-go')" % text_of('#import-note'))
    reads = state['reads']
    press('#import-note', 'Import')
    post = posted(8, 'the import was not sent once')
    assert post['body'] == {'kind': 'importLegacy', 'baseRevision': '12'}, post['body']
    check("%s.includes('Imported 2 monthly credits, 2 refill dates, 2 Console readings and 3 renewal dates from Quota Glance.')" % text_of('#import-note'))
    # The status read the import asks for takes the import button away and
    # brings Dismiss. Focus waits on the result line, which that read keeps,
    # rather than on the button.
    until(lambda: state['reads'] > reads, 'the view was not read again after the import')
    check("(t => t.includes('Imported from Quota Glance on') && t.includes('Imported 2 monthly credits'))(%s) && !document.querySelector('#qc-import-open') && document.querySelector('#qc-import-dismiss') && document.activeElement === document.querySelector('#qc-import-status')" % text_of('#import-note'))

    # Claude renewal dates: a date set, one back to the estimate, and a
    # leftover date named by its shortened auth index.
    press('#renewals', 'Set renewal dates')
    pick_date('#qc-renewal-1', '2026-11-05')
    named_by_text('#renewals')
    press('#renewals', 'Use estimate for foxtrot@example.com')
    press('#renewals', 'Remove the saved renewal date for 9c1e…a1a1')
    check("/^qc-renewal-orphan-\\d+-undo$/.test(document.activeElement.id)")
    check("(t => t.includes('back to the estimate') && t.includes('was ') && t.includes('estimated') && t.includes('9c1e…a1a1') && t.includes('removing') && t.includes('3 changes'))(%s)" % text_of('#renewals'))
    screenshot('editor-renewals.png')
    audit('editor-renewals')
    press('#renewals', 'Save')
    post = posted(9, 'the renewal save was not sent once')
    assert post['body'] == {'kind': 'renewals', 'items': [{'id': '5f2b8c41d09e7a36', 'baseRevision': '3', 'date': None}, {'id': '0a1b2c3d4e5f6071', 'baseRevision': '', 'date': '2026-11-05'},
                                                       {'id': '9c1e00000000a1a1', 'baseRevision': '2', 'date': None}]}, post['body']
    check("!document.querySelector('#renewals form') && %s.includes('Saved 3 renewal dates')" % text_of('#renewals'))

    # Organizations: rename, remove after confirming, add with the ID as it
    # will be saved; a conflict takes the other device's list and keeps the
    # add row.
    press('#organizations', 'Edit organizations')
    browser('fill', '#qc-org-0-label', 'alpha-one')
    press('#organizations', 'Remove charlie')
    check("%s.includes('Remove charlie? Its monthly credit from Quota Cache’s configuration goes with it, and adding the same Organization ID again does not bring it back. Amounts and readings entered here stay as an orphan you can clear below, and come back with it.')" % text_of('#organizations'))
    check("document.activeElement === document.querySelector('#qc-org-keep')")
    press('#organizations', 'Keep')
    check("!document.querySelector('#qc-org-keep') && document.activeElement.getAttribute('aria-label') === 'Remove charlie'")
    press('#organizations', 'Remove charlie')
    browser('eval', "document.querySelector('#organizations button.danger').click()")
    check("%s.includes('removing') && /^qc-org-\\d+-undo$/.test(document.activeElement.id)" % text_of('#organizations'))
    # The add row's fields say they are invalid exactly when their boxes
    # show it, as they are typed in and after the card is drawn again.
    invalid = lambda field, bad: check("(e => e.getAttribute('aria-invalid') === %s && e.parentElement.classList.contains('is-bad') === %s)(document.querySelector(%s))"
                                       % (json.dumps('true' if bad else 'false'), json.dumps(bad), json.dumps(field)))
    browser('fill', '#qc-org-add-label', 'Alpha-One')
    check("%s.includes('Another organization already has this label.') && document.querySelector('#organizations button.save').disabled" % text_of('#organizations'))
    invalid('#qc-org-add-label', True)
    browser('fill', '#qc-org-add-label', 'team-2')
    invalid('#qc-org-add-label', False)
    browser('fill', '#qc-org-add-id', '0000-1111')
    invalid('#qc-org-add-id', True)
    press('#organizations', 'Remove alpha')
    press('#organizations', 'Keep')
    invalid('#qc-org-add-id', True)
    browser('fill', '#qc-org-add-id', ' 00000000-0000-4000-8000-0000000000AA')
    invalid('#qc-org-add-id', False)
    check("(t => t.includes('Will be saved as 00000000-0000-4000-8000-0000000000aa') && t.includes('3 changes') && t.includes('Console → Settings → Organization. It is not a secret.'))(%s)" % text_of('#organizations'))
    screenshot('editor-organizations.png')
    audit('editor-organizations')
    elsewhere = copy.deepcopy(editor['editor']['organizations']['items']) + [{'id': org_account(org_id(4)), 'label': 'delta', 'organization_id': org_id(4), 'has_default_monthly_usd': False, 'has_default_renews': False}]
    editor['editor']['organizations'].update(items=elsewhere, revision='5')
    press('#organizations', 'Save')
    post = posted(10, 'the organizations save was not sent once')
    assert post['body'] == {'kind': 'organizations', 'baseRevision': '4', 'ops': [{'op': 'remove', 'id': org_account(org_id(3))}, {'op': 'rename', 'id': org_account(org_id(1)), 'label': 'alpha-one'},
                                                                              {'op': 'add', 'label': 'team-2', 'organizationId': '00000000-0000-4000-8000-0000000000aa'}]}, post['body']
    check("(t => t.includes('Another device changed the organizations first') && t.includes('1 change'))(%s) && document.querySelector('#qc-org-3-label').value === 'delta' && document.querySelector('#qc-org-0-label').value === 'alpha' && document.querySelector('#qc-org-add-label').value === 'team-2'" % text_of('#organizations'))
    press('#organizations', 'Try again')
    post = posted(11, 'the organizations retry was not sent once')
    assert post['body'] == {'kind': 'organizations', 'baseRevision': '5', 'ops': [{'op': 'add', 'label': 'team-2', 'organizationId': '00000000-0000-4000-8000-0000000000aa'}]}, post['body']
    check("!document.querySelector('#organizations form') && %s.includes('Saved 1 change')" % text_of('#organizations'))
    check("%s.includes('team-2')" % text_of('#organizations tbody'))
    # A note's Dismiss puts focus on what followed the note: the next note,
    # then the table.
    browser('focus', '#qc-orgs-moved-dismiss')
    browser('press', 'Enter')
    check("!%s.includes('Moved 3 organizations') && document.activeElement === document.querySelector('#qc-orgs-config-dismiss')" % text_of('#organizations'))
    browser('press', 'Enter')
    check("!%s.includes('still lists organizations under claude-api-credits') && document.activeElement === document.querySelector('#qc-orgs-table')" % text_of('#organizations'))

    # Strings CPA escaped below schema 6 render as they were meant.
    state['escaped'] = True
    refreshed()
    check("(t => t.includes(\"o'neil & co\") && t.includes(\"Quota Cache's meter was full & dropped records\") && !t.includes('&#39;') && !t.includes('&amp;'))(%s)" % text_of('#content'))
    state['escaped'] = False

    # Unreadable files: the cards are read-only and say why and what to do.
    editor['editor']['organizations'].update(available=False, reason='organizationsUnreadable', items=[])
    editor['editor'].update(available=False, reason='settingsUnreadable')
    for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
        block.update(state='legacyUnreadable', importable=False)
    refreshed()
    # The settings card offers the import once the file is moved aside, so
    # the note says Quota Glance's file rules that out.
    check("(t => t.includes('Quota Glance’s settings.json could not be read, so it cannot be imported again.'))(%s) && !document.querySelector('#import-note').hidden" % text_of('#import-note'))
    # Fixing the file comes first: moving it aside drops what was changed here.
    check("(t => t.includes('Quota Cache cannot read its organizations file') && t.includes('Fix snapshot.organizations.json in place, or put back a good copy') && t.includes('Moving it aside instead moves the configuration’s list in again and drops the organization changes made here'))(%s) && !document.querySelector('#organizations .head-actions')" % text_of('#organizations'))
    check("(t => !t.includes('No Console organizations yet') && t.includes('4 Claude Console organizations from Quota Cache’s configuration') && t.includes('charlie') && t.includes('no Organization ID') && t.includes('cannot be changed here'))(%s) && !document.querySelector('#organizations button:not(.link)')" % text_of('#organizations'))
    # A good copy put back comes first, and importing again brings only what
    # Quota Glance's file holds.
    check("(t => t.includes('Quota Cache cannot read its saved settings, so only config values apply. To keep the values saved here, put back a good copy of snapshot.settings.json. Otherwise move it aside: Quota Cache then starts empty') && t.includes('brings back only what Quota Glance’s settings.json holds') && !t.includes('save Quota Cache'))(%s) && !document.querySelector('#api-credits .head-actions') && !document.querySelector('#renewals .head-actions')" % text_of('#api-credits'))
    # A view update keeps each alert's node and the import note's, so a
    # screen reader announces each once, not at every update.
    kept = "[...['#organizations', '#api-credits', '#renewals'].map(s => document.querySelector(s + ' .alert.error')), document.querySelector('#import-note').firstChild]"
    browser('eval', kept + ".forEach(node => { node.kept = true })")
    refreshed()
    check(kept + ".every(node => node.isConnected && node.kept === true)")
    # With no import on record, moving the settings file aside is a first
    # start, not an empty store, so the remedy says so.
    for block in (editor['editor']['legacy'], editor['credit_state']['settings']['import']):
        block.update(kind=None)
    refreshed()
    check("(t => t.includes('To keep the values saved here, put back a good copy of snapshot.settings.json. Moving it aside instead starts Quota Cache empty once an import from Quota Glance is on record, and otherwise counts as a first start') && !t.includes('then starts empty'))(%s)" % text_of('#api-credits'))
    editor['editor'].update(reason='legacyUnreadable')
    refreshed()
    check("%s.includes('Quota Glance’s settings.json could not be read, so nothing was imported and only config values apply. Fix it or move it aside.')" % text_of('#renewals'))
    # When that file is what blocks editing, the cards say so and the note does not.
    check("document.querySelector('#import-note').hidden")
    screenshot('editor-unavailable.png')
    editor = editor_state()

    # With the last organization removed api_credits is null; what was saved
    # for it is still listed, from the editor, and can be cleared.
    alpha = org_account(org_id(1))
    editor['credit_state']['api_credits'] = None
    editor['editor']['organizations'].update(items=[])
    editor['editor']['api_credit_orphans'] = [{'id': alpha, 'monthlyUsd': '275', 'renews': '', 'hasReading': False, 'revision': '13', 'updatedAtEpoch': epoch(now)}]
    refreshed()
    check("(t => t.includes('No Claude Console organizations yet') && t.includes('Saved values for an organization no longer listed under Organizations: %s') && t.includes('Edit credits and dates'))(%s)" % (alpha, text_of('#api-credits')))
    press('#api-credits', 'Edit credits and dates')
    press('#api-credits', 'Remove saved values for ' + alpha)
    # Saved from the keyboard, the card puts focus on its Edit button, which
    # the status the save asked for takes away with the orphan. Focus then
    # goes to the card's heading, never to the top of the page.
    browser('focus', '#qc-credits-save')
    browser('press', 'Enter')
    post = posted(12, 'clearing an orphan with no organization listed was not sent once')
    assert post['body'] == {'kind': 'apiCredits', 'items': [{'id': alpha, 'baseRevision': '13', 'monthlyUsd': None, 'renews': None, 'reading': None}]}, post['body']
    check("!document.querySelector('#api-credits form') && !%s.includes('Saved values for an organization') && !document.querySelector('#qc-credits-edit') && document.activeElement === document.querySelector('#api-credits-heading')" % text_of('#api-credits'))
    editor = editor_state()
    refreshed()

    # A save refused by something in front of CPA, not for the key: Save
    # stays on, nothing is sent again on its own, and the next press is the
    # one deliberate try, which clears the refusal and reads the view again.
    reads = state['reads']
    state['answers'] = [(403, {'error': 'Forbidden by the proxy'}, {})]
    press('#api-credits', 'Edit credits and dates')
    browser('fill', '#qc-credit-0-monthly', '263')
    press('#api-credits', 'Save')
    posted(13, 'a save a proxy refused was not sent once')
    check("(t => t.includes('Something in front of CPA refused the save (HTTP 403)') && t.includes('Select Save to try once more.'))(%s) && !document.querySelector('#api-credits button.save').disabled" % text_of('#api-credits .edit-foot'))
    check("(t => t.includes('refused the save (HTTP 403)') && t.includes('Save again to try once more.') && !t.includes('Refresh view'))(%s)" % text_of('#error'))
    # Once the card is cancelled Refresh view is the way to try again, and the
    # banner says so; a card opened again makes its Save the way once more.
    press('#api-credits', 'Cancel')
    check("(t => t.includes('refused the request (HTTP 403)') && t.includes('Select Refresh view to try once more.') && !t.includes('Save again'))(%s) && !document.querySelector('#refresh').disabled" % text_of('#error'))
    press('#api-credits', 'Edit credits and dates')
    check("(t => t.includes('refused the save (HTTP 403)') && t.includes('Save again to try once more.'))(%s) && document.querySelector('#refresh').disabled" % text_of('#error'))
    browser('fill', '#qc-credit-0-monthly', '263')
    ticks(3)
    assert len(state['posts']) == 13 and state['reads'] == reads, 'a refused save was sent again on its own'
    press('#api-credits', 'Save')
    posted(14, 'Save after a proxy refusal did not send once')
    check("!document.querySelector('#api-credits form') && localStorage.getItem('%s') === null" % REFUSED)
    until(lambda: state['reads'] > reads, 'the view was not read again after the save that followed a refusal')
    check("document.querySelector('#error').hidden && document.querySelector('#health').textContent !== 'Session refused'")

    # A lockout met by a save keeps Save off until it ends, then turns it back
    # on by itself, with the draft kept.
    state['answers'] = [(403, {'error': 'IP banned due to too many failed attempts. Try again in 0m2s'}, {})]
    press('#api-credits', 'Edit credits and dates')
    browser('fill', '#qc-credit-0-monthly', '264')
    press('#api-credits', 'Save')
    posted(15, 'a save met by a lockout was not sent once')
    check("(t => t.includes('until about') && t.includes('Save turns back on then.'))(%s) && document.querySelector('#api-credits button.save').disabled" % text_of('#api-credits .edit-foot'))
    check("(t => t.includes('should be over') && t.includes('Select Save to try once more.'))(%s) && !document.querySelector('#api-credits button.save').disabled && document.querySelector('#qc-credit-0-monthly').value === '264'" % text_of('#api-credits .edit-foot'))
    assert len(state['posts']) == 15, 'a save was sent when the lockout ended'
    press('#api-credits', 'Save')
    posted(16, 'Save after the lockout did not send once')
    check("!document.querySelector('#api-credits form')")
    refreshed()

    # A status read already under way when a card opens, which then gets no
    # answer, hides the view and the card with it. Refresh view stays on, and
    # the card comes back with its draft.
    state['mode'] = 'slowdrop'
    ticks(1)
    press('#api-credits', 'Edit credits and dates')
    browser('fill', '#qc-credit-0-monthly', '266')
    check("document.querySelector('#content').hidden && !document.querySelector('#refresh').disabled && %s.includes('select Refresh view')" % text_of('#error'))
    state['mode'] = 'editor'
    reads = state['reads']
    browser('click', '#refresh')
    until(lambda: state['reads'] > reads, 'Refresh view did not read the status')
    check("!document.querySelector('#content').hidden && document.querySelector('#qc-credit-0-monthly').value === '266' && document.querySelector('#refresh').disabled")
    press('#api-credits .edit-foot', 'Cancel')
    check("!document.querySelector('#refresh').disabled")

    # A row another device took away: the plugin refuses it by id, and the
    # page drops that change by name, keeps the other, and sends only it next.
    orphan = 'org-7c1e09a4d2b3'
    remove_orphan = 'Remove saved values for ' + orphan
    press('#api-credits', 'Edit credits and dates')
    browser('fill', '#qc-credit-0-monthly', '267')
    pick_date('#qc-credit-2-refill', '2026-10-16')
    press('#api-credits', remove_orphan)
    state['answers'] = [(409, {'error': 'not_editable', 'id': org_account(org_id(1)), 'ids': [org_account(org_id(1)), orphan]}, {})]
    press('#api-credits', 'Save')
    posted(17, 'a save the plugin refused rows of was not sent once')
    offered = "[...document.querySelectorAll('#api-credits button')].some(b => b.getAttribute('aria-label') === %s)" % json.dumps(remove_orphan)
    check("(t => t.includes('Not saved. alpha and %s can no longer be edited here, so their changes were dropped. Your other changes are kept.') && t.includes('no longer offers this organization'))(%s) && !document.querySelector('#qc-credit-0-monthly') && !%s && %s.includes('Try again')"
          % (orphan, text_of('#api-credits'), offered, text_of('#api-credits button.save')))
    press('#api-credits', 'Try again')
    post = posted(18, 'the save after a refusal was not sent once')
    assert [item['id'] for item in post['body']['items']] == [org_account(org_id(3))], post['body']
    check("!document.querySelector('#api-credits form')")
    # An orphan another device stored again conflicts; marked again, its
    # removal sends the revision it is stored at now.
    press('#api-credits', 'Edit credits and dates')
    press('#api-credits', remove_orphan)
    state['answers'] = [(409, {'error': 'conflict', 'revision': '20', 'conflicts': [orphan], 'current': {orphan: {'revision': '20', 'monthlyUsd': '50', 'renews': '', 'reading': None}}}, {})]
    press('#api-credits', 'Save')
    posted(19, 'a save that conflicted on an orphan was not sent once')
    check("%s.includes('Not saved. Another device saved first.') && %s" % (text_of('#api-credits .edit-foot'), offered))
    press('#api-credits', remove_orphan)
    press('#api-credits', 'Save')
    post = posted(20, 'the orphan removal was not sent again once')
    assert post['body'] == {'kind': 'apiCredits', 'items': [{'id': orphan, 'baseRevision': '20', 'monthlyUsd': None, 'renews': None, 'reading': None}]}, post['body']
    check("!document.querySelector('#api-credits form')")

    # While the organizations file cannot be read, values saved for an
    # organization the configuration lacks are shown as kept, never offered
    # for removal.
    held = 'org-0123456789ab'
    editor['credit_state']['organizations'].update(state='unreadable', source='config')
    editor['editor']['organizations'].update(available=False, reason='organizationsUnreadable')
    editor['editor']['held_api_credits'] = [{'id': held, 'monthlyUsd': '40', 'renews': '', 'hasReading': False, 'revision': '9', 'updatedAtEpoch': epoch(now)}]
    refreshed()
    kept = 'Saved values for an organization Quota Cache’s configuration does not list: %s · kept, and used again once Quota Cache’s organizations file is fixed or a good copy put back' % held
    check("%s.includes(%s)" % (text_of('#api-credits'), json.dumps(kept)))
    check("(t => t.includes('The organizations come from Quota Cache’s configuration until it can read its organizations file again.') && !t.includes('and organizations above'))(%s)" % text_of('#api-credits'))
    press('#api-credits', 'Edit credits and dates')
    check("%s.includes(%s) && ![...document.querySelectorAll('#api-credits button')].some(b => (b.getAttribute('aria-label') || '').includes(%s))" % (text_of('#api-credits form'), json.dumps(kept), json.dumps(held)))
    # The locked row and the foot point to what can be done while the
    # Organizations card cannot edit, never to that card.
    check("(t => t.includes('no usable organization-id. Organizations can be added above again once Quota Cache can read its organizations file.') && !t.includes('Add the organization under Organizations above') && t.includes('Labels and Organization IDs come from Quota Cache’s configuration until it can read its organizations file again.'))(%s)" % text_of('#api-credits'))
    press('#api-credits .edit-foot', 'Cancel')
    editor = editor_state()

    # A reading's time is judged by the page's rule, 48 hours back to 5
    # minutes ahead, never by the bounds its control was drawn with: a reading
    # taken a couple of minutes after the card opened saves, and so does a row
    # whose reading in use, older than the control's lower bound, is sent back
    # as it was.
    refreshed()
    press('#api-credits', 'Enter Console reading for charlie')
    browser('fill', '#qc-credit-2-amount', '31.80')
    browser('eval', "(e => { const at = new Date(Date.now() + 120000), pad = n => String(n).padStart(2, '0'); e.value = at.getFullYear() + '-' + pad(at.getMonth() + 1) + '-' + pad(at.getDate()) + 'T' + pad(at.getHours()) + ':' + pad(at.getMinutes()); e.dispatchEvent(new Event('input', {bubbles: true})); })(document.querySelector('#qc-credit-2-at'))")
    check("%s.includes('1 change') && !document.querySelector('#api-credits button.save').disabled" % text_of('#api-credits .edit-foot'))
    press('#api-credits', 'Save')
    post = posted(21, 'a reading taken after the card opened was not sent once')
    assert [(item['id'], item['reading']['remainingUsd']) for item in post['body']['items']] == [(org_account(org_id(3)), '31.80')], post['body']
    check("!document.querySelector('#api-credits form')")
    editor = editor_state()
    old_reading = epoch((now - timedelta(hours=50)).replace(second=0, microsecond=0))
    editor['credit_state']['api_credits']['accounts'][1]['settings']['reading'] = {'remainingUsd': '180.00', 'atEpoch': old_reading, 'enteredAtEpoch': old_reading}
    refreshed()
    press('#api-credits', 'Edit credits and dates')
    press('#api-credits', "Change the Console reading for o'neil & co")
    named_by_text('#api-credits')
    check("document.querySelector('#qc-credit-1-reading-cancel').textContent === 'Keep the saved one'")
    browser('fill', '#qc-credit-1-monthly', '270')
    check("document.querySelector('#qc-credit-1-at').validity.rangeUnderflow && %s.includes('1 change')" % text_of('#api-credits .edit-foot'))
    press('#api-credits', 'Save')
    post = posted(22, 'a row whose reading is older than the time control allows was not sent once')
    assert post['body']['items'] == [{'id': org_account(org_id(2)), 'baseRevision': '12', 'monthlyUsd': '270', 'renews': '2026-09-20',
                                      'reading': {'remainingUsd': '180.00', 'at': iso(datetime.fromtimestamp(old_reading, timezone.utc))}}], post['body']
    check("!document.querySelector('#api-credits form')")
    editor = editor_state()

    # A label is counted in characters, never in the UTF-16 units a browser's
    # maxlength counts: a rename to 39 emoji, 78 units, saves.
    refreshed()
    press('#organizations', 'Edit organizations')
    browser('eval', "(e => { e.focus(); e.value = '\\u{1F600}'.repeat(40); e.dispatchEvent(new Event('input', {bubbles: true})); })(document.querySelector('#qc-org-0-label'))")
    browser('press', 'End')
    browser('press', 'Backspace')
    check("document.querySelector('#qc-org-0-label').value.length === 78 && %s.includes('1 change')" % text_of('#organizations .edit-foot'))
    press('#organizations', 'Save')
    post = posted(23, 'a rename to 39 emoji was not sent once')
    assert post['body'] == {'kind': 'organizations', 'baseRevision': '4', 'ops': [{'op': 'rename', 'id': org_account(org_id(1)), 'label': '\U0001F600' * 39}]}, post['body']
    check("!document.querySelector('#organizations form')")
    editor = editor_state()

    # A rename made against one revision and carried onto a newer status
    # while the card is open still goes out at the revision it was made
    # against, so another device's rename meets a conflict instead of being
    # overwritten, and the dropped rename is named.
    refreshed()
    state['mode'] = 'slowdrop'
    ticks(1)
    press('#organizations', 'Edit organizations')
    browser('fill', '#qc-org-0-label', 'alpha-mine')
    check("document.querySelector('#content').hidden && !document.querySelector('#refresh').disabled")
    theirs = copy.deepcopy(editor['editor']['organizations']['items'])
    theirs[0]['label'] = 'alpha-theirs'
    editor['editor']['organizations'].update(items=theirs, revision='5')
    state['mode'] = 'editor'
    reads = state['reads']
    browser('click', '#refresh')
    until(lambda: state['reads'] > reads, 'Refresh view did not read the status')
    check("!document.querySelector('#content').hidden && document.querySelector('#qc-org-0-label').value === 'alpha-mine'")
    press('#organizations', 'Save')
    post = posted(24, 'a rename carried onto a newer status was not sent once')
    assert post['body'] == {'kind': 'organizations', 'baseRevision': '4', 'ops': [{'op': 'rename', 'id': org_account(org_id(1)), 'label': 'alpha-mine'}]}, post['body']
    check("%s.includes('so your change to alpha was dropped') && document.querySelector('#qc-org-0-label').value === 'alpha-theirs'" % text_of('#organizations'))
    press('#organizations .edit-foot', 'Cancel')
    check("!document.querySelector('#organizations form')")
    editor = editor_state()

    # The renewal card whose only row is a saved date for an account no
    # longer in CPA has nothing to type in: opening it puts focus on that
    # date's Remove.
    editor['editor']['credentials'] = []
    last_date = copy.deepcopy(editor['editor']['renewal_orphans'][0])
    refreshed()
    press('#renewals', 'Set renewal dates')
    check("/^qc-renewal-orphan-\\d+-remove$/.test(document.activeElement.id)")
    # Removing it from the keyboard leaves the card nothing to edit once the
    # status the save asked for arrives, so focus goes to its heading.
    browser('press', 'Enter')
    browser('focus', '#qc-renewals-save')
    browser('press', 'Enter')
    post = posted(25, 'removing the last renewal date was not sent once')
    assert post['body'] == {'kind': 'renewals', 'items': [{'id': last_date['id'], 'baseRevision': last_date['revision'], 'date': None}]}, post['body']
    check("!document.querySelector('#renewals form') && %s.includes('No Claude subscriptions in CPA yet') && document.activeElement === document.querySelector('#renewals-heading')" % text_of('#renewals'))
    editor = editor_state()

    # The first organization saved, from the keyboard: the card puts focus on
    # Add organization, which the status the save asked for replaces with
    # Edit organizations, so focus goes there. Removing the last one does the
    # reverse.
    editor['editor']['organizations'].update(items=[])
    refreshed()
    browser('focus', '#qc-orgs-add')
    browser('press', 'Enter')
    browser('fill', '#qc-org-add-label', 'lima')
    browser('fill', '#qc-org-add-id', org_id(6))
    browser('focus', '#qc-orgs-save')
    browser('press', 'Enter')
    post = posted(26, 'the first organization was not sent once')
    assert post['body'] == {'kind': 'organizations', 'baseRevision': '4', 'ops': [{'op': 'add', 'label': 'lima', 'organizationId': org_id(6)}]}, post['body']
    check("!document.querySelector('#organizations form') && %s.includes('lima') && document.activeElement === document.querySelector('#qc-orgs-edit')" % text_of('#organizations'))
    browser('press', 'Enter')
    press('#organizations', 'Remove lima')
    browser('eval', "document.querySelector('#organizations button.danger').click()")
    browser('focus', '#qc-orgs-save')
    browser('press', 'Enter')
    post = posted(27, 'removing the last organization was not sent once')
    assert post['body'] == {'kind': 'organizations', 'baseRevision': '5', 'ops': [{'op': 'remove', 'id': org_account(org_id(6))}]}, post['body']
    check("!document.querySelector('#organizations form') && %s.includes('No Console organizations yet') && document.activeElement === document.querySelector('#qc-orgs-add')" % text_of('#organizations'))
    editor = editor_state()

    # Organizations listed, none counted and none with a monthly credit: the
    # button reads Set credits and dates, and the orphan line names it.
    credit_doc = editor['credit_state']['api_credits']
    credit_doc['accounts'] = [account(0, 'kilo', org_id(5), 'needsSettings', estimate=False)]
    credit_doc['pool'].update(hasEstimate=False, countedCount=0, accountCount=1)
    refreshed()
    check("(t => t.includes('Saved values for an organization no longer listed under Organizations: org-7c1e09a4d2b3 · clear them under Set credits and dates') && !t.includes('Edit credits and dates'))(%s) && document.querySelector('#qc-credits-edit').textContent === 'Set credits and dates'" % text_of('#api-credits'))
    check("%s === '1 organization · none counted yet: set each one’s monthly credit and refill date'" % text_of('#api-credits .section-head .hint'))
    # One with its credit and date, waiting on the meter: the head says none
    # is counted and leaves why to the row.
    credit_doc['accounts'] = [account(0, 'kilo', org_id(5), 'pending', config_monthly='200', config_renews='2026-09-29', estimate=False,
                                      issue='Quota Cache has not saved its API meter yet; counting starts when it next saves.', issues=['meterMissing'])]
    refreshed()
    check("%s === '1 organization · none counted yet' && %s.includes('Quota Cache has not saved its API meter yet')" % (text_of('#api-credits .section-head .hint'), text_of('#api-credits')))
    editor = editor_state()

    # A value Quota Cache refuses at a save is marked on its field as one the
    # page refuses is: a red box, aria-invalid, and the reason read with the
    # field. Typing in it again takes the mark away. A reading's time is
    # judged by CPA's clock, which this device's may not match, so the page
    # cannot always see that refusal coming.
    refreshed()
    def marked_bad(selector, reason):
        described = "(e.getAttribute('aria-describedby') || '').split(' ').map(id => (document.getElementById(id) || {}).textContent || '').join(' ')"
        return "(e => e.getAttribute('aria-invalid') === 'true' && e.parentElement.classList.contains('is-bad') && %s.includes(%s))(document.querySelector(%s))" % (described, json.dumps(reason), json.dumps(selector))
    def unmarked(selector):
        return "(e => e.getAttribute('aria-invalid') !== 'true' && !e.parentElement.classList.contains('is-bad'))(document.querySelector(%s))" % json.dumps(selector)
    state['answers'] = [(400, {'error': 'invalid_monthly_usd', 'id': org_account(org_id(1)), 'field': 'monthlyUsd'}, {})]
    press('#api-credits', 'Edit credits and dates')
    browser('fill', '#qc-credit-0-monthly', '268')
    press('#api-credits', 'Save')
    posted(28, 'a save the plugin refused a monthly credit of was not sent once')
    check("%s.includes('Not saved. Fix the marked field.') && %s" % (text_of('#api-credits .edit-foot'), marked_bad('#qc-credit-0-monthly', 'Enter a dollar amount, like 200 or 260.50.')))
    audit('editor-credits-refused')
    browser('fill', '#qc-credit-0-monthly', '269')
    check(unmarked('#qc-credit-0-monthly'))
    press('#api-credits .edit-foot', 'Cancel')
    state['answers'] = [(400, {'error': 'invalid_reading_time', 'id': org_account(org_id(3)), 'field': 'reading.at'}, {})]
    press('#api-credits', 'Enter Console reading for charlie')
    browser('fill', '#qc-credit-2-amount', '31.80')
    press('#api-credits', 'Save')
    posted(29, 'a save the plugin refused a reading time of was not sent once')
    check("%s && %s" % (marked_bad('#qc-credit-2-at', 'by CPA’s clock'), unmarked('#qc-credit-2-amount')))
    press('#api-credits .edit-foot', 'Cancel')
    state['answers'] = [(400, {'error': 'invalid_date', 'id': '0a1b2c3d4e5f6071', 'field': 'date'}, {})]
    press('#renewals', 'Set renewal dates')
    pick_date('#qc-renewal-1', '2026-11-06')
    press('#renewals', 'Save')
    posted(30, 'a save the plugin refused a renewal date of was not sent once')
    check("%s.includes('Not saved. Fix the marked field.') && %s" % (text_of('#renewals .edit-foot'), marked_bad('#qc-renewal-1', 'Enter a real date between 2000 and 2099.')))
    audit('editor-renewals-refused')
    pick_date('#qc-renewal-1', '2026-11-07')
    check(unmarked('#qc-renewal-1'))
    press('#renewals .edit-foot', 'Cancel')

    # Dark and narrow, while editing.
    refreshed()
    browser('set', 'media', 'dark')
    press('#api-credits', 'Edit credits and dates')
    screenshot('editor-dark.png')
    audit('editor-dark')
    browser('set', 'viewport', '390', '844')
    check('document.documentElement.scrollWidth <= innerWidth')
    screenshot('editor-mobile.png')
    press('#api-credits .edit-foot', 'Cancel')
    assert browser('errors').get('errors', []) == [], 'the page threw'
    assert len(state['posts']) == 30 and all(p['auth'] == 'Bearer synthetic-sidebar-key' and p['type'] == 'application/json' for p in state['posts']), state['posts']

try:
    if args.serve_only:
        state['mode'] = args.mode
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
    check("!document.querySelector('#refresh-round').hidden && document.querySelector('#refresh-round').textContent === 'Refreshing after a banked reset: 2 left'")
    # The line is a live region: an update with the same count keeps its text
    # node, so a screen reader announces the count once.
    browser('eval', "document.querySelector('#refresh-round').firstChild.kept = true")
    before = state['reads']
    ticks(1)
    assert state['reads'] == before+1, 'the automatic update did not run with an accepted session'
    check("!document.querySelector('#refresh').disabled && document.querySelector('#refresh-round').firstChild.kept === true")
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
    check("document.querySelector('#refresh-round').hidden")
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
    editor_checks()
    assert state['keyless'] == 0, 'the page sent a management request without a key'
    print('PASS: native sidebar, strict session reading, no keyless requests, refusal and lockout stop with a persistent record (kept per CPA prefix, and in the tab when the origin storage is full), no automatic retry after a request gets no answer, provider filters, Claude API credits, cooldown/history, empty/error states, safe text rendering, light/dark/mobile, cache-only view refresh; editor: organizations, API credits, renewal dates, import notes, saves only from a press with the remembered key, no retry after a refusal or a lost answer, conflicts (organizations across a newer status too), fields judged by the page and never blocked by the browser, a field Quota Cache refused marked on the field and read with it, the drop-in import note, focus on opening every card, keyboard focus never hidden under a pinned foot, caption links kept while focus moves and the window takes focus again, escaped strings')
finally:
    if not args.serve_only:
        subprocess.run([binary,'--session',session,'close'],stdout=subprocess.DEVNULL,check=False,timeout=30)
    server.shutdown()
    native['api'] = api
    api.shutdown()
    tmp.cleanup()
