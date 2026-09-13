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
counts = {'http': 0}
mode = {'status': 200}
@HostCall
def host_call(ctx, method, request, length, response):
    try:
        method = method.decode()
        if method == 'host.auth.list':
            result = {'files': [{'auth_index': 'synthetic-one', 'name': 'synthetic.json', 'provider': 'claude', 'source': 'file'}]}
        elif method == 'host.auth.get':
            result = {'json': {'access_token': 'synthetic-not-a-real-token'}}
        elif method == 'host.http.do':
            counts['http'] += 1
            body = {'seven_day': {'utilization': 42, 'resets_at': (datetime.now(timezone.utc)+timedelta(days=3)).isoformat()}}
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

def start(path):
    api = API()
    assert library.cliproxy_plugin_init(c.byref(Host(99, None, host_call, host_free)), c.byref(api)) != 0
    assert library.cliproxy_plugin_init(c.byref(host), c.byref(api)) == 0
    assert library.cliproxy_plugin_init(c.byref(host), c.byref(API())) != 0
    import os
    cfg = {'cache-path': os.path.relpath(path), 'poll-interval': '15m', 'request-spacing': '1s'}
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
    return api

def snapshot(api):
    result = call(api, 'management.handle', {'Method': 'GET', 'Path': '/v0/management/plugins/quota-cache/status'})
    return result['StatusCode'], json.loads(base64.b64decode(result['Body']))

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
        assert data['poll_interval'] == '5m0s' and data['request_spacing'] == '2s'
        for _ in range(100): assert snapshot(api)[0] == 200
        assert counts['http'] == 1
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
        assert counts['http'] == 1, 'restart bypassed persisted interval'
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
        assert counts['http'] == 2, 'restart bypassed persisted Retry-After'
    finally:
        api.shutdown()
assert not buffers, 'host buffers leaked'
print('PASS: native registration, 100 cache-only reads, restart TTL, persisted 429 cooldown, buffer release; 2 synthetic provider calls total')
