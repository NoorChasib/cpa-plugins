(() => {
  'use strict';

  // Session reader, endpoint allowlist, same-origin fetch, 15 s abort and 2 MiB bounded reader
  // are adapted from the Token Usage sidebar. A rejected session (401/403) stops all requests:
  // there is no automatic retry. The page renders counters, names and times only.
  const RESOURCE_SUFFIX = '/v0/resource/plugins/chat-archive/status';
  const AUTH_NAME = 'cli-proxy-auth';
  const STORAGE_NAMES = new Set([AUTH_NAME, 'isLoggedIn', 'apiBase', 'apiUrl', 'managementKey']);
  const HOOKS = ['before_auth', 'after_auth', 'response', 'stream_header_init', 'stream_chunk', 'lifecycle', 'usage', 'management'];
  const STATES = { running: 'ok', degraded: 'warn', unconfigured: 'warn', stopping: 'warn', stopped: 'warn', config_error: 'err', storage_unavailable: 'err' };
  const NAME = /^[A-Za-z0-9_.:-]{1,64}$/;
  const $ = (id) => document.getElementById(id);
  const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value);
  const text = (value, max = 4096) => typeof value === 'string' && value.length <= max;
  const whole = (value) => Number.isSafeInteger(value) && value >= 0;
  const name = (value) => text(value, 64) && NAME.test(value);
  const node = (tag, value, className) => {
    const element = document.createElement(tag);
    if (value !== undefined) element.textContent = value;
    if (className) element.className = className;
    return element;
  };
  class PageError extends Error {
    constructor(kind, message) { super(message); this.kind = kind; }
  }
  const incompatible = () => new PageError('response', 'The server returned an incompatible Chat Archive response. Check that the console and plug-in versions are compatible, then refresh.');
  let generation = 0;
  const pending = new Set();

  function apiRoot() {
    if (!['http:', 'https:'].includes(location.protocol) || !location.pathname.endsWith(RESOURCE_SUFFIX)) return null;
    return location.origin + location.pathname.slice(0, -RESOURCE_SUFFIX.length);
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
  function stored(key) {
    const value = localStorage.getItem(key);
    if (value !== null && value.length > 32768) throw new Error('storage value too large');
    return value;
  }
  // secureStorage JSON-serializes the Zustand {state, version} envelope before its optional
  // XOR/Base64 codec: enc::v2:: (console v1.27.x) is keyed by host only, legacy enc::v1::
  // (v1.22.15) by host and user agent. Reversible obfuscation, not encryption.
  function decode(raw, legacy = false) {
    if (raw === null) return null;
    let value = raw;
    const v1 = value.startsWith('enc::v1::');
    if (v1 || value.startsWith('enc::v2::')) {
      const bytes = Uint8Array.from(atob(value.slice(9)), (c) => c.charCodeAt(0));
      const salt = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + (v1 ? '|' + navigator.userAgent : ''));
      for (let i = 0; i < bytes.length; i++) bytes[i] ^= salt[i % salt.length];
      value = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
    }
    try { return JSON.parse(value); } catch (error) { if (legacy) return value; throw error; }
  }
  function validKey(value) {
    if (!text(value, 4096) || !value || value.trim() !== value || /[\x00-\x08\x0a-\x1f\x7f]/.test(value)) return false;
    try {
      const authorization = 'Bearer ' + value;
      return new Headers({ Authorization: authorization }).get('Authorization') === authorization;
    } catch { return false; }
  }
  function readSession() {
    try {
      const modern = stored(AUTH_NAME);
      if (modern !== null) {
        // Presence is authoritative even after logout or remember-off: never revive legacy keys.
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
  function clearPrivate() {
    $('workspace').hidden = true;
    for (const id of ['faults', 'outbox', 'hook-rows', 'details']) $(id).replaceChildren();
    for (const id of ['pending', 'oldest', 'last-ack']) $(id).textContent = '—';
    $('snapshot-at').textContent = '—';
    $('plugin-version').textContent = 'Version: —';
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
    $('plugin-state').className = 'pill';
    $('plugin-state').textContent = 'State: session required';
    notice(message || 'No compatible remembered console session is available. Sign in to the console using this page’s exact origin and API base' + (root ? ' (' + root + ')' : '') + ', enable “Remember password”, then refresh. Browser storage must be allowed.');
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
  async function request(endpoint, run) {
    if (run !== generation) throw new PageError('cancelled', '');
    const session = readSession();
    if (!session) throw new PageError('session', '');
    if (endpoint !== 'status') throw incompatible();
    const url = new URL(root + '/v0/management/plugins/chat-archive/' + endpoint);
    const controller = new AbortController();
    pending.add(controller);
    let timedOut = false;
    const timeout = setTimeout(() => { timedOut = true; controller.abort(); }, 15000);
    try {
      const response = await fetch(url.href, { method: 'GET', headers: { Authorization: 'Bearer ' + session.key, Accept: 'application/json' }, mode: 'same-origin', credentials: 'same-origin', cache: 'no-store', redirect: 'error', signal: controller.signal });
      if (run !== generation) throw new PageError('cancelled', '');
      if (!sameSession(session, readSession())) throw new PageError('session', '');
      if (response.status === 401 || response.status === 403) throw new PageError('auth', 'The console session was rejected or is no longer authorized. Requests have stopped. Sign in again using this exact console origin and API base, remember the session, then refresh.');
      if (!response.ok) throw incompatible();
      const value = await readJSON(response);
      if (run !== generation) throw new PageError('cancelled', '');
      if (!sameSession(session, readSession())) throw new PageError('session', '');
      return value;
    } catch (error) {
      if (run !== generation) throw new PageError('cancelled', '');
      if (timedOut) throw new PageError('timeout', 'The request timed out. No automatic retry was made. Refresh to try again.');
      if (error instanceof PageError) throw error;
      throw new PageError('network', 'Unable to load Chat Archive status. The request failed or was redirected; redirected login pages are not followed. Check the console connection, then refresh.');
    } finally {
      controller.abort();
      clearTimeout(timeout);
      pending.delete(controller);
    }
  }

  // ---- rendering: allowlisted fields only, every value checked before it is shown ----
  const count = (value) => whole(value) ? value.toLocaleString('en-US') : '—';
  const bytes = (value) => {
    if (!whole(value)) return '—';
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    let v = value, i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (i ? v.toFixed(1) : String(v)) + ' ' + units[i];
  };
  const age = (seconds) => {
    if (!whole(seconds)) return '—';
    if (seconds < 120) return seconds + ' s';
    if (seconds < 7200) return Math.round(seconds / 60) + ' min';
    return (seconds / 3600).toFixed(1) + ' h';
  };
  function when(value) {
    if (!text(value, 40) || !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{1,9})?(Z|[+-]\d\d:\d\d)$/.test(value)) return '—';
    if (value.startsWith('0001-')) return 'never';
    const time = new Date(value);
    return Number.isFinite(time.getTime()) ? time.toISOString().slice(0, 19).replace('T', ' ') + ' UTC' : '—';
  }
  function pairs(container, entries) {
    container.replaceChildren();
    for (const [label, value] of entries) {
      const item = node('div');
      item.append(node('dt', label), node('dd', value));
      container.append(item);
    }
  }
  function counterMap(value) {
    if (!object(value)) return [];
    return Object.entries(value).filter(([k, v]) => name(k) && whole(v)).slice(0, 64);
  }
  function names(value) {
    return Array.isArray(value) ? value.filter(name).slice(0, 64) : [];
  }
  function validate(value) {
    if (!object(value) || value.api_schema !== 1 || value.plugin !== 'chat-archive' || !name(value.state) || !text(value.version, 32)) throw incompatible();
    return value;
  }
  function render(status) {
    const state = status.state;
    $('plugin-state').textContent = 'State: ' + state + (name(status.reason) ? ' (' + status.reason + ')' : '');
    $('plugin-state').className = 'pill ' + (STATES[state] || 'warn');
    $('plugin-version').textContent = 'Version: ' + status.version + (whole(status.host_schema) ? ' · CPA schema ' + status.host_schema : '');
    $('snapshot-at').textContent = when(status.snapshot_at);
    const outbox = object(status.outbox) ? status.outbox : {};
    const uploader = object(status.uploader) ? status.uploader : {};
    $('pending').textContent = count(outbox.pending);
    $('oldest').textContent = age(outbox.oldest_pending_age_s);
    $('last-ack').textContent = when(outbox.last_ack_at);
    const dropped = counterMap(status.dropped).filter(([, v]) => v > 0);
    const panics = counterMap(status.recovered_panics).filter(([, v]) => v > 0);
    const faults = names(status.faults);
    pairs($('faults'), [
      ['Active faults', faults.join(', ') || 'None'],
      ['Dropped since start', dropped.map(([k, v]) => k + ' ' + count(v)).join(', ') || 'None'],
      ['Recovered panics', panics.map(([k, v]) => k + ' ' + count(v)).join(', ') || 'None'],
      ['Dead letters', counterMap(status.dead_letters).map(([k, v]) => k + ' ' + count(v)).join(', ') || 'None']
    ]);
    pairs($('outbox'), [
      ['Pending bytes', bytes(outbox.pending_bytes)], ['Dead letters', count(outbox.dead)],
      ['Acked, kept for replay', count(outbox.acked_retained)], ['Disk used', bytes(outbox.disk_bytes)],
      ['Outbox cap', bytes(outbox.cap_bytes)], ['Free space', bytes(outbox.free_bytes)],
      ['Pressure level', whole(outbox.pressure) ? String(outbox.pressure) : '—'], ['Next seq / hw', count(outbox.next_seq) + ' / ' + count(outbox.hw)],
      ['Last upload success', when(uploader.last_success_at)], ['Last error class', name(uploader.last_error_class) ? uploader.last_error_class : 'None'],
      ['Consecutive failures', count(uploader.consecutive_failures)], ['Archive epoch seen', count(uploader.epoch_seen)],
      ['Last usage record', when(status.last_usage_at)], ['Media fetches queued', count(status.media_fetch_queue)]
    ]);
    $('hook-rows').replaceChildren();
    const calls = object(status.calls) ? status.calls : {};
    const errors = object(status.hook_errors) ? status.hook_errors : {};
    const p50 = object(status.hook_p50_us) ? status.hook_p50_us : {};
    const p99 = object(status.hook_p99_us) ? status.hook_p99_us : {};
    const last = object(status.last_hook_at) ? status.last_hook_at : {};
    for (const hook of HOOKS) {
      const tr = node('tr');
      tr.append(node('td', hook.replaceAll('_', ' '), 'name'), node('td', count(calls[hook] ?? 0), 'num'), node('td', count(errors[hook] ?? 0), 'num'),
        node('td', count(p50[hook] ?? 0), 'num'), node('td', count(p99[hook] ?? 0), 'num'), node('td', last[hook] ? when(last[hook]) : 'never'));
      $('hook-rows').append(tr);
    }
    const config = object(status.config) ? status.config : {};
    const details = $('details');
    details.replaceChildren();
    const dl = node('dl', undefined, 'detail-grid');
    details.append(dl);
    pairs(dl, [
      ['Archive URL configured', config.archive_url_configured === true ? 'yes' : 'no'],
      ['Source token', name(config.source_token) ? config.source_token : '—'], ['HMAC key', name(config.hmac_key) ? config.hmac_key : '—'],
      ['Capture mode', name(config.capture) ? config.capture : '—'], ['Workers', count(config.workers)],
      ['Memory limit', whole(config.memory_limit_mib) ? config.memory_limit_mib + ' MiB' : '—'],
      ['Replay retention', whole(config.replay_retention_hours) ? config.replay_retention_hours + ' h' : '—'],
      ['Account labels', count(config.account_labels)], ['Extra exact fingerprints', count(config.exact_fingerprints)],
      ['Unclean previous runs', count(status.unclean_runs)], ['Plugin started', when(status.started_at)]
    ]);
    const jobs = Array.isArray(status.requeue_jobs) ? status.requeue_jobs.slice(-8) : [];
    details.append(node('p', jobs.length ? 'Requeue jobs:' : 'No requeue jobs since this plugin instance started.'));
    for (const job of jobs) {
      if (!object(job) || !name(job.id) || !name(job.state) || !name(job.reason)) continue;
      details.append(node('p', job.id + ' · ' + job.reason + (job.remask ? ' · remask' : '') + ' · ' + job.state + ' · processed ' + count(job.processed) + ', requeued ' + count(job.requeued) + ', skipped ' + count(job.skipped) + ', failed ' + count(job.failed)));
    }
    $('workspace').hidden = false;
    const warn = state !== 'running' || faults.length || dropped.length || panics.length;
    notice(warn ? 'Attention: the plugin is ' + state + (faults.length ? ', with active faults' : '') + (dropped.length ? ', and some captures were dropped' : '') + '. Details below.' : '', warn ? 'warn' : 'info');
    announce('Chat Archive status loaded: ' + state + ', ' + count(outbox.pending) + ' pending.');
  }
  async function load() {
    cancel();
    const run = generation;
    if (!root || !readSession()) { sessionHelp(); return; }
    $('workspace').setAttribute('aria-busy', 'true');
    notice('Loading chat archive status…');
    try {
      const status = validate(await request('status', run));
      if (run !== generation) return;
      render(status);
    } catch (error) {
      if (run !== generation) return;
      const failure = error instanceof PageError ? error : incompatible();
      if (failure.kind === 'cancelled') return;
      if (failure.kind === 'session' || failure.kind === 'auth') { sessionHelp(failure.message); return; }
      clearPrivate();
      notice(failure.message, '');
    } finally {
      if (run === generation) $('workspace').setAttribute('aria-busy', 'false');
    }
  }
  $('refresh').addEventListener('click', load);
  // Storage events come from the console or other tabs. Any session change clears private DOM
  // at once and stops requests; only an explicit refresh resumes.
  window.addEventListener('storage', (event) => { if (event.key === null || STORAGE_NAMES.has(event.key)) sessionHelp('The console session changed. Status has been cleared and requests stopped. Finish signing in to this exact console origin and API base with “Remember password”, then refresh.'); });
  window.addEventListener('focus', () => { if (!readSession()) sessionHelp(); });
  document.addEventListener('visibilitychange', () => { if (!document.hidden && !readSession()) sessionHelp(); });
  window.addEventListener('pagehide', () => { cancel(); clearPrivate(); });
  load();
})();
