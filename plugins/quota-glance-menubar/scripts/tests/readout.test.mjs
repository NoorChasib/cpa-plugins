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
function harness(settings = config) {
  const messages = [], requests = [], keyListeners = [], dialogs = [];
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
    addEventListener: (type, listener) => { if (type === 'keydown') keyListeners.push(listener); },
  };
  const document = { querySelectorAll: selector => selector === 'dialog:modal' ? dialogs.filter(d => d.open) : [] };
  const context = vm.createContext({ window, document, location: { origin, pathname: settings.path },
    Request: BrowserRequest, Headers, URL, AbortController, Event, setTimeout, clearTimeout });
  vm.runInContext(source.replace('__QUOTA_GLANCE_CONFIG__', JSON.stringify(settings)), context);
  const fetchPage = (path = route, init = {}) => window.fetch(path, { headers: { Authorization: authorization }, ...init });
  // Drain the response-clone stream and the observation queue deterministically.
  const settle = async () => {
    for (let i = 0; i < 20; i++) await new Promise(resolve => setImmediate(resolve));
  };
  // A key as WebKit delivers it to window, once the page's own handlers have run.
  const press = (fields = {}) => {
    const event = { isTrusted: true, key: 'Escape', isComposing: false, defaultPrevented: false, target: null,
      preventDefault() { this.defaultPrevented = true; }, ...fields };
    for (const listener of keyListeners) listener(event);
    return event;
  };
  const showModal = () => {
    const dialog = Object.assign(new EventTarget(), { open: true, close() { this.open = false; } });
    dialogs.push(dialog);
    return dialog;
  };
  return { messages, requests, window, fetchPage, settle, press, showModal, respond: fn => { responder = fn; } };
}

test('projects the real summary without consuming the page response or exposing credentials', async () => {
  const h = harness();
  assert.deepEqual(await (await h.fetchPage()).json(), fixture);
  await h.settle();
  const message = h.messages.at(-1);
  assert.equal(message.kind, 'snapshot');
  assert.equal(message.session, config.session);
  assert.deepEqual(message.snapshot.windows.find(w => w.selection.rowID === 'weekly_fable'), {
    selection: { providerID: 'claude', rowID: 'weekly_fable' }, title: 'Claude · Weekly (Fable)', remainingPercent: 36,
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

test('a resource refresh with no answer asks the host to poll again soon and keeps its request', async () => {
  const h = harness();
  await h.fetchPage();
  await h.settle();
  h.respond(() => { throw new TypeError('Load failed'); });
  await h.window.__quotaGlanceReadout.refresh();
  assert.deepEqual([h.messages.at(-1).kind, h.messages.at(-1).reason], ['unavailable', 'network']);
  h.respond(() => new Response(JSON.stringify(fixture)));
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(h.requests.at(-1).headers.get('Authorization'), authorization);
  assert.equal(h.messages.at(-1).kind, 'snapshot');
});

// CPA counts a refused management key toward its ban, and a console request
// that got no answer may have been counted: the dashboard never repeats one
// on its own (access.ts readThrough), so neither does the native clock.
const consoleRoute = config.summaryPaths[0];
const pageURL = `${origin}${config.path}`;
// CPA dispatches only GET to a resource route, so it answers a HEAD for the
// dashboard page with a 404 (pluginhost ServeResourceHTTP declines it).
const serverUp = request => request.method === 'HEAD'
  ? new Response(null, { status: 404 })
  : new Response(JSON.stringify(fixture));
const noAnswer = {
  'a network error': () => { throw new TypeError('Load failed'); },
  'a redirect': request => {
    assert.equal(request.redirect, 'error');
    throw new TypeError('Load failed');
  },
  'a deadline': () => { throw new DOMException('The operation was aborted.', 'AbortError'); },
};
for (const [name, failure] of Object.entries(noAnswer)) {
  test(`a console refresh the server was up for that got ${name} is never repeated`, async () => {
    const h = harness();
    await h.fetchPage(consoleRoute);
    await h.settle();
    assert.equal(h.messages.at(-1).kind, 'snapshot');
    const before = h.requests.length;
    h.respond(request => request.method === 'HEAD' ? serverUp(request) : failure(request));
    await h.window.__quotaGlanceReadout.refresh();
    assert.deepEqual(h.requests.slice(before).map(r => [r.method, new URL(r.url).pathname]),
      [['HEAD', config.path], ['GET', consoleRoute]]);
    assert.deepEqual([h.messages.at(-1).kind, h.messages.at(-1).reason], ['unavailable', 'unanswered']);
    const count = h.requests.length;
    h.respond(() => new Response(JSON.stringify(fixture)));
    for (let i = 0; i < 3; i++) await h.window.__quotaGlanceReadout.refresh();
    assert.equal(h.requests.length, count);
    assert.deepEqual([h.messages.at(-1).kind, h.messages.at(-1).reason], ['unavailable', null]);
    // The page asking again is the reader's choice, and resumes the readout.
    await h.fetchPage(consoleRoute);
    await h.settle();
    assert.equal(h.messages.at(-1).kind, 'snapshot');
  });
}

// The host asks for a reading as soon as the Mac wakes, usually before Wi-Fi
// or a VPN is back. A console replay sent then got no answer and was dropped,
// and the readout showed a dash until the popover was opened.
test('a console refresh while the server cannot be reached sends no key and keeps its request', async () => {
  const h = harness();
  await h.fetchPage(consoleRoute);
  await h.settle();
  const before = h.requests.length;
  h.respond(() => { throw new TypeError('Load failed'); });
  await h.window.__quotaGlanceReadout.refresh();
  const asked = h.requests.slice(before);
  assert.deepEqual(asked.map(r => [r.method, r.url]), [['HEAD', pageURL]], 'Nothing was sent to the console');
  assert.deepEqual([asked[0].cache, asked[0].credentials, asked[0].redirect], ['no-store', 'omit', 'manual']);
  assert.equal(asked[0].headers.get('Authorization'), null);
  assert.deepEqual([h.messages.at(-1).kind, h.messages.at(-1).reason], ['unavailable', 'network']);
  // The host's quick retry, once the network is back.
  h.respond(serverUp);
  await h.window.__quotaGlanceReadout.refresh();
  const retried = h.requests.slice(before + 1);
  assert.deepEqual(retried.map(r => [r.method, new URL(r.url).pathname]), [['HEAD', config.path], ['GET', consoleRoute]]);
  assert.equal(retried[1].headers.get('Authorization'), authorization);
  assert.equal(h.messages.at(-1).kind, 'snapshot');
});

test('any answer to the reachability check, whatever its status, lets the console replay go ahead', async () => {
  for (const answer of [() => new Response(null, { status: 405 }), () => Response.redirect(`${origin}/sign-in`, 302),
    () => new Response(null, { status: 502 })]) {
    const h = harness();
    await h.fetchPage(consoleRoute);
    await h.settle();
    h.respond(request => request.method === 'HEAD' ? answer() : new Response(JSON.stringify(fixture)));
    await h.window.__quotaGlanceReadout.refresh();
    assert.deepEqual(h.requests.slice(1).map(r => r.method), ['HEAD', 'GET']);
    assert.equal(h.messages.at(-1).kind, 'snapshot');
  }
});

// The page reads through the console on its own too, for one when the network
// comes back (TanStack's refetchOnReconnect), often while the bridge's check is
// out. Its answer stands: after a refusal or no answer, the replay built before
// it would present the key once more, past the page's own hold.
function checkHeld(h, read) {
  let answerCheck;
  h.respond(request => request.method === 'HEAD'
    ? new Promise(resolve => { answerCheck = () => resolve(new Response(null, { status: 404 })); })
    : read(request));
  return () => answerCheck();
}
const pageRead = {
  'gets no answer': [() => { throw new TypeError('Load failed'); }, 'unanswered'],
  'is refused': [() => new Response(null, { status: 401 }), null],
};
for (const [name, [read, reason]] of Object.entries(pageRead)) {
  test(`a console replay waiting on the check is not sent once the page's own read ${name}`, async () => {
    const h = harness();
    await h.fetchPage(consoleRoute);
    await h.settle();
    const answerCheck = checkHeld(h, read);
    const refreshing = h.window.__quotaGlanceReadout.refresh();
    await h.settle();
    assert.equal(h.requests.at(-1).method, 'HEAD');
    await h.fetchPage(consoleRoute).catch(() => {});
    await h.settle();
    assert.deepEqual([h.messages.at(-1).kind, h.messages.at(-1).reason], ['unavailable', reason]);
    const [count, reported] = [h.requests.length, h.messages.length];
    answerCheck();
    await refreshing;
    assert.equal(h.requests.length, count, 'The key was not presented again');
    assert.equal(h.messages.length, reported);
    h.respond(serverUp);
    await h.window.__quotaGlanceReadout.refresh();
    assert.equal(h.requests.length, count);
  });
}

test("a page read that answers while the check is out stands, and is not replayed", async () => {
  const h = harness(), changed = structuredClone(fixture);
  changed.providers[0].rows[0].aggregate.remainingPercent = 17;
  await h.fetchPage(consoleRoute);
  await h.settle();
  const answerCheck = checkHeld(h, () => new Response(JSON.stringify(changed), { headers: { ETag: 'v2' } }));
  const refreshing = h.window.__quotaGlanceReadout.refresh();
  await h.settle();
  await h.fetchPage(consoleRoute);
  await h.settle();
  const count = h.requests.length;
  answerCheck();
  await refreshing;
  assert.equal(h.requests.length, count);
  assert.equal(h.messages.at(-1).kind, 'snapshot', 'The fresh reading is not marked unavailable');
  assert.equal(h.messages.at(-1).snapshot.windows[0].remainingPercent, 17);
});

test('a page read still out when the check answers is left to report', async () => {
  const h = harness();
  await h.fetchPage(consoleRoute);
  await h.settle();
  let answerPage;
  const answerCheck = checkHeld(h, () => new Promise(resolve => { answerPage = resolve; }));
  const refreshing = h.window.__quotaGlanceReadout.refresh();
  await h.settle();
  const reading = h.fetchPage(consoleRoute);
  await h.settle();
  answerCheck();
  await h.settle();
  assert.deepEqual(h.requests.slice(1).map(r => r.method), ['HEAD', 'GET'], 'Only the page read went out');
  answerPage(new Response(JSON.stringify(fixture)));
  await Promise.all([reading, refreshing]);
  await h.settle();
  assert.equal(h.messages.at(-1).kind, 'snapshot');
});

test('a page read refused while an earlier one is still being read is observed before the replay', async () => {
  const h = harness();
  await h.fetchPage(consoleRoute);
  await h.settle();
  let body;
  const reads = [() => new Response(new ReadableStream({ start(controller) { body = controller; } })),
    () => new Response(null, { status: 401 })];
  const answerCheck = checkHeld(h, () => reads.shift()());
  const refreshing = h.window.__quotaGlanceReadout.refresh();
  await h.settle();
  await h.fetchPage(consoleRoute);
  answerCheck();
  await h.settle();
  await h.fetchPage(consoleRoute);
  const count = h.requests.length;
  body.enqueue(new TextEncoder().encode('{"schemaVersion":2}'));
  body.close();
  await refreshing;
  assert.equal(h.requests.length, count, 'The key was not presented again');
  assert.deepEqual([h.messages.at(-1).kind, h.messages.at(-1).reason], ['unavailable', null]);
});

test('a resource refresh asks nothing first, so it costs no extra request', async () => {
  const h = harness();
  await h.fetchPage();
  await h.settle();
  h.respond(serverUp);
  await h.window.__quotaGlanceReadout.refresh();
  h.respond(() => { throw new TypeError('Load failed'); });
  await h.window.__quotaGlanceReadout.refresh();
  assert.deepEqual(h.requests.map(r => [r.method, new URL(r.url).pathname]), [['GET', route], ['GET', route], ['GET', route]]);
  assert.deepEqual([h.messages.at(-1).kind, h.messages.at(-1).reason], ['unavailable', 'network']);
});

// Settings refuses a Management API address, but should the page ever sit
// there, a HEAD without the key would itself count toward CPA's ban. CPA's
// /v8/management tree checks the key the same way.
for (const path of ['/proxy/v0/management/plugins/quota-glance/app', '/proxy/V0/%4Danagement/plugins/quota-glance/app',
  '/v8/management/plugins/quota-glance/app']) {
  test(`a page at ${path} replays the console without asking first, and drops it unanswered`, async () => {
    const h = harness({ ...config, path });
    await h.fetchPage(consoleRoute);
    await h.settle();
    h.respond(() => { throw new TypeError('Load failed'); });
    await h.window.__quotaGlanceReadout.refresh();
    assert.deepEqual(h.requests.map(r => [r.method, new URL(r.url).pathname]), [['GET', consoleRoute], ['GET', consoleRoute]]);
    assert.deepEqual([h.messages.at(-1).kind, h.messages.at(-1).reason], ['unavailable', 'unanswered']);
    await h.window.__quotaGlanceReadout.refresh();
    assert.equal(h.requests.length, 2);
  });
}

test("the page's own console read with no answer stops the native clock repeating it", async () => {
  const h = harness();
  await h.fetchPage(consoleRoute);
  await h.settle();
  h.respond(() => { throw new TypeError('Load failed'); });
  await assert.rejects(h.fetchPage(consoleRoute), TypeError);
  await h.settle();
  assert.deepEqual([h.messages.at(-1).kind, h.messages.at(-1).reason], ['unavailable', 'unanswered']);
  const count = h.requests.length;
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(h.requests.length, count);
});

test('a console read with no answer leaves the resource door working', async () => {
  const h = harness();
  await h.fetchPage();
  await h.settle();
  h.respond(request => {
    if (request.url.includes('/management/')) throw new TypeError('Load failed');
    return new Response(JSON.stringify(fixture));
  });
  await assert.rejects(h.fetchPage(consoleRoute), TypeError);
  await h.settle();
  assert.equal(h.messages.at(-1).reason, 'unanswered');
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(new URL(h.requests.at(-1).url).pathname, route);
  assert.equal(h.messages.at(-1).kind, 'snapshot');
});

// The page's own reads follow redirects, so a redirect shows only on the response.
class FollowedRedirect extends Response {
  get redirected() { return true; }
  clone() { return Object.defineProperty(super.clone(), 'redirected', { value: true }); }
}

test("a redirect the page's console read followed counts as no answer", async () => {
  const h = harness();
  await h.fetchPage(consoleRoute);
  await h.settle();
  h.respond(() => new FollowedRedirect(JSON.stringify(fixture)));
  await h.fetchPage(consoleRoute);
  await h.settle();
  assert.deepEqual([h.messages.at(-1).kind, h.messages.at(-1).reason], ['unavailable', 'unanswered']);
  const count = h.requests.length;
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(h.requests.length, count);
});

test('a redirect the page followed on the resource door is not projected', async () => {
  const h = harness();
  h.respond(() => new FollowedRedirect(JSON.stringify(fixture)));
  await h.fetchPage();
  await h.settle();
  assert.deepEqual(h.messages.map(m => [m.kind, m.reason]), [['unavailable', null]]);
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

// Quota Glance 0.5.0 spends a banked reset through a GET, because CPA
// dispatches only GET to a resource route. It is a GET that acts, so the bridge
// must neither project it nor keep it as the request refresh() repeats.
test('passes a token-door spend through once and never repeats it', async () => {
  const spend = '/proxy/v0/resource/plugins/quota-glance/spend';
  const press = { Authorization: authorization, 'X-Quota-Glance-Spend': 'eyJjb25maXJtZWQiOnRydWV9' };
  const h = harness();
  h.respond(() => new Response('{"provider":"codex","outcome":"reset"}'));
  await h.window.fetch(spend, { headers: press });
  await h.settle();
  assert.equal(h.messages.length, 0);
  await h.window.__quotaGlanceReadout.refresh();
  assert.equal(h.requests.length, 1);
  assert.equal(h.messages.at(-1).kind, 'unavailable');

  const after = harness();
  await after.fetchPage();
  await after.settle();
  const projected = after.messages.length;
  after.respond(request => request.url.endsWith('/spend')
    ? new Response('{"provider":"codex","outcome":"reset"}')
    : new Response(JSON.stringify(fixture)));
  await after.window.fetch(spend, { headers: press });
  await after.settle();
  assert.equal(after.messages.length, projected);
  await after.window.__quotaGlanceReadout.refresh();
  const sent = after.requests.map(request => new URL(request.url).pathname);
  assert.deepEqual(sent, [route, spend, route]);
  assert.equal(after.requests.at(-1).headers.get('X-Quota-Glance-Spend'), null);
  assert.equal(after.messages.at(-1).kind, 'snapshot');
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

// Quota Glance holds a Claude account out of the session mean once its weekly
// limit is spent. Both cases send the same memberless 0% row, so only
// heldOutCount separates a real 0% from an older summary with no reading.
async function projectClaudeSession(edit) {
  const h = harness(), changed = structuredClone(fixture);
  const row = changed.providers.find(p => p.id === 'claude').rows.find(r => r.rowId === 'session');
  Object.assign(row.aggregate, { memberCount: 0, excludedCount: 5, remainingPercent: 0 });
  edit(row.aggregate);
  h.respond(() => new Response(JSON.stringify(changed)));
  await h.fetchPage();
  await h.settle();
  return h.messages.at(-1).snapshot.windows.find(w => w.selection.providerID === 'claude' && w.selection.rowID === 'session');
}

test('a session with every reporting account held out shows its real 0%', async () => {
  const session = await projectClaudeSession(aggregate => { aggregate.heldOutCount = 5; });
  assert.equal(session.remainingPercent, 0);
});

test('a memberless row from a summary without heldOutCount still has no reading', async () => {
  const session = await projectClaudeSession(aggregate => { delete aggregate.heldOutCount; });
  assert.equal(session.remainingPercent, null);
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

// WebKit's own Escape close of a modal dialog leaves the key unhandled, and the
// app then closes the popover as well. The bridge closes the dialog and keeps it.
test('Escape closes an open page dialog and is kept from the popover', async () => {
  const h = harness();
  const dialog = h.showModal();
  let cancels = 0;
  dialog.addEventListener('cancel', () => { cancels += 1; });
  const event = h.press({ target: { closest: selector => selector === 'dialog:modal' ? dialog : null } });
  assert.deepEqual([event.defaultPrevented, dialog.open, cancels], [true, false, 1]);
  // Focus on the page body still finds the open dialog.
  const other = h.showModal();
  assert.equal(h.press({ target: { closest: () => null } }).defaultPrevented, true);
  assert.equal(other.open, false);
  assert.equal(h.messages.length, 0);
});

test('a page dialog that refuses to cancel stays open, and Escape stays in the page', async () => {
  const h = harness();
  const dialog = h.showModal();
  dialog.addEventListener('cancel', event => event.preventDefault());
  assert.equal(h.press().defaultPrevented, true);
  assert.equal(dialog.open, true);
});

test('Escape with no dialog, one the page handled, a synthetic one, and other keys pass through', async () => {
  const h = harness();
  assert.equal(h.press().defaultPrevented, false);
  const dialog = h.showModal();
  let cancels = 0;
  dialog.addEventListener('cancel', () => { cancels += 1; });
  h.press({ defaultPrevented: true });
  for (const fields of [{ isTrusted: false }, { key: 'Enter' }, { isComposing: true }]) {
    assert.equal(h.press(fields).defaultPrevented, false);
  }
  assert.deepEqual([dialog.open, cancels], [true, 0]);
});
