import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const source = readFileSync(new URL('../../Resources/QuotaReadout.js', import.meta.url), 'utf8');
const fixture = JSON.parse(readFileSync(new URL('../../../quota-glance/testdata/golden/summary.json', import.meta.url), 'utf8'));
const origin = 'https://quota.example.com';
const config = {
  origin, path: '/proxy/v0/resource/plugins/quota-glance/app', session: 'test-session',
  summaryPaths: ['management', 'resource'].map(route => `/proxy/v0/${route}/plugins/quota-glance/summary`),
};
const route = config.summaryPaths[1];
const authorization = 'Bearer test-only-credential';
function harness() {
  const messages = [], requests = [];
  let responder = () => new Response(JSON.stringify(fixture), { headers: { ETag: 'v1' } });
  class BrowserRequest extends Request {
    constructor(input, init) { super(typeof input === 'string' ? new URL(input, origin) : input, init); }
  }
  const window = {
    webkit: { messageHandlers: { quotaGlanceReadout: { postMessage: value => messages.push(JSON.parse(JSON.stringify(value))) } } },
    fetch: async (input, init) => {
      const request = new BrowserRequest(input, init);
      requests.push(request);
      return responder(request);
    },
  };
  const context = vm.createContext({ window, location: { origin, pathname: config.path },
    Request: BrowserRequest, Headers, URL, AbortController, setTimeout, clearTimeout });
  vm.runInContext(source.replace('__QUOTA_GLANCE_CONFIG__', JSON.stringify(config)), context);
  const fetchPage = (path = route, init = {}) => window.fetch(path, { headers: { Authorization: authorization }, ...init });
  // Drain the response-clone stream and the observation queue deterministically.
  const settle = async () => {
    for (let i = 0; i < 20; i++) await new Promise(resolve => setImmediate(resolve));
  };
  return { messages, requests, window, fetchPage, settle, respond: fn => { responder = fn; } };
}

test('projects the real summary without consuming the page response or exposing credentials', async () => {
  const h = harness();
  assert.deepEqual(await (await h.fetchPage()).json(), fixture);
  await h.settle();
  const message = h.messages.at(-1);
  assert.equal(message.kind, 'snapshot');
  assert.equal(message.session, config.session);
  assert.deepEqual(message.snapshot.windows.find(w => w.selection.rowID === 'weekly_fable'), {
    selection: { providerID: 'claude', rowID: 'weekly_fable' }, title: 'Claude · Weekly (Fable)', remainingPercent: 43,
  });
  assert.deepEqual(Object.keys(message.snapshot).sort(), ['stale', 'windows']);
  assert.equal(JSON.stringify(message).includes(authorization), false);
  for (const window of message.snapshot.windows) assert.deepEqual(Object.keys(window).sort(), ['remainingPercent', 'selection', 'title']);
});

test('native refresh updates while the page is idle and does not reuse an expired page signal', async () => {
  const h = harness(), controller = new AbortController();
  await h.fetchPage(route, { signal: controller.signal });
  await h.settle();
  controller.abort();
  const changed = structuredClone(fixture);
  changed.providers[0].rows[0].aggregate.remainingPercent = 17;
  h.respond(request => {
    assert.equal(request.signal.aborted, false);
    assert.equal(request.headers.get('Authorization'), authorization);
    assert.equal(request.headers.get('If-None-Match'), 'v1');
    assert.equal(request.redirect, 'error');
    return new Response(JSON.stringify(changed), { headers: { ETag: 'v2' } });
  });
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(h.messages.at(-1).snapshot.windows[0].remainingPercent, 17);
  h.respond(request => {
    assert.equal(request.headers.get('If-None-Match'), 'v2');
    return new Response(null, { status: 304 });
  });
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(h.messages.at(-1).snapshot.windows[0].remainingPercent, 17);
});

test('management rejection can recover via resource fallback; background rejection stops credential retries', async () => {
  const h = harness();
  h.respond(r => r.url.includes('/management/') ? new Response(null, { status: 401 }) : new Response(JSON.stringify(fixture)));
  await h.fetchPage(config.summaryPaths[0]);
  await h.fetchPage();
  await h.settle();
  assert.equal(h.messages.at(-1).kind, 'snapshot');
  h.respond(() => new Response(null, { status: 401 }));
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(h.messages.at(-1).kind, 'unavailable');
  const count = h.requests.length;
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(h.requests.length, count);
  h.respond(() => new Response(JSON.stringify(fixture)));
  await h.fetchPage();
  await h.settle();
  assert.equal(h.messages.at(-1).kind, 'snapshot');
});

test('network failure and unknown schema report unavailable and recover on the next successful read', async () => {
  const h = harness();
  await h.fetchPage();
  await h.settle();
  for (const responder of [() => { throw new Error('offline'); }, () => new Response('{"schemaVersion":2}')]) {
    h.respond(responder);
    await h.window.__quotaGlanceReadout.refresh();
    assert.equal(h.messages.at(-1).kind, 'unavailable');
  }
  h.respond(() => new Response(JSON.stringify(fixture)));
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(h.messages.at(-1).kind, 'snapshot');
});

test('ignores unrelated origins, routes and POSTs and never refreshes without a successful summary', async () => {
  const h = harness();
  await h.fetchPage(`https://other.example.com${route}`);
  await h.fetchPage('/unrelated');
  await h.fetchPage(route, { method: 'POST' });
  await h.settle();
  assert.equal(h.messages.length, 0);
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(h.requests.length, 3);
  assert.equal(h.messages.at(-1).kind, 'unavailable');
});

test('304 cannot reuse a snapshot from another route or credential', async () => {
  const h = harness();
  await h.fetchPage();
  await h.settle();
  h.respond(() => new Response(null, { status: 304 }));
  await h.fetchPage(config.summaryPaths[0]);
  await h.settle();
  assert.equal(h.messages.at(-1).kind, 'unavailable');
  await h.fetchPage(route, { headers: { Authorization: 'Bearer different', 'If-None-Match': 'v1' } });
  await h.settle();
  assert.equal(h.messages.at(-1).kind, 'unavailable');
});

test('coalesces simultaneous native refreshes', async () => {
  const h = harness();
  await h.fetchPage();
  await h.settle();
  let complete;
  h.respond(() => new Promise(resolve => { complete = resolve; }));
  const a = h.window.__quotaGlanceReadout.refresh();
  const b = h.window.__quotaGlanceReadout.refresh();
  await h.settle();
  assert.equal(h.requests.length, 2);
  complete(new Response(JSON.stringify(fixture)));
  await Promise.all([a, b]);
});

test('zero, absent data and stale snapshots stay distinct', async () => {
  const h = harness(), changed = structuredClone(fixture);
  changed.stale = true;
  changed.providers[0].rows[0].aggregate.remainingPercent = 0;
  changed.providers[0].rows[1].aggregate.memberCount = 0;
  h.respond(() => new Response(JSON.stringify(changed)));
  await h.fetchPage();
  await h.settle();
  const snapshot = h.messages.at(-1).snapshot;
  assert.equal(snapshot.stale, true);
  assert.equal(snapshot.windows[0].remainingPercent, 0);
  assert.equal(snapshot.windows[1].remainingPercent, null);
});

test('leaves non-summary Request bodies usable by dashboard actions', async () => {
  const h = harness();
  let body;
  h.respond(async request => {
    body = await request.text();
    return new Response('{}');
  });
  await h.window.fetch(new Request(`${origin}/action`, { method: 'POST', body: '{"confirm":true}' }));
  assert.equal(body, '{"confirm":true}');
  assert.equal(h.messages.length, 0);
});
