(function () {
  'use strict';
  const $ = id => document.getElementById(id);
  const providers = {claude: 'Claude', codex: 'Codex', xai: 'Grok', openrouter: 'OpenRouter', 'anthropic-api': 'Claude API credits'};
  const endpoints = {claude: 'api.anthropic.com/api/oauth/usage', codex: 'chatgpt.com/backend-api/wham/usage', xai: 'cli-chat-proxy.grok.com/v1/billing?format=credits', openrouter: 'openrouter.ai/api/v1/credits', 'anthropic-api': 'api.anthropic.com/v1/organizations/cost_report'};
  // Providers with no weekly window: their observation is dated by the nested
  // quota.observed_at, and the weekly observed_at stays empty.
  const noWeekly = ['openrouter', 'anthropic-api'];
  const EMPTY = 'No supported accounts found. Enable a Claude, Codex, or Grok OAuth account in CPA, or set an OpenRouter management key in this plugin’s configuration, or add Claude API credits (claude-api-credits) to this plugin’s configuration; the next scheduled scan will discover it.';

  // Status is read with the management key the CPA console remembers, and CPA
  // locks an address out of its management API for 30 minutes after five failed
  // sign-ins, counting a missing key as one. So this page sends nothing without
  // a remembered session, never presents a refused key again on its timer or
  // after a reload, stops its timer when a request gets no answer at all, and
  // resumes after either only on Refresh view.
  //
  // Session reading is ported from Codex Catalog Filter's page, itself adapted
  // from Token Usage's audit of the Management Center storage format. Only a
  // remembered console session on this exact origin and API base is used.
  const RESOURCE_SUFFIX = '/v0/resource/plugins/quota-cache/status';
  const STATUS_PATH = '/v0/management/plugins/quota-cache/status';
  const AUTH_NAME = 'cli-proxy-auth';
  const REFUSED_NAME = 'quota-cache.console-refused';
  const REASONS = ['refused', 'banned', 'remote', 'off', 'other'];
  const BAN_SECONDS = 1800;
  const COUNTED = ' Each refused try counts toward CPA’s limit of five failed sign-ins, after which it locks this address out of management for 30 minutes.';
  const SIGN_IN = 'Sign in to CPA’s management console on this same address with “Remember password” ticked, then select Refresh view. This page sends nothing until a remembered session is available.';

  const text = (value, max) => typeof value === 'string' && value.length <= max;
  const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);

  function apiRoot() {
    const path = location.pathname.replace(/\/+$/, '');
    if (!['http:', 'https:'].includes(location.protocol) || !path.endsWith(RESOURCE_SUFFIX)) return null;
    return location.origin + path.slice(0, -RESOURCE_SUFFIX.length);
  }
  const root = apiRoot();
  function matchesRoot(value) {
    if (!root || !text(value, 2048) || !/^https?:\/\//i.test(value.trim()) || /[?#]/.test(value) || /^https?:\/\/[^/]*@/i.test(value)) return false;
    try {
      const url = new URL(value.trim());
      if (url.username || url.password || url.search || url.hash) return false;
      // The console saves its API base without /v8/management; older builds kept /v0/management.
      url.pathname = url.pathname.replace(/\/+$/, '').replace(/\/v[08]\/management$/i, '').replace(/\/+$/, '') || '/';
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
      const bytes = Uint8Array.from(atob(value.slice(9)), c => c.charCodeAt(0));
      const salt = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + '|' + navigator.userAgent);
      for (let i = 0; i < bytes.length; i++) bytes[i] ^= salt[i % salt.length];
      value = new TextDecoder('utf-8', {fatal: true}).decode(bytes);
    }
    try { return JSON.parse(value); } catch (error) { if (legacy) return value; throw error; }
  }
  function validKey(value) {
    if (!text(value, 4096) || !value || value.trim() !== value || /[\x00-\x08\x0a-\x1f\x7f]/.test(value)) return false;
    try {
      const authorization = ['Bearer', value].join(' ');
      return new Headers({Authorization: authorization}).get('Authorization') === authorization;
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
        return {key: state.managementKey, base: state.apiBase};
      }
      if (stored('isLoggedIn') !== 'true') return null;
      const base = decode(stored('apiBase'), true) || decode(stored('apiUrl'), true);
      const key = decode(stored('managementKey'), true);
      return matchesRoot(base) && validKey(key) ? {key, base} : null;
    } catch { return null; }
  }
  const sameSession = (a, b) => a && b && a.key === b.key && a.base === b.base;

  // FNV-1a, because crypto.subtle is missing on plain-HTTP origins. The
  // fingerprint only recognises a refused key beside the console's own copy of
  // it; it is not meant to hide the key.
  function fingerprint(key) {
    let hash = 0xcbf29ce484222325n;
    for (const byte of new TextEncoder().encode(root + '\n' + key)) hash = ((hash ^ BigInt(byte)) * 0x100000001b3n) & 0xffffffffffffffffn;
    return hash.toString(16).padStart(16, '0');
  }
  // A refusal is remembered against the key that was refused, so neither a
  // reload nor another open copy of this page presents it again. The record is
  // dropped once the console holds a different key or none, and otherwise only
  // by Refresh view.
  //
  // The record names the CPA it belongs to. Two CPAs behind one origin under
  // different prefixes share this storage, and the page for one finds no
  // session of its own while the console is signed in to the other. It must
  // leave the other's record alone, or that page would present its refused key
  // again.
  //
  // The record goes to this origin's storage or, when that cannot take it, to
  // this tab's. The origin's quota is shared with the console and every other
  // plugin page, so it can be full; a refusal only this page remembered would
  // be presented again by a reload.
  function stores() {
    const found = [];
    try { found.push(localStorage); } catch {}
    try { found.push(sessionStorage); } catch {}
    return found;
  }
  // The record in one store: undefined when there is none or it is another
  // CPA's, null when it cannot be read.
  function recordIn(store) {
    let record;
    try {
      const raw = store.getItem(REFUSED_NAME);
      if (raw === null) return undefined;
      record = raw.length > 32768 ? null : JSON.parse(raw);
    } catch { return null; }
    if (object(record) && typeof record.root === 'string' && record.root !== root) return undefined;
    return record;
  }
  function drop(store) { try { store.removeItem(REFUSED_NAME); } catch {} }
  function refusal(session) {
    let found = null;
    for (const store of stores()) {
      const record = recordIn(store);
      if (record === undefined) continue;
      if (session && object(record) && record.v === 1 && record.root === root && record.fp === fingerprint(session.key) && REASONS.includes(record.reason) &&
        Number.isInteger(record.status) && Number.isFinite(record.at) && (record.until === null || Number.isFinite(record.until))) {
        if (!found || record.at > found.at) found = record;
      } else drop(store);
    }
    return found;
  }
  function forget() { for (const store of stores()) if (recordIn(store) !== undefined) drop(store); }
  function remember(session, status, error) {
    const at = Math.floor(Date.now() / 1000);
    const reason = classify(status, error);
    const record = {v: 1, root, fp: fingerprint(session.key), reason, status, at, until: reason === 'banned' ? at + banSeconds(error) : null};
    for (const store of stores()) {
      try { store.setItem(REFUSED_NAME, JSON.stringify(record)); break; } catch {}
    }
    return record;
  }
  // CPA's own refusal messages, from AuthenticateManagementKey in
  // internal/api/handlers/management/handler.go. Anything else came from
  // something in front of CPA.
  function classify(status, error) {
    if (status === 401 && (error === 'missing management key' || error === 'invalid management key')) return 'refused';
    if (status === 403 && typeof error === 'string' && error.startsWith('IP banned')) return 'banned';
    if (status === 403 && error === 'remote management disabled') return 'remote';
    if (status === 403 && error === 'remote management key not set') return 'off';
    return 'other';
  }
  // "Try again in 29m59s" is a Go duration rounded to the second.
  function banSeconds(error) {
    const match = /Try again in (?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/.exec(typeof error === 'string' ? error : '');
    if (!match || match[0] === 'Try again in ') return BAN_SECONDS;
    return Math.min(86400, Number(match[1] || 0) * 3600 + Number(match[2] || 0) * 60 + Number(match[3] || 0));
  }
  const banned = record => !!record && record.reason === 'banned' && record.until !== null && record.until * 1000 > Date.now();
  function refusalMessage(record) {
    if (record.reason === 'banned') return banned(record)
      ? 'CPA is refusing management requests from this address until about ' + new Date(record.until * 1000).toLocaleTimeString([], {hour: 'numeric', minute: '2-digit'}) + ' after repeated failed sign-ins. This page will not ask again before then.'
      : 'CPA’s lockout of this address after repeated failed sign-ins should be over. Select Refresh view to try once more.' + COUNTED;
    if (record.reason === 'remote') return 'CPA accepts management sign-ins only on the machine it runs on, so this page stopped asking. Open Quota Cache on that machine instead.';
    if (record.reason === 'off') return 'CPA’s management API is switched off on this server, so this page stopped asking.';
    if (record.reason === 'refused') return 'CPA refused the console session saved in this browser, so this page stopped asking. Sign in to CPA’s management console again with “Remember password” ticked, then select Refresh view.' + COUNTED;
    return 'Something in front of CPA refused the status request (HTTP ' + record.status + '), so this page stopped asking. Select Refresh view to try once more.' + COUNTED;
  }

  let snapshot = null;
  let busy = false;
  let stopped = false; // set after a refusal; only Refresh view resumes
  let release = 0; // re-enables Refresh view when a lockout ends; sends nothing
  const date = value => { const t = Date.parse(value); return Number.isFinite(t) && t > 0 ? t : 0; };
  function node(tag, text, className) {
    const e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (className) e.className = className;
    return e;
  }
  function time(value, now) {
    const t = date(value);
    if (!t) return node('span', '—');
    const span = node('span');
    const stamp = node('time', new Date(t).toLocaleString([], {month:'short', day:'numeric', hour:'numeric', minute:'2-digit', second:'2-digit'}));
    stamp.dateTime = new Date(t).toISOString();
    stamp.title = new Date(t).toISOString();
    span.append(stamp);
    const seconds = Math.round((t - now) / 1000);
    const units = Math.abs(seconds) < 60 ? [seconds, 'second'] : Math.abs(seconds) < 3600 ? [Math.round(seconds / 60), 'minute'] : [Math.round(seconds / 3600), 'hour'];
    span.append(node('span', new Intl.RelativeTimeFormat(undefined, {numeric:'auto'}).format(...units), 'relative'));
    return span;
  }
  function account(item) {
    const cell = node('td', providers[item.provider] || 'Unknown provider', 'account');
    const id = String(item.auth_index || '');
    const label = item.api_credit && typeof item.api_credit.label === 'string' ? item.api_credit.label : '';
    cell.append(node('span', label || (id.length > 12 ? '…' + id.slice(-12) : id), 'secondary'));
    cell.title = id;
    return cell;
  }
  function addCell(row, child, className) {
    const cell = node('td', undefined, className);
    cell.append(typeof child === 'string' ? document.createTextNode(child) : child);
    row.append(cell);
    return cell;
  }
  function fresh(e, now) {
    const observed = date(e.observed_at);
    const reset = date(e.reset_at);
    return observed > 0 && observed <= now && now - observed <= 30 * 60000 && !e.last_error && (!reset || reset > now) && Number.isFinite(e.used_percent) && e.used_percent >= 0 && e.used_percent <= 100;
  }
  // OpenRouter and Claude API credits have no weekly window, so their
  // freshness is the nested observation's rather than the weekly observed_at,
  // which stays empty.
  function freshBalance(e, now) {
    const observed = date(e.quota && e.quota.observed_at);
    return noWeekly.includes(e.provider) && observed > 0 && observed <= now && now - observed <= 30 * 60000 && !e.last_error;
  }
  // The configuration problem of a Claude API credit item, if any. Such an
  // item is listed but never polled.
  const notPolled = e => (e.api_credit && e.api_credit.problem) || '';
  function next(e, s, now) {
    return Math.max(now, date(e.next_attempt), date(s.next_request), date((s.provider_cooldown || {})[e.provider]));
  }
  const utcDay = value => date(value) ? new Date(date(value)).toISOString().slice(0, 10) : '';
  // A Claude API credit entry prints what was configured and what Anthropic
  // reported, verbatim. Amounts are cents as Anthropic sent them; nothing is
  // added up here, which is Quota Glance's job.
  function creditDetails(entry, now) {
    const c = entry.api_credit || {};
    const details = node('details');
    details.className = 'quota-details';
    details.append(node('summary', 'Configured credit and daily spend'));
    details.append(node('p', 'Monthly credit: ' + (c.monthly_usd ? c.monthly_usd + ' USD' : 'not set') + ' (configured)'));
    details.append(node('p', 'Renews: ' + (c.renews || 'not set') + ' (configured)'));
    if (c.key_fingerprint) details.append(node('p', 'Key: ' + c.key_fingerprint));
    if (c.problem) details.append(node('p', 'Configuration problem: ' + c.problem + '. This item is not polled.', 'secondary'));
    const q = entry.quota;
    const r = q && q.schema === 1 ? q.cost_report : null;
    if (!r) { details.append(node('p', c.problem ? 'No reading.' : 'Spend arrives after the next successful poll', 'secondary')); return details; }
    const observed = date(q.observed_at);
    details.append(node('p', entry.last_error || !observed || observed > now || now - observed > 30 * 60000 ? 'Last known response — not fresh' : 'Latest successful response', 'secondary'));
    details.append(time(q.observed_at, now));
    if (r.organization_id) details.append(node('p', 'Organization: ' + r.organization_id));
    if (r.key_fingerprint && c.key_fingerprint && r.key_fingerprint !== c.key_fingerprint) details.append(node('p', 'Read with an earlier key (' + r.key_fingerprint + ')', 'secondary'));
    details.append(node('p', 'Asked: ' + (r.starting_at || '—') + ' to ' + (r.ending_at || '—')));
    const days = Array.isArray(r.days) ? r.days : [];
    details.append(node('p', 'Days read: ' + days.length));
    const last = days.length ? utcDay(days[days.length - 1].starting_at) : '';
    if (!last || last < utcDay(q.observed_at)) details.append(node('p', 'Today not reported yet', 'secondary'));
    const list = node('ul');
    for (const day of days) {
      const amounts = Array.isArray(day.amounts) ? day.amounts : [];
      list.append(node('li', utcDay(day.starting_at) + ': ' + (amounts.length ? amounts.map(a => a.amount + ' ' + a.currency).join(', ') + ' (lowest units)' : 'no cost')));
    }
    details.append(list);
    return details;
  }
  function extendedQuota(entry, now) {
    if (entry.provider === 'anthropic-api') return creditDetails(entry, now);
    const q = entry.quota;
    if (!q || q.schema !== 1) return node('span', 'Additional fields arrive after the next successful poll', 'secondary');
    const details = node('details');
    details.className = 'quota-details';
    details.append(node('summary', 'All cached quota fields'));
    const observed = date(q.observed_at);
    details.append(node('p', entry.last_error || !observed || observed > now || now - observed > 30 * 60000 ? 'Last known response — not fresh' : 'Latest successful response', 'secondary'));
    details.append(time(q.observed_at, now));
    if (q.active_limit) details.append(node('p', 'Active limit: ' + q.active_limit));
    if (q.limit_reached_reason) details.append(node('p', 'Limit reason: ' + q.limit_reached_reason));
    if (q.plan) details.append(node('p', 'Plan: ' + q.plan));
    if (typeof q.unified_billing === 'boolean') details.append(node('p', 'Unified billing: ' + (q.unified_billing ? 'yes' : 'no')));
    const list = node('ul');
    for (const [id, w] of Object.entries(q.windows || {})) {
      const item = node('li', id + ': ' + (Number.isFinite(w.used_percent) ? w.used_percent + '% used' : 'usage not supplied'));
      if (w.duration_seconds) item.append(node('span', ' · ' + w.duration_seconds + ' second window'));
      if (w.period) item.append(node('span', ' · ' + w.period));
      if (date(w.starts_at)) { item.append(node('span', ' · starts '), time(w.starts_at, now)); }
      if (date(w.resets_at)) { item.append(node('span', ' · resets '), time(w.resets_at, now)); if (date(w.resets_at) <= now) item.append(node('strong', ' · Window expired')); }
      list.append(item);
    }
    for (const [id, l] of Object.entries(q.limits || {})) {
      const flags = [];
      if (l.metered_feature) flags.push('feature: ' + l.metered_feature);
      if (typeof l.allowed === 'boolean') flags.push('allowed: ' + l.allowed);
      if (typeof l.reached === 'boolean') flags.push('limit reached: ' + l.reached);
      list.append(node('li', id + ' — ' + flags.join(' · ')));
    }
    for (const [id, b] of Object.entries(q.balances || {})) {
      const fields = [];
      for (const key of ['used', 'limit', 'remaining']) if (typeof b[key] === 'string') fields.push(key + ': ' + b[key] + ' ' + b.unit);
      if (b.source) fields.push('source: ' + b.source);
      if (Number.isFinite(b.remaining_percent)) fields.push(b.remaining_percent + '% remaining');
      if (date(b.resets_at)) fields.push('resets: ' + new Date(date(b.resets_at)).toLocaleString());
      if (Number.isFinite(b.used_percent)) fields.push(b.used_percent + '% used');
      for (const key of ['enabled', 'has_credits', 'unlimited']) if (typeof b[key] === 'boolean') fields.push(key.replaceAll('_', ' ') + ': ' + b[key]);
      list.append(node('li', id + ' — ' + fields.join(' · ')));
    }
    details.append(list);
    if (q.truncated) details.append(node('p', 'Provider returned more entries than the cache limit; this list is incomplete.', 'secondary'));
    return details;
  }
  function render(s) {
    const now = Date.now();
    const all = Object.values(s.entries || {});
    const selected = $('provider').value;
    const entries = all.filter(e => selected === 'all' || e.provider === selected).sort((a,b) => (a.provider + a.auth_index).localeCompare(b.provider + b.auth_index));
    const cooldowns = Object.entries(s.provider_cooldown || {}).filter(([,until]) => date(until) > now);
    const usable = all.filter(e => fresh(e, now) || freshBalance(e, now)).length;
    // Waiting for CPA to load its credentials is an expected startup state,
    // not a failed check.
    const waiting = Boolean(s.activity?.waiting);
    const failed = Boolean(s.activity?.error) && !waiting;
    $('health').textContent = cooldowns.length ? 'Provider cooldown' : waiting ? 'Starting' : failed ? 'Needs attention' : all.length ? 'Running' : 'Waiting for accounts';
    $('health').className = 'pill ' + (cooldowns.length || failed ? 'warn' : waiting ? '' : all.length ? 'ok' : '');
    $('fresh').textContent = usable + ' / ' + all.length;
    const calls = (s.history || []).filter(p => p.request_sent);
    const last = calls[calls.length - 1];
    $('last-call').replaceChildren(last ? time(last.started_at, now) : node('span', 'Not recorded yet'));
    $('last-call-detail').textContent = last ? (providers[last.provider] || last.provider) + ' · ' + (last.http_status ? 'HTTP ' + last.http_status : 'No HTTP response') : 'History begins with the next completed poll';
    // A misconfigured credit item is never polled, so it has no next poll and
    // must not make the header read as due now.
    const polled = all.filter(e => !notPolled(e));
    const due = polled.length ? Math.min(...polled.map(e => next(e, s, now))) : 0;
    $('next-call').replaceChildren(due ? time(new Date(due).toISOString(), now) : node('span', all.length ? 'Nothing to poll' : 'Waiting for accounts'));
    $('cooldowns').replaceChildren();
    for (const [provider, until] of cooldowns) {
      const p = node('p', (providers[provider] || provider) + ' requests are paused after a rate limit. Eligible again ');
      p.append(time(until, now));
      $('cooldowns').append(p);
    }
    $('cooldowns').hidden = !cooldowns.length;
    $('activity-error').textContent = waiting ? 'Waiting for CPA to finish loading credentials. Saved observations are kept, and polling resumes once they are loaded.' : failed ? 'The last scheduled check failed: ' + s.activity.error + '. Check CPA logs and the cache storage path.' : '';
    $('activity-error').className = 'alert' + (waiting ? '' : ' error');
    $('activity-error').hidden = !waiting && !failed;
    $('account-count').textContent = entries.length + ' shown / ' + all.length + ' total';
    $('accounts').replaceChildren();
    for (const e of entries) {
      const row = node('tr');
      row.append(account(e));
      const known = date(e.observed_at) > 0;
      const pct = Number(e.used_percent);
      const quotaCell = addCell(row, known && Number.isFinite(pct) ? pct.toFixed(1) + '%' : '—');
      if (known && Number.isFinite(pct)) {
        const progress = node('progress');
        progress.max = 100; progress.value = pct;
        progress.setAttribute('aria-label', (providers[e.provider] || e.provider) + ' quota used');
        quotaCell.append(progress);
        quotaCell.append(node('span', fresh(e, now) ? 'Current observation' : 'Last known value', 'secondary'));
        if (date(e.reset_at)) { const reset = node('span', 'Resets ', 'secondary'); reset.append(time(e.reset_at, now)); quotaCell.append(reset); }
      }
      quotaCell.append(extendedQuota(e, now));
      addCell(row, time(e.observed_at, now));
      addCell(row, time(e.last_attempt, now));
      const problem = notPolled(e);
      addCell(row, problem ? node('span', 'Not polled', 'secondary') : time(new Date(next(e,s,now)).toISOString(), now));
      const cooling = date((s.provider_cooldown || {})[e.provider]) > now;
      const label = problem ? 'Not polled' : cooling ? 'Cooldown' : e.last_error === 'refresh pending' ? 'Polling' : e.last_error ? 'Failed' : fresh(e,now) || freshBalance(e,now) ? 'Fresh' : known || (noWeekly.includes(e.provider) && e.quota) ? 'Stale' : e.quota ? 'Extended only' : 'Queued';
      const status = addCell(row, node('span', label, 'pill ' + (label === 'Fresh' ? 'ok' : label === 'Failed' || label === 'Not polled' ? 'error' : 'warn')));
      if (problem) status.append(node('span', problem, 'secondary'));
      else if (e.last_error) status.append(node('span', e.last_error, 'secondary'));
      $('accounts').append(row);
    }
    $('accounts-empty').hidden = entries.length > 0;
    $('accounts-empty').textContent = all.length ? 'No accounts match this provider.' : EMPTY;
    const history = (s.history || []).filter(p => selected === 'all' || p.provider === selected).slice().reverse();
    $('history').replaceChildren();
    $('history-count').textContent = history.length + ' recorded';
    for (const p of history) {
      const row = node('tr');
      addCell(row, time(p.started_at,now)); row.append(account(p));
      addCell(row, p.request_sent ? 'GET ' + (endpoints[p.provider] || 'Provider quota endpoint') : 'No provider request sent', 'endpoint');
      const label = p.outcome === 'success' ? 'Success' : p.outcome === 'rate_limited' ? 'Rate limited' : 'Failed';
      const cell = addCell(row,node('span',label,'pill ' + (p.outcome === 'success' ? 'ok' : 'warn')));
      cell.append(node('span', p.http_status ? 'HTTP ' + p.http_status : p.request_sent ? 'No HTTP response' : 'Stopped before HTTP', 'secondary'));
      if (p.error) cell.append(node('span',p.error,'secondary'));
      addCell(row, String(p.duration_ms || 0) + ' ms');
      $('history').append(row);
    }
    $('history-empty').hidden = history.length > 0;
    const totals = s.totals || {};
    const details = [
      ['Polling interval', s.poll_interval], ['Request spacing', s.request_spacing],
      ['Last account scan', time(s.activity?.last_scan,now)], ['Cache last written', time(s.written_at,now)],
      ['Completed poll attempts', String(totals.attempts || 0)], ['Provider request attempts', String(totals.requests || 0)],
      ['Successful polls', String(totals.successes || 0)], ['Failed polls', String(totals.failures || 0)], ['Rate limits', String(totals.rate_limits || 0)],
      ['Status reads since plugin start', String(s.status_reads || 0)], ['Previous status read', time(s.previous_status_read,now)],
      ['Cache file', s.cache_path], ['Time zone', Intl.DateTimeFormat().resolvedOptions().timeZone]
    ];
    $('details').replaceChildren();
    for (const [label,value] of details) { const item = node('div'); const dd = node('dd'); dd.append(value instanceof Node ? value : document.createTextNode(value || '—')); item.append(node('dt',label),dd); $('details').append(item); }
  }
  function syncButton(record) {
    clearTimeout(release);
    const wait = banned(record) ? record.until * 1000 - Date.now() : 0;
    $('refresh').disabled = busy || wait > 0;
    if (wait > 0) release = setTimeout(() => syncButton(refusal(readSession())), wait + 1000);
  }
  function unavailable(message, health, view) {
    // Hide prior account data on loss of authorization; never leave it looking live.
    $('content').hidden = true;
    $('error').textContent = message;
    $('error').hidden = false; $('health').textContent = health; $('health').className = 'pill warn'; $('view-status').textContent = view;
  }
  // manual is true only for Refresh view: the one action that resumes after a
  // refusal and the only way to present a refused key again.
  async function refresh(manual) {
    if (busy || (stopped && !manual)) return;
    if (!root) { unavailable('Open Quota Cache from CPA’s sidebar to load its status.', 'Status unavailable', 'View could not be updated'); return; }
    const session = readSession();
    let refused = refusal(session);
    if (!session) { syncButton(null); unavailable(SIGN_IN, 'Sign-in needed', 'No remembered console session'); return; }
    if (refused && manual && !banned(refused)) { forget(); refused = null; }
    if (refused) { stopped = true; syncButton(refused); unavailable(refusalMessage(refused), 'Session refused', 'Updates paused after CPA refused the session'); return; }
    stopped = false;
    busy = true; syncButton(null);
    let answered = false;
    try {
      const response = await fetch(root + STATUS_PATH, {headers: {Authorization: ['Bearer', session.key].join(' '), Accept: 'application/json'}, mode: 'same-origin', credentials: 'same-origin', cache: 'no-store', redirect: 'error', signal: AbortSignal.timeout(10000)});
      answered = true;
      if (response.status === 401 || response.status === 403) {
        // Stop and remember the refusal before anything else runs, then
        // refine its reason from CPA's message.
        stopped = true;
        refused = remember(session, response.status, null);
        let error = null;
        try { const body = await response.json(); if (object(body) && text(body.error, 512)) error = body.error; } catch {}
        refused = remember(session, response.status, error);
        return;
      }
      if (!sameSession(session, readSession())) throw new Error('The console session changed while the view was updating, so its result was discarded. The next update uses the current session.');
      if (!response.ok) throw new Error('Quota Cache status is unavailable (HTTP ' + response.status + '). Check that the plugin is enabled and registered in CPA, and inspect its logs.');
      const data = await response.json();
      if (data.schema !== 1 || !data.entries || !data.running) throw new Error('The cache has not supplied a running status. Check the plugin version and CPA logs.');
      snapshot = data; render(data); $('content').hidden = false; $('error').hidden = true;
      $('view-status').textContent = 'View updated ' + new Date().toLocaleTimeString();
    } catch (error) {
      if (!answered) {
        // No status came back, so CPA may have refused the key and counted it
        // without this page ever learning so. The timer stops as it does after
        // a refusal. Nothing is remembered, because the key may be fine, so a
        // reload or Refresh view asks once more.
        stopped = true;
        unavailable('The status request ' + (error.name === 'TimeoutError' ? 'got no answer within 10 seconds' : 'failed before an answer arrived') + ', so this page stopped updating on its own: if CPA refused the session, asking again would count toward its lockout. Check CPA connectivity, then select Refresh view.', 'Status unavailable', 'Updates paused until Refresh view');
      } else {
        unavailable(error.name === 'TimeoutError' ? 'The status request timed out. Check CPA connectivity, then refresh this view.' : error.message, 'Status unavailable', 'View could not be updated');
      }
    } finally {
      busy = false; syncButton(refused);
      if (refused) unavailable(refusalMessage(refused), 'Session refused', 'Updates paused after CPA refused the session');
    }
  }
  $('refresh').addEventListener('click', () => refresh(true));
  $('provider').addEventListener('change', () => { if (snapshot) render(snapshot); });
  setInterval(() => { if ($('auto').checked && !document.hidden) refresh(false); }, 30000);
  refresh(false);
})();
