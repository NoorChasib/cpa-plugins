(() => {
  'use strict';

  // Session reading is adapted from Token Usage 4286578, which audited the
  // official Management Center v1.22.15 storage format. Only a remembered
  // console session on this exact origin and API base is used.
  const RESOURCE_SUFFIX = '/v0/resource/plugins/codex-catalog-filter/settings';
  const STATE_PATH = '/v0/management/plugins/codex-catalog-filter/state';
  const CONFIG_PATH = '/v0/management/plugins/codex-catalog-filter/config';
  const CATALOG_PATH = '/v0/resource/plugins/codex-catalog-filter/models';
  const AUTH_NAME = 'cli-proxy-auth';
  const STORAGE_NAMES = new Set([AUTH_NAME, 'isLoggedIn', 'apiBase', 'apiUrl', 'managementKey']);
  const MAX_BODY = 4 * 1024 * 1024;

  const $ = (id) => document.getElementById(id);
  const text = (value, max) => typeof value === 'string' && value.length <= max;
  const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value);
  function node(tag, content, className) {
    const element = document.createElement(tag);
    if (content !== undefined) element.textContent = content;
    if (className) element.className = className;
    return element;
  }

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
  function stored(name) {
    const value = localStorage.getItem(name);
    if (value !== null && value.length > 32768) throw new Error('storage value too large');
    return value;
  }
  // The console's optional XOR/Base64 codec is reversible obfuscation, NOT
  // encryption or a security boundary.
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
  const sameSession = (a, b) => a && b && a.key === b.key && a.base === b.base;

  class PageError extends Error {
    constructor(kind, message) { super(message); this.kind = kind; }
  }

  let state = null; // the last state the plugin reported
  let changes = new Map(); // slug -> enabled, not yet saved
  let forgotten = new Set(); // saved switches to delete on save
  let busy = false;
  let stopped = false; // set after an auth failure; only a manual refresh resumes

  function announce(message) { $('announcement').textContent = message; }
  function notice(message, tone = 'info') {
    $('notice').className = 'alert ' + tone;
    $('notice').textContent = message;
    $('notice').hidden = !message;
    if (message) announce(message);
  }
  function clearPrivate() {
    state = null;
    changes = new Map();
    forgotten = new Set();
    $('workspace').hidden = true;
    for (const id of ['rows', 'stale-list', 'codex-setting', 'counts', 'seen', 'warning', 'empty']) $(id).replaceChildren();
    for (const id of ['new-models', 'action']) $(id).hidden = true;
    $('search').value = '';
  }
  function sessionHelp(message) {
    clearPrivate();
    $('list-state').className = 'pill warn';
    $('list-state').textContent = 'Session required';
    notice(message || 'No compatible remembered console session is available. Sign in to the console using this page’s exact origin and API base' + (root ? ' (' + root + ')' : '') + ', enable “Remember password”, then refresh. Browser storage must be allowed.', 'warn');
  }

  async function readJSON(response) {
    if (response.headers.get('content-type')?.split(';')[0].trim().toLowerCase() !== 'application/json') throw new PageError('incompatible', 'The server returned an unexpected response.');
    const raw = await response.text();
    if (raw.length > MAX_BODY) throw new PageError('incompatible', 'The server returned an unexpectedly large response.');
    try { return JSON.parse(raw); } catch { throw new PageError('incompatible', 'The server returned malformed data.'); }
  }

  // One request per user action; a rejected key is never retried, because CPA
  // bans an IP after repeated management-key failures.
  async function call(path, init = {}) {
    if (stopped) throw new PageError('auth', '');
    const session = readSession();
    if (!session) throw new PageError('session', '');
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 15000);
    try {
      const headers = { Authorization: 'Bearer ' + session.key, Accept: 'application/json' };
      if (init.body) headers['Content-Type'] = 'application/json';
      const response = await fetch(root + path, { method: init.method || 'GET', body: init.body, headers, mode: 'same-origin', credentials: 'same-origin', cache: 'no-store', redirect: 'error', signal: controller.signal });
      if (!sameSession(session, readSession())) throw new PageError('session', '');
      if (response.status === 401 || response.status === 403) {
        stopped = true;
        throw new PageError('auth', 'The console session was rejected or is no longer authorized. Requests have stopped. Sign in again with “Remember password”, then refresh.');
      }
      const value = await readJSON(response);
      if (response.status === 503 && value.error === 'not_configured') throw new PageError('unavailable', 'Codex Catalog Filter is not configured yet. Check its entry under plugins.configs in CPA’s configuration.');
      if (!response.ok) throw new PageError('failed', 'The request failed with HTTP ' + response.status + '. Nothing was retried.');
      return value;
    } catch (error) {
      if (error instanceof PageError) throw error;
      throw new PageError('network', 'The request failed or timed out. Nothing was retried; refresh to try again.');
    } finally {
      clearTimeout(timeout);
    }
  }

  function validState(value) {
    return object(value) && Array.isArray(value.models) && object(value.switches) &&
      ['enabled', 'disabled'].includes(value.new_models) && ['remove', 'hide'].includes(value.action) &&
      value.models.every((m) => object(m) && text(m.slug, 256) && m.slug && typeof m.enabled === 'boolean' && typeof m.switched === 'boolean') &&
      Object.entries(value.switches).every(([slug, enabled]) => text(slug, 256) && typeof enabled === 'boolean');
  }

  function effective(model) { return changes.has(model.slug) ? changes.get(model.slug) : model.enabled; }
  function dirty() { return changes.size > 0 || forgotten.size > 0; }

  function noteFor(model) {
    const notes = [];
    if (changes.has(model.slug)) notes.push('Changed, not saved');
    else if (model.switched) notes.push('Set by you');
    else notes.push(state.new_models === 'enabled' ? 'Default: on' : 'Default: off');
    if (model.visibility === 'hide') notes.push('Hidden by CPA: never in the picker, but kept for Codex’s own use when on');
    if (model.slug === 'codex-auto-review') notes.push('Used by approvals_reviewer = "auto_review"');
    return notes.join(' · ');
  }

  function matches(model, query) {
    if (!query) return true;
    return model.slug.toLowerCase().includes(query) || (typeof model.display_name === 'string' && model.display_name.toLowerCase().includes(query));
  }
  function shownModels() {
    const query = $('search').value.trim().toLowerCase();
    return state ? state.models.filter((m) => matches(m, query)) : [];
  }

  function render() {
    if (!state) return;
    const rows = [];
    for (const model of shownModels()) {
      const row = node('tr');
      if (changes.has(model.slug)) row.className = 'changed';
      const toggle = node('input');
      toggle.type = 'checkbox';
      toggle.setAttribute('role', 'switch');
      toggle.checked = effective(model);
      toggle.disabled = busy;
      toggle.setAttribute('aria-label', 'Show ' + model.slug + ' in Codex');
      toggle.addEventListener('change', () => { setModel(model, toggle.checked); render(); });
      const holder = node('label', undefined, 'switch');
      holder.append(toggle, node('span'));
      const cell = node('td');
      cell.append(holder);
      row.append(cell, node('td', model.slug, 'slug'), node('td', model.display_name || '—'), node('td', noteFor(model), 'notes'));
      rows.push(row);
    }
    $('rows').replaceChildren(...rows);
    const empty = state.models.length === 0
      ? 'Codex has not fetched its model list through this plugin yet. Add the Codex setting above, then start Codex. The list appears after its first fetch.'
      : rows.length === 0 ? 'No models match the filter.' : '';
    $('empty').textContent = empty;
    $('empty').hidden = !empty;

    const inPicker = state.models.filter((m) => effective(m) && m.visibility !== 'hide').length;
    const on = state.models.filter(effective).length;
    $('counts').textContent = on + ' of ' + state.models.length + ' on · ' + inPicker + ' in the picker';
    const warning = state.models.length > 0 && inPicker === 0
      ? 'No model would appear in Codex’s picker. The plugin never sends Codex an empty list, so it would serve CPA’s full list instead. Turn at least one model on.'
      : '';
    $('warning').textContent = warning;
    $('warning').hidden = !warning;

    const stale = Object.keys(state.switches).filter((slug) => !state.models.some((m) => m.slug === slug)).sort();
    $('stale').hidden = stale.length === 0;
    $('stale-list').replaceChildren(...stale.map((slug) => {
      const item = node('li');
      const label = node('code', slug + ': ' + (state.switches[slug] ? 'on' : 'off'));
      if (forgotten.has(slug)) label.className = 'gone';
      const button = node('button', forgotten.has(slug) ? 'Keep' : 'Forget', 'link');
      button.type = 'button';
      button.disabled = busy;
      button.addEventListener('click', () => { if (forgotten.has(slug)) forgotten.delete(slug); else forgotten.add(slug); render(); });
      item.append(label, button);
      return item;
    }));

    for (const id of ['enable-shown', 'disable-shown']) $(id).disabled = busy || rows.length === 0;
    $('save').disabled = busy || !dirty();
    $('discard').disabled = busy || !dirty();
    $('refresh').disabled = busy;
    $('workspace').setAttribute('aria-busy', String(busy));
  }

  // A change is pending only while it differs from what is applied now.
  function setModel(model, enabled) {
    if (enabled === model.enabled) changes.delete(model.slug);
    else changes.set(model.slug, enabled);
  }

  function showState(value) {
    state = value;
    changes = new Map();
    forgotten = new Set();
    $('codex-setting').textContent = 'model_catalog_url = "' + root + CATALOG_PATH + '"';
    $('new-models').textContent = 'New models: ' + (value.new_models === 'enabled' ? 'on until switched off' : 'off until switched on');
    $('action').textContent = 'Switched off: ' + (value.action === 'remove' ? 'removed from Codex' : 'hidden in Codex');
    for (const id of ['new-models', 'action']) $(id).hidden = false;
    $('list-state').className = value.seen_at ? 'pill ok' : 'pill warn';
    $('list-state').textContent = value.seen_at ? 'List from Codex’s last fetch' : 'Waiting for Codex’s first fetch';
    $('seen').textContent = value.seen_at
      ? 'List last offered to Codex ' + value.seen_at.replace('T', ' ').replace(/\.\d+Z$|Z$/, ' UTC') + '.' + (value.saved ? '' : ' It could not be saved to disk and will be empty after a restart until Codex fetches again.')
      : '';
    $('workspace').hidden = false;
    render();
  }

  async function load() {
    if (busy) return;
    if (!root) { notice('Open this page through CPA’s plugin menu.', 'error'); return; }
    if (!readSession()) { sessionHelp(); return; }
    busy = true;
    render();
    try {
      const value = await call(STATE_PATH);
      if (!validState(value)) throw new PageError('incompatible', 'The plugin returned data this page does not understand. Update the plugin and refresh.');
      busy = false;
      showState(value);
      notice('');
    } catch (error) {
      busy = false;
      failure(error);
    }
  }

  function failure(error) {
    if (error.kind === 'session') { sessionHelp(); return; }
    if (error.kind === 'auth') { clearPrivate(); $('list-state').className = 'pill warn'; $('list-state').textContent = 'Session rejected'; notice(error.message || 'Requests have stopped. Refresh after signing in again.', 'error'); return; }
    render();
    notice(error.message, 'error');
  }

  async function save() {
    if (busy || !dirty() || !state) return;
    const switches = { ...state.switches };
    for (const slug of forgotten) delete switches[slug];
    for (const [slug, enabled] of changes) switches[slug] = enabled;
    busy = true;
    render();
    notice('Saving…');
    try {
      await call(CONFIG_PATH, { method: 'PATCH', body: JSON.stringify({ models: switches }) });
      // CPA applies the change on its configuration reload; confirm it landed.
      const wanted = JSON.stringify(Object.entries(switches).sort());
      let latest = null;
      for (let attempt = 0; attempt < 10; attempt++) {
        await new Promise((resolve) => setTimeout(resolve, 500));
        latest = await call(STATE_PATH);
        if (validState(latest) && JSON.stringify(Object.entries(latest.switches).sort()) === wanted) break;
        latest = null;
      }
      busy = false;
      if (!latest) { notice('Saved to CPA’s configuration, but the plugin has not applied it yet. Refresh in a moment to check.', 'warn'); render(); return; }
      showState(latest);
      notice('Saved. Codex shows the new list after its next refresh, within five minutes.', 'ok');
    } catch (error) {
      busy = false;
      failure(error);
    }
  }

  function bulk(enabled) {
    for (const model of shownModels()) setModel(model, enabled);
    render();
  }

  $('refresh').addEventListener('click', () => {
    if (dirty() && !confirm('Discard unsaved changes and reload?')) return;
    stopped = false;
    load();
  });
  $('search').addEventListener('input', render);
  $('enable-shown').addEventListener('click', () => bulk(true));
  $('disable-shown').addEventListener('click', () => bulk(false));
  $('discard').addEventListener('click', () => { changes = new Map(); forgotten = new Set(); render(); notice(''); });
  $('save').addEventListener('click', save);
  // Any console session change clears private values and stops requests; only
  // an explicit refresh resumes.
  window.addEventListener('storage', (event) => { if (event.key === null || STORAGE_NAMES.has(event.key)) sessionHelp('The console session changed. The list has been cleared and requests stopped. Sign in again with “Remember password”, then refresh.'); });
  window.addEventListener('beforeunload', (event) => { if (dirty()) event.preventDefault(); });
  window.addEventListener('pagehide', clearPrivate);
  load();
})();
