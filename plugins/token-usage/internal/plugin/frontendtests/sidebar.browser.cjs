// Real browser execution of the shipped page using the installed agent-browser.
// No npm project or frontend runtime dependency is introduced. Node 22.13+.
// The two upstream .ts fixtures are byte-for-byte sources from Management Center
// v1.22.15, ed5f1c48e11ba7335f1e8f676f228c280196af85. Their MIT notice is adjacent.
// Type stripping lets tests generate sessions with the ACTUAL console codec and
// storage writer rather than an independently invented encryption format.
const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');
const { readFileSync, writeFileSync, mkdirSync, mkdtempSync, rmSync } = require('node:fs');
const { stripTypeScriptTypes } = require('node:module');
const { join } = require('node:path');
const { tmpdir } = require('node:os');

const base = process.env.TOKEN_USAGE_FIXTURE_URL;
const binary = process.env.TOKEN_USAGE_AGENT_BROWSER;
assert(base && binary, 'Run through TestSidebarBrowser to start the loopback fixture');
const session = 'token-usage-test-' + process.pid;
const temp = mkdtempSync(join(tmpdir(), 'token-usage-browser-'));
const artifacts = process.env.TOKEN_USAGE_BROWSER_ARTIFACTS || join(temp, 'artifacts');
mkdirSync(artifacts, { recursive: true });
const initPath = join(temp, 'observe.js');
writeFileSync(initPath, `(() => {
  if (!location.pathname.endsWith('/v0/resource/plugins/token-usage/status')) return;
  const fixture = window.__fixture = { requests: [], violations: [], holdNextSummary: false, held: null };
  document.addEventListener('securitypolicyviolation', e => fixture.violations.push(e.violatedDirective));
  const original = window.fetch;
  const expectedKey = window.name === 'keyspaces' ? 'key with space' : window.name === 'keylatin1' ? 'fixture-key-café' : 'fixture-management-key';
  window.fetch = function(url, options) {
    const record = { url: String(url), mode: options.mode, credentials: options.credentials, cache: options.cache, redirect: options.redirect, authorized: options.headers.Authorization === 'Bearer ' + expectedKey, hasSignal: options.signal instanceof AbortSignal, aborted: options.signal.aborted };
    fixture.requests.push(record);
    options.signal.addEventListener('abort', () => { record.aborted = true; }, { once: true });
    const response = original.call(this, url, options);
    if (!fixture.holdNextSummary || !new URL(url).pathname.endsWith('/summary')) return response;
    fixture.holdNextSummary = false;
    // Buffer a real response, then deliberately deliver it even after abort.
    // This exercises the page's stale-generation guard, not just native fetch
    // cancellation or a server that silently stops writing after disconnect.
    return response.then(async (value) => {
      const body = await value.arrayBuffer();
      const buffered = new Response(body, { status: value.status, headers: value.headers });
      return new Promise((resolve) => {
        const held = fixture.held = { record, released: false, release() { held.released = true; resolve(buffered); } };
      });
    });
  };
  if (window.name === 'blocked-storage') Object.defineProperty(window, 'localStorage', { get() { throw new DOMException('Blocked', 'SecurityError'); } });
})();`);
const codec = ['encryption.ts', 'secureStorage.ts'].map((name) => {
  const source = readFileSync(join(__dirname, 'upstream', name), 'utf8').replace(/^import .*;\r?\n/gm, '');
  return stripTypeScriptTypes(source).replace(/\bexport /g, '');
}).join('\n');
function browser(...args) {
  let output;
  try { output = execFileSync(binary, ['--session', session, '--json', ...args], { encoding: 'utf8', timeout: 45000, maxBuffer: 4 * 1024 * 1024 }); }
  catch (error) { throw new Error(String(error.stdout || '') + '\n' + error.message); }
  const result = JSON.parse(output);
  assert.equal(result.success, true, result.error || output);
  return result.data;
}
function evaluate(source) { return browser('eval', source).result; }
function click(selector) { browser('scrollintoview', selector); browser('click', selector); }
function settled() { browser('wait', '--fn', "document.getElementById('workspace')?.getAttribute('aria-busy') === 'false'"); }
const sourceSuffix = '/v0/resource/plugins/token-usage/status';
const modern = (apiBase, extra = {}) => ({ state: { apiBase, rememberPassword: true, managementKey: 'fixture-management-key', ...extra }, version: 0 });
async function events() { return (await fetch(base + '/fixture/events')).json(); }
function instantNS(value) {
  const [whole, fraction = ''] = value.slice(0, -1).split('.');
  return BigInt(Date.parse(whole + 'Z')) * 1000000n + BigInt(fraction.padEnd(9, '0'));
}
const shownUTC = (value) => value.replace('T', ' ').replace(/Z$/, ' UTC');
function setup({ scenario = 'normal', prefix = '', record = modern(base + prefix), legacy, raw, obfuscated = false, blocked = false, query = '' } = {}) {
  browser('open', base + '/fixture?scenario=' + scenario);
  evaluate(`localStorage.clear(); window.name = ${JSON.stringify(blocked ? 'blocked-storage' : scenario)}; true`);
  const writes = [];
  if (raw !== undefined) writes.push(`localStorage.setItem('cli-proxy-auth', ${JSON.stringify(raw)});`);
  else if (record !== null) writes.push(`obfuscatedStorage.setItem('cli-proxy-auth', ${JSON.stringify(record)}, {obfuscate:${obfuscated}});`);
  if (legacy) {
    writes.push(`localStorage.setItem('isLoggedIn', ${JSON.stringify(legacy.loggedIn === false ? 'false' : 'true')});`);
    if (legacy.base !== undefined) writes.push(`obfuscatedStorage.setItem('apiBase', ${JSON.stringify(legacy.base)});`);
    if (legacy.url !== undefined) writes.push(`obfuscatedStorage.setItem('apiUrl', ${JSON.stringify(legacy.url)});`);
    writes.push(`obfuscatedStorage.setItem('managementKey', 'fixture-management-key');`);
  }
  evaluate('(() => {' + codec + '\n' + writes.join('\n') + 'return true;})()');
  browser('open', base + prefix + sourceSuffix + query);
  settled();
}
function page() {
  return evaluate(`({ notice: document.getElementById('notice').textContent, statistics: !document.getElementById('statistics').hidden, workspace: !document.getElementById('workspace').hidden, state: document.getElementById('collection-state').textContent, input: document.getElementById('input-total').textContent, output: document.getElementById('output-total').textContent, events: document.getElementById('events-total').textContent, interval: document.getElementById('interval').textContent, rangeNote: document.getElementById('range-note').textContent, rows: document.getElementById('model-rows').textContent, privateMarkup: document.getElementById('workspace').textContent, fixture: { requests: window.__fixture.requests, violations: window.__fixture.violations }, xss: window.fixtureXSS === true, images: document.querySelectorAll('img').length })`);
}
function assertLoaded() {
  const p = page();
  assert.equal(p.statistics, true, p.notice);
  assert.equal(p.input, '9,007,199,254,740,993');
  assert.equal(p.events, '9,007,199,254,740,995');
  assert.equal(p.output, '200');
  assert.equal(p.xss, false);
  assert.equal(p.images, 0);
  assert.equal(p.fixture.violations.length, 0, 'CSP must permit only the hashed shipped script/style');
  for (const request of p.fixture.requests) {
    assert.equal(new URL(request.url).origin, base);
    assert.match(new URL(request.url).pathname, /\/v0\/management\/plugins\/token-usage\/(status|summary|models)$/);
    assert.deepEqual({ ...request, url: undefined }, { url: undefined, mode: 'same-origin', credentials: 'same-origin', cache: 'no-store', redirect: 'error', authorized: true, hasSignal: true, aborted: true });
  }
  assert(!p.privateMarkup.includes('fixture-management-key'));
}
let passed = 0;
async function test(name, fn) { await fn(); passed++; process.stdout.write('PASS ' + name + '\n'); }
(async () => {
  browser('--init-script', initPath, 'open', base + '/fixture');
  await test('modern plaintext autoload, exact large counters, inert hostile model, three allowlisted fetches and CSP', async () => {
    setup({ query: '?managementKey=URL-KEY&provider=SHOULD-NOT-FORWARD&redirect=https://example.invalid' });
    assertLoaded();
    const calls = await events();
    assert.deepEqual(calls.map((c) => c.path.split('/').pop()), ['status', 'summary', 'models']);
    assert.equal(calls[1].query.from[0], '2026-09-08T12:00:00.123456789Z');
    assert.equal(calls[1].query.to[0], '2026-09-09T12:00:00.123456789Z');
    assert.deepEqual(calls[0].query, {});
    assert.deepEqual(calls[1].query, { from: ['2026-09-08T12:00:00.123456789Z'], to: ['2026-09-09T12:00:00.123456789Z'] });
    assert.deepEqual(calls[2].query, { ...calls[1].query, limit: ['25'], offset: ['0'] });
    assert(calls.every((c) => c.authorized && c.method === 'GET'));
    assert(evaluate("Array.from(document.querySelectorAll('#model-rows .details-body')).every(body => body.childNodes.length === 0)"));
    browser('snapshot', '-i');
    click('#model-rows button');
    browser('wait', '--fn', "document.querySelector('#model-rows button').getAttribute('aria-expanded') === 'true'");
    assert.equal(evaluate("document.querySelector('#model-rows button').getAttribute('aria-expanded')"), 'true');
    assert.match(evaluate("document.getElementById('row-details-0').textContent"), /Reported total tokens42/);
    assert.match(evaluate("document.getElementById('row-details-0').textContent"), /Truncated to 32/);
    const expandedNodes = evaluate("document.getElementById('model-rows').querySelectorAll('*').length");
    evaluate("window.__fixture.firstDetail = document.querySelector('#row-details-0 .details-body').firstChild; true");
    click('#model-rows button');
    browser('wait', '--fn', "document.getElementById('row-details-0').hidden");
    click('#model-rows button');
    browser('wait', '--fn', "!document.getElementById('row-details-0').hidden");
    assert.equal(evaluate("document.getElementById('model-rows').querySelectorAll('*').length"), expandedNodes);
    assert(evaluate("document.querySelector('#row-details-0 .details-body').firstChild === window.__fixture.firstDetail"));
    assert(evaluate("Array.from(document.querySelectorAll('#model-rows .details-body')).slice(1).every(body => body.childNodes.length === 0)"));
    browser('set', 'viewport', '1280', '960');
    browser('set', 'media', 'light');
    browser('screenshot', join(artifacts, 'sidebar-light.png'), '--full');
    const lightAudit = browser('a11y', '--tags', 'wcag2a,wcag2aa');
    writeFileSync(join(artifacts, 'accessibility-light.json'), JSON.stringify(lightAudit, null, 2));
    assert.equal(lightAudit.violations?.length, 0, JSON.stringify(lightAudit));
    assert.equal(lightAudit.incomplete?.length, 0, JSON.stringify(lightAudit));
    browser('set', 'media', 'dark');
    browser('screenshot', join(artifacts, 'sidebar-dark.png'), '--full');
    const darkAudit = browser('a11y', '--tags', 'wcag2a,wcag2aa');
    writeFileSync(join(artifacts, 'accessibility-dark.json'), JSON.stringify(darkAudit, null, 2));
    assert.equal(darkAudit.violations?.length, 0, JSON.stringify(darkAudit));
    assert.equal(darkAudit.incomplete?.length, 0, JSON.stringify(darkAudit));
    assert.equal(evaluate("getComputedStyle(document.body).backgroundColor"), 'rgb(12, 17, 29)');
    browser('set', 'viewport', '390', '844');
    browser('screenshot', join(artifacts, 'sidebar-narrow-dark.png'), '--full');
    assert(evaluate('document.documentElement.scrollWidth <= innerWidth'), 'only the table region may overflow');
    browser('focus', '#refresh');
    browser('press', 'Tab');
    assert.equal(evaluate('document.activeElement.id'), 'period');
    browser('set', 'viewport', '1280', '960');
    browser('set', 'media', 'light');
  });
  await test('actual official obfuscated storage codec supports remembered modern state', () => { setup({ obfuscated: true }); assertLoaded(); });
  for (const [scenario, key, obfuscated] of [['keyspaces', 'key with space', false], ['keyspaces', 'key with space', true], ['keylatin1', 'fixture-key-café', false]]) await test(scenario + (obfuscated ? ' obfuscated' : ' plaintext') + ' sends the exact accepted header bytes', async () => {
    setup({ scenario, record: modern(base, { managementKey: key }), obfuscated });
    assertLoaded();
    const calls = await events();
    assert.equal(calls.length, 3);
    assert(calls.every((call) => call.authorized), 'the Go server must accept the actual header bytes, not a decoded assumption');
    assert(!page().privateMarkup.includes(key));
  });
  await test('same-origin console iframe automatically loads its own remembered session', () => {
    setup(); browser('open', base + '/fixture/frame');
    browser('wait', '--fn', "document.querySelector('iframe').contentDocument?.getElementById('statistics')?.hidden === false");
    assert.equal(evaluate("document.querySelector('iframe').contentDocument.getElementById('input-total').textContent"), '9,007,199,254,740,993');
  });
  await test('actual cross-tab storage change clears the sidebar without a credential handoff', async () => {
    setup(); browser('tab', 'new', base + '/fixture/frame');
    evaluate("localStorage.setItem('cli-proxy-auth', JSON.stringify({state:{apiBase:'',managementKey:'',rememberPassword:true},version:0})); true");
    browser('tab', 'close');
    browser('wait', '--fn', "document.getElementById('workspace').hidden");
    assert.equal(page().rows, ''); assert.equal(page().input, '—'); assert.match(page().notice, /session/);
  });
  await test('same-origin reverse-proxy prefix and terminal management normalization', async () => {
    setup({ prefix: '/proxy/cpa', record: modern(base + '/proxy/cpa/v0/management///'), obfuscated: true });
    assertLoaded(); assert((await events()).every((c) => c.path.startsWith('/proxy/cpa/v0/management/')));
  });
  await test('legacy-only recovery uses the official codec and requires login marker plus matching base', () => {
    setup({ record: null, legacy: { base } }); assertLoaded();
    setup({ record: null, legacy: { url: base + '/v0/management' } }); assertLoaded();
  });
  const stale = { base };
  const denied = [
    ['missing session', { record: null }], ['remember off', { record: modern(base, { rememberPassword: false }), legacy: stale }],
    ['modern logout blocks stale legacy', { record: modern('', { managementKey: '' }), legacy: stale }],
    ['modern missing key blocks stale legacy', { record: { state: { apiBase: base, rememberPassword: true }, version: 0 }, legacy: stale }],
    ['modern invalid record blocks stale legacy', { raw: '{broken', legacy: stale }],
    ['modern null blocks stale legacy', { raw: 'null', legacy: stale }],
    ['modern empty record blocks stale legacy', { raw: '', legacy: stale }],
    ['malformed obfuscation blocks stale legacy', { raw: 'enc::v1::%%%invalid%%%', legacy: stale }],
    ['wrong origin', { record: modern('https://example.invalid') }],
    ['wrong port', { record: modern('http://127.0.0.1:1') }], ['wrong scheme', { record: modern(base.replace('http:', 'https:')) }],
    ['wrong prefix', { prefix: '/proxy', record: modern(base) }], ['userinfo', { record: modern(base.replace('http://', 'http://person@')) }],
    ['base query', { record: modern(base + '?key=bad') }], ['base fragment', { record: modern(base + '#x') }],
    ['unknown storage version', { record: { ...modern(base), version: 1 } }],
    ['oversized storage', { raw: 'x'.repeat(32769) }], ['invalid header credential', { record: modern(base, { managementKey: 'key\r\nInjected: bad' }) }],
    ['NUL header credential', { record: modern(base, { managementKey: 'key' + String.fromCharCode(0) + 'value' }) }],
    ['non-ByteString Unicode credential', { record: modern(base, { managementKey: 'key-€' }) }],
    ['untrimmed credential', { record: modern(base, { managementKey: ' key with space ' }) }],
    ['blocked storage', { blocked: true }], ['legacy logged out', { record: null, legacy: { base, loggedIn: false } }],
    ['legacy missing base', { record: null, legacy: {} }], ['legacy wrong base', { record: null, legacy: { base: 'https://example.invalid' } }]
  ];
  for (const [name, options] of denied) await test(name + ' makes zero authenticated requests', async () => {
    setup(options); const p = page(); assert.equal(p.workspace, false); assert.equal(p.statistics, false); assert.equal((await events()).length, 0); assert.equal(p.fixture.requests.length, 0); assert.match(p.notice, /remembered console session/);
  });
  for (const [scenario, expectedCalls, copy] of [
    ['401', 1, /rejected/], ['403', 1, /rejected/], ['summary401', 2, /rejected/],
    ['redirect', 1, /redirected/], ['html', 1, /incompatible/], ['wrongmime', 1, /incompatible/],
    ['invalidjson', 1, /incompatible/], ['schema', 1, /incompatible/], ['source', 1, /incompatible/], ['badcounter', 2, /incompatible/],
    ['unavailable', 1, /storage is unavailable/], ['stopped', 1, /stopped/], ['timeout', 1, /timed out/]
  ]) await test(scenario + ' stops safely without invented counters', async () => {
    setup({ scenario }); const p = page(); assert.equal(p.statistics, false); assert.match(p.notice, copy); assert.equal((await events()).length, expectedCalls); assert.equal(p.input, '—'); assert.equal(p.xss, false);
  });
  for (const scenario of ['stream401', 'stream403', 'streamwrongmime']) await test(scenario + ' closes the rejected streaming body and stops management requests', async () => {
    setup({ scenario });
    const acknowledgement = await (await fetch(base + '/fixture/stream-closed')).json();
    assert.deepEqual(acknowledgement, { closed: true }, 'the server must observe actual connection cancellation');
    const p = page();
    assert.equal(p.statistics, false);
    assert.equal(p.fixture.requests.length, 1);
    assert.equal(p.fixture.requests[0].aborted, true);
    assert.equal((await events()).length, 1);
    assert.match(p.notice, scenario === 'streamwrongmime' ? /incompatible/ : /rejected/);
  });
  await test('empty retained history differs from unavailable coverage', () => {
    setup({ scenario: 'empty' }); assert.equal(page().statistics, true); assert.equal(page().input, '0'); assert.match(evaluate("document.getElementById('empty').textContent"), /No observed usage events/);
    setup({ scenario: 'emptycoverage' }); assert.equal(page().statistics, false); assert.match(page().notice, /No elapsed interval/);
  });
  await test('degraded collection keeps committed counters and visible warning', () => { setup({ scenario: 'degraded' }); assertLoaded(); assert.match(page().notice, /degraded/); });
  for (const [scenario, period, hours] of [['mature720h', '30d', 720], ['mature168h', '7d', 168], ['mature24h', '24h', 24], ['mature12h', '24h', 12], ['mature168h', '30d', 168]]) await test(scenario + ' ' + period + ' loads once with a disclosed moving-boundary exclusion', async () => {
    setup({ scenario }); assertLoaded();
    if (hours > 24) assert.equal(page().rangeNote, '', 'interior presets must not lose an arbitrary minute');
    if (period !== '24h') { browser('select', '#period', period); settled(); assertLoaded(); }
    const calls = await events();
    assert.equal(calls.length, period === '24h' ? 3 : 6, 'one status/summary/models sequence per selection, no retry after an initial 416');
    const [status, summary, models] = calls.slice(-3);
    assert.deepEqual(calls.slice(-3).map((call) => call.path.split('/').pop()), ['status', 'summary', 'models']);
    assert.equal(status.coverage_from, status.retention_floor);
    assert.equal(instantNS(summary.coverage_from) - instantNS(status.coverage_from), 2000000n);
    assert.equal(instantNS(models.coverage_from) - instantNS(status.coverage_from), 4000000n);
    const from = summary.query.from[0];
    assert.equal(instantNS(from) - instantNS(status.retention_floor), 60000000000n);
    assert.equal(models.query.from[0], from);
    assert.equal(summary.query.to[0], status.coverage_to);
    assert.equal(models.query.to[0], status.coverage_to);
    assert(instantNS(from) > instantNS(models.coverage_from));
    assert(page().rangeNote.includes('[' + shownUTC(status.retention_floor) + ', ' + shownUTC(from) + ')'));
    assert.match(page().rangeNote, /moving retention boundary/);
    assert.match(page().rangeNote, /one-minute margin/);
    assert.match(page().rangeNote, /excluded usage is not estimated/);
    assert.match(page().rangeNote, /not the full retained window/);
    if (({ '24h': 24, '7d': 168, '30d': 720 })[period] > hours) assert.match(page().rangeNote, /clipped/);
    writeFileSync(join(artifacts, 'retention-' + scenario + '-' + period + '.json'), JSON.stringify({ requests: calls.slice(-3), selectedInterval: page().interval, disclosure: page().rangeNote }, null, 2));
    if (scenario === 'mature24h') browser('screenshot', join(artifacts, 'sidebar-moving-retention.png'), '--full');
  });
  await test('young installation preserves every available nanosecond for all presets', async () => {
    setup({ scenario: 'young24h' });
    for (const period of ['24h', '7d', '30d']) {
      if (period !== '24h') { browser('select', '#period', period); settled(); }
      assertLoaded();
      const [status, summary, models] = (await events()).slice(-3);
      assert(instantNS(status.coverage_from) > instantNS(status.retention_floor));
      assert.equal(summary.query.from[0], '2026-09-09T11:59:50.123456789Z');
      assert.equal(summary.query.from[0], status.coverage_from);
      assert.equal(models.query.from[0], status.coverage_from);
      assert.equal(models.coverage_from, status.coverage_from, 'the installation start stays fixed while its retention floor advances');
      assert.match(page().rangeNote, /clipped/);
      assert(!page().rangeNote.includes('moving retention boundary'));
      assert(!page().rangeNote.includes('excluded'));
    }
  });
  await test('preset equality at a stable installation start does not exclude a minute', async () => {
    setup({ scenario: 'young-equality' }); assertLoaded();
    const [status, summary] = await events();
    assert(instantNS(status.coverage_from) > instantNS(status.retention_floor));
    assert.equal(instantNS(status.coverage_to) - instantNS(status.coverage_from), 86400000000000n);
    assert.equal(summary.query.from[0], status.coverage_from);
    assert.equal(page().rangeNote, '');
  });
  for (const scenario of ['mature24h', 'mature720h', 'young24h']) await test(scenario + ' custom prefill reuses the selected range without an initial 416', async () => {
    setup({ scenario }); assertLoaded();
    const selection = (await events()).at(-1).query;
    browser('select', '#period', 'custom');
    assert.equal(evaluate("document.getElementById('from').value"), selection.from[0]);
    assert.equal(evaluate("document.getElementById('to').value"), selection.to[0]);
    assert.equal((await events()).length, 3, 'opening custom input does not fetch or change the applied data interval');
    click('#apply'); settled(); assertLoaded();
    const calls = await events(); assert.equal(calls.length, 6);
    assert.equal(calls.at(-1).query.from[0], selection.from[0]);
    assert.equal(calls.at(-1).query.to[0], selection.to[0]);
    if (scenario === 'mature24h') assert.match(page().rangeNote, /moving retention boundary/);
  });
  await test('custom prefill without a current selection leaves exact disclosed retention headroom', async () => {
    setup({ scenario: 'mature24h' });
    const status = (await events())[0];
    browser('fill', '#model', 'model-filter');
    browser('select', '#period', 'custom');
    const from = evaluate("document.getElementById('from').value");
    assert.equal(instantNS(from) - instantNS(status.retention_floor), 60000000000n);
    assert.match(page().rangeNote, /moving retention boundary/);
    click('#apply'); settled(); assertLoaded();
    assert.equal((await events()).at(-1).query.from[0], from);
  });
  await test('typed custom starts are never adjusted, including expired moving-boundary dates', async () => {
    setup({ scenario: 'mature24h' });
    const expired = (await events())[0].coverage_from;
    browser('select', '#period', 'custom');
    const explicit = '2026-09-08T12:00:30.000000001Z';
    browser('fill', '#from', explicit); click('#apply'); settled(); assertLoaded();
    assert.equal((await events()).at(-1).query.from[0], explicit, 'a valid explicit start inside the one-minute margin must stay exact');
    assert.equal(page().rangeNote, '');
    const before = (await events()).length;
    browser('fill', '#from', expired); click('#apply'); settled();
    assert.equal(evaluate("document.getElementById('from').value"), expired);
    assert.equal((await events()).length, before + 1, 'invalid custom dates stop after fresh status validation');
    assert.equal(page().statistics, false);
    assert.match(page().notice, /fit entirely inside the available coverage/);
    assert.match(page().notice, /entered dates have not been changed/);
    browser('select', '#period', '24h'); settled(); assertLoaded();
    browser('select', '#period', 'custom');
    assert.equal(evaluate("document.getElementById('from').value"), expired, 'returning to custom input must preserve an explicitly entered date');
  });
  await test('exact nanosecond clipping and non-retrying 416 coverage guidance', async () => {
    setup({ scenario: 'clipped' }); assertLoaded(); let calls = await events(); assert.equal(calls[1].query.from[0], '2026-09-09T01:02:03.123456789Z'); assert.match(page().rangeNote, /clipped/);
    setup({ scenario: '416' }); calls = await events(); assert.equal(calls.length, 2); assert.equal(page().statistics, false); assert.match(page().rangeNote, /02:03:04.987654321 UTC/); assert.match(page().rangeNote, /No usage totals were substituted/);
  });
  await test('moving retention recovers only through explicit exact-range selection', async () => {
    setup({ scenario: 'rolling' });
    assert.equal(page().statistics, false); assert.equal((await events()).length, 2);
    assert.match(page().rangeNote, /exactly one minute/);
    click('#notice button'); settled(); assertLoaded();
    const calls = await events(); assert.equal(calls.length, 5);
    assert.equal(calls.at(-1).query.from[0], '2026-09-09T02:04:04.987654321Z');
    assert.equal(calls.at(-1).query.to[0], '2026-09-09T12:00:00.123456789Z');
    assert.equal(evaluate("document.getElementById('period').value"), 'custom');
    assert.match(page().rangeNote, /is excluded/);
    assert.match(page().rangeNote, /usage is not estimated/);
  });
  await test('pagination and case-sensitive URLSearchParams filters reset the page', async () => {
    setup(); click('#next'); settled(); assert.match(evaluate("document.getElementById('page-label').textContent"), /Page 2/);
    let calls = await events(); assert.equal(calls.at(-1).query.offset[0], '25');
    browser('fill', '#provider', 'OpenAI & provider=other'); browser('fill', '#model', 'Model/Case + <tag>');
    assert.equal(page().statistics, false);
    click('#apply'); settled(); calls = await events();
    assert.equal(calls.at(-1).query.offset[0], '0'); assert.deepEqual(calls.at(-1).query.provider, ['OpenAI & provider=other']); assert.deepEqual(calls.at(-1).query.model, ['Model/Case + <tag>']);
    assert.deepEqual(Object.keys(calls.at(-2).query).sort(), ['from', 'model', 'provider', 'to']);
    assert.deepEqual(Object.keys(calls.at(-1).query).sort(), ['from', 'limit', 'model', 'offset', 'provider', 'to']);
    assert(Object.values(calls.at(-1).query).every((values) => values.length === 1));
    browser('select', '#period', '7d'); settled(); calls = await events(); assert.equal(calls.at(-1).query.from[0], '2026-09-02T12:00:00.123456789Z');
    browser('select', '#period', '30d'); settled(); calls = await events(); assert.equal(calls.at(-1).query.from[0], '2026-08-10T12:01:00.123456789Z');
    assert.match(page().rangeNote, /moving retention boundary/);
  });
  await test('custom UTC dates preserve nanoseconds and Refresh respects edited inputs', async () => {
    setup(); browser('select', '#period', 'custom');
    browser('fill', '#from', '2026-09-01T01:02:03.000000001Z'); browser('fill', '#to', '2026-09-02T02:03:04.999999999Z'); click('#apply'); settled();
    let calls = await events(); assert.equal(calls.at(-1).query.from[0], '2026-09-01T01:02:03.000000001Z'); assert.equal(calls.at(-1).query.to[0], '2026-09-02T02:03:04.999999999Z');
    browser('fill', '#from', '2026-09-01T01:02:04.123456789Z'); click('#refresh'); settled(); calls = await events(); assert.equal(calls.at(-1).query.from[0], '2026-09-01T01:02:04.123456789Z');
    const before = calls.length; browser('fill', '#to', '2026-02-30T00:00:00Z'); click('#apply'); settled(); assert.equal((await events()).length, before + 1); assert.match(page().notice, /valid UTC timestamps/); assert.equal(page().statistics, false);
  });
  await test('storage logout clears private DOM, filters and in-flight work despite stale legacy', async () => {
    setup({ legacy: stale }); assertLoaded();
    evaluate("localStorage.setItem('cli-proxy-auth', JSON.stringify({state:{apiBase:'',managementKey:'',rememberPassword:true},version:0})); window.dispatchEvent(new StorageEvent('storage',{key:'cli-proxy-auth'})); true");
    assert.equal(page().workspace, false); assert.equal(page().input, '—'); assert.equal(page().rows, ''); assert.equal(evaluate("document.getElementById('provider').value"), '');
    const before = (await events()).length; click('#refresh'); settled(); assert.equal((await events()).length, before);
  });
  for (const mutation of ['logout', 'filter']) await test('held late response cannot restore private data after ' + mutation, async () => {
    setup();
    evaluate("window.__fixture.holdNextSummary = true; document.getElementById('refresh').click(); true");
    browser('wait', '--fn', "window.__fixture.held !== null");
    assert.equal(evaluate('window.__fixture.held.record.aborted'), false);
    const heldCalls = await events();
    assert.equal(heldCalls.at(-1).path.split('/').pop(), 'summary');
    if (mutation === 'logout') {
      evaluate("localStorage.removeItem('cli-proxy-auth'); window.dispatchEvent(new StorageEvent('storage',{key:'cli-proxy-auth'})); true");
      browser('wait', '--fn', "document.getElementById('workspace').hidden");
    } else {
      evaluate("const field=document.getElementById('model'); field.value='new-filter'; field.dispatchEvent(new Event('input',{bubbles:true})); true");
      browser('wait', '--fn', "document.getElementById('notice').textContent.includes('Filters changed')");
    }
    assert.equal(evaluate('window.__fixture.held.record.aborted'), true);
    // Release a successful response AFTER cancellation. The animation-frame
    // checkpoint runs after the response's promise/microtask continuations,
    // rather than guessing server timing with a fixed sleep.
    evaluate("(async () => { window.__fixture.held.release(); await new Promise(requestAnimationFrame); return true; })()");
    assert.equal(evaluate('window.__fixture.held.released'), true);
    assert.equal((await events()).length, heldCalls.length, 'no model request may follow the stale summary');
    assert.equal(page().statistics, false); assert.equal(page().rows, ''); assert.equal(page().input, '—');
  });
  process.stdout.write('Browser suite passed: ' + passed + ' scenarios. Screenshots: ' + artifacts + '\n');
})().catch((error) => { process.stderr.write(error.stack + '\n'); try { browser('screenshot', join(artifacts, 'failure.png')); process.stderr.write(JSON.stringify(browser('errors')) + '\n'); } catch {} process.exitCode = 1; }).finally(() => {
  try { browser('close'); } catch {}
  // Keep screenshots for review when no explicit artifact directory was supplied.
  rmSync(initPath, { force: true });
});
