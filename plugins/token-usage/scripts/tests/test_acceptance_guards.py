"""Negative acceptance-gate tests; all fixtures are disposable and synthetic."""
import base64
from contextlib import redirect_stdout
from email.message import Message
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/native"))
import store_smoke as smoke
from http_contract import assert_public_headers


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


go_gate = load("go_gate", "scripts/browser/require-go-pass.py")
candidate = load("candidate", "scripts/check-candidate.py")


class HeaderContracts(unittest.TestCase):
    RAW = b'<!doctype html><html><head><style>body { color: black; }</style></head><body><script>window.fixture = "a & b";</script></body></html>'

    def headers(self):
        digest = lambda raw: "'sha256-" + base64.b64encode(hashlib.sha256(raw).digest()).decode() + "'"
        policy = "default-src 'none'; script-src " + digest(b'window.fixture = "a & b";')
        policy += "; style-src " + digest(b'body { color: black; }')
        policy += "; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'; object-src 'none'"
        result = Message()
        for name, value in (("Content-Type", "text/html; charset=utf-8"), ("Cache-Control", "no-store"),
                            ("X-Content-Type-Options", "nosniff"), ("Referrer-Policy", "no-referrer"),
                            ("Content-Security-Policy", policy)):
            result[name] = value
        return result

    def test_exact_security_headers_and_hashes(self):
        assert_public_headers(self.headers(), self.RAW)
        with self.assertRaises(AssertionError):
            assert_public_headers(self.headers(), self.RAW.replace(b'color: black', b'color: white'))

    def test_missing_bad_duplicate_or_relaxed_csp_cannot_pass(self):
        good = self.headers()["Content-Security-Policy"]
        cases = [None, good.replace("script-src ", "script-src 'unsafe-inline' "),
                 good.replace("style-src ", "style-src 'self' "),
                 good.replace("frame-ancestors 'self'", "frame-ancestors *"),
                 good.replace("connect-src 'self'", "connect-src https:"),
                 good.replace("base-uri 'none'; ", ""),
                 good.replace("form-action 'none'", "form-action 'self'"),
                 good.replace("'sha256-", "'sha384-", 1), good + "; script-src 'unsafe-inline'"]
        for policy in cases:
            headers = self.headers()
            del headers["Content-Security-Policy"]
            if policy is not None:
                headers["Content-Security-Policy"] = policy
            with self.subTest(policy=policy), self.assertRaises(AssertionError):
                assert_public_headers(headers, self.RAW)
        headers = self.headers()
        headers["Content-Security-Policy"] = good
        with self.assertRaises(AssertionError):
            assert_public_headers(headers, self.RAW)

    def test_other_security_headers_are_required(self):
        for name in ("Content-Type", "Cache-Control", "X-Content-Type-Options", "Referrer-Policy"):
            headers = self.headers()
            headers.replace_header(name, "incorrect")
            with self.subTest(name=name), self.assertRaises(AssertionError):
                assert_public_headers(headers, self.RAW)

    def test_public_shell_checks_actual_http_headers_not_only_bytes(self):
        raw, headers = self.RAW, self.headers()
        class Fixture(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                public = self.path.split("?")[0] == smoke.PUBLIC
                self.send_response(200 if public else 404)
                if public:
                    for name, value in headers.items():
                        self.send_header(name, value)
                self.end_headers()
                self.wfile.write(raw if public else b"")
        server = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        base = "http://127.0.0.1:" + str(server.server_port)
        try:
            self.assertEqual(smoke.public_shell(base), raw)
            del headers["Content-Security-Policy"]
            with self.assertRaisesRegex(AssertionError, "Content-Security-Policy"):
                smoke.public_shell(base)
            headers["Content-Security-Policy"] = "script-src 'unsafe-inline'"
            with self.assertRaisesRegex(AssertionError, "CSP"):
                smoke.public_shell(base)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


class GoBrowserGate(unittest.TestCase):
    def events(self, actions, package="pass", test=go_gate.TEST):
        result = [{"Action": action, "Package": go_gate.PACKAGE, "Test": test} for action in actions]
        result += [{"Action": package, "Package": go_gate.PACKAGE}]
        return "\n".join(map(json.dumps, result)) + "\n"

    def test_requires_exact_run_and_pass(self):
        go_gate.require_browser_pass(io.StringIO(self.events(["run", "pass"])), io.StringIO())

    def test_no_match_skip_failure_other_test_or_duplicate_are_red(self):
        cases = [self.events([]), self.events(["run", "skip"]), self.events(["run", "fail"], "fail"),
                 self.events(["run", "pass"], "fail"), self.events(["run", "pass"], test="TestSidebarOther"),
                 self.events(["run", "pass", "run", "pass"])]
        for events in cases:
            with self.subTest(events=events):
                result = subprocess.run([sys.executable, str(ROOT / "scripts/browser/require-go-pass.py")],
                                        input=events, text=True, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("mandatory TestSidebarBrowser", result.stderr)


class CandidateVersionGuard(unittest.TestCase):
    def test_all_active_versions_match_canonical_registry(self):
        self.assertEqual(candidate.validate(), "0.1.1")

    def test_mixed_native_build_and_future_registry_bump_fail_in_fixture_copy(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for path in (*candidate.PATTERNS, *candidate.JSON_FILES):
                target = root / path
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(ROOT / path, target)
            candidate.validate(root)
            for path, patterns in candidate.PATTERNS.items():
                target = root / path
                original = target.read_text()
                match = re.search(patterns[0], original, re.M)
                target.write_text(original[:match.start(1)] + "9.9.9" + original[match.end(1):])
                with self.subTest(path=path), self.assertRaisesRegex(AssertionError, "version mismatch"):
                    candidate.validate(root)
                target.write_text(original)
            registry = root / "registry.json"
            registry.write_text(registry.read_text().replace('"0.1.1"', '"0.1.2"'))
            result = subprocess.run([sys.executable, str(ROOT / "scripts/check-candidate.py"), "--root", str(root)], capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("candidate version mismatch", result.stderr)


class BrowserCleanup(unittest.TestCase):
    def executable(self, path, body):
        path.write_text("#!" + sys.executable + "\n" + body)
        path.chmod(0o700)

    def test_node_timeout_and_failure_close_only_parent_owned_session(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            node, cli, calls = root / "node", root / "browser", root / "calls.jsonl"
            self.executable(cli, "import json,os,sys\nwith open(os.environ['CALLS'],'a') as f: f.write(json.dumps(sys.argv[1:])+'\\n')\nprint(json.dumps({'success':True,'data':{}}))\n")
            for mode in ("timeout", "fail"):
                self.executable(node, "import os,time\nfrom pathlib import Path\nPath(os.environ['SESSION_FILE']).write_text(os.environ['TOKEN_USAGE_NATIVE_BROWSER_SESSION'])\n" + ("time.sleep(5)\n" if mode == "timeout" else "raise SystemExit(3)\n"))
                environment = {"PATH": directory + ":" + os.environ["PATH"], "TOKEN_USAGE_AGENT_BROWSER": str(cli),
                               "CALLS": str(calls), "SESSION_FILE": str(root / "session")}
                with patch.dict(os.environ, environment), redirect_stdout(io.StringIO()):
                    with self.assertRaises((RuntimeError, AssertionError)):
                        smoke.browser("http://127.0.0.1:1", root / mode, 1, timeout=0.2)
                session = (root / "session").read_text()
                self.assertRegex(session, r"^token-usage-native-[a-f0-9]{32}$")
                command = json.loads(calls.read_text().splitlines()[-1])
                self.assertEqual(command, ["--session", session, "--json", "close"])
            self.assertEqual(len(calls.read_text().splitlines()), 2)

    def test_native_observer_accepts_header_shapes_and_request_headers_without_logging_keys(self):
        script = r'''
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(process.argv[1], 'utf8');
const template = source.match(/writeFileSync\(init, `([\s\S]*?)`\);/)[1];
const key = 'synthetic-management-secret-canary';
const observer = new Function('key', 'return `' + template + '`;')(key);
const window = {fetch: () => true};
vm.runInNewContext(observer, {window, document:{addEventListener(){}}, Request, Headers});
for (const headers of [{Authorization:'Bearer '+key}, {authorization:'Bearer '+key}, new Headers({authorization:'Bearer '+key}), [['AUTHORIZATION','Bearer '+key]]]) {
  window.fetch('http://127.0.0.1:1/status', {headers,mode:'same-origin',redirect:'error'});
}
window.fetch(new Request('http://127.0.0.1:1/status', {headers:{authorization:'Bearer '+key},mode:'same-origin',redirect:'error'}));
assert.equal(window.__nativeTest.requests.length, 5);
assert(window.__nativeTest.requests.every(r => r.authorized && r.mode==='same-origin' && r.redirect==='error'));
assert(!JSON.stringify(window.__nativeTest).includes(key));
'''
        subprocess.run(["node", "-e", script, str(ROOT / "scripts/browser/native.cjs")], check=True, capture_output=True, text=True)

    def test_cleanup_only_failure_is_not_silently_successful(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cli = root / "browser"
            self.executable(cli, r'''import json,sys
args=sys.argv[1:]
if args[-1]=='close':
    print(json.dumps({'success':False,'error':'CLEANUP_ONLY'})); raise SystemExit(0)
result=True
if 'eval' in args:
    source=args[-1]
    if source=='window.__nativeTest.requests.length': result=0
    elif source.startswith('({ events:'):
        requests=[{'url':'http://127.0.0.1:1/v0/management/plugins/token-usage/'+name,'authorized':True,'mode':'same-origin','redirect':'error'} for name in ('status','summary','models')]
        result={'events':'1','input':'100','output':'20','rows':'oa-normal','text':'Token Usage','external':[], 'fixture':{'violations':[],'requests':requests}}
    elif source.startswith('({executed:'): result={'executed':False,'violations':['script-src-elem']}
print(json.dumps({'success':True,'data':{'result':result}}))
''')
            result = subprocess.run(["node", str(ROOT / "scripts/browser/native.cjs"), "http://127.0.0.1:1", str(root / "artifacts"), "1"],
                env={**os.environ, "TOKEN_USAGE_AGENT_BROWSER": str(cli), "TOKEN_USAGE_NATIVE_BROWSER_SESSION": "token-usage-native-" + "b" * 32},
                capture_output=True, text=True, timeout=15)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("PASS: official-image native browser", result.stdout, result.stderr)
            self.assertIn("AssertionError [ERR_ASSERTION]: CLEANUP_ONLY", result.stderr)

    def test_node_cleanup_failure_preserves_primary_error_and_sanitizes_warning(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cli = root / "browser"
            self.executable(cli, "import json,sys\nerror='CLEANUP_SENTINEL synthetic-management-secret-canary' if sys.argv[-1]=='close' else 'PRIMARY_SENTINEL'\nprint(json.dumps({'success':False,'error':error}))\n")
            result = subprocess.run(["node", str(ROOT / "scripts/browser/native.cjs"), "http://127.0.0.1:1", str(root / "artifacts"), "1"],
                env={**os.environ, "TOKEN_USAGE_AGENT_BROWSER": str(cli), "TOKEN_USAGE_NATIVE_BROWSER_SESSION": "token-usage-native-" + "a" * 32},
                capture_output=True, text=True, timeout=15)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Native browser cleanup failed:", result.stderr)
            self.assertIn("AssertionError [ERR_ASSERTION]: PRIMARY_SENTINEL", result.stderr)
            self.assertNotIn("synthetic-management-secret-canary", result.stderr)
