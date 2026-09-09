(() => {
  'use strict';

  const RESOURCE_SUFFIX = '/v0/resource/plugins/token-usage/status';
  const AUTH_NAME = 'cli-proxy-auth';
  const STORAGE_NAMES = new Set([AUTH_NAME, 'isLoggedIn', 'apiBase', 'apiUrl', 'managementKey']);
  const PAGE_SIZE = 25;
  const NS = 1000000000n;
  const RETENTION_MARGIN = 60n * NS;
  const TOKEN_FIELDS = [
    ['input_tokens', 'Input tokens'], ['output_tokens', 'Output tokens'],
    ['total_tokens', 'Reported total tokens'], ['reasoning_tokens', 'Reasoning tokens'],
    ['cached_tokens', 'Cached tokens'], ['cache_read_tokens', 'Cache read tokens'],
    ['cache_creation_tokens', 'Cache creation tokens']
  ];
  const EVENT_FIELDS = [
    ['observed_events', 'Observed events'], ['successful_events', 'Successful events'],
    ['failed_events', 'Failed events'], ['anomalous_events', 'Anomalous events']
  ];
  const $ = (id) => document.getElementById(id);
  const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value);
  const text = (value, max = 4096) => typeof value === 'string' && value.length <= max;
  const whole = (value) => Number.isSafeInteger(value) && value >= 0;
  const decimal = (value, signed = false) => text(value, 128) && (signed ? /^-?(0|[1-9]\d*)$/ : /^(0|[1-9]\d*)$/).test(value);
  const count = (value) => BigInt(value).toLocaleString('en-US');
  const node = (tag, value, className) => {
    const element = document.createElement(tag);
    if (value !== undefined) element.textContent = value;
    if (className) element.className = className;
    return element;
  };
  class PageError extends Error {
    constructor(kind, message, coverage) { super(message); this.kind = kind; this.coverage = coverage; }
  }
  const incompatible = () => new PageError('response', 'The server returned an incompatible Token Usage response. Check that the console and plug-in versions are compatible, then refresh.');
  let generation = 0;
  const pending = new Set();
  let currentRange = null;
  let currentCoverage = null;
  let offset = 0;
  let hasMore = false;
  let selectionNote = '';
  let applied = { period: '24h', provider: '', model: '', from: '', to: '' };

  function apiRoot() {
    if (!['http:', 'https:'].includes(location.protocol) || !location.pathname.endsWith(RESOURCE_SUFFIX)) return null;
    const prefix = location.pathname.slice(0, -RESOURCE_SUFFIX.length);
    return location.origin + prefix;
  }
  const root = apiRoot();
  function matchesRoot(value) {
    if (!root || !text(value, 2048) || !/^https?:\/\//i.test(value.trim()) || /[?#]/.test(value) || /^https?:\/\/[^/]*@/i.test(value)) return false;
    try {
      const url = new URL(value.trim());
      if (url.username || url.password || url.search || url.hash) return false;
      url.pathname = url.pathname.replace(/\/+$/, '').replace(/\/v0\/management$/i, '').replace(/\/+$/, '') || '/';
      const expected = new URL(root);
      return url.origin === expected.origin && url.pathname === expected.pathname;
    } catch { return false; }
  }
  function stored(name) {
    const value = localStorage.getItem(name);
    if (value !== null && value.length > 32768) throw new Error('storage value too large');
    return value;
  }
  // Audited console v1.22.15 (ed5f1c48): secureStorage JSON-serializes the
  // Zustand {state, version} envelope before its optional XOR/Base64 codec.
  // This is reversible obfuscation, NOT encryption or a security boundary.
  function decode(raw, legacy = false) {
    if (raw === null) return null;
    let value = raw;
    if (value.startsWith('enc::v1::')) {
      const bytes = Uint8Array.from(atob(value.slice(9)), (c) => c.charCodeAt(0));
      const salt = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + '|' + navigator.userAgent);
      for (let i = 0; i < bytes.length; i++) bytes[i] ^= salt[i % salt.length];
      value = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
    }
    try { return JSON.parse(value); } catch (error) { if (legacy) return value; throw error; }
  }
  function validKey(value) {
    if (!text(value, 4096) || !value || value.trim() !== value || /[\x00-\x08\x0a-\x1f\x7f]/.test(value)) return false;
    try {
      // Match Fetch's bounded, single-line ByteString contract rather than
      // rejecting valid internal spaces or printable Latin-1 header bytes.
      const authorization = 'Bearer ' + value;
      return new Headers({ Authorization: authorization }).get('Authorization') === authorization;
    } catch { return false; }
  }
  function readSession() {
    try {
      const modern = stored(AUTH_NAME);
      if (modern !== null) {
        // Presence is authoritative even after logout, malformed storage, or
        // remember-off. NEVER revive legacy credentials in those cases.
        const record = decode(modern);
        const state = object(record) && record.state;
        if (!object(state) || record.version !== 0 || state.rememberPassword !== true || state.isAuthenticated === false || !matchesRoot(state.apiBase) || !validKey(state.managementKey)) return null;
        return { key: state.managementKey, base: state.apiBase };
      }
      if (stored('isLoggedIn') !== 'true') return null;
      const base = decode(stored('apiBase'), true) || decode(stored('apiUrl'), true);
      const key = decode(stored('managementKey'), true);
      return matchesRoot(base) && validKey(key) ? { key, base } : null;
    } catch { return null; }
  }
  function sameSession(a, b) { return a && b && a.key === b.key && a.base === b.base; }
  function cancel() {
    generation++;
    for (const controller of pending) controller.abort();
    pending.clear();
    $('workspace').setAttribute('aria-busy', 'false');
  }
  function clearStatistics() {
    $('statistics').hidden = true;
    for (const id of ['input-total', 'output-total', 'events-total']) $(id).textContent = '—';
    for (const id of ['model-rows', 'summary-details', 'page-label', 'empty']) $(id).replaceChildren();
    $('previous').disabled = true;
    $('next').disabled = true;
    hasMore = false;
  }
  function clearPrivate() {
    clearStatistics();
    $('workspace').hidden = true;
    for (const id of ['health', 'diagnostics', 'interval', 'range-note']) $(id).replaceChildren();
    $('range-note').hidden = true;
    $('last-persisted').textContent = '—';
    // Filter strings can contain private provider/model metadata too.
    for (const id of ['provider', 'model', 'from', 'to']) $(id).value = '';
    $('period').value = '24h';
    $('custom-range').hidden = true;
    currentRange = null;
    currentCoverage = null;
    offset = 0;
    selectionNote = '';
    applied = { period: '24h', provider: '', model: '', from: '', to: '' };
  }
  function announce(message) { $('announcement').textContent = message; }
  function notice(message, tone = 'info') {
    $('notice').className = 'alert ' + tone;
    $('notice').replaceChildren(node('span', message));
    $('notice').hidden = !message;
    announce(message);
  }
  function sessionHelp(message) {
    cancel();
    clearPrivate();
    $('collection-state').className = 'pill';
    $('collection-state').textContent = 'Collection: session required';
    notice(message || 'No compatible remembered console session is available. Sign in to the console using this page’s exact origin and API base' + (root ? ' (' + root + ')' : '') + ', enable “Remember password”, then refresh. A non-remembered in-memory console session cannot be read by this sidebar. Browser storage must be allowed.');
  }

  // Date arithmetic alone uses Number for whole-second calendar conversion.
  // Nanosecond bounds and ALL token/event counters remain exact BigInt values.
  function instant(value) {
    if (!text(value, 30)) throw incompatible();
    const match = /^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z$/.exec(value);
    if (!match) throw incompatible();
    const milliseconds = Date.parse(match[1] + 'Z');
    if (!Number.isFinite(milliseconds) || new Date(milliseconds).toISOString().slice(0, 19) !== match[1]) throw incompatible();
    return BigInt(milliseconds) * 1000000n + BigInt((match[2] || '').padEnd(9, '0'));
  }
  function timestamp(value) {
    const seconds = value / NS;
    const fraction = (value % NS).toString().padStart(9, '0').replace(/0+$/, '');
    return new Date(Number(seconds) * 1000).toISOString().slice(0, 19) + (fraction ? '.' + fraction : '') + 'Z';
  }
  function when(value) { return value.replace('T', ' ').replace(/Z$/, ' UTC'); }
  function coverage(value) {
    if (!object(value) || value.resolution !== 'raw' || value.upstream_completeness !== 'unknown' || !text(value.raw_retention, 64)) throw incompatible();
    const from = instant(value.from), to = instant(value.to);
    instant(value.retention_floor);
    if (from > to || from < 0n || to > instant('2100-12-31T23:59:59Z')) throw incompatible();
    return value;
  }
  function envelope(value) {
    if (!object(value) || value.api_schema !== 1 || value.source !== 'cpa_reported' || value.upstream_completeness !== 'unknown') throw incompatible();
    return value;
  }
  function collection(value) {
    if (!object(value) || !['running', 'degraded', 'stopped', 'unavailable'].includes(value.state) || !text(value.reason, 512)) throw incompatible();
    if (value.state === 'unavailable') return value;
    if (typeof value.degraded !== 'boolean' || !text(value.last_persisted_at, 30) || !decimal(value.disk_bytes) || !decimal(value.previous_unclean_runs) || !whole(value.queue_depth) || !object(value.diagnostics) || !Array.isArray(value.active_faults) || value.active_faults.some((s) => !text(s, 512)) || !text(value.last_failure_reason, 512) || typeof value.retention_cleanup_pending !== 'boolean' || value.upstream_completeness !== 'unknown') throw incompatible();
    if (value.last_persisted_at) instant(value.last_persisted_at);
    if (Object.entries(value.diagnostics).length > 50 || Object.entries(value.diagnostics).some(([k, v]) => !text(k, 100) || !decimal(v))) throw incompatible();
    return value;
  }
  function totals(value) {
    if (!object(value) || !object(value.reported_tokens) || EVENT_FIELDS.some(([key]) => !decimal(value[key])) || TOKEN_FIELDS.some(([key]) => !decimal(value.reported_tokens[key], true)) || !Array.isArray(value.executor_types) || value.executor_types.length > 32 || value.executor_types.some((s) => !text(s, 256)) || typeof value.executor_types_truncated !== 'boolean') throw incompatible();
    return value;
  }
  function validateStats(value, range, models) {
    envelope(value);
    coverage(value.coverage);
    collection(value.collection);
    if (!object(value.interval) || instant(value.interval.from) !== instant(range.from) || instant(value.interval.to) !== instant(range.to) || value.auxiliary_counters_additive !== false || value.aggregation !== 'provider_model_combined_executors') throw incompatible();
    if (models) {
      if (!Array.isArray(value.models) || value.models.length > PAGE_SIZE || value.limit !== PAGE_SIZE || value.offset !== offset || typeof value.has_more !== 'boolean') throw incompatible();
      for (const row of value.models) {
        if (!text(row.provider, 256) || !text(row.model, 256)) throw incompatible();
        totals(row);
      }
    } else totals(value.totals);
    return value;
  }
  async function readJSON(response) {
    if (!response.body || response.headers.get('content-type')?.split(';')[0].trim().toLowerCase() !== 'application/json') throw incompatible();
    const reader = response.body.getReader();
    const decoder = new TextDecoder('utf-8', { fatal: true });
    let result = '', size = 0;
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        size += value.length;
        if (size > 2 * 1024 * 1024) throw incompatible();
        result += decoder.decode(value, { stream: true });
      }
      result += decoder.decode();
      return JSON.parse(result);
    } catch { await reader.cancel().catch(() => {}); throw incompatible(); }
  }
  async function request(endpoint, params, run) {
    if (run !== generation) throw new PageError('cancelled', '');
    const session = readSession();
    if (!session) throw new PageError('session', '');
    if (!['status', 'summary', 'models'].includes(endpoint)) throw incompatible();
    const url = new URL(root + '/v0/management/plugins/token-usage/' + endpoint);
    if (params) url.search = params.toString();
    const controller = new AbortController();
    pending.add(controller);
    let timedOut = false;
    const timeout = setTimeout(() => { timedOut = true; controller.abort(); }, 15000);
    try {
      const response = await fetch(url.href, { method: 'GET', headers: { Authorization: 'Bearer ' + session.key, Accept: 'application/json' }, mode: 'same-origin', credentials: 'same-origin', cache: 'no-store', redirect: 'error', signal: controller.signal });
      if (run !== generation) throw new PageError('cancelled', '');
      if (!sameSession(session, readSession())) throw new PageError('session', '');
      if (response.status === 401 || response.status === 403) throw new PageError('auth', 'The console session was rejected or is no longer authorized. Requests have stopped. Sign in again using this exact console origin and API base, remember the session, then refresh.');
      const value = await readJSON(response);
      if (run !== generation) throw new PageError('cancelled', '');
      if (!sameSession(session, readSession())) throw new PageError('session', '');
      if (response.status === 416 && object(value) && value.error === 'outside_retained_coverage') throw new PageError('coverage', 'The retained coverage moved while this range was loading.', coverage(value.coverage));
      if (endpoint === 'status' && response.status === 503) {
        envelope(value);
        if (value.error !== 'storage_unavailable' || collection(value.collection).state !== 'unavailable') throw incompatible();
        return value;
      }
      if (!response.ok) {
        if (response.status === 503) throw new PageError('unavailable', 'Committed usage history is unavailable. No totals are shown. Check local storage and collection health, then refresh.');
        if (response.status === 504) throw new PageError('timeout', 'The history query timed out. Choose a shorter range or refresh to try again.');
        if (response.status === 400) throw new PageError('range', 'The server could not accept this range or exact-match filter. Check the UTC dates and retained coverage below.');
        throw incompatible();
      }
      return value;
    } catch (error) {
      if (run !== generation) throw new PageError('cancelled', '');
      if (timedOut) throw new PageError('timeout', 'The request timed out. No automatic retry was made. Refresh to try again.');
      if (error instanceof PageError) throw error;
      throw new PageError('network', 'Unable to load Token Usage. The request failed or was redirected; redirected login pages are not followed. Check the console connection, then refresh.');
    } finally {
      // Rejected auth/MIME responses may still have streaming bodies. Closing
      // the controller is harmless after consumption and releases them early.
      controller.abort();
      clearTimeout(timeout);
      pending.delete(controller);
    }
  }
  function pairs(container, entries, className = 'detail-grid') {
    const dl = node('dl', undefined, className);
    for (const [label, value] of entries) {
      const item = node('div');
      item.append(node('dt', label), node('dd', value));
      dl.append(item);
    }
    container.append(dl);
  }
  function detailBody(container, value) {
    container.replaceChildren();
    container.append(node('p', 'Raw CPA counters are shown independently. Cache, reasoning, and reported total are not additional tokens to add to input or output.'));
    pairs(container, TOKEN_FIELDS.map(([key, label]) => [label, count(value.reported_tokens[key])]));
    pairs(container, EVENT_FIELDS.map(([key, label]) => [label, count(value[key])]));
    pairs(container, [['Executor types', value.executor_types.length ? value.executor_types.map((s) => s || '(empty)').join(', ') : 'None reported'], ['Executor provenance', value.executor_types_truncated ? 'Truncated to 32 types; more types contributed.' : 'All reported types for this result.']]);
  }
  function renderHealth(value, cov) {
    const state = value.state;
    $('collection-state').textContent = 'Collection: ' + state;
    $('collection-state').className = 'pill ' + ({ running: 'ok', degraded: 'warn', stopped: 'warn', unavailable: 'err' }[state]);
    $('last-persisted').replaceChildren();
    if (value.last_persisted_at) {
      const time = node('time', when(value.last_persisted_at));
      time.dateTime = value.last_persisted_at;
      $('last-persisted').append(time);
    } else $('last-persisted').textContent = state === 'unavailable' ? 'Unavailable' : 'Not yet persisted';
    $('health').replaceChildren();
    const entries = [['Collection state', state], ['Upstream completeness', 'Unknown — CPA-reported only']];
    if (cov) entries.push(['Available from (inclusive)', when(cov.from)], ['Available to (exclusive)', when(cov.to)], ['Raw retention', cov.raw_retention], ['Retention floor', when(cov.retention_floor)]);
    if (value.reason) entries.push(['Current reason', value.reason]);
    if (state !== 'unavailable') entries.push(['Disk usage (bytes)', count(value.disk_bytes)], ['Queue depth (events)', String(value.queue_depth)], ['Retention cleanup', value.retention_cleanup_pending ? 'Pending' : 'Current'], ['Previous unclean runs', count(value.previous_unclean_runs)]);
    for (const [label, content] of entries) { const item = node('div'); item.append(node('dt', label), node('dd', content)); $('health').append(item); }
    $('diagnostics').replaceChildren();
    if (state !== 'unavailable') {
      $('diagnostics').append(node('p', 'Lifetime best-effort diagnostics, not totals for the selected date range. An unclean shutdown or CPA delivery loss can leave gaps.'));
      pairs($('diagnostics'), Object.entries(value.diagnostics).map(([label, value]) => [label.replaceAll('_', ' '), count(value)]));
      pairs($('diagnostics'), [['Active faults', value.active_faults.join(', ') || 'None reported'], ['Last failure reason', value.last_failure_reason || 'None reported']]);
    } else $('diagnostics').append(node('p', 'Storage has not opened successfully. Coverage and counters are unavailable, not zero.'));
  }
  function retentionExclusion(floor, from) {
    return 'To avoid the moving retention boundary, a one-minute margin [' + when(floor) + ', ' + when(from) + ') is excluded from the selected range. The excluded usage is not estimated; this is not the full retained window.';
  }
  function retainedRange(cov, requestedFrom) {
    const floor = instant(cov.from), ceiling = instant(cov.to);
    if (floor === ceiling) return null;
    let from = requestedFrom <= floor ? cov.from : timestamp(requestedFrom);
    let note = requestedFrom < floor ? 'The selected range is clipped to available local coverage. Earlier usage is not retained here. ' : '';
    // Only the rolling retention edge needs headroom. A young installation's
    // stable coverage start must retain every available nanosecond, unchanged.
    if (requestedFrom <= floor && floor === instant(cov.retention_floor)) {
      from = timestamp(floor + RETENTION_MARGIN);
      if (instant(from) >= ceiling) throw new PageError('range', 'The retained interval is too short to leave a one-minute margin after the moving retention boundary. Choose an explicit custom UTC range. No usage has been estimated.');
      note += retentionExclusion(cov.retention_floor, from);
    }
    return { from, to: cov.to, note: note.trim() };
  }
  function rangeFor(cov) {
    const floor = instant(cov.from), ceiling = instant(cov.to);
    if (floor === ceiling) return null;
    let from, to;
    if (applied.period === 'custom') {
      try { from = instant(applied.from); to = instant(applied.to); }
      catch { throw new PageError('range', 'Enter valid UTC timestamps, including seconds and a final Z. Up to 9 fractional-second digits are supported.'); }
      if (from < 0n || from >= to || from < floor || to > ceiling) throw new PageError('range', 'The custom UTC range must start before its end and fit entirely inside the available coverage below. Your entered dates have not been changed.');
      return { from: applied.from, to: applied.to, note: selectionNote };
    }
    const duration = { '24h': 86400n, '7d': 604800n, '30d': 2592000n }[applied.period] * NS;
    return retainedRange(cov, ceiling - duration);
  }
  function query(range, models) {
    // Allowlisted UI values only: never forward location.search or a stored URL.
    const params = new URLSearchParams({ from: range.from, to: range.to });
    for (const key of ['provider', 'model']) if (applied[key] !== '') params.set(key, applied[key]);
    if (models) { params.set('limit', String(PAGE_SIZE)); params.set('offset', String(offset)); }
    return params;
  }
  function showRange(range) {
    const filter = [applied.provider && 'Provider: ' + applied.provider, applied.model && 'Model: ' + applied.model].filter(Boolean).join('; ');
    $('interval').textContent = 'Selected interval: [' + when(range.from) + ', ' + when(range.to) + ')' + (filter ? ' — ' + filter : ' — all providers and models');
    $('range-note').textContent = range.note;
    $('range-note').hidden = !range.note;
  }
  function renderRows(value) {
    $('model-rows').replaceChildren();
    value.models.forEach((row, index) => {
      const tr = node('tr');
      tr.append(node('td', row.provider || '(empty provider)', 'name'), node('td', row.model || '(empty model)', 'name'), node('td', count(row.reported_tokens.input_tokens), 'num'), node('td', count(row.reported_tokens.output_tokens), 'num'), node('td', count(row.observed_events), 'num'));
      const button = node('button', 'Details', 'secondary row');
      button.type = 'button';
      button.setAttribute('aria-expanded', 'false');
      button.setAttribute('aria-controls', 'row-details-' + index);
      button.setAttribute('aria-label', 'Details for ' + row.provider + ' / ' + row.model);
      const control = node('td'); control.append(button); tr.append(control);
      const details = node('tr'); details.id = 'row-details-' + index; details.hidden = true;
      const cell = node('td', undefined, 'detail-cell'); cell.colSpan = 6;
      const body = node('div', undefined, 'details-body'); cell.append(body); details.append(cell);
      button.addEventListener('click', () => {
        if (!body.hasChildNodes()) detailBody(body, row);
        details.hidden = !details.hidden;
        button.setAttribute('aria-expanded', String(!details.hidden));
      });
      $('model-rows').append(tr, details);
    });
    hasMore = value.has_more;
    $('previous').disabled = offset === 0;
    $('next').disabled = !hasMore || offset + PAGE_SIZE > 100000;
    $('page-label').textContent = 'Page ' + (offset / PAGE_SIZE + 1) + ' — ' + value.models.length + ' rows' + (hasMore ? '; more available' : '');
    $('empty').hidden = value.models.length !== 0;
    $('empty').textContent = offset ? 'No rows remain on this page. Go to the previous page or refresh.' : 'No observed usage events match this retained interval and these filters. This does not prove zero consumption.';
  }
  function showFailure(error) {
    if (error.kind === 'cancelled') return;
    if (error.kind === 'session' || error.kind === 'auth') { sessionHelp(error.message); return; }
    clearStatistics();
    notice(error.message, error.kind === 'range' || error.kind === 'coverage' ? 'warn' : '');
    if (error.kind === 'coverage') {
      currentCoverage = error.coverage;
      $('range-note').hidden = false;
      $('range-note').textContent = 'Current retained coverage: [' + when(error.coverage.from) + ', ' + when(error.coverage.to) + '). No usage totals were substituted. Choose a later custom start, or refresh a shorter preset.';
      // A moving retention floor can reject the exact boundary on every request.
      // Never round timestamps or silently retry with a different interval. Offer
      // an explicit custom selection with a disclosed one-minute excluded edge.
      const from = timestamp(instant(error.coverage.from) + RETENTION_MARGIN);
      const to = currentRange ? currentRange.to : error.coverage.to;
      if (instant(from) < instant(to)) {
        const omitted = retentionExclusion(error.coverage.from, from);
        $('range-note').textContent += ' You can instead load [' + when(from) + ', ' + when(to) + '), starting exactly one minute after the reported boundary. ' + omitted;
        const recover = node('button', 'Load range after retention edge', 'secondary');
        recover.type = 'button';
        recover.setAttribute('aria-describedby', 'range-note');
        recover.addEventListener('click', () => {
          $('period').value = 'custom';
          $('custom-range').hidden = false;
          $('from').value = from;
          $('to').value = to;
          selectionNote = omitted;
          applyFilters();
        });
        $('notice').append(node('br'), recover);
      }
    }
  }
  async function load({ pageOnly = false } = {}) {
    cancel();
    const run = generation;
    if (!root || !readSession()) { sessionHelp(); return; }
    $('workspace').hidden = false;
    $('workspace').setAttribute('aria-busy', 'true');
    clearStatistics();
    notice(pageOnly ? 'Loading model page…' : 'Loading retained usage…');
    try {
      const status = envelope(await request('status', null, run));
      const health = collection(status.collection);
      if (status.state !== health.state) throw incompatible();
      if (health.state === 'unavailable') {
        renderHealth(health, null);
        notice('Local storage is unavailable. Collection cannot start and no history or counters are available. Correct the plug-in’s storage configuration, then refresh.', '');
        return;
      }
      currentCoverage = coverage(status.coverage);
      renderHealth(health, currentCoverage);
      if (health.state === 'stopped') { notice('Collection is stopped. Stored totals cannot be queried until the plug-in is running again. Last persistence and collection diagnostics are shown below.', 'warn'); return; }
      const range = pageOnly && currentRange ? currentRange : rangeFor(currentCoverage);
      if (!range) { $('interval').textContent = 'No elapsed coverage interval is available yet. Refresh after collection has started.'; notice('Collection has just started. No elapsed interval is available to query; no zero totals have been invented.'); return; }
      currentRange = range;
      showRange(range);
      const summary = validateStats(await request('summary', query(range, false), run), range, false);
      const models = validateStats(await request('models', query(range, true), run), range, true);
      if (run !== generation) return;
      $('input-total').textContent = count(summary.totals.reported_tokens.input_tokens);
      $('output-total').textContent = count(summary.totals.reported_tokens.output_tokens);
      $('events-total').textContent = count(summary.totals.observed_events);
      detailBody($('summary-details'), summary.totals);
      renderRows(models);
      renderHealth(models.collection, models.coverage);
      $('statistics').hidden = false;
      notice(health.degraded || models.collection.degraded ? 'Local collection is degraded. These are committed reported counters, but events may be missing. Review the collection diagnostics below.' : '', 'warn');
      announce('Token usage loaded. ' + count(summary.totals.observed_events) + ' observed events. Model page ' + (offset / PAGE_SIZE + 1) + '.');
    } catch (error) { if (run === generation) showFailure(error instanceof PageError ? error : incompatible()); }
    finally { if (run === generation) $('workspace').setAttribute('aria-busy', 'false'); }
  }
  function applyFilters(event) {
    if (event) event.preventDefault();
    applied = { period: $('period').value, provider: $('provider').value, model: $('model').value, from: $('from').value.trim(), to: $('to').value.trim() };
    offset = 0;
    currentRange = null;
    load();
  }
  function invalidateSelection() {
    cancel();
    clearStatistics();
    offset = 0;
    currentRange = null;
    selectionNote = '';
  }
  $('filters').addEventListener('submit', applyFilters);
  $('period').addEventListener('change', () => {
    const selected = currentRange;
    invalidateSelection();
    const custom = $('period').value === 'custom';
    $('custom-range').hidden = !custom;
    if (custom) {
      try {
        if (currentCoverage && (!$('from').value || !$('to').value)) {
          const floor = instant(currentCoverage.from);
          const safeStart = floor + (floor === instant(currentCoverage.retention_floor) ? RETENTION_MARGIN : 0n);
          const reusable = selected && instant(selected.from) >= safeStart && instant(selected.to) <= instant(currentCoverage.to);
          const prefill = reusable ? selected : retainedRange(currentCoverage, floor);
          if (prefill) {
            // Never replace a date the user explicitly entered. Newly suggested
            // dates reuse the selection or leave disclosed retention headroom.
            if (!$('from').value) { $('from').value = prefill.from; selectionNote = prefill.note; }
            if (!$('to').value) $('to').value = prefill.to;
            $('range-note').textContent = selectionNote;
            $('range-note').hidden = !selectionNote;
          }
        }
        notice('Enter a custom UTC range, then apply filters.');
        $('from').focus();
      } catch (error) { showFailure(error instanceof PageError ? error : incompatible()); }
    } else applyFilters();
  });
  for (const id of ['provider', 'model', 'from', 'to']) $(id).addEventListener('input', () => {
    invalidateSelection();
    notice('Filters changed. Apply filters to load this selection.');
  });
  $('refresh').addEventListener('click', applyFilters);
  $('previous').addEventListener('click', () => { if (offset > 0) { offset -= PAGE_SIZE; load({ pageOnly: true }); } });
  $('next').addEventListener('click', () => { if (hasMore && offset + PAGE_SIZE <= 100000) { offset += PAGE_SIZE; load({ pageOnly: true }); } });
  // Storage events come from the console/other tabs. Any session mutation clears
  // private DOM immediately and stops requests; only an explicit refresh resumes.
  window.addEventListener('storage', (event) => { if (event.key === null || STORAGE_NAMES.has(event.key)) sessionHelp('The console session changed. Private values have been cleared and requests stopped. Finish signing in to this exact console origin and API base with “Remember password”, then refresh.'); });
  window.addEventListener('focus', () => { if (!readSession()) sessionHelp(); });
  document.addEventListener('visibilitychange', () => { if (!document.hidden && !readSession()) sessionHelp(); });
  window.addEventListener('pagehide', () => { cancel(); clearPrivate(); });
  load();
})();
