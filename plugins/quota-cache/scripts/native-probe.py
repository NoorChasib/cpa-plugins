#!/usr/bin/env python3
"""Exercise the real shared library with synthetic host callbacks; no provider traffic."""
import base64
import ctypes as c
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import sys
import tempfile
import time

class Buffer(c.Structure):
    _fields_ = [('ptr', c.c_void_p), ('len', c.c_size_t)]
HostCall = c.CFUNCTYPE(c.c_int, c.c_void_p, c.c_char_p, c.c_void_p, c.c_size_t, c.POINTER(Buffer))
Free = c.CFUNCTYPE(None, c.c_void_p, c.c_size_t)
Call = c.CFUNCTYPE(c.c_int, c.c_char_p, c.c_void_p, c.c_size_t, c.POINTER(Buffer))
Shutdown = c.CFUNCTYPE(None)
class Host(c.Structure):
    _fields_ = [('abi', c.c_uint32), ('ctx', c.c_void_p), ('call', HostCall), ('free', Free)]
class API(c.Structure):
    _fields_ = [('abi', c.c_uint32), ('call', Call), ('free', Free), ('shutdown', Shutdown)]

buffers = {}
# http counts every provider request; usage and profile split it, so the probe
# can tell a poll from the account-detail read that rides on its first success.
counts = {'http': 0, 'usage': 0, 'profile': 0}
mode = {'status': 200}
PROFILE_URL = 'https://api.anthropic.com/api/oauth/profile'
# The warm-up phases list one idle Claude subscription under a real-shaped auth
# index and answer the model callbacks; cancel is off for a host before
# CPA v7.3.16, which does not know host.http.cancel.
WARM_INDEX, WARM_ID = '00000000000000aa', 'claude-synthetic@example.com.json'
warm = {'on': False, 'cancel': True, 'calls': [], 'requests': []}
def warm_entry():
    return {'id': WARM_ID, 'auth_index': WARM_INDEX, 'name': 'synthetic.json', 'provider': 'claude', 'type': 'claude',
            'source': 'file', 'email': 'synthetic@example.com', 'account_type': 'oauth', 'account': 'synthetic@example.com'}
@HostCall
def host_call(ctx, method, request, length, response):
    try:
        method = method.decode()
        if method == 'host.auth.list':
            result = {'files': [warm_entry() if warm['on'] else {'auth_index': 'synthetic-one', 'name': 'synthetic.json', 'provider': 'claude', 'source': 'file'}]}
        elif method == 'host.auth.get_runtime' and warm['on']:
            assert json.loads(c.string_at(request, length)) == {'auth_index': WARM_INDEX}
            warm['calls'].append(method)
            result = {'auth': warm_entry()}
        elif method == 'host.http.cancel' and warm['on'] and warm['cancel']:
            assert json.loads(c.string_at(request, length))['operation_id'].startswith('quota-cache-warmup-probe-')
            warm['calls'].append(method)
            result = {}
        elif method == 'host.model.execute' and warm['on']:
            warm['calls'].append(method)
            warm['requests'].append(json.loads(c.string_at(request, length)))
            message = {'id': 'msg_synthetic', 'type': 'message', 'role': 'assistant', 'content': [{'type': 'text', 'text': 'OK'}]}
            result = {'status_code': 200, 'headers': {}, 'body': base64.b64encode(json.dumps(message).encode()).decode()}
        elif method == 'host.auth.get':
            result = {'json': {'access_token': 'synthetic-not-a-real-token'}}
        elif method == 'host.http.do':
            counts['http'] += 1
            url = json.loads(c.string_at(request, length))['url']
            if url == PROFILE_URL:
                counts['profile'] += 1
                body = {'organization': {'organization_type': 'claude_max', 'rate_limit_tier': 'default_claude_max_20x'}}
            else:
                assert url.startswith('https://api.anthropic.com/api/oauth/usage'), url
                counts['usage'] += 1
                body = {'seven_day': {'utilization': 42, 'resets_at': (datetime.now(timezone.utc)+timedelta(days=3)).isoformat()}}
                body['five_hour'] = {'utilization': 18, 'resets_at': (datetime.now(timezone.utc)+timedelta(hours=2)).isoformat()}
                if warm['on']:
                    body['five_hour'] = {'utilization': 0, 'resets_at': None}
                body['extra_usage'] = {'is_enabled': False, 'monthly_limit': 0}
            result = {'StatusCode': mode['status'], 'Headers': {'Retry-After': ['3600']}, 'Body': base64.b64encode(json.dumps(body).encode()).decode()}
        elif method == 'host.log':
            result = {}
        else:
            raise AssertionError('unexpected host callback')
        raw = json.dumps({'ok': True, 'result': result}).encode()
        buf = c.create_string_buffer(raw)
        ptr = c.addressof(buf)
        buffers[ptr] = buf
        response.contents.ptr, response.contents.len = ptr, len(raw)
        return 0
    except Exception:
        return 1
@Free
def host_free(ptr, length):
    buffers.pop(ptr, None)

library = c.CDLL(sys.argv[1])
library.cliproxy_plugin_init.argtypes = [c.POINTER(Host), c.POINTER(API)]
library.cliproxy_plugin_init.restype = c.c_int
host = Host(1, None, host_call, host_free)

def call(api, method, value=None):
    raw = json.dumps(value or {}).encode()
    request = c.create_string_buffer(raw)
    response = Buffer()
    rc = api.call(method.encode(), request, len(raw), c.byref(response))
    try:
        data = json.loads(c.string_at(response.ptr, response.len))
    finally:
        api.free(response.ptr, response.len)
    assert rc == 0, data
    assert data['ok'], data
    return data['result']

def start(path, extra=None):
    api = API()
    assert library.cliproxy_plugin_init(c.byref(Host(99, None, host_call, host_free)), c.byref(api)) != 0
    assert library.cliproxy_plugin_init(c.byref(host), c.byref(api)) == 0
    assert library.cliproxy_plugin_init(c.byref(host), c.byref(API())) != 0
    import os
    cfg = {'cache-path': os.path.relpath(path), 'poll-interval': '15m', 'request-spacing': '1s', **(extra or {})}
    registered = call(api, 'plugin.register', {'schema_version': 6, 'config_yaml': base64.b64encode(json.dumps(cfg).encode()).decode()})
    assert registered['metadata']['Name'] == 'quota-cache'
    assert registered['capabilities']['management_api'] is True
    routes = call(api, 'management.register')
    assert routes['resources'][0]['Menu'] == 'Quota Cache'
    assert routes['routes'][0]['Menu'] == ''
    # A CPA config save can spell the same location as an absolute path.
    cfg['cache-path'] = str(path)
    cfg['poll-interval'] = '5m'
    cfg['request-spacing'] = '2s'
    call(api, 'plugin.reconfigure', {'schema_version': 6, 'config_yaml': base64.b64encode(json.dumps(cfg).encode()).decode()})
    shell = call(api, 'management.handle', {'Method': 'GET', 'Path': '/v0/resource/plugins/quota-cache/status'})
    assert shell['StatusCode'] == 200
    shell_bytes = base64.b64decode(shell['Body'])
    assert b'synthetic-one' not in shell_bytes and b'synthetic-not-a-real-token' not in shell_bytes
    assert 'Content-Security-Policy' in shell['Headers']
    # The library serves the page that remembers a refused console key rather
    # than presenting it to CPA again; sidebar-smoke.py exercises it.
    assert b"'quota-cache.console-refused'" in shell_bytes and b'state.rememberPassword !== true' in shell_bytes
    return api

def snapshot(api):
    result = call(api, 'management.handle', {'Method': 'GET', 'Path': '/v0/management/plugins/quota-cache/status'})
    return result['StatusCode'], json.loads(base64.b64decode(result['Body']))

def await_warmup(api, done, what):
    deadline = time.monotonic()+20
    while time.monotonic() < deadline:
        status, data = snapshot(api)
        accounts = (data.get('warmup') or {}).get('accounts', [])
        if status == 200 and done(accounts):
            return accounts
        time.sleep(.05)
    raise AssertionError(what)

def await_result(api, expected_error):
    deadline = time.monotonic()+5
    while time.monotonic() < deadline:
        status, data = snapshot(api)
        entry = data.get('entries', {}).get('claude:synthetic-one', {})
        if status == 200 and entry.get('last_attempt') and entry.get('last_error', '') == expected_error:
            return data
        time.sleep(.02)
    raise AssertionError('cache did not complete synthetic poll')

with tempfile.TemporaryDirectory(prefix='quota-cache-native-') as tmp:
    path = Path(tmp)/'success'/'snapshot.json'
    api = start(path)
    try:
        data = await_result(api, '')
        assert data['entries']['claude:synthetic-one']['used_percent'] == 42
        details = data['entries']['claude:synthetic-one']['quota']
        assert details['windows']['five_hour']['used_percent'] == 18
        assert details['balances']['extra_usage']['enabled'] is False
        assert details['balances']['extra_usage']['limit'] == '0'
        assert data['poll_interval'] == '5m0s' and data['request_spacing'] == '2s'
        for _ in range(100): assert snapshot(api)[0] == 200
        assert counts['usage'] == 1 and counts['profile'] == 1 and counts['http'] == 2
        entry = data['entries']['claude:synthetic-one']
        assert entry['plan'] == 'max_20x' and entry['account_details']['plan'] == 'max_20x'
        assert data['totals']['requests'] == 1 and data['totals']['successes'] == 1
        assert data['history'][0]['http_status'] == 200
        serialized = path.read_text()
        assert 'synthetic-not-a-real-token' not in serialized
        assert 'access_token' not in serialized
    finally:
        api.shutdown()
    api = start(path)
    try:
        await_result(api, '')
        time.sleep(1.1)
        assert counts['http'] == 2, 'restart bypassed persisted interval or re-read account details'
    finally:
        api.shutdown()
    path = Path(tmp)/'limited'/'snapshot.json'
    mode['status'] = 429
    api = start(path)
    try:
        data = await_result(api, 'provider rate limited')
        cooldown = datetime.fromisoformat(data['provider_cooldown']['claude'].replace('Z','+00:00'))
        assert cooldown > datetime.now(timezone.utc)+timedelta(minutes=59)
    finally:
        api.shutdown()
    api = start(path)
    try:
        time.sleep(1.1)
        assert counts['http'] == 3, 'restart bypassed persisted Retry-After'
        assert counts['profile'] == 1, 'a rate-limited poll read account details'
    finally:
        api.shutdown()
    mode['status'] = 200
    warm['on'] = True
    path = Path(tmp)/'warmup'/'snapshot.json'
    api = start(path, {'warmup-enabled': True, 'warmup-cli-path': 'claude'})
    try:
        # Go writes an unset time as year 1, which is not an attempt.
        accounts = await_warmup(api, lambda accounts: any(not a.get('last_attempt', '0001').startswith('0001') and a['state'] != 'warming' for a in accounts),
                                'the idle subscription was not warmed')
        assert warm['calls'][:3] == ['host.http.cancel', 'host.auth.get_runtime', 'host.model.execute'], warm['calls']
        assert len(warm['requests']) == 1, warm['requests']
        sent = warm['requests'][0]
        assert sent['auth_id'] == WARM_ID and sent['forced_provider'] == 'claude', sent
        assert sent['entry_protocol'] == 'claude' and sent['exit_protocol'] == 'claude' and sent['stream'] is False, sent
        assert sent['model'] == 'claude-haiku-5-5', sent
        body = json.loads(base64.b64decode(sent['body']))
        assert body['model'] == 'claude-haiku-5-5' and body['output_config'] == {'effort': 'low'} and 'thinking' not in body, body
        assert accounts[0]['auth_index'] == WARM_INDEX and not accounts[0].get('error'), accounts
        assert accounts[0]['state'] == 'awaiting_confirmation', accounts
        time.sleep(2.5)
        assert len(warm['requests']) == 1, 'the guard let a second warm-up through'
        journal = (path.parent/'snapshot.warmup.json').read_text()
        assert WARM_ID not in journal and 'synthetic@example.com' not in journal and 'synthetic-not-a-real-token' not in journal
    finally:
        api.shutdown()
    warm.update(cancel=False, calls=[], requests=[])
    path = Path(tmp)/'old-host'/'snapshot.json'
    api = start(path, {'warmup-enabled': True})
    try:
        accounts = await_warmup(api, lambda accounts: any(a.get('error') for a in accounts), 'an old host recorded no warm-up failure')
        assert accounts[0]['error'] == 'host_unsupported', accounts
        assert 'host.model.execute' not in warm['calls'] and 'host.auth.get_runtime' not in warm['calls'], warm['calls']
    finally:
        api.shutdown()
        # sidebar-smoke.py reuses this module's host for a fresh cache.
        warm.update(on=False, cancel=True, calls=[], requests=[])
assert not buffers, 'host buffers leaked'
print('PASS: native registration, 100 cache-only reads, restart TTL, persisted account details, persisted 429 cooldown, '
      'one pinned warm-up through host.model.execute, no warm-up on a host without host.http.cancel, buffer release')
