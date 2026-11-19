// Per-client selection logic, without DOM, storage or network. It is a
// top-level constant so scripts/tests can call it; it holds no data. The
// Antigravity preview mirrors internal/agy Build for the page's live counter;
// the plugin's own /state preview stays authoritative once saved.
const ModelPickerLogic = (() => {
  'use strict';

  const CLIENTS = Object.freeze(['codex', 'antigravity', 'claude']);
  // agy 1.5.12 keeps at most this many ids it has no built-in entry for.
  const SLOT_LIMIT = 32;
  const FLASH_LITE = 'flash-lite';

  const idOf = (client, model) => (client === 'codex' ? model.slug : model.id);
  // Orders ids as Go's strings.Compare does (code point order), as the plugin
  // sorts agy's list: CPA's own order changes on every configuration reload.
  function compareIds(a, b) {
    const x = Array.from(a, (c) => c.codePointAt(0));
    const y = Array.from(b, (c) => c.codePointAt(0));
    for (let i = 0; i < Math.min(x.length, y.length); i++) if (x[i] !== y[i]) return x[i] - y[i];
    return x.length - y.length;
  }

  // The part of a /state response that describes client.
  function section(client, state) {
    if (client === 'codex') return state;
    return client === 'antigravity' ? state.antigravity : state.claude_code;
  }

  // The client's saved settings as /state reports them. Switches are a Map so
  // that no model id can reach an object's prototype.
  function base(client, state) {
    const s = section(client, state);
    const config = { switches: new Map(Object.entries(s.switches)), newModels: s.new_models };
    if (client === 'codex') config.action = s.action;
    if (client === 'antigravity') { config.defaultModel = s.default_model; config.titleModel = s.title_model; }
    if (client === 'claude') config.filter = s.filter;
    return config;
  }

  // Unsaved changes: switches set, saved switches to forget, and settings
  // (newModels, action, defaultModel, titleModel, filter).
  const emptyPending = () => ({ switches: new Map(), forgotten: new Set(), settings: new Map() });
  const clonePending = (p) => ({ switches: new Map(p.switches), forgotten: new Set(p.forgotten), settings: new Map(p.settings) });
  const dirty = (p) => p.switches.size > 0 || p.forgotten.size > 0 || p.settings.size > 0;

  function apply(config, pending) {
    const out = { ...config, switches: new Map(config.switches) };
    for (const id of pending.forgotten) out.switches.delete(id);
    for (const [id, on] of pending.switches) out.switches.set(id, on);
    for (const [name, value] of pending.settings) out[name] = value;
    return out;
  }

  function sameConfig(a, b) {
    if (a.switches.size !== b.switches.size) return false;
    for (const [id, on] of a.switches) if (b.switches.get(id) !== on) return false;
    return ['newModels', 'action', 'defaultModel', 'titleModel', 'filter'].every((name) => a[name] === b[name]);
  }

  // Whether model is on under config, as the plugin decides it: an explicit
  // switch wins; for agy the default and title models are then on and models
  // that do not answer in text off; everything else follows newModels.
  function isEnabled(client, model, config) {
    const id = idOf(client, model);
    if (config.switches.has(id)) return config.switches.get(id);
    if (client === 'antigravity') {
      if (id === config.defaultModel || id === config.titleModel) return true;
      return config.newModels === 'enabled' && !model.non_text;
    }
    return config.newModels === 'enabled';
  }

  // A change is pending only while it differs from what applies without it.
  function setSwitch(client, pending, config, model, on) {
    const id = idOf(client, model);
    const without = clonePending(pending);
    without.switches.delete(id);
    if (isEnabled(client, model, apply(config, without)) === on) pending.switches.delete(id);
    else pending.switches.set(id, on);
  }

  function setSetting(pending, config, name, value) {
    if (config[name] === value) pending.settings.delete(name);
    else pending.settings.set(name, value);
  }

  // Carries unsaved changes onto a newer saved config, dropping those it
  // already holds.
  function normalize(client, pending, config, models) {
    for (const [name, value] of [...pending.settings]) if (config[name] === value) pending.settings.delete(name);
    for (const id of [...pending.forgotten]) if (!config.switches.has(id)) pending.forgotten.delete(id);
    const byId = new Map(models.map((m) => [idOf(client, m), m]));
    for (const [id, on] of [...pending.switches]) {
      const model = byId.get(id);
      if (model) setSwitch(client, pending, config, model, on);
      else if (config.switches.get(id) === on) pending.switches.delete(id);
    }
  }

  const sortedObject = (map) => Object.fromEntries([...map].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));

  // The PATCH body for client. CPA replaces whole top-level keys, so the
  // Antigravity and Claude Code bodies always carry the complete subtree; Codex
  // sends its switches as before, and new-models or action only when changed.
  function patchBody(client, config, pending) {
    const models = sortedObject(config.switches);
    if (client === 'codex') {
      const body = { models };
      if (pending.settings.has('newModels')) body['new-models'] = config.newModels;
      if (pending.settings.has('action')) body.action = config.action;
      return body;
    }
    if (client === 'antigravity') {
      const subtree = { models, 'new-models': config.newModels };
      if (config.defaultModel) subtree['default-model'] = config.defaultModel;
      if (config.titleModel) subtree['title-model'] = config.titleModel;
      return { antigravity: subtree };
    }
    return { 'claude-code': { filter: config.filter, models, 'new-models': config.newModels } };
  }

  // Whether a /state response shows config applied for client without error.
  function applied(client, state, config) {
    const s = section(client, state);
    const switches = Object.entries(s.switches);
    if (switches.length !== config.switches.size || !switches.every(([id, on]) => config.switches.get(id) === on) || s.new_models !== config.newModels) return false;
    if (client === 'codex') return s.action === config.action;
    if (s.error !== '') return false;
    if (client === 'antigravity') return s.default_model === (config.defaultModel || '') && s.title_model === (config.titleModel || '');
    return s.filter === config.filter;
  }

  // The list agy would receive under config, from the /state antigravity
  // models: selection, fallback, id order, default and title model, slots, and
  // the plugin's own warnings.
  function agyPreview(models, config) {
    const warnings = [];
    let list = models.filter((m) => isEnabled('antigravity', m, config));
    let fallback = false;
    // agy hides image models itself, so a list of only those is no list.
    if (!list.some((m) => !m.hidden_by_agy)) {
      const reason = list.length > 0 ? 'Every switched-on model is an image model agy hides' : 'No model is switched on';
      fallback = true;
      const text = (m) => !m.non_text && !m.hidden_by_agy;
      list = models.filter((m) => text(m) && m.id.startsWith('gemini-'));
      if (list.length === 0) list = models.filter(text);
      if (list.length > 0) warnings.push(reason + ", so CPA's Gemini text models are served instead.");
    }
    const preview = { entries: [], positions: new Map(), skipped: [], slotsUsed: 0, slotLimit: SLOT_LIMIT, defaultId: '', titleId: '', fallback, warnings };
    if (list.length === 0) return preview;

    const out = list.slice().sort((a, b) => compareIds(a.id, b.id));
    const index = (id) => out.findIndex((m) => m.id === id);
    if (config.defaultModel) {
      const i = index(config.defaultModel);
      if (i > 0) out.unshift(...out.splice(i, 1));
    }
    const title = config.titleModel || '';
    if (title.includes(FLASH_LITE) && title !== out[0].id) {
      const i = index(title);
      let first = -1;
      for (let j = 1; j < i; j++) if (out[j].id.includes(FLASH_LITE)) { first = j; break; }
      if (first > 0) out.splice(first, 0, ...out.splice(i, 1));
    }

    const used = new Set();
    for (const m of out) {
      let slot;
      if (m.hidden_by_agy) slot = 'image';
      else if (m.builtin && !used.has(m.builtin_enum)) { used.add(m.builtin_enum); slot = 'builtin'; }
      else if (preview.slotsUsed >= SLOT_LIMIT) { preview.skipped.push(m.id); continue; }
      else { preview.slotsUsed++; slot = 'placeholder'; }
      preview.entries.push({ id: m.id, model: m, slot });
      preview.positions.set(m.id, preview.entries.length);
    }
    // agy drops image models, so one cannot be its default.
    preview.defaultId = (preview.entries.find((e) => e.slot !== 'image') || preview.entries[0]).id;
    if (config.defaultModel && config.defaultModel !== preview.defaultId) {
      const why = preview.positions.has(config.defaultModel) ? ' is an image model agy hides' : ' is not in the list';
      warnings.push('The default model ' + config.defaultModel + why + ', so ' + preview.defaultId + ' is the default.');
    }
    if (preview.skipped.length > 0) warnings.push(preview.skipped.length + ' switched-on models are left out: agy shows at most 32 models it has no built-in entry for.');
    preview.titleId = (preview.entries.find((e) => e.id.includes(FLASH_LITE)) || { id: '' }).id;
    if (title && preview.titleId !== title) {
      if (!preview.positions.has(title)) warnings.push('The title model ' + title + ' is not in the list.');
      else if (!title.includes(FLASH_LITE)) warnings.push('agy titles threads with the first model whose id contains flash-lite, so ' + title + ' is not used for titles.');
      else warnings.push('The default model also contains flash-lite and comes first, so agy titles threads with it.');
    }
    return preview;
  }

  return Object.freeze({ CLIENTS, SLOT_LIMIT, idOf, compareIds, section, base, emptyPending, clonePending, dirty, apply, sameConfig, isEnabled, setSwitch, setSetting, normalize, patchBody, applied, agyPreview });
})();

(() => {
  'use strict';

  // Session reading is adapted from Token Usage b7683a1, which audited the
  // official Management Center v1.22.15 storage format. Only a remembered
  // console session on this exact origin and API base is used.
  const RESOURCE_SUFFIX = '/v0/resource/plugins/codex-catalog-filter/settings';
  const STATE_PATH = '/v0/management/plugins/codex-catalog-filter/state';
  const CONFIG_PATH = '/v0/management/plugins/codex-catalog-filter/config';
  const CATALOG_PATH = '/v0/resource/plugins/codex-catalog-filter/models';
  const AGY_PATH = '/v0/resource/plugins/codex-catalog-filter/agy/models';
  const AUTH_NAME = 'cli-proxy-auth';
  const STORAGE_NAMES = new Set([AUTH_NAME, 'isLoggedIn', 'apiBase', 'apiUrl', 'managementKey']);
  // The page's own entry: which client sections are open. Never a credential.
  const SECTIONS_KEY = 'codex-catalog-filter:open-sections';
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
  // encryption or a security boundary. Management Center v1.27.0 and later
  // write enc::v2::, keyed by the host alone; older consoles wrote enc::v1::,
  // keyed by the host and the browser's user agent. Another enc:: version is
  // not decoded, so it fails to parse and gives no session.
  function decode(raw, legacy = false) {
    if (raw === null) return null;
    let value = raw;
    const current = value.startsWith('enc::v2::');
    if (current || value.startsWith('enc::v1::')) {
      const bytes = Uint8Array.from(atob(value.slice(9)), (c) => c.charCodeAt(0));
      const salt = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + (current ? '' : '|' + navigator.userAgent));
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

  const L = ModelPickerLogic;
  // Each client's element id prefix and wording.
  const CLIENT = {
    codex: { prefix: 'codex', name: 'Codex', key: 'models' },
    antigravity: { prefix: 'agy', name: 'Antigravity', short: 'agy', key: 'antigravity' },
    claude: { prefix: 'claude', name: 'Claude Code', key: 'claude-code' },
  };
  const part = (client, name) => $(CLIENT[client].prefix + '-' + name);

  let state = null; // the last state the plugin reported
  let usable = { codex: false, antigravity: false, claude: false }; // section data understood
  let pending = { codex: L.emptyPending(), antigravity: L.emptyPending(), claude: L.emptyPending() };
  const toggles = { codex: new Map(), antigravity: new Map(), claude: new Map() }; // id -> switch, for focus
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
    usable = { codex: false, antigravity: false, claude: false };
    pending = { codex: L.emptyPending(), antigravity: L.emptyPending(), claude: L.emptyPending() };
    $('workspace').hidden = true;
    $('version').hidden = true;
    for (const client of L.CLIENTS) {
      toggles[client].clear();
      for (const name of ['rows', 'stale-list', 'setup', 'counts', 'seen', 'empty', 'summary', 'unusable']) part(client, name).replaceChildren();
      for (const name of ['stale', 'dirty', 'unusable']) part(client, name).hidden = true;
      part(client, 'search').value = '';
    }
    for (const id of ['codex-warning', 'agy-error', 'agy-source', 'agy-warnings', 'agy-slots', 'agy-roles', 'agy-default', 'agy-title', 'agy-order-list', 'claude-error', 'claude-source', 'claude-warning']) $(id).replaceChildren();
    for (const id of ['codex-warning', 'agy-error', 'agy-source', 'agy-warnings', 'claude-error', 'claude-source', 'claude-warning']) $(id).hidden = true;
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
      if (response.status === 503 && value.error === 'not_configured') throw new PageError('unavailable', 'Model Picker is not configured yet. Check its codex-catalog-filter entry under plugins.configs in CPA’s configuration.');
      if (!response.ok) throw new PageError('failed', 'The request failed with HTTP ' + response.status + '. Nothing was retried.');
      return value;
    } catch (error) {
      if (error instanceof PageError) throw error;
      throw new PageError('network', 'The request failed or timed out. Nothing was retried; refresh to try again.');
    } finally {
      clearTimeout(timeout);
    }
  }

  // The Codex fields are 0.1.1's and must be understood; each other client's
  // section is checked on its own, so a section this page cannot read is shown
  // as unavailable without taking the others down.
  const NEW_MODELS = ['enabled', 'disabled'];
  const validSwitches = (value) => object(value) && Object.entries(value).every(([id, on]) => text(id, 256) && id && typeof on === 'boolean');
  const optionalText = (value, max) => value === undefined || value === null || text(value, max);
  function validState(value) {
    return object(value) && Array.isArray(value.models) && validSwitches(value.switches) &&
      NEW_MODELS.includes(value.new_models) && ['remove', 'hide'].includes(value.action) &&
      value.models.every((m) => object(m) && text(m.slug, 256) && m.slug && typeof m.enabled === 'boolean' && typeof m.switched === 'boolean');
  }
  function validClient(s, key) {
    return object(s) && s.config_key === key && text(s.error, 4096) && text(s.using, 32) && NEW_MODELS.includes(s.new_models) &&
      validSwitches(s.switches) && text(s.models_source, 32) && Array.isArray(s.models);
  }
  function validAntigravity(s) {
    return validClient(s, 'antigravity') && text(s.default_model, 256) && text(s.title_model, 256) &&
      s.models.every((m) => object(m) && text(m.id, 256) && m.id && text(m.label, 4096) && text(m.tokens, 16) &&
        ['enabled', 'switched', 'non_text', 'hidden_by_agy', 'builtin', 'skipped'].every((name) => typeof m[name] === 'boolean') &&
        (m.builtin_enum === undefined || Number.isInteger(m.builtin_enum)));
  }
  function validClaude(s) {
    return validClient(s, 'claude-code') && typeof s.filter === 'boolean' &&
      s.models.every((m) => object(m) && text(m.id, 256) && m.id && typeof m.enabled === 'boolean' && typeof m.switched === 'boolean' && optionalText(m.cloaked_id, 512));
  }
  function validFor(client, value) {
    if (!validState(value)) return false;
    if (client === 'antigravity') return validAntigravity(value.antigravity);
    if (client === 'claude') return validClaude(value.claude_code);
    return true;
  }

  const when = (value) => (text(value, 64) ? value.replace('T', ' ').replace(/\.\d+Z$|Z$/, ' UTC') : '');
  const models = (client) => (state && usable[client] ? L.section(client, state).models : []);
  const saved = (client) => L.base(client, state);
  const current = (client) => L.apply(saved(client), pending[client]);
  const byId = (client, id) => models(client).find((m) => L.idOf(client, m) === id);

  function matches(client, model, query) {
    if (!query) return true;
    return [L.idOf(client, model), model.display_name, model.label, model.cloaked_id].some((value) => typeof value === 'string' && value.toLowerCase().includes(query));
  }
  function shownModels(client) {
    const query = part(client, 'search').value.trim().toLowerCase();
    return models(client).filter((m) => matches(client, m, query));
  }

  function chip(content, tone) { return node('span', content, 'chip' + (tone ? ' ' + tone : '')); }
  function statusNote(client, model, config) {
    const id = L.idOf(client, model);
    if (pending[client].switches.has(id)) return chip('Changed, not saved', 'info');
    if (saved(client).switches.has(id)) return chip('Set by you');
    if (client === 'antigravity' && (id === config.defaultModel || id === config.titleModel)) return chip('On: chosen as ' + (id === config.defaultModel ? 'default' : 'title') + ' model');
    // No switch: the model follows "Models without a switch", except that agy
    // leaves models that do not answer in text out.
    if (client === 'antigravity' && model.non_text) return chip('No switch: off (not a text model)');
    return chip(config.newModels === 'enabled' ? 'No switch: on' : 'No switch: off');
  }
  const number = (value) => (Number.isInteger(value) ? value.toLocaleString('en-US') : '');

  function notesFor(client, model, config, preview) {
    const notes = [statusNote(client, model, config)];
    if (client === 'codex') {
      if (model.visibility === 'hide') notes.push(chip('Hidden by CPA: never in the picker, but kept for Codex’s own use when on'));
      if (model.slug === 'codex-auto-review') notes.push(chip('Used by approvals_reviewer = "auto_review"'));
    } else if (client === 'antigravity') {
      if (preview.defaultId === model.id) notes.push(chip('Default model', 'ok'));
      if (preview.titleId === model.id) notes.push(chip('Title model', 'ok'));
      if (preview.skipped.includes(model.id)) notes.push(chip('Left out: over 32 slots', 'warn'));
      if (model.hidden_by_agy) notes.push(chip('Image model: agy hides it', 'warn'));
      else if (model.non_text) notes.push(chip('Not a text model', 'warn'));
      else if (model.builtin) notes.push(chip('Built in to agy'));
      if (model.tokens === 'clamped') notes.push(chip('Output limit fixed: ' + number(model.max_output_tokens) + ' (CPA: ' + number(model.output_token_limit) + ')', 'info'));
      if (Number.isInteger(model.thinking_budget)) notes.push(chip('Effort kept: thinking budget ' + number(model.thinking_budget)));
    } else if (text(model.cloaked_id, 512) && model.cloaked_id) {
      notes.push(chip('Claude Code sees ' + model.cloaked_id));
    }
    return notes;
  }

  function switchCell(client, model, on) {
    const id = L.idOf(client, model);
    const toggle = node('input');
    toggle.type = 'checkbox';
    toggle.setAttribute('role', 'switch');
    toggle.checked = on;
    toggle.disabled = busy;
    toggle.setAttribute('aria-label', 'Show ' + id + ' in ' + CLIENT[client].name);
    toggle.addEventListener('change', () => { setModel(client, model, toggle.checked); render(client); focusToggle(client, id); });
    toggles[client].set(id, toggle);
    const holder = node('label', undefined, 'switch');
    holder.append(toggle, node('span'));
    const cell = node('td');
    cell.append(holder);
    return cell;
  }
  function focusToggle(client, id) {
    const toggle = toggles[client].get(id);
    if (toggle && typeof toggle.focus === 'function') toggle.focus();
  }

  function renderRows(client, config, preview) {
    toggles[client].clear();
    const rows = [];
    for (const model of shownModels(client)) {
      const id = L.idOf(client, model);
      const row = node('tr');
      if (pending[client].switches.has(id)) row.className = 'changed';
      const notes = node('td', undefined, 'notes');
      notes.append(...notesFor(client, model, config, preview));
      row.append(switchCell(client, model, L.isEnabled(client, model, config)));
      if (client === 'antigravity') {
        const position = preview.positions.get(id);
        row.append(node('td', position ? String(position) : '—', 'pos'), node('td', id, 'slug'), node('td', text(model.label, 4096) && model.label ? model.label : id, 'name'));
      } else {
        row.append(node('td', id, 'slug'), node('td', text(model.display_name, 4096) && model.display_name ? model.display_name : '—', 'name'));
      }
      row.append(notes);
      rows.push(row);
    }
    part(client, 'rows').replaceChildren(...rows);
    return rows.length;
  }

  function renderStale(client) {
    const ids = new Set(models(client).map((m) => L.idOf(client, m)));
    const config = saved(client);
    const stale = [...config.switches.keys()].filter((id) => !ids.has(id)).sort();
    part(client, 'stale').hidden = stale.length === 0;
    part(client, 'stale-list').replaceChildren(...stale.map((id) => {
      const forgotten = pending[client].forgotten.has(id);
      const item = node('li');
      const label = node('code', id + ': ' + (config.switches.get(id) ? 'on' : 'off'));
      if (forgotten) label.className = 'gone';
      const button = node('button', forgotten ? 'Keep' : 'Forget', 'link');
      button.type = 'button';
      button.disabled = busy;
      button.addEventListener('click', () => { if (forgotten) pending[client].forgotten.delete(id); else pending[client].forgotten.add(id); edited(); render(client); });
      item.append(label, button);
      return item;
    }));
  }

  function setText(id, message) {
    $(id).textContent = message;
    $(id).hidden = !message;
  }

  // The client's saved value was rejected by the plugin: say what applies.
  function clientError(client) {
    const s = L.section(client, state);
    if (!s.error) return '';
    const using = s.using === 'last-good' ? 'the last valid settings this plugin applied' : 'its defaults';
    return 'The plugin rejected the ' + CLIENT[client].key + ' value in CPA’s configuration: ' + s.error + '. ' + CLIENT[client].name + ' uses ' + using + ' until it is replaced. Save this section to replace it with the settings shown here.';
  }
  // A rejected value can be replaced by saving, even with nothing changed.
  const rejected = (client) => client !== 'codex' && state !== null && usable[client] && L.section(client, state).error !== '';
  const saveable = (client) => L.dirty(pending[client]) || rejected(client);
  const own = (map, key) => Object.prototype.hasOwnProperty.call(map, key);
  const SOURCES = { codex: 'Codex’s', antigravity: 'agy’s', claude_code: 'Claude Code’s' };
  function sourceNote(client) {
    const source = L.section(client, state).models_source;
    if (!own(SOURCES, source)) return '';
    const who = client === 'antigravity' ? 'agy has' : 'Claude Code has';
    return who + ' not fetched its list through the plugin yet, so these models come from ' + SOURCES[source] + ' last fetch.' + (client === 'antigravity' ? ' Flags and token limits are complete after agy’s first fetch.' : '');
  }

  const AGY_ERRORS = { cpa_unavailable: 'CPA could not answer', cpa_invalid_list: 'CPA did not return a model list', no_text_models: 'CPA listed no text model' };
  function seenText(client) {
    const s = L.section(client, state);
    const unsaved = s.saved === false ? ' The lists could not be saved to disk, so this one is lost after a restart until the client fetches again.' : '';
    if (client === 'codex') {
      return when(s.seen_at) ? 'List last offered to Codex ' + when(s.seen_at) + '.' + (s.saved ? '' : ' It could not be saved to disk and will be empty after a restart until Codex fetches again.') : '';
    }
    if (client === 'antigravity') {
      let out = when(s.fetched_at) ? 'agy last fetched its list ' + when(s.fetched_at) + '.' : '';
      if (s.served_from === 'last-good' && when(s.served_at)) out += ' Its last request, ' + when(s.served_at) + ', got the saved list because CPA could not answer.';
      if (own(AGY_ERRORS, s.last_error) && when(s.last_error_at)) out += ' Last problem: ' + AGY_ERRORS[s.last_error] + ', ' + when(s.last_error_at) + '.';
      return out + (out ? unsaved : '');
    }
    if (!when(s.seen_at)) return '';
    const counted = Number.isInteger(s.lists) ? ' Since the plugin loaded: ' + s.lists + ' lists seen, ' + (Number.isInteger(s.rewritten) ? s.rewritten : 0) + ' changed.' : '';
    const errors = Number.isInteger(s.intercept_errors) && s.intercept_errors > 0 ? ' ' + s.intercept_errors + ' responses could not be checked and were passed through unchanged.' : '';
    return 'Claude Code last fetched its list ' + when(s.seen_at) + '.' + counted + errors + unsaved;
  }

  function fillSelect(select, options, value) {
    select.replaceChildren(...options.map(([optionValue, label]) => {
      const option = node('option', label);
      option.value = optionValue;
      return option;
    }));
    select.value = value;
  }
  function agySelects(config) {
    const list = models('antigravity').slice().sort((a, b) => L.compareIds(a.id, b.id));
    const option = (m) => [m.id, m.id];
    const chosen = (value, offered, why) => (value && !offered.some((m) => m.id === value) ? [[value, value + ' (' + (byId('antigravity', value) ? why : 'not offered') + ')']] : []);
    const textModels = list.filter((m) => !m.non_text);
    fillSelect(part('antigravity', 'default'), [['', 'First model by id'], ...textModels.map(option), ...chosen(config.defaultModel, textModels, 'not a text model')], config.defaultModel || '');
    const lite = textModels.filter((m) => m.id.includes('flash-lite'));
    fillSelect(part('antigravity', 'title'), [['', 'First flash-lite model in the list'], ...lite.map(option), ...chosen(config.titleModel, lite, 'not flash-lite: never used for titles')], config.titleModel || '');
  }

  function render(client) {
    if (!state) return;
    const unusable = !usable[client];
    part(client, 'content').hidden = unusable;
    setText(CLIENT[client].prefix + '-unusable', unusable ? 'The plugin returned data for ' + CLIENT[client].name + ' that this page does not understand. Update the plugin and refresh.' : '');
    part(client, 'dirty').hidden = unusable || !L.dirty(pending[client]);
    if (unusable) { part(client, 'summary').textContent = ''; return; }

    const config = current(client);
    const list = models(client);
    const preview = client === 'antigravity' ? L.agyPreview(list, config) : null;
    const shown = renderRows(client, config, preview);
    const on = list.filter((m) => L.isEnabled(client, m, config)).length;
    let empty = '';
    if (list.length === 0) {
      empty = client === 'codex'
        ? 'Codex has not fetched its model list through this plugin yet. Add the Codex setting above, then start Codex. The list appears after its first fetch.'
        : 'No client has fetched a model list through the plugin yet. Add the setup above, then start ' + (client === 'antigravity' ? 'agy' : 'Claude Code') + '. The list appears after its first fetch.';
    } else if (shown === 0) empty = 'No models match the filter.';
    setText(CLIENT[client].prefix + '-empty', empty);
    renderStale(client);

    if (client === 'codex') {
      const inPicker = list.filter((m) => L.isEnabled(client, m, config) && m.visibility !== 'hide').length;
      part(client, 'counts').textContent = on + ' of ' + list.length + ' on · ' + inPicker + ' in the picker';
      part(client, 'summary').textContent = list.length ? on + ' of ' + list.length + ' on' : 'No list yet';
      setText('codex-warning', list.length > 0 && inPicker === 0
        ? 'No model would appear in Codex’s picker. The plugin never sends Codex an empty list, so it would serve CPA’s full list instead. Turn at least one model on.'
        : '');
      part(client, 'new-models').value = config.newModels;
      part(client, 'action').value = config.action;
    } else if (client === 'antigravity') {
      renderAgy(config, preview, on, list);
    } else {
      part(client, 'counts').textContent = on + ' of ' + list.length + ' on';
      part(client, 'summary').textContent = !config.filter ? 'Filter off' : list.length ? on + ' of ' + list.length + ' on' : 'No list yet';
      part(client, 'filter').checked = config.filter;
      part(client, 'new-models').value = config.newModels;
      setText('claude-warning', !config.filter
        ? 'The filter is off: Claude Code gets CPA’s full list, and the plugin no longer sees CPA’s responses. The switches below are kept but not applied.'
        : list.length > 0 && on === 0 ? 'No model is on. The plugin never sends Claude Code an empty list, so it would get CPA’s full list instead. Turn at least one model on.' : '');
      setText('claude-error', clientError(client));
      setText('claude-source', sourceNote(client));
      part(client, 'seen').textContent = seenText(client);
    }
    if (client === 'codex') part(client, 'seen').textContent = seenText(client);

    for (const name of ['new-models', 'action', 'default', 'title', 'filter']) {
      const control = $(CLIENT[client].prefix + '-' + name);
      if (control) control.disabled = busy;
    }
    for (const name of ['enable-shown', 'disable-shown']) part(client, name).disabled = busy || shown === 0;
    part(client, 'save').disabled = busy || !saveable(client);
    part(client, 'discard').disabled = busy || !L.dirty(pending[client]);
  }

  function renderAgy(config, preview, on, list) {
    const client = 'antigravity';
    const listed = preview.entries.length;
    part(client, 'counts').textContent = on + ' of ' + list.length + ' on · ' + listed + ' in agy’s list';
    part(client, 'summary').textContent = list.length ? listed + ' in the list · ' + preview.slotsUsed + ' of ' + preview.slotLimit + ' slots' : 'No list yet';
    const slots = part(client, 'slots');
    slots.textContent = preview.slotsUsed + ' of ' + preview.slotLimit + ' slots';
    slots.className = 'pill ' + (preview.skipped.length > 0 ? 'warn' : preview.slotsUsed > 0 ? 'ok' : '');
    const label = (id) => { const m = byId(client, id); return m && text(m.label, 4096) && m.label && m.label !== id ? m.label + ' (' + id + ')' : id; };
    part(client, 'roles').textContent = listed
      ? 'Default: ' + label(preview.defaultId) + ' · Titles: ' + (preview.titleId ? label(preview.titleId) : 'no flash-lite model in the list')
      : '';
    const warnings = part(client, 'warnings');
    warnings.replaceChildren(...preview.warnings.map((warning) => node('li', warning)));
    warnings.hidden = preview.warnings.length === 0 || list.length === 0;
    part(client, 'order-summary').textContent = 'Order agy receives (' + listed + (preview.skipped.length ? ', ' + preview.skipped.length + ' left out' : '') + ')';
    part(client, 'order-list').replaceChildren(...preview.entries.map((entry) => {
      const item = node('li');
      item.append(node('code', entry.id), node('span', ' ' + (text(entry.model.label, 4096) && entry.model.label ? entry.model.label : '')));
      if (entry.slot === 'builtin') item.append(chip('built in'));
      if (entry.slot === 'image') item.append(chip('hidden by agy', 'warn'));
      if (entry.id === preview.defaultId) item.append(chip('default', 'ok'));
      if (entry.id === preview.titleId) item.append(chip('titles', 'ok'));
      return item;
    }));
    agySelects(config);
    part(client, 'new-models').value = config.newModels;
    setText('agy-error', clientError(client));
    setText('agy-source', sourceNote(client));
    part(client, 'seen').textContent = seenText(client);
  }

  function renderAll() {
    for (const client of L.CLIENTS) render(client);
    $('refresh').disabled = busy;
    $('workspace').setAttribute('aria-busy', String(busy));
  }

  // A success notice describes the last save; it goes once anything changes.
  function edited() {
    if ($('notice').className === 'alert ok') notice('');
  }
  function setModel(client, model, enabled) {
    L.setSwitch(client, pending[client], saved(client), model, enabled);
    edited();
  }
  function setSetting(client, name, value) {
    L.setSetting(pending[client], saved(client), name, value);
    edited();
  }

  // Choosing a default or title model that is switched off switches it on:
  // an explicit switch wins over the choice, so it would not be in the list.
  function chooseAgyModel(name, id) {
    setSetting('antigravity', name, id);
    const model = id && byId('antigravity', id);
    if (model && !L.isEnabled('antigravity', model, current('antigravity'))) {
      setModel('antigravity', model, true);
      announce(id + ' was switched on so it can be the ' + (name === 'defaultModel' ? 'default' : 'title') + ' model.');
    }
    render('antigravity');
  }

  // Shows a new state. Unsaved changes of the clients in keep are carried
  // onto its saved settings; the others are dropped.
  function showState(value, keep = []) {
    const previous = pending;
    state = value;
    usable = { codex: true, antigravity: validAntigravity(value.antigravity), claude: validClaude(value.claude_code) };
    pending = {};
    for (const client of L.CLIENTS) {
      pending[client] = usable[client] && keep.includes(client) ? L.clonePending(previous[client]) : L.emptyPending();
      if (usable[client]) L.normalize(client, pending[client], saved(client), models(client));
    }
    $('codex-setup').textContent = 'model_catalog_url = "' + root + CATALOG_PATH + '"';
    $('agy-setup').textContent = JSON.stringify({ models: { listSource: root + AGY_PATH } }, null, 2);
    $('claude-setup').textContent = 'CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1';
    $('list-state').className = 'pill ok';
    $('list-state').textContent = 'Connected';
    $('version').textContent = text(value.version, 32) ? 'Plugin ' + value.version : '';
    $('version').hidden = !$('version').textContent;
    $('workspace').hidden = false;
    renderAll();
  }

  async function load() {
    if (busy) return;
    if (!root) { notice('Open this page through CPA’s plugin menu.', 'error'); return; }
    if (!readSession()) { sessionHelp(); return; }
    busy = true;
    renderAll();
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
    renderAll();
    notice(error.message, 'error');
  }

  const PICKUP = {
    codex: 'Codex shows the new list after its next refresh, within five minutes.',
    antigravity: 'agy gets the new list on its next run; a running agy acp, as in T3, within five minutes.',
    claude: 'Claude Code shows the new list the next time it starts.',
  };

  // Saves one client. CPA's PATCH replaces whole top-level keys, so the state
  // is read again first and this tab's changes are merged onto that client's
  // newest saved settings: a stale tab never clobbers newer switches.
  async function save(client) {
    if (busy || !state || !usable[client] || !saveable(client)) return;
    const name = CLIENT[client].name;
    const loaded = saved(client);
    busy = true;
    renderAll();
    notice('Saving ' + name + '…');
    try {
      const fresh = await call(STATE_PATH);
      if (!validFor(client, fresh)) throw new PageError('incompatible', 'The plugin returned data this page does not understand. Nothing was saved. Update the plugin and refresh.');
      const newest = L.base(client, fresh);
      const wanted = L.apply(newest, pending[client]);
      await call(CONFIG_PATH, { method: 'PATCH', body: JSON.stringify(L.patchBody(client, wanted, pending[client])) });
      // CPA applies the change on its configuration reload; confirm it landed.
      let latest = null;
      let last = null;
      for (let attempt = 0; attempt < 10; attempt++) {
        await new Promise((resolve) => setTimeout(resolve, 500));
        last = await call(STATE_PATH);
        if (validFor(client, last) && L.applied(client, last, wanted)) { latest = last; break; }
      }
      busy = false;
      if (!latest) {
        const rejected = last && validFor(client, last) && client !== 'codex' ? L.section(client, last).error : '';
        notice(rejected
          ? 'CPA saved ' + name + '’s settings, but the plugin rejected them: ' + rejected + ' Your changes are still here; refresh to see what applies.'
          : 'Saved to CPA’s configuration, but the plugin has not applied it yet. Refresh in a moment to check.', 'warn');
        renderAll();
        return;
      }
      showState(latest, L.CLIENTS.filter((other) => other !== client));
      notice('Saved ' + name + '. ' + PICKUP[client] + (L.sameConfig(loaded, newest) ? '' : ' Changes saved elsewhere since this page loaded were kept.'), 'ok');
    } catch (error) {
      busy = false;
      failure(error);
    }
  }

  // Enable shown leaves agy's non-text models alone unless the filter names
  // them: an explicit switch would offer them to agy as chat models.
  function bulk(client, enabled) {
    const filtered = part(client, 'search').value.trim() !== '';
    let skipped = 0;
    for (const model of shownModels(client)) {
      if (enabled && client === 'antigravity' && !filtered && (model.non_text || model.hidden_by_agy)) { skipped++; continue; }
      setModel(client, model, enabled);
    }
    render(client);
    if (skipped > 0) notice('Enable shown left ' + skipped + ' Antigravity models that do not answer in text as they were. Filter for one to switch it on.');
  }

  // Which sections are open is remembered per browser; storage may be
  // unavailable, and the page works the same without it.
  function readOpen() {
    try {
      const raw = localStorage.getItem(SECTIONS_KEY);
      const value = raw !== null && raw.length <= 256 ? JSON.parse(raw) : null;
      return object(value) ? value : {};
    } catch { return {}; }
  }
  function writeOpen() {
    try {
      const value = {};
      for (const client of L.CLIENTS) value[client] = part(client, 'section').open === true;
      localStorage.setItem(SECTIONS_KEY, JSON.stringify(value));
    } catch { /* not remembered */ }
  }

  const opened = readOpen();
  for (const client of L.CLIENTS) {
    const section = part(client, 'section');
    if (typeof opened[client] === 'boolean') section.open = opened[client];
    section.addEventListener('toggle', writeOpen);
    part(client, 'search').addEventListener('input', () => render(client));
    part(client, 'enable-shown').addEventListener('click', () => bulk(client, true));
    part(client, 'disable-shown').addEventListener('click', () => bulk(client, false));
    part(client, 'discard').addEventListener('click', () => { pending[client] = L.emptyPending(); render(client); notice(''); });
    part(client, 'save').addEventListener('click', () => save(client));
    part(client, 'new-models').addEventListener('change', () => { setSetting(client, 'newModels', part(client, 'new-models').value); render(client); });
  }
  $('codex-action').addEventListener('change', () => { setSetting('codex', 'action', $('codex-action').value); render('codex'); });
  $('agy-default').addEventListener('change', () => chooseAgyModel('defaultModel', $('agy-default').value));
  $('agy-title').addEventListener('change', () => chooseAgyModel('titleModel', $('agy-title').value));
  $('claude-filter').addEventListener('change', () => { setSetting('claude', 'filter', $('claude-filter').checked); render('claude'); });

  const anyDirty = () => L.CLIENTS.some((client) => L.dirty(pending[client]));
  $('refresh').addEventListener('click', () => {
    if (anyDirty() && !confirm('Discard unsaved changes and reload?')) return;
    stopped = false;
    load();
  });
  // Any console session change clears private values and stops requests; only
  // an explicit refresh resumes.
  window.addEventListener('storage', (event) => { if (event.key === null || STORAGE_NAMES.has(event.key)) sessionHelp('The console session changed. The lists have been cleared and requests stopped. Sign in again with “Remember password”, then refresh.'); });
  window.addEventListener('beforeunload', (event) => { if (anyDirty()) event.preventDefault(); });
  window.addEventListener('pagehide', clearPrivate);
  load();
})();
