// Real official-CPA native page + real private JSON, not a stubbed API server.
// This seam seeds a synthetic remembered session with the pinned official codec;
// it does NOT claim to execute the entire upstream console login/navigation UI.
const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');
const { readFileSync, writeFileSync, mkdtempSync, rmSync, mkdirSync } = require('node:fs');
const { stripTypeScriptTypes } = require('node:module');
const { join, resolve } = require('node:path');
const { tmpdir } = require('node:os');
const [base, artifacts, expected] = process.argv.slice(2);
assert(/^http:\/\/127\.0\.0\.1:\d+$/.test(base));
assert(/^[12]$/.test(expected));
const binary = process.env.TOKEN_USAGE_AGENT_BROWSER;
assert(binary);
const key = 'synthetic-management-secret-canary';
const session = process.env.TOKEN_USAGE_NATIVE_BROWSER_SESSION;
assert(/^token-usage-native-[a-f0-9]{32}$/.test(session || ''), 'parent must supply its unique disposable session');
const temp = mkdtempSync(join(tmpdir(), 'token-usage-native-browser-'));
mkdirSync(artifacts, { recursive: true });
const init = join(temp, 'observe.js');
writeFileSync(init, `(() => {
  window.__nativeTest = { requests: [], violations: [] };
  document.addEventListener('securitypolicyviolation', e => window.__nativeTest.violations.push(e.violatedDirective));
  const original = window.fetch;
  window.fetch = function(url, options = {}) {
    const inherited = url instanceof Request ? url : null;
    const headers = new Headers(options.headers ?? inherited?.headers);
    window.__nativeTest.requests.push({url:inherited?.url ?? String(url), authorized:headers.get('Authorization') === ${JSON.stringify('Bearer ' + key)}, mode:options.mode ?? inherited?.mode, redirect:options.redirect ?? inherited?.redirect});
    return original.call(this, url, options);
  };
})();`);
function browser(...args) {
  let output;
  try { output = execFileSync(binary, ['--session', session, '--allowed-domains', '127.0.0.1', '--json', ...args], { encoding: 'utf8', timeout: 45000, maxBuffer: 4 * 1024 * 1024 }); }
  catch (error) { throw new Error(String(error.stdout || error.message).replaceAll(key, '<REDACTED>')); }
  const result = JSON.parse(output);
  assert.equal(result.success, true, String(result.error).replaceAll(key, '<REDACTED>'));
  return result.data;
}
function evaluate(source) { return browser('eval', source).result; }
const codec = ['encryption.ts', 'secureStorage.ts'].map(name => {
  const source = readFileSync(resolve(__dirname, '../../internal/plugin/frontendtests/upstream', name), 'utf8').replace(/^import .*;\r?\n/gm, '');
  return stripTypeScriptTypes(source).replace(/\bexport /g, '');
}).join('\n');
let primaryError;
try {
  const url = base + '/v0/resource/plugins/token-usage/status';
  browser('--init-script', init, 'open', url);
  browser('wait', '--fn', "document.getElementById('notice')?.textContent.includes('remembered console session')");
  assert.equal(evaluate('window.__nativeTest.requests.length'), 0, 'signed-out shell must not request private data');
  const record = { state: { apiBase: base, rememberPassword: true, managementKey: key }, version: 0 };
  evaluate('(() => {' + codec + '\nobfuscatedStorage.setItem("cli-proxy-auth", ' + JSON.stringify(record) + ', {obfuscate:true}); return true;})()');
  browser('reload');
  browser('wait', '--fn', "document.getElementById('workspace')?.getAttribute('aria-busy') === 'false' && document.getElementById('statistics')?.hidden === false");
  browser('snapshot', '-i');
  const result = evaluate(`({ events:document.getElementById('events-total').textContent, input:document.getElementById('input-total').textContent, output:document.getElementById('output-total').textContent, rows:document.getElementById('model-rows').textContent, text:document.body.textContent, fixture:window.__nativeTest, external:performance.getEntriesByType('resource').filter(r => new URL(r.name).origin !== location.origin).map(r=>r.name) })`);
  assert.equal(result.events, expected);
  assert.equal(result.input, String(Number(expected) * 100));
  assert.equal(result.output, String(Number(expected) * 20));
  assert.match(result.rows, /oa-normal/);
  assert(!result.text.includes(key));
  assert.deepEqual(result.external, []);
  assert.deepEqual(result.fixture.violations, []);
  // Positive enforcement probe: a missing/unsafe CSP must be a red failure,
  // not a misleading empty violations list. This CDP eval only inserts a DOM
  // script; Chrome, not the automation evaluator, decides whether it executes.
  evaluate("(() => { const script = document.createElement('script'); script.textContent = 'window.__nativeInlineExecuted = true'; document.body.appendChild(script); return true; })()");
  browser('wait', '--fn', "window.__nativeInlineExecuted === true || window.__nativeTest.violations.length > 0");
  const enforcement = evaluate('({executed: window.__nativeInlineExecuted === true, violations: window.__nativeTest.violations})');
  assert.equal(enforcement.executed, false, 'CSP must block an unapproved inline script');
  assert.equal(enforcement.violations.length, 1, 'expected exactly the deliberate CSP probe violation');
  assert.match(enforcement.violations[0], /^script-src(?:-elem)?$/);
  result.cspEnforcement = enforcement;
  assert.deepEqual(result.fixture.requests.map(r => new URL(r.url).pathname.split('/').pop()), ['status', 'summary', 'models']);
  assert(result.fixture.requests.every(r => new URL(r.url).origin === base && r.authorized && r.mode === 'same-origin' && r.redirect === 'error'));
  delete result.text;
  writeFileSync(join(artifacts, 'native-browser.json'), JSON.stringify({ seam: 'official codec remembered session -> native full page -> actual CPA private JSON; not full upstream console navigation', ...result }, null, 2));
  browser('set', 'viewport', '1280', '960');
  browser('screenshot', join(artifacts, 'native-sidebar.png'), '--full');
  console.log('PASS: official-image native browser autoauth, actual status/summary/models, exact ' + expected + ' committed events, CSP, no external requests');
} catch (error) {
  primaryError = error;
  throw error;
} finally {
  let cleanupError;
  for (const action of [() => browser('close'), () => rmSync(temp, { recursive: true, force: true })]) {
    try { action(); }
    catch (error) {
      cleanupError ||= error;
      console.error('Native browser cleanup failed: ' + String(error.message).replaceAll(key, '<REDACTED>'));
    }
  }
  // Preserve the useful primary failure. A cleanup-only error still fails the
  // gate so a leaked daemon is visible, rather than silently declaring success.
  if (!primaryError && cleanupError) throw cleanupError;
}
