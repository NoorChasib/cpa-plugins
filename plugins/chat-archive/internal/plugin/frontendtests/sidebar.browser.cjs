// Real-browser execution of the shipped chat-archive sidebar with the installed agent-browser.
// Run through TestSidebarBrowser, which serves the page and a synthetic status route on loopback.
// Scenarios: a remembered console session renders counters only; a rejected session (401)
// stops after exactly one request and shows the sign-in help; no session makes no request.
const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');

const base = process.env.CHAT_ARCHIVE_FIXTURE_URL;
const binary = process.env.CHAT_ARCHIVE_AGENT_BROWSER;
assert(base && binary, 'Run through TestSidebarBrowser to start the loopback fixture');
const session = 'chat-archive-test-' + process.pid;
function browser(...args) {
  let output;
  try { output = execFileSync(binary, ['--session', session, '--json', ...args], { encoding: 'utf8', timeout: 45000, maxBuffer: 4 * 1024 * 1024 }); }
  catch (error) { throw new Error(String(error.stdout || '') + '\n' + error.message); }
  const result = JSON.parse(output);
  assert.equal(result.success, true, result.error || output);
  return result.data;
}
const evaluate = (source) => browser('eval', source).result;
const page = base + '/v0/resource/plugins/chat-archive/status';
const remembered = JSON.stringify({ state: { apiBase: base, managementKey: 'fixture-management-key', rememberPassword: true }, version: 0 });

function seed(scenario, stored) {
  browser('open', base + '/fixture?scenario=' + scenario);
  evaluate(`localStorage.clear(); ${stored ? `localStorage.setItem('cli-proxy-auth', ${JSON.stringify(stored)});` : ''} true`);
  browser('open', page);
  browser('wait', '--fn', "document.getElementById('workspace')?.getAttribute('aria-busy') === 'false' && !document.getElementById('notice').textContent.startsWith('Loading')");
}
async function calls() {
  const response = await fetch(base + '/fixture/calls');
  return (await response.json()).status_calls;
}

(async () => {
  try {
    seed('normal', remembered);
    const text = evaluate('document.body.innerText');
    assert.match(text, /State: running/);
    assert.match(text, /write_unavailable/);
    assert.match(text, /body_budget 2/);
    assert.equal(evaluate("document.getElementById('pending').textContent"), '12');
    assert.doesNotMatch(text, /synthetic content that must not render/);
    assert.equal(await calls(), 1);
    console.log('PASS browser: remembered session renders counters only (1 request)');

    seed('rejected', remembered);
    assert.match(evaluate('document.body.innerText'), /session was rejected/);
    assert.equal(evaluate("document.getElementById('workspace').hidden"), true);
    await new Promise((resolve) => setTimeout(resolve, 1500));
    assert.equal(await calls(), 1, 'a rejected session must not be retried');
    console.log('PASS browser: 401 stops after one request, private DOM hidden, no retry');

    seed('normal', null);
    assert.match(evaluate('document.body.innerText'), /No compatible remembered console session/);
    assert.equal(await calls(), 0);
    console.log('PASS browser: no session, no request');
  } finally {
    try { browser('close'); } catch { /* the session may already be gone */ }
  }
})().catch((error) => { console.error(error.message); process.exit(1); });
