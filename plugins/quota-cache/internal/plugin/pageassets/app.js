(function () {
  'use strict';
  const $ = id => document.getElementById(id);
  const auth = window.quotaCacheAuth;
  const providers = {claude: 'Claude', codex: 'Codex', xai: 'Grok'};
  const endpoints = {claude: 'api.anthropic.com/api/oauth/usage', codex: 'chatgpt.com/backend-api/wham/usage', xai: 'cli-chat-proxy.grok.com/v1/billing?format=credits'};
  let snapshot = null;
  let busy = false;
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
    cell.append(node('span', id.length > 12 ? '…' + id.slice(-12) : id, 'secondary'));
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
  function next(e, s, now) {
    return Math.max(now, date(e.next_attempt), date(s.next_request), date((s.provider_cooldown || {})[e.provider]));
  }
  function render(s) {
    const now = Date.now();
    const all = Object.values(s.entries || {});
    const selected = $('provider').value;
    const entries = all.filter(e => selected === 'all' || e.provider === selected).sort((a,b) => (a.provider + a.auth_index).localeCompare(b.provider + b.auth_index));
    const cooldowns = Object.entries(s.provider_cooldown || {}).filter(([,until]) => date(until) > now);
    const usable = all.filter(e => fresh(e, now)).length;
    $('health').textContent = cooldowns.length ? 'Provider cooldown' : s.activity?.error ? 'Needs attention' : all.length ? 'Running' : 'Waiting for accounts';
    $('health').className = 'pill ' + (cooldowns.length || s.activity?.error ? 'warn' : all.length ? 'ok' : '');
    $('fresh').textContent = usable + ' / ' + all.length;
    const calls = (s.history || []).filter(p => p.request_sent);
    const last = calls[calls.length - 1];
    $('last-call').replaceChildren(last ? time(last.started_at, now) : node('span', 'Not recorded yet'));
    $('last-call-detail').textContent = last ? (providers[last.provider] || last.provider) + ' · ' + (last.http_status ? 'HTTP ' + last.http_status : 'No HTTP response') : 'History begins with the next completed poll';
    const due = all.length ? Math.min(...all.map(e => next(e, s, now))) : 0;
    $('next-call').replaceChildren(due ? time(new Date(due).toISOString(), now) : node('span', 'Waiting for accounts'));
    $('cooldowns').replaceChildren();
    for (const [provider, until] of cooldowns) {
      const p = node('p', (providers[provider] || provider) + ' requests are paused after a rate limit. Eligible again ');
      p.append(time(until, now));
      $('cooldowns').append(p);
    }
    $('cooldowns').hidden = !cooldowns.length;
    $('activity-error').textContent = s.activity?.error ? 'The last scheduled check failed: ' + s.activity.error + '. Check CPA logs and the cache storage path.' : '';
    $('activity-error').hidden = !s.activity?.error;
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
      addCell(row, time(e.observed_at, now));
      addCell(row, time(e.last_attempt, now));
      addCell(row, time(new Date(next(e,s,now)).toISOString(), now));
      const cooling = date((s.provider_cooldown || {})[e.provider]) > now;
      const label = cooling ? 'Cooldown' : e.last_error === 'refresh pending' ? 'Polling' : e.last_error ? 'Failed' : fresh(e,now) ? 'Fresh' : known ? 'Stale' : 'Queued';
      const status = addCell(row, node('span', label, 'pill ' + (label === 'Fresh' ? 'ok' : label === 'Failed' ? 'error' : 'warn')));
      if (e.last_error) status.append(node('span', e.last_error, 'secondary'));
      $('accounts').append(row);
    }
    $('accounts-empty').hidden = entries.length > 0;
    $('accounts-empty').textContent = all.length ? 'No accounts match this provider.' : 'No supported accounts found. Enable a Claude, Codex, or Grok OAuth account in CPA; the next scheduled scan will discover it.';
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
  async function refresh() {
    if (busy) return;
    busy = true; $('refresh').disabled = true;
    try {
      const response = await fetch(auth.managementPath('/status'), {credentials:'same-origin',cache:'no-store',headers:auth.authHeaders({}),signal:AbortSignal.timeout(10000)});
      if (response.status === 401 || response.status === 403) throw new Error('Sign in to CPA on this same address, remember your management session, then refresh this view.');
      if (!response.ok) throw new Error('Quota Cache status is unavailable (HTTP ' + response.status + '). Check that the plugin is enabled and registered in CPA, and inspect its logs.');
      const data = await response.json();
      if (data.schema !== 1 || !data.entries || !data.running) throw new Error('The cache has not supplied a running status. Check the plugin version and CPA logs.');
      snapshot = data; render(data); $('content').hidden = false; $('error').hidden = true;
      $('view-status').textContent = 'View updated ' + new Date().toLocaleTimeString();
    } catch (error) {
      // Hide prior account data on loss of authorization; never leave it looking live.
      $('content').hidden = true;
      $('error').textContent = error.name === 'TimeoutError' ? 'The status request timed out. Check CPA connectivity, then refresh this view.' : error.message;
      $('error').hidden = false; $('health').textContent = 'Status unavailable'; $('health').className = 'pill warn'; $('view-status').textContent = 'View could not be updated';
    } finally { busy = false; $('refresh').disabled = false; }
  }
  $('refresh').addEventListener('click',refresh);
  $('provider').addEventListener('change',()=>{if(snapshot)render(snapshot);});
  setInterval(()=>{if($('auto').checked && !document.hidden)refresh();},30000);
  refresh();
})();
