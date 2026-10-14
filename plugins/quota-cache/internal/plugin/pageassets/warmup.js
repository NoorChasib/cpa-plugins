(function (page) {
  'use strict';
  if (!page) return;
  const $ = id => document.getElementById(id);
  let busy = false;
  let enabled = false;
  const labels = {
    active: 'Session active', idle: 'Ready to warm', warming: 'Warming',
    awaiting_confirmation: 'Waiting for quota confirmation', weekly_spent: 'Weekly limit spent',
    waiting_for_quota: 'Waiting for fresh quota', outside_hours: 'Outside automatic hours',
    provider_cooldown: 'Provider cooldown', waiting: 'Waiting before another attempt', disabled: 'Account disabled', failed: 'Warm-up failed', not_listed: 'Account no longer listed'
  };
  const errors = {
    profile_missing: 'Sign in to a Claude Code profile named by this auth index.',
    subscription_login_required: 'The profile needs a paid Claude subscription sign-in.',
    profile_identity_mismatch: 'The profile is signed into a different account.',
    identity_unknown: 'CPA has not reported an account email.',
    cli_unavailable: 'Claude Code is not installed at the configured path.',
    runner_unavailable: 'The runner could not start.',
    outcome_unknown: 'The request may have succeeded. The retry guard remains in place.',
    request_failed: 'Check the selected model, effort and native Claude Code profile. No model fallback is used.'
  };
  function render(status) {
    const state = status.warmup;
    $('warmup').hidden = !state;
    if (!state) return;
    enabled = state.enabled === true && !state.last_error;
    $('warm-all').disabled = busy || !enabled || Boolean(page.refusal(page.readSession()));
    $('warmup-config').textContent = `${state.enabled ? 'Enabled' : 'Off'} · ${state.model} · ${state.effort} effort · ${state.hours} (${state.timezone})`;
    if (state.last_error) $('warmup-result').textContent = 'Warm-up journal unavailable. No prompts will be sent; see the setup guide.';
    const names = new Map((status.editor?.credentials || []).map(account => [account.auth_index, account.name]));
    $('warmup-accounts').replaceChildren();
    for (const account of state.accounts || []) {
      const item = page.node('li');
      item.append(page.node('b', names.get(account.auth_index) || account.auth_index), document.createTextNode(' · ' + (labels[account.state] || 'Waiting')));
      if (account.error) item.append(page.node('span', errors[account.error] || 'Warm-up unavailable', 'secondary'));
      if (account.reset_at && !account.reset_at.startsWith('0001-')) item.append(page.node('span', 'Reset: ' + new Date(account.reset_at).toLocaleString(), 'secondary'));
      item.append(page.node('span', 'Profile: ' + account.auth_index, 'secondary'));
      $('warmup-accounts').append(item);
    }
  }
  $('warm-all').addEventListener('click', async () => {
    const session = page.readSession();
    if (busy || !enabled || !page.root || !session || page.refusal(session)) return;
    busy = true; $('warm-all').disabled = true;
    try {
      const response = await fetch(page.root + '/v0/management/plugins/quota-cache/warmup', {
        method: 'POST', headers: {Authorization: 'Bearer ' + session.key, Accept: 'application/json'},
        mode: 'same-origin', credentials: 'same-origin', cache: 'no-store', redirect: 'error', signal: AbortSignal.timeout(10000)
      });
      if (response.status === 401 || response.status === 403) {
        page.refusedSave(page.remember(session, response.status, null));
        $('warmup-result').textContent = 'CPA refused the session. Use Refresh view after signing in; no automatic retry was made.';
        return;
      }
      $('warmup-result').textContent = response.status === 202
        ? 'Queued idle accounts. Active windows and exhausted accounts are skipped.'
        : 'Warm-up could not be queued. Check that it is enabled and the journal is available.';
      page.reload();
    } catch {
      page.unansweredSave();
      $('warmup-result').textContent = 'No response. Refresh the view to check status; this request will not be retried automatically.';
    } finally { busy = false; $('warm-all').disabled = !enabled || Boolean(page.refusal(page.readSession())); }
  });
  window.QuotaCacheWarmup = Object.freeze({render});
})(window.QuotaCachePage);
