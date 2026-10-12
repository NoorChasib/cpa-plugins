(function (page) {
  'use strict';
  // The editor: the Organizations, Claude API credits and Claude renewal dates
  // cards, and the note on the import from Quota Glance. It is embedded after
  // app.js in the page's one script element and reads the session, refusal
  // and decoding rules from app.js's namespace, so a save follows exactly the
  // rules a status read does:
  //
  // - nothing is sent without a remembered console session on this exact
  //   origin and API base, and a save goes only from a button press;
  // - a key CPA refused (401 or 403) is remembered at once, before the answer
  //   is read, and never sent again, by a save or by the status timer;
  // - a save that gets no answer stops the status timer, and is sent again
  //   only from another press;
  // - every string from a management answer is decoded by unescapeHost before
  //   it reaches a text node, and the page builds every node itself: it never
  //   turns a string into markup or code, and sets no inline handler.
  //
  // Every value rule here mirrors one the plugin enforces, so a value it would
  // refuse is said under its field before anything is sent. The model is a
  // port of Quota Glance's lib/settings.ts editor, which this replaces.

  // ---------------------------------------------------------------------------
  // Money

  // client.ValidMonthlyUSD: 1 to 7 whole digits, at most 2 decimals, zero allowed.
  const MONTHLY_USD = /^[0-9]{1,7}(\.[0-9]{1,2})?$/;
  const groupThousands = digits => digits.replace(/\B(?=(\d{3})+(?!\d))/g, ',');

  // A dollar amount as a reader types one: a leading $, spaces and thousands
  // commas are dropped; what is left must be what the plugin accepts. A comma
  // before one or two final digits is cents in some places and thousands in
  // others, so both readings come back for the reader to pick.
  function readMoney(input) {
    let value = String(input).trim();
    if (value.startsWith('$')) value = value.slice(1);
    value = value.replace(/\s+/g, '');
    if (value === '') return {kind: 'empty'};
    if (value.includes(',')) {
      const tail = /^([0-9,]*),([0-9]{1,2})$/.exec(value);
      if (tail) {
        const whole = tail[1].replace(/,/g, '').replace(/^0+(?=\d)/, '');
        return {kind: 'ambiguous', decimal: (whole === '' ? '0' : whole) + '.' + tail[2].padEnd(2, '0'), thousands: groupThousands(value.replace(/,/g, '').replace(/^0+(?=\d)/, '')) + '.00'};
      }
      if (!/^[0-9]{1,3}(,[0-9]{3})+(\.[0-9]*)?$/.test(value)) return {kind: 'invalid'};
      value = value.replace(/,/g, '');
    }
    return MONTHLY_USD.test(value) ? {kind: 'ok', value} : {kind: 'invalid'};
  }
  // A valid amount as money: 1200.5 is $1,200.50. String work only.
  function moneyText(value) {
    const [whole = '0', cents = ''] = String(value).split('.');
    return '$' + groupThousands(whole.replace(/^0+(?=\d)/, '')) + '.' + cents.padEnd(2, '0');
  }
  // Whether two valid amounts are the same number of cents.
  function sameAmount(a, b) {
    const cents = value => { const [whole = '0', fraction = ''] = value.split('.'); return Number(whole) * 100 + Number(fraction.padEnd(2, '0')); };
    return MONTHLY_USD.test(a) && MONTHLY_USD.test(b) && cents(a) === cents(b);
  }

  // ---------------------------------------------------------------------------
  // Dates

  const MONTH_NAMES = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];
  const daysIn = (year, month) => new Date(Date.UTC(year, month, 0)).getUTCDate();

  // A YYYY-MM-DD date as the plugin reads one (client.ParseRenewalDate): a day
  // that exists, in 2000 to 2099. incomplete is the date control's own report
  // of a date half typed, which it hands over as an empty value.
  function readDate(input, incomplete) {
    if (incomplete) return {kind: 'invalid', message: 'Enter a full date.'};
    const value = String(input).trim();
    if (value === '') return {kind: 'empty'};
    const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
    if (!match) return {kind: 'invalid', message: 'Enter a full date.'};
    const [year, month, day] = [Number(match[1]), Number(match[2]), Number(match[3])];
    if (month < 1 || month > 12 || day < 1) return {kind: 'invalid', message: 'Enter a full date.'};
    const last = daysIn(year, month);
    if (day > last) return {kind: 'invalid', message: month === 2 ? 'February ' + year + ' has ' + last + ' days.' : MONTH_NAMES[month - 1] + ' has ' + last + ' days.'};
    if (year < 2000 || year > 2099) return {kind: 'invalid', message: 'Enter a date between 2000 and 2099.'};
    return {kind: 'ok', value};
  }
  // 00:00 UTC of a valid date, in epoch seconds.
  function dateEpoch(value) {
    const [year, month, day] = value.split('-').map(Number);
    return Date.UTC(year, month - 1, day) / 1000;
  }
  // A calendar day as UTC sees it: "Oct 29".
  const utcDay = epoch => new Date(epoch * 1000).toLocaleDateString([], {month: 'short', day: 'numeric', timeZone: 'UTC'});
  const localDay = epoch => new Date(epoch * 1000).toLocaleDateString([], {month: 'short', day: 'numeric'});
  const localDateTime = epoch => new Date(epoch * 1000).toLocaleString([], {month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit'});
  function ordinal(day) {
    const tens = day % 100;
    if (tens >= 11 && tens <= 13) return day + 'th';
    return day + (['th', 'st', 'nd', 'rd'][day % 10] || 'th');
  }

  // 00:00 UTC on day of a month, clamped to its last day. month is 0-based and
  // may run past either end.
  function refillIn(year, month, day) {
    const first = new Date(Date.UTC(year, month, 1));
    const y = first.getUTCFullYear();
    const m = first.getUTCMonth();
    return Date.UTC(y, m, Math.min(day, daysIn(y, m + 1))) / 1000;
  }
  // The cycle a refill date gives at an instant, [start, end) in epoch
  // seconds: client.CreditCycleAt, ported. It starts at 00:00 UTC on the most
  // recent occurrence of the date's day of the month, clamped to a shorter
  // month, and ends at the next. Null for a date the plugin would not accept.
  function cycleOn(renews, at) {
    const date = readDate(renews);
    if (date.kind !== 'ok') return null;
    const day = Number(date.value.slice(8, 10));
    const now = new Date(at * 1000);
    const year = now.getUTCFullYear();
    const month = now.getUTCMonth();
    const thisOne = refillIn(year, month, day);
    if (thisOne <= at) return {start: thisOne, end: refillIn(year, month + 1, day)};
    return {start: refillIn(year, month - 1, day), end: thisOne};
  }
  const cycleText = cycle => 'this cycle ' + utcDay(cycle.start) + ' – ' + utcDay(cycle.end);
  // What a refill date means, under the field being edited; days 29 to 31
  // say what happens in a shorter month.
  function refillRule(renews) {
    const date = readDate(renews);
    if (date.kind !== 'ok') return null;
    const day = Number(date.value.slice(8, 10));
    return day >= 29 ? 'Refills on the ' + ordinal(day) + ', or on the last day of shorter months.' : 'Refills on the ' + ordinal(day) + ' of every month at 00:00 UTC';
  }

  // ---------------------------------------------------------------------------
  // Reading times

  // How old a new reading may be, and how far ahead of the plugin's clock
  // (internal/settings), and the control's own lower bound, a few minutes
  // inside the plugin's so a slow save still lands. The control's bounds only
  // guide its picker: they are those of the moment the card was drawn, and
  // its form is novalidate, so they never stop a Save.
  const READING_MAX_AGE = 48 * 3600;
  const READING_MAX_AHEAD = 5 * 60;
  const READING_PICK_AGE = 47 * 3600 + 55 * 60;

  // A datetime-local value, read in the reader's own zone; null when it is not
  // one, or when the clock time does not exist there.
  function localInstant(value) {
    if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$/.test(value)) return null;
    if (readDate(value.slice(0, 10)).kind !== 'ok') return null;
    const at = new Date(value);
    const [hour, minute, second = 0] = value.slice(11).split(':').map(Number);
    if (!Number.isFinite(at.getTime()) || at.getHours() !== hour || at.getMinutes() !== minute || at.getSeconds() !== second) return null;
    return Math.floor(at.getTime() / 1000);
  }
  function localInputValue(epoch) {
    const at = new Date(epoch * 1000);
    const pad = n => String(n).padStart(2, '0');
    return at.getFullYear() + '-' + pad(at.getMonth() + 1) + '-' + pad(at.getDate()) + 'T' + pad(at.getHours()) + ':' + pad(at.getMinutes());
  }
  // An instant as the plugin takes one: RFC 3339 in UTC, whole seconds.
  const rfc3339 = epoch => new Date(Math.floor(epoch) * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z');
  const utcClock = epoch => new Date(epoch * 1000).toISOString().slice(11, 16) + ' UTC';
  // "Oct 10, 23:30 UTC", or only the clock when the reader's day is the same.
  const utcStamp = epoch => (localDay(epoch) === utcDay(epoch) ? '' : utcDay(epoch) + ', ') + utcClock(epoch);

  // ---------------------------------------------------------------------------
  // Fields

  const AMOUNT_MESSAGE = 'Enter a dollar amount, like 200 or 260.50.';
  const READING_AMOUNT_MESSAGE = 'Enter the amount Console shows, like 143.20.';
  const READING_TIME_MESSAGE = 'Enter a time in the last 48 hours.';
  // Quota Cache judges a reading's time by CPA's clock, which this device's
  // may not match, so its refusal says so.
  const READING_TIME_REFUSED = 'Enter a time from 48 hours ago to 5 minutes ahead by CPA’s clock, which this device’s clock may not match.';
  const ambiguousMessage = choice => 'A comma is ambiguous. Did you mean ' + choice.decimal + ' or ' + choice.thousands + '?';

  // One money or date field from what is stored, what the config says and
  // the reader's edit: {kind: 'set', text, incomplete?} for a value typed, or
  // {kind: 'drop'} for Use config or Use estimate. desired is what the batch
  // sends: a value, or null for none.
  function field(stored, config, configInvalid, edit, read, same, emptyMessage) {
    const base = stored || config;
    const current = stored === '' ? null : stored;
    const state = {text: base, source: stored ? 'dashboard' : config ? 'config' : 'none', stored, config, configInvalid, edit, changed: false, error: null, ambiguous: null, desired: current};
    if (edit === undefined) return state;
    if (edit.kind === 'drop') return Object.assign(state, {text: config, desired: null, changed: current !== null});
    state.text = edit.text;
    const value = read(edit);
    switch (value.kind) {
      case 'empty': if (base !== '') state.error = emptyMessage; return state;
      case 'invalid': state.error = value.message; return state;
      case 'ambiguous': state.error = ambiguousMessage(value); state.ambiguous = {decimal: value.decimal, thousands: value.thousands}; return state;
      default:
        // Typed back to what it opened with: no change, and a value that came
        // from the config keeps coming from it.
        if (base !== '' && same(value.value, base)) return state;
        return Object.assign(state, {desired: value.value, changed: value.value !== current});
    }
  }
  const moneyReader = message => edit => { const value = readMoney(edit.text); return value.kind === 'invalid' ? {kind: 'invalid', message} : value; };
  const dateReader = edit => readDate(edit.text, edit.incomplete === true);
  const creditField = (settings, edit) => field(settings.monthlyUsd, settings.configMonthlyUsdInvalid ? '' : settings.configMonthlyUsd, settings.configMonthlyUsdInvalid, edit, moneyReader(AMOUNT_MESSAGE), sameAmount, 'Enter a dollar amount.');
  const refillField = (settings, edit) => field(settings.renews, settings.configRenewsInvalid ? '' : settings.configRenews, settings.configRenewsInvalid, edit, dateReader, (a, b) => a === b, 'Enter a full date.');
  // A Claude renewal date. Its fallback is the estimate, which has no value to show.
  const renewalField = (setting, edit) => field(setting ? setting.date : '', '', false, edit, dateReader, (a, b) => a === b, 'Enter a full date.');

  const beforeRefillMessage = start => 'The credit refilled on ' + utcDay(start) + ', after this time. Enter a reading taken since.';
  // A Console reading: cleared ({kind: 'clear'}), or a new one typed
  // ({kind: 'set', amount, at}). renews is the refill date the batch makes
  // effective for the row, which is what the plugin checks a new reading
  // against; now is the page's clock.
  function readingField(settings, edit, context) {
    const stored = settings.reading || null;
    const sent = stored ? {remainingUsd: stored.remainingUsd, at: rfc3339(stored.atEpoch)} : null;
    const state = {stored, unusedReason: settings.readingUnusedReason || '', edit, changed: false, amount: null, at: null, atError: null, onRefillDay: false, desired: sent};
    if (edit === undefined) return state;
    if (edit.kind === 'clear') return Object.assign(state, {desired: null, changed: stored !== null});
    const amount = readMoney(edit.amount);
    state.amount = {
      text: edit.amount,
      error: amount.kind === 'ok' ? null : amount.kind === 'ambiguous' ? ambiguousMessage(amount) : amount.kind === 'empty' ? 'Enter the amount Console shows.' : READING_AMOUNT_MESSAGE,
      ambiguous: amount.kind === 'ambiguous' ? {decimal: amount.decimal, thousands: amount.thousands} : null,
    };
    const at = localInstant(edit.at);
    const cycle = context.renews ? cycleOn(context.renews, context.now) : null;
    const desired = amount.kind === 'ok' && at !== null ? {remainingUsd: amount.value, at: rfc3339(at)} : null;
    // The stored reading sent back as it was is not judged against the clock
    // again, by the plugin or here.
    const same = desired !== null && sent !== null && desired.remainingUsd === sent.remainingUsd && desired.at === sent.at;
    if (at === null) state.atError = 'Enter a full date and time.';
    else if (same) state.atError = null;
    else if (at < context.now - READING_MAX_AGE || at > context.now + READING_MAX_AHEAD) state.atError = READING_TIME_MESSAGE;
    else if (cycle !== null && at < cycle.start) state.atError = beforeRefillMessage(cycle.start);
    state.at = at;
    state.onRefillDay = at !== null && cycle !== null && at >= cycle.start && at < cycle.start + 86400;
    state.desired = desired || sent;
    state.changed = !same;
    return state;
  }

  // ---------------------------------------------------------------------------
  // Drafts and batches

  // One organization's three fields, and what they add up to. unsaved counts
  // the fields edited and not typed back, whether or not they are valid yet:
  // what leaving the page would lose.
  function creditRow(settings, edit, now) {
    const monthlyUsd = creditField(settings, edit && edit.monthlyUsd);
    const renews = refillField(settings, edit && edit.renews);
    // The refill date the plugin checks a new reading against: this batch's
    // own, else the config's.
    const effective = renews.error === null ? (renews.desired || (settings.configRenewsInvalid ? null : settings.configRenews || null)) : null;
    const reading = readingField(settings, edit && edit.reading, {now, renews: effective});
    const fields = [monthlyUsd, renews];
    return {
      monthlyUsd, renews, reading,
      changes: fields.filter(one => one.changed).length + (reading.changed ? 1 : 0),
      errors: fields.filter(one => one.error !== null).length + (reading.amount && reading.amount.error ? 1 : 0) + (reading.atError ? 1 : 0),
      // A reading typed is a change until it is the stored one again, valid
      // or not.
      unsaved: fields.filter(one => one.changed || one.error !== null).length + (reading.changed ? 1 : 0),
    };
  }
  // A draft is {rows: {id: {baseRevision, name, monthlyUsd?, renews?,
  // reading?}}, remove: {id: revision}}: what the reader changed and nothing
  // else. A row's revision is taken at its first edit and kept until its last
  // is undone; an edit of undefined is Undo.
  const emptyDraft = () => ({rows: {}, remove: {}});
  function editCredit(draft, account, name, key, edit) {
    const row = Object.assign({}, draft.rows[account.id] || {baseRevision: account.settings.revision, name});
    if (edit === undefined) delete row[key];
    else row[key] = edit;
    const rows = Object.assign({}, draft.rows);
    if (row.monthlyUsd === undefined && row.renews === undefined && row.reading === undefined) delete rows[account.id];
    else rows[account.id] = row;
    return {rows, remove: draft.remove};
  }
  function markRemoved(draft, orphan, remove) {
    const next = Object.assign({}, draft.remove);
    if (remove) next[orphan.id] = orphan.revision;
    else delete next[orphan.id];
    return {rows: draft.rows, remove: next};
  }
  // What one Save would send: {changes, errors, unsaved, batch, tooLarge}.
  // unsaved counts the edits leaving would lose, those still to fix
  // included. batch is null with nothing to send, anything to fix, or past
  // the limits the plugin reads: maxItems rows and maxBytes of JSON.
  function plan(items, changes, errors, unsaved, kind, limits) {
    if (errors > 0 || items.length === 0) return {changes, errors, unsaved, batch: null, tooLarge: false};
    const batch = {kind, items};
    const tooLarge = items.length > limits.maxItems || new TextEncoder().encode(JSON.stringify(batch)).length > limits.maxBytes;
    return {changes, errors, unsaved, batch: tooLarge ? null : batch, tooLarge};
  }
  // The API credit card's Save: every changed editable row, in account order,
  // each with its full desired state, then each orphan marked for removal
  // with every value null.
  function creditPlan(accounts, draft, now, settingsOf, limits) {
    const items = [];
    let changes = 0;
    let errors = 0;
    let unsaved = 0;
    for (const account of accounts) {
      const edit = draft.rows[account.id];
      const settings = settingsOf(account);
      if (edit === undefined || !settings.editable) continue;
      const row = creditRow(settings, edit, now);
      changes += row.changes;
      errors += row.errors;
      unsaved += row.unsaved;
      if (row.changes === 0) continue;
      items.push({id: account.id, baseRevision: edit.baseRevision, monthlyUsd: row.monthlyUsd.desired, renews: row.renews.desired, reading: row.reading.desired});
    }
    for (const [id, revision] of Object.entries(draft.remove)) {
      changes++;
      unsaved++;
      items.push({id, baseRevision: revision, monthlyUsd: null, renews: null, reading: null});
    }
    return plan(items, changes, errors, unsaved, 'apiCredits', limits);
  }
  // The draft after the rows changed: rows no longer listed are dropped, each
  // with the sentence that says so, and removals of orphans already gone are
  // forgotten. Everything else is kept.
  function reconcile(draft, listed, orphaned, gone) {
    const rows = {};
    const dropped = [];
    for (const [id, row] of Object.entries(draft.rows)) {
      if (listed.has(id)) rows[id] = row;
      else dropped.push(gone(row.name));
    }
    const remove = Object.fromEntries(Object.entries(draft.remove).filter(([id]) => orphaned.has(id)));
    const unchanged = dropped.length === 0 && Object.keys(remove).length === Object.keys(draft.remove).length;
    return {draft: unchanged ? draft : {rows, remove}, dropped};
  }
  const reconcileCredits = (draft, accounts, orphans) => reconcile(draft, new Set(accounts.map(a => a.id)), new Set(orphans.map(o => o.id)), name => name + ' is no longer listed under Organizations; its unsaved change was dropped.');
  const reconcileRenewals = (draft, credentials, orphans) => reconcile(draft, new Set(credentials.map(c => c.auth_index)), new Set(orphans.map(o => o.id)), name => name + ' is no longer in CPA; its unsaved change was dropped.');
  function editRenewal(draft, credential, name, edit) {
    const rows = Object.assign({}, draft.rows);
    if (edit === undefined) delete rows[credential.auth_index];
    else {
      const held = rows[credential.auth_index];
      rows[credential.auth_index] = {baseRevision: held ? held.baseRevision : credential.renewal ? credential.renewal.revision : '', name: held ? held.name : name, date: edit};
    }
    return {rows, remove: draft.remove};
  }
  // The renewal card's Save: every changed date, in roster order, then each
  // renewal orphan marked for removal with date null.
  function renewalPlan(credentials, draft, settingOf, limits) {
    const items = [];
    let changes = 0;
    let errors = 0;
    for (const credential of credentials) {
      const edit = draft.rows[credential.auth_index];
      if (edit === undefined || credential.renewal_editable !== true) continue;
      const state = renewalField(settingOf(credential), edit.date);
      if (state.error !== null) errors++;
      if (!state.changed) continue;
      changes++;
      items.push({id: credential.auth_index, baseRevision: edit.baseRevision, date: state.desired});
    }
    for (const [id, revision] of Object.entries(draft.remove)) {
      changes++;
      items.push({id, baseRevision: revision, date: null});
    }
    // A date to fix is never also a change, so the two add up to the edits.
    return plan(items, changes, errors, changes + errors, 'renewals', limits);
  }
  // A draft after a 409 conflict: each conflicting row's edit, and any removal
  // it marked, is dropped, since its fields now show what the other device
  // saved; every other row's is kept. retry says whether anything is left.
  function dropConflicts(draft, ids) {
    const rows = Object.assign({}, draft.rows);
    const remove = Object.assign({}, draft.remove);
    for (const id of ids) { delete rows[id]; delete remove[id]; }
    return {draft: Object.assign({}, draft, {rows, remove}), retry: Object.keys(rows).length > 0 || Object.keys(remove).length > 0};
  }
  // A draft after a 409 not_editable: each row the plugin no longer offers,
  // and each removal of an orphan already gone, is dropped and named, by the
  // name its edit kept or else by nameOf(id); every other row's is kept.
  // retry says whether anything is left.
  function dropRefused(draft, ids, nameOf) {
    const names = ids.filter(id => draft.rows[id] !== undefined || draft.remove[id] !== undefined).map(id => (draft.rows[id] && draft.rows[id].name) || nameOf(id));
    return Object.assign(dropConflicts(draft, ids), {names});
  }
  // "alpha", "alpha and bravo", "alpha, bravo and charlie".
  const andList = names => (names.length === 1 ? names[0] : names.slice(0, -1).join(', ') + ' and ' + names[names.length - 1]);
  // The foot after a 409 not_editable: what was dropped, by name.
  function refusedRowsText(names, retry) {
    if (names.length === 0) return saveErrorText('not_editable');
    return 'Not saved. ' + andList(names) + ' can no longer be edited here, so ' + (names.length === 1 ? 'its change was' : 'their changes were') + ' dropped.' + (retry ? ' Your other changes are kept.' : '');
  }
  // The orphans a card being edited offers: those the last status listed,
  // less those a save found gone, each at the revision a conflict said it is
  // stored at now, so a removal marked again is not refused again for the
  // revision the view still shows. The view is held still while a card is
  // open, so nothing else would bring either news.
  function liveOrphans(orphans, gone, revised) {
    return orphans.filter(orphan => !gone.has(orphan.id)).map(orphan => (revised[orphan.id] !== undefined ? Object.assign({}, orphan, {revision: revised[orphan.id]}) : orphan));
  }
  // What a 409 conflict says of an orphan's row: gone when nothing is stored
  // now, else the revision it is stored at.
  function orphanAfterConflict(current, gone, revised, id) {
    const revision = isObject(current) && typeof current.revision === 'string' ? current.revision : '';
    if (revision === '') gone.add(id);
    else revised[id] = revision;
  }
  // Whether a revision the plugin sent is newer than another: decimal, "" for nothing stored.
  const newerRevision = (a, b) => BigInt(a || '0') > BigInt(b || '0');
  // The settings block a row shows while its card is open: the published one,
  // or kept, {settings, over}, what a 409 conflict said is stored now and the
  // published revision it stood in for. The conflict's block wins while the
  // published one has not moved since, even when its own revision is lower,
  // as for a row another device cleared, which is "" once nothing is stored.
  // A published block that moved on is the later news, unless the conflict's
  // is newer still.
  function overlaid(published, kept) {
    if (!kept) return published;
    if (published.revision !== kept.over && !newerRevision(kept.settings.revision, published.revision)) return published;
    return Object.assign({}, kept.settings, {editable: published.editable, notEditableReason: published.notEditableReason});
  }
  // The fields a conflict replaced on one row: those the reader had changed,
  // and those whose stored value moved under them.
  function conflictFields(before, current, edit) {
    const moved = {
      monthlyUsd: before.monthlyUsd !== current.monthlyUsd,
      renews: before.renews !== current.renews,
      reading: (before.reading && before.reading.remainingUsd) !== (current.reading && current.reading.remainingUsd) || (before.reading && before.reading.atEpoch) !== (current.reading && current.reading.atEpoch),
    };
    return ['monthlyUsd', 'renews', 'reading'].filter(key => moved[key] || (edit && edit[key] !== undefined));
  }

  // ---------------------------------------------------------------------------
  // Organizations

  // client.NormalizeOrganizationID, ported: a UUID in its 8-4-4-4-12 hex form,
  // trimmed and lower-cased, and not the nil UUID. Null for anything else.
  const ORGANIZATION_ID = /^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$/;
  function normalizeOrganizationID(input) {
    const value = String(input).trim();
    if (!ORGANIZATION_ID.test(value) || /^[0-]*$/.test(value)) return null;
    return value.toLowerCase();
  }
  // settings.ValidOrganizationLabel: 1 to 64 characters, no surrounding
  // spaces, each one printable (a letter, mark, number, punctuation, symbol
  // or the plain space; unicode.IsPrint).
  const PRINTABLE = /^[\p{L}\p{M}\p{N}\p{P}\p{S} ]+$/u;
  const validOrganizationLabel = label => typeof label === 'string' && label !== '' && label === label.trim() && [...label].length <= 64 && PRINTABLE.test(label);
  const sameLabel = (a, b) => a.toLowerCase() === b.toLowerCase();
  const LABEL_MESSAGE = 'Enter a label of 1 to 64 letters, numbers, spaces or punctuation.';
  const DUPLICATE_LABEL_MESSAGE = 'Another organization already has this label.';
  const ORGANIZATION_ID_MESSAGE = 'Enter the Organization ID as Console shows it: 32 hex digits in five groups, like 00000000-0000-4000-8000-000000000000.';
  const DUPLICATE_ORGANIZATION_MESSAGE = 'This organization is already listed.';
  const tooManyOrganizations = max => max + ' organizations at most. Remove one before adding another.';

  // An organizations draft: {rename: {id: label typed}, remove: {id: true},
  // add: {label, organizationId}}. Its Save is one batch whose ops the plugin
  // applies in order, all or nothing: removes, then renames in list order,
  // then the add, each judged against the list the ops before it left, as the
  // plugin judges them. errors are keyed "<id>:label", "add:label",
  // "add:organizationId" and "add:". refs[i] names the row and field of op i,
  // so a refusal the plugin names by op index lands under its field.
  const emptyOrganizationsDraft = () => ({rename: {}, remove: {}, add: {label: '', organizationId: ''}});
  function organizationsPlan(items, draft, revision, limits) {
    const ops = [];
    const refs = [];
    const errors = {};
    let changes = 0;
    let list = items.map(item => ({id: item.id, label: item.label, organizationId: item.organization_id}));
    for (const item of items) {
      if (!draft.remove[item.id]) continue;
      changes++;
      list = list.filter(one => one.id !== item.id);
      ops.push({op: 'remove', id: item.id});
      refs.push({row: item.id, field: 'id'});
    }
    for (const item of items) {
      if (draft.remove[item.id] || draft.rename[item.id] === undefined) continue;
      const label = draft.rename[item.id].trim();
      if (label === item.label) continue;
      changes++;
      if (!validOrganizationLabel(label)) { errors[item.id + ':label'] = LABEL_MESSAGE; continue; }
      if (list.some(one => one.id !== item.id && sameLabel(one.label, label))) { errors[item.id + ':label'] = DUPLICATE_LABEL_MESSAGE; continue; }
      list = list.map(one => (one.id === item.id ? Object.assign({}, one, {label}) : one));
      ops.push({op: 'rename', id: item.id, label});
      refs.push({row: item.id, field: 'label'});
    }
    const addLabel = draft.add.label.trim();
    const typedID = draft.add.organizationId;
    let normalized = null;
    if (addLabel !== '' || typedID.trim() !== '') {
      changes++;
      normalized = normalizeOrganizationID(typedID);
      if (!validOrganizationLabel(addLabel)) errors['add:label'] = LABEL_MESSAGE;
      else if (list.some(one => sameLabel(one.label, addLabel))) errors['add:label'] = DUPLICATE_LABEL_MESSAGE;
      if (normalized === null) errors['add:organizationId'] = ORGANIZATION_ID_MESSAGE;
      else if (list.some(one => one.organizationId === normalized)) errors['add:organizationId'] = DUPLICATE_ORGANIZATION_MESSAGE;
      if (errors['add:label'] === undefined && errors['add:organizationId'] === undefined) {
        if (list.length >= limits.maxItems) errors['add:'] = tooManyOrganizations(limits.maxItems);
        else {
          ops.push({op: 'add', label: addLabel, organizationId: normalized});
          refs.push({row: 'add', field: 'label'});
        }
      }
    }
    const count = Object.keys(errors).length;
    // changes already counts the edits still to fix, so it is also unsaved.
    const result = {changes, errors, count, unsaved: changes, ops, refs, normalized, batch: null, tooLarge: false};
    if (count > 0 || ops.length === 0) return result;
    const batch = {kind: 'organizations', baseRevision: revision, ops};
    result.tooLarge = ops.length > limits.maxOps || new TextEncoder().encode(JSON.stringify(batch)).length > limits.maxBytes;
    if (!result.tooLarge) result.batch = batch;
    return result;
  }
  // An organizations draft after a 409 conflict: the rows now are the
  // plugin's, so the renames and removals made against the old list go, and
  // the unsaved add row stays.
  const organizationsAfterConflict = draft => ({rename: {}, remove: {}, add: Object.assign({}, draft.add)});
  // The items of the list a draft was made against that it renames or
  // removes.
  const organizationEdits = (items, draft) => items.filter(item => draft.remove[item.id] === true || (draft.rename[item.id] !== undefined && draft.rename[item.id].trim() !== item.label));
  // The list an organizations draft keeps when a newer status is drawn while
  // the card is open: the list before, {revision, items}, or null. The file's
  // revision covers the whole list, so renames and removals made against one
  // revision must go out at it: sent at the newer one, the plugin would take
  // them over what another device saved, without the conflict the revision
  // exists to raise. Kept, Save meets that conflict, which shows the plugin's
  // list and names what was dropped. A draft with no rename or removal, such
  // as one with only the add row filled, goes on against the newer list,
  // whose rules it is judged by.
  function organizationsHeld(before, after, draft) {
    if (!isObject(before) || !isObject(after) || before.revision === after.revision) return null;
    const items = Array.isArray(before.items) ? before.items : [];
    return organizationEdits(items, draft).length > 0 ? {revision: before.revision, items} : null;
  }
  // The foot after an organizations 409: the rows are the plugin's list now,
  // so the renames and removals made against the old one, named by the labels
  // they were made against, were dropped.
  function organizationsConflictText(labels) {
    if (labels.length === 0) return ORGANIZATIONS_CONFLICT_FOOT;
    const one = labels.length === 1;
    return 'Not saved. Another device changed the organizations first. The list now shows theirs, so your ' + (one ? 'change to ' : 'changes to ') + andList(labels) + (one ? ' was' : ' were') +
      ' dropped; check it, then make ' + (one ? 'it' : 'them') + ' again.';
  }
  // The question Remove asks. A moved organization's monthly credit and refill
  // date from the configuration are its entry's and go with it, so adding the
  // same Organization ID again does not bring them back; what was entered
  // here stays in the settings file and does.
  function removeQuestion(label, item) {
    const both = item.has_default_monthly_usd === true && item.has_default_renews === true;
    const configured = both ? 'monthly credit and refill date' : item.has_default_monthly_usd === true ? 'monthly credit' : item.has_default_renews === true ? 'refill date' : '';
    if (!configured) return 'Remove ' + label + '? Its amounts and readings entered here stay as an orphan you can clear below, and adding the same Organization ID again brings them back.';
    return 'Remove ' + label + '? Its ' + configured + ' from Quota Cache’s configuration ' + (both ? 'go' : 'goes') + ' with it, and adding the same Organization ID again does not bring ' + (both ? 'them' : 'it') + ' back. ' +
      'Amounts and readings entered here stay as an orphan you can clear below, and come back with it.';
  }
  // The rows the Organizations card shows at rest: the file's list, editable
  // there; or, while the file cannot be read, the configuration's, which then
  // applies and is what the API credit card lists, shown read-only.
  function organizationRows(info, doc) {
    if (isObject(info) && info.reason === 'organizationsUnreadable') {
      const accounts = isObject(doc) && Array.isArray(doc.accounts) ? doc.accounts : [];
      return {fromConfig: true, items: accounts.map(account => ({id: account.id, label: account.label || account.id, organization_id: account.organizationId || ''}))};
    }
    return {fromConfig: false, items: isObject(info) && Array.isArray(info.items) ? info.items : []};
  }
  // An Organization ID cut to its first 8 and last 4 characters.
  const shortOrganizationID = id => (id.length > 12 ? id.slice(0, 8) + '…' + id.slice(-4) : id);
  // A CPA auth index as an orphan line names it: "0123…cdef".
  const shortCredentialID = id => (id.length > 8 ? id.slice(0, 4) + '…' + id.slice(-4) : id);
  // The API credit card's edit button, which its orphan lines name too: Set
  // credits and dates, the primary action, while organizations are listed
  // and none is counted or has a monthly credit, else Edit credits and dates.
  const SET_CREDITS = 'Set credits and dates';
  function creditsEditLabel(accounts, counted) {
    const unset = accounts.length > 0 && counted === 0 && !accounts.some(account => account.settings && (account.settings.monthlyUsd || account.settings.configMonthlyUsd));
    return unset ? SET_CREDITS : 'Edit credits and dates';
  }
  // The API credit card's head while no organization is counted. It says to
  // set credits and dates only when every organization lacks one of them: one
  // waiting on the meter, misconfigured or with no Organization ID yet may
  // have both, and its row says what it waits for.
  function noneCountedText(accounts) {
    const lacks = account => {
      const issues = Array.isArray(account.dataIssues) ? account.dataIssues : [];
      return account.state === 'needsSettings' || issues.includes('needsCredit') || issues.includes('needsRefillDate');
    };
    const remedy = accounts.length > 0 && accounts.every(lacks) ? ': set each one’s monthly credit and refill date' : '';
    return plural(accounts.length, 'organization', 'organizations') + ' · none counted yet' + remedy;
  }

  // ---------------------------------------------------------------------------
  // What came of a save

  const FIELD_CODES = new Set(['invalid_monthly_usd', 'invalid_renews', 'invalid_date', 'invalid_reading_amount', 'invalid_reading_time', 'reading_before_refill']);
  const OPERATION_CODES = new Set(['invalid_label', 'duplicate_label', 'invalid_organization_id', 'duplicate_organization', 'too_many_organizations', 'unknown_id']);
  // Every code the save route answers with (settings_route.go).
  const PLUGIN_CODES = new Set([...FIELD_CODES, ...OPERATION_CODES, 'cache_stopped', 'cross_site', 'unsupported_media_type', 'invalid_request', 'settings_unavailable',
    'organizations_unavailable', 'not_editable', 'conflict', 'too_many_writes', 'settings_full', 'settings_unwritable']);
  const FIELD_FOOT = 'Not saved. Fix the marked field.';
  const CONFLICT_FOOT = 'Not saved. Another device saved first. Your other changes are kept.';
  const CONFLICT_FIELD = 'Changed from another device. Showing the latest; check and save again.';
  const ORGANIZATIONS_CONFLICT_FOOT = 'Not saved. Another device changed the organizations first. The list now shows theirs; check it, then save again.';
  // The words a sentence about a request uses: a card's Save, which keeps
  // the reader's changes, or Import Quota Glance's settings, which has none.
  const ACTIONS = {
    save: {noun: 'save', done: 'saved', again: 'save again', writes: 'saves', kept: ' Your changes are kept until you reload.'},
    import: {noun: 'import', done: 'imported', again: 'import again', writes: 'saves and imports', kept: ''},
  };
  const actionOf = action => ACTIONS[action] || ACTIONS.save;
  const signInText = act => 'Sign in to CPA’s management console on this address with “Remember password” ticked, then ' + act.again + '.' + act.kept;
  const SIGN_IN_FOOT = signInText(ACTIONS.save);
  const LOST = 'No answer came back, so this may or may not have saved. Saving again is safe: a save that already landed is not applied twice.';
  const COUNTED = ' Each refused try counts toward CPA’s limit of five failed sign-ins, after which it locks this address out of management for 30 minutes.';

  function fieldMessage(code) {
    switch (code) {
      case 'invalid_monthly_usd': return AMOUNT_MESSAGE;
      case 'invalid_renews':
      case 'invalid_date': return 'Enter a real date between 2000 and 2099.';
      case 'invalid_reading_amount': return READING_AMOUNT_MESSAGE;
      case 'invalid_reading_time': return READING_TIME_REFUSED;
      case 'reading_before_refill': return 'This is before the credit last refilled. Enter a reading taken since.';
      case 'invalid_label': return LABEL_MESSAGE;
      case 'duplicate_label': return DUPLICATE_LABEL_MESSAGE;
      case 'invalid_organization_id': return ORGANIZATION_ID_MESSAGE;
      case 'duplicate_organization': return DUPLICATE_ORGANIZATION_MESSAGE;
      case 'too_many_organizations': return tooManyOrganizations(16);
      case 'unknown_id': return 'This organization is no longer listed. Cancel, then select Refresh view to see the current list.';
      default: return 'Check this value.';
    }
  }
  // "in a minute" from a Retry-After header in seconds; a minute when absent.
  function retryText(header) {
    const seconds = /^\d{1,6}$/.test(String(header || '').trim()) ? Number(header) : 60;
    return seconds <= 60 ? 'in a minute' : 'in ' + Math.ceil(seconds / 60) + ' minutes';
  }
  // The foot's sentence for a save that did not save, or the import note's
  // for an import (action 'import'). Each is proof nothing was written: the
  // plugin answers a code only before it commits, or after a commit that
  // failed and changed nothing.
  function saveErrorText(code, status, retryAfter, action) {
    const act = actionOf(action);
    switch (code) {
      case 'cache_stopped': return 'Quota Cache is stopped, so nothing was ' + act.done + '. Turn it back on in CPA, then ' + act.again + '.';
      case 'cross_site': return 'Quota Cache refused this request because it did not come from this page. Nothing was ' + act.done + '.';
      case 'unsupported_media_type':
      case 'invalid_request': return 'Quota Cache could not read this ' + act.noun + ', so nothing was ' + act.done + '. Reload the page and try again.';
      case 'settings_unavailable': return 'Quota Cache cannot read its saved settings, so nothing was ' + act.done + '.';
      case 'organizations_unavailable': return 'Quota Cache cannot read its organizations file, so nothing was ' + act.done + '.';
      case 'settings_unwritable': return 'Quota Cache could not write its file. Nothing was ' + act.done + '; try again.';
      case 'settings_full': return 'Quota Cache’s saved settings are full. Remove saved values for organizations and accounts no longer in use, then ' + act.again + '.';
      case 'too_many_writes': return 'Too many ' + act.writes + ' in the last minute, so nothing was ' + act.done + '. Try again ' + retryText(retryAfter) + '.';
      case 'not_editable': return 'Some of these can no longer be edited here. Nothing was saved.';
      case 'conflict': return CONFLICT_FOOT;
      case 'route_missing': return 'This server did not offer the save route, so nothing was ' + act.done + '. Reload the page; if this persists, check that Quota Cache 0.2.0 or newer is loaded.';
      case 'no_session': return signInText(act);
      default:
        if (FIELD_CODES.has(code) || OPERATION_CODES.has(code)) return FIELD_FOOT;
        return status >= 400 && status < 500 ? 'Something between this page and CPA refused the ' + act.noun + ' (HTTP ' + status + '). Nothing was ' + act.done + '.' : LOST;
    }
  }
  // Why a save, or an import (action 'import'), was refused before or by
  // CPA, from the remembered refusal. over says a lockout has run out.
  function refusedText(record, over, action) {
    const act = actionOf(action);
    switch (record && record.reason) {
      case 'banned':
        if (over) return 'CPA’s lockout of this address after repeated failed sign-ins should be over.' + act.kept + COUNTED;
        return 'CPA is refusing management requests from this address until about ' + new Date(record.until * 1000).toLocaleTimeString([], {hour: 'numeric', minute: '2-digit'}) + ' after repeated failed sign-ins, so nothing was ' + act.done + '.' + act.kept;
      case 'remote': return 'CPA accepts management sign-ins only on the machine it runs on, so nothing was ' + act.done + '. Open Quota Cache on that machine instead.';
      case 'off': return 'CPA’s management API is switched off on this server, so nothing was ' + act.done + '.';
      case 'refused': return 'CPA refused the remembered management key, so nothing was ' + act.done + '.' + act.kept + ' Sign in to CPA’s management console again on this address with “Remember password” ticked, then ' + act.again + '.' + COUNTED;
      default: return 'Something in front of CPA refused the ' + act.noun + (record && record.status ? ' (HTTP ' + record.status + ')' : '') + ', so nothing was ' + act.done + '.' + act.kept + COUNTED;
    }
  }
  // What a remembered refusal does to Save: {block} keeps it off whatever the
  // fields say, {note} leaves it on and says what pressing it does; null for
  // no refusal. active is app.js's banned, whether a lockout still runs. A key
  // CPA itself refused is never sent again by a save, so Save stays off until
  // the console holds another; so it does while a lockout runs. Any other refusal
  // was not about the key (a lockout that is over, a proxy in front of CPA),
  // and pressing Save is then the one deliberate try that clears it, as
  // Refresh view is for the status, which is disabled while a card is open.
  function refusalHold(record, active) {
    if (!record) return null;
    if (record.reason === 'refused') return {block: refusedText(record) + ' Save stays off until the console holds a different key. Only Refresh view, once no card is open, sends this key again.'};
    if (record.reason === 'banned' && active(record)) return {block: refusedText(record) + ' Save turns back on then.'};
    return {note: refusedText(record, record.reason === 'banned') + ' Select Save to try once more.'};
  }
  const isObject = value => value !== null && typeof value === 'object' && !Array.isArray(value);
  const strings = value => (Array.isArray(value) ? value.filter(item => typeof item === 'string') : []);
  // What a save, or an import (action 'import'), came to, from what send()
  // got back: {kind: 'blocked', reason: 'session'} or {kind: 'blocked',
  // reason: 'refused', record} when nothing was sent; {kind: 'lost'} when no
  // answer came; {kind: 'refused', record} for a 401 or 403; else {kind:
  // 'answer', status, body, retryAfter}.
  function outcomeOf(answer, action) {
    const failed = (code, status, extra) => Object.assign({kind: 'failed', code, text: saveErrorText(code, status, answer.retryAfter, action), signIn: false}, extra || {});
    if (answer.kind === 'blocked') return answer.reason === 'session' ? Object.assign(failed('no_session', 0), {signIn: true}) : {kind: 'failed', code: 'refused', text: refusedText(answer.record, false, action), signIn: true};
    if (answer.kind === 'lost') return {kind: 'lost', text: LOST};
    if (answer.kind === 'refused') return {kind: 'failed', code: 'refused', text: refusedText(answer.record, false, action), signIn: true};
    const body = isObject(answer.body) ? answer.body : null;
    if (answer.status === 200) {
      // The plugin answers 200 only with a body; an unreadable one is that
      // answer lost on the way back.
      if (body === null || body.ok !== true) return {kind: 'lost', text: LOST};
      return {
        kind: 'saved', unchanged: body.unchanged === true, revision: typeof body.revision === 'string' ? body.revision : '',
        settings: isObject(body.settings) ? body.settings : {}, organizations: Array.isArray(body.organizations) ? body.organizations : null, counts: isObject(body.counts) ? body.counts : null,
      };
    }
    const code = body !== null && typeof body.error === 'string' ? body.error : '';
    if (answer.status === 404) return failed('route_missing', 404);
    // Not the plugin's answer: a 4xx is something in front of it turning the
    // request away; anything else is an answer lost.
    if (!PLUGIN_CODES.has(code)) return answer.status >= 400 && answer.status < 500 ? failed('', answer.status) : {kind: 'lost', text: LOST};
    if (code === 'conflict') {
      return {kind: 'conflict', ids: strings(body.conflicts), current: isObject(body.current) ? body.current : {}, revision: typeof body.revision === 'string' ? body.revision : '',
        organizations: Array.isArray(body.organizations) ? body.organizations : null, text: CONFLICT_FOOT};
    }
    if (FIELD_CODES.has(code) && typeof body.id === 'string' && typeof body.field === 'string') return {kind: 'field', code, id: body.id, field: body.field, text: fieldMessage(code)};
    if (OPERATION_CODES.has(code) && Number.isInteger(body.op)) return {kind: 'operation', code, op: body.op, field: typeof body.field === 'string' ? body.field : '', text: fieldMessage(code)};
    if (code === 'not_editable') return failed(code, answer.status, {ids: strings(body.ids).length ? strings(body.ids) : strings([body.id])});
    return failed(code, answer.status);
  }

  // ---------------------------------------------------------------------------
  // The import from Quota Glance

  const plural = (count, one, many) => count + ' ' + (count === 1 ? one : many);
  // "2 monthly credits, 2 refill dates, 2 Console readings and 3 renewal
  // dates", zero items left out; "" when every count is zero.
  function countsText(counts) {
    if (!isObject(counts)) return '';
    const parts = [
      [counts.monthly_credits, 'monthly credit', 'monthly credits'], [counts.refill_dates, 'refill date', 'refill dates'],
      [counts.readings, 'Console reading', 'Console readings'], [counts.renewal_dates, 'renewal date', 'renewal dates'],
    ].filter(([count]) => Number.isInteger(count) && count > 0).map(([count, one, many]) => plural(count, one, many));
    if (parts.length <= 1) return parts.join('');
    return parts.slice(0, -1).join(', ') + ' and ' + parts[parts.length - 1];
  }
  const IMPORT_NOTE_DAYS = 30;
  const epochOf = value => { const t = Date.parse(value); return Number.isFinite(t) ? Math.floor(t / 1000) : null; };
  // An import from the drop-in, the copy of Quota Glance's settings.json
  // placed beside the snapshot (client.LegacySettingsDropIn). Quota Glance
  // never saves to it, so its later saves are never picked up from it.
  const LEGACY_DROP_IN = 'quota-glance-settings.json';
  const fromDropIn = legacy => isObject(legacy) && typeof legacy.source_path === 'string' && legacy.source_path.split(/[\\/]/).pop() === LEGACY_DROP_IN;
  // Whether this page still picks up what Quota Glance saves in its own file.
  // The plugin never says so of a drop-in, and the page holds to that too.
  const followsQuotaGlance = legacy => isObject(legacy) && legacy.following === true && !fromDropIn(legacy);
  // The import note: {tone, pill, lines, button, dismiss} for the legacy
  // block, or null when nothing is to be said. dismissed(key) says whether
  // the reader dismissed a note; available is whether the settings can be
  // edited, without which nothing can be imported, and reason the editor's
  // reason when they cannot. An import brought only what Quota Glance's file
  // held, which Quota Glance 0.8.0 no longer writes. So the note says nothing
  // needs entering again only when the settings file holds that file's
  // values with nothing saved here since (matched or follow). One pressed on
  // the page replaced what was saved here. An automatic import is a first
  // start, but it is also how a store is filled again after its settings file
  // and import record were both lost, which the store cannot tell apart. A
  // follow or matched record that continues either keeps its kind, so its
  // line stays.
  function importNote(legacy, available, reason, now, dismissed) {
    if (!isObject(legacy)) return null;
    const following = 'Quota Glance older than 0.8.0 still saves these values in its own file, and this page picks up its changes until you save here. Update Quota Glance first.';
    const copied = 'These values came from a copy of Quota Glance’s settings.json beside Quota Cache’s snapshot, so changes saved later in Quota Glance older than 0.8.0 are not picked up. Update Quota Glance, or copy the file again.';
    const onlyHere = 'A value saved only on this page is not among these, since Quota Glance’s settings.json never had it: enter it again.';
    const savedBefore = 'A value saved on this page before this import, if any, is not among these: enter it again.';
    const button = label => (available && legacy.importable === true ? label : null);
    switch (legacy.state) {
      case 'imported': {
        const at = epochOf(legacy.imported_at);
        const key = 'import:' + (legacy.imported_at || '');
        if (at === null || now - at > IMPORT_NOTE_DAYS * 86400 || dismissed(key)) return null;
        const counts = countsText(legacy.counts);
        const changed = epochOf(legacy.legacy_updated_at);
        const complete = legacy.kind === 'matched' || legacy.kind === 'follow';
        const lines = ['Imported from Quota Glance on ' + localDay(at) + (counts ? ': ' + counts : '') + (changed !== null ? ', last changed there ' + localDay(changed) : '') + '.' + (complete ? ' Nothing needs entering again.' : '')];
        if (legacy.kind === 'manual') lines.push(onlyHere);
        else if (legacy.kind === 'automatic') lines.push(savedBefore);
        if (fromDropIn(legacy)) lines.push(copied);
        else if (followsQuotaGlance(legacy)) lines.push(following);
        return {tone: 'ok', pill: 'Imported', lines, button: null, dismiss: key};
      }
      case 'changedSinceImport':
        if (fromDropIn(legacy)) return {tone: 'warn', pill: 'Copy changed', lines: ['The copy of Quota Glance’s settings.json beside Quota Cache’s snapshot changed after your last save here.'], button: button('Import Quota Glance’s settings again'), dismiss: null};
        return {tone: 'warn', pill: 'Changed in Quota Glance', lines: ['Quota Glance’s settings.json changed after your last save here. Quota Glance older than 0.8.0 still edits it; update Quota Glance.'], button: button('Import Quota Glance’s settings again'), dismiss: null};
      case 'notImported':
        return {tone: 'warn', pill: 'Not imported', lines: ['Quota Glance’s settings.json has values that were not imported here.'], button: button('Import Quota Glance’s settings'), dismiss: null};
      case 'settingsMissing':
        // The store says so whether or not Quota Glance's file is there, and
        // only one that can be read can be imported.
        if (legacy.importable !== true) return {tone: 'warn', pill: 'Saved settings gone', lines: ['This page’s saved settings are gone, so only config values apply until you enter values again. There is no Quota Glance settings.json to import.'], button: null, dismiss: null};
        return {tone: 'warn', pill: 'Saved settings gone', lines: ['This page’s saved settings are gone, so only config values apply until you enter values or import.'], button: button('Import Quota Glance’s settings again'), dismiss: null};
      case 'legacyUnreadable':
        // While this file is what blocks editing, with nothing stored, the
        // cards say this themselves, as the reason they cannot be edited.
        // While the settings file is what cannot be read, the cards offer
        // the import once it is moved aside, which this file rules out.
        if (!available && reason === 'legacyUnreadable') return null;
        return {tone: 'warn', pill: 'Cannot read Quota Glance’s file', lines: ['Quota Glance’s settings.json could not be read, so it cannot be imported again. Fix it or move it aside.'], button: null, dismiss: null};
      default:
        return null;
    }
  }
  // The saved values of organizations no longer listed: api_credits' orphans,
  // or, while no organization is listed and api_credits is null, the
  // editor's.
  function creditOrphansOf(doc, editor) {
    if (isObject(doc)) return Array.isArray(doc.orphans) ? doc.orphans : [];
    return isObject(editor) && Array.isArray(editor.api_credit_orphans) ? editor.api_credit_orphans : [];
  }
  // The saved values of organizations the configuration does not list while
  // the organizations file cannot be read: no orphans, since they may belong
  // to organizations in the file, so they are shown and never offered to
  // clear.
  const heldCreditsOf = editor => (isObject(editor) && Array.isArray(editor.held_api_credits) ? editor.held_api_credits : []);
  // What an import would replace: the counts of what is saved here now.
  function storedCounts(state, editor) {
    const counts = {monthly_credits: 0, refill_dates: 0, readings: 0, renewal_dates: 0};
    const credits = isObject(state) && isObject(state.api_credits) ? state.api_credits : null;
    for (const account of credits && Array.isArray(credits.accounts) ? credits.accounts : []) {
      const s = account.settings || {};
      if (s.monthlyUsd) counts.monthly_credits++;
      if (s.renews) counts.refill_dates++;
      if (s.reading) counts.readings++;
    }
    for (const orphan of [...creditOrphansOf(credits, editor), ...heldCreditsOf(editor)]) {
      if (orphan.monthlyUsd) counts.monthly_credits++;
      if (orphan.renews) counts.refill_dates++;
      if (orphan.hasReading) counts.readings++;
    }
    if (isObject(state) && isObject(state.renewals)) counts.renewal_dates = Object.keys(state.renewals).length;
    else if (isObject(editor)) counts.renewal_dates = (editor.credentials || []).filter(c => c.renewal).length + (editor.renewal_orphans || []).length;
    return counts;
  }
  // The import's confirmation: what is saved here now, which the import
  // replaces, and what Quota Glance's file holds now, which it brings
  // (editor.legacy.file_counts; left unsaid when the status does not give it),
  // with when it last changed there (editor.legacy.file_updated_at): nothing
  // saved on this page since is in it, which matters most when the import
  // refills a store whose saved settings were lost.
  function importQuestion(stored, fromFile, fileUpdatedAt) {
    const saved = countsText(stored);
    const lines = [saved ? 'This replaces what is saved here: ' + saved + '. Values entered here since the last import are lost.' : 'Nothing is saved here now, so nothing is replaced.'];
    if (isObject(fromFile)) {
      const brought = countsText(fromFile);
      lines.push(brought ? 'It brings what Quota Glance’s settings.json holds now: ' + brought + '.' : 'Quota Glance’s settings.json holds no values' + (saved ? ', so nothing stays saved here.' : ' either.'));
      const changed = typeof fileUpdatedAt === 'string' ? epochOf(fileUpdatedAt) : null;
      if (brought && changed !== null) lines.push('It was last changed there ' + localDay(changed) + ', so nothing saved on this page since is in it.');
    }
    lines.push('Import Quota Glance’s settings.json now?');
    return lines.join(' ');
  }
  // What to do about a settings file Quota Cache cannot read, named file,
  // given the import block (editor.legacy). A good copy put back keeps what
  // was saved here, so it comes first. Moved aside, the file starts empty
  // only once an import is on record (its kind), and importing again brings
  // back only what Quota Glance's file holds; before that the next tick is a
  // first start, which imports Quota Glance's file or, when that cannot be
  // read, keeps editing off. A record that cannot be read is published with
  // no kind but still counts, so that text allows for one.
  function settingsUnreadableText(file, legacy) {
    const lead = 'Quota Cache cannot read its saved settings, so only config values apply. ';
    if (isObject(legacy) && typeof legacy.kind === 'string') {
      return lead + 'To keep the values saved here, put back a good copy of ' + file + '. Otherwise move it aside: Quota Cache then starts empty, and importing Quota Glance’s settings again brings back only what Quota Glance’s settings.json holds, without the values saved only on this page.';
    }
    return lead + 'To keep the values saved here, put back a good copy of ' + file + '. Moving it aside instead starts Quota Cache empty once an import from Quota Glance is on record, and otherwise counts as a first start: ' +
      'Quota Glance’s settings.json is imported if there is one, and one that cannot be read keeps editing off until it is fixed.';
  }
  // The editor's reason while the import record, named file, is there but
  // cannot be opened or read, as one restored as another user. Nothing may be
  // saved or imported until it can, since a save could not tell whether the
  // file it writes holds the import, but the saved values still apply. A
  // later read is enough, so the remedy is the file's owner or permissions.
  const IMPORT_RECORD_UNREADABLE = 'importRecordUnreadable';
  function importRecordUnreadableText(file) {
    return 'Quota Cache cannot open or read its import record, ' + file + ', so nothing can be saved or imported here until it can. The values saved here still apply. ' +
      'Fix the file’s owner or permissions; the page picks that up within a few seconds.';
  }

  // The rules above, for scripts/tests/editor.test.mjs.
  const logic = Object.freeze({
    readMoney, moneyText, sameAmount, readDate, dateEpoch, utcDay, cycleOn, cycleText, refillRule, ordinal, localInstant, localInputValue, rfc3339, utcClock,
    READING_MAX_AGE, READING_MAX_AHEAD, AMOUNT_MESSAGE, READING_AMOUNT_MESSAGE, READING_TIME_MESSAGE, READING_TIME_REFUSED, creditField, refillField, renewalField, readingField,
    creditRow, emptyDraft, editCredit, markRemoved, creditPlan, reconcileCredits, reconcileRenewals, editRenewal, renewalPlan, dropConflicts, dropRefused, refusedRowsText,
    liveOrphans, orphanAfterConflict, newerRevision,
    overlaid, conflictFields, normalizeOrganizationID, validOrganizationLabel, emptyOrganizationsDraft, organizationsPlan, organizationsAfterConflict, organizationsHeld, organizationsConflictText, removeQuestion,
    organizationRows, shortOrganizationID, shortCredentialID, creditsEditLabel, noneCountedText, FIELD_CODES, OPERATION_CODES, PLUGIN_CODES, FIELD_FOOT, CONFLICT_FOOT, CONFLICT_FIELD, SIGN_IN_FOOT, LOST,
    fieldMessage, saveErrorText, refusedText, refusalHold, retryText, outcomeOf, countsText, fromDropIn, followsQuotaGlance, importNote, creditOrphansOf, heldCreditsOf, storedCounts, importQuestion,
    settingsUnreadableText, IMPORT_RECORD_UNREADABLE, importRecordUnreadableText,
  });

  // ---------------------------------------------------------------------------
  // The page

  if (!page || typeof document === 'undefined') {
    if (page && typeof page.attach === 'function') page.attach(Object.freeze({render() {}, editing: () => false, logic}));
    return;
  }

  const SETTINGS_PATH = '/v0/management/plugins/quota-cache/settings';
  const DISMISSED_NAME = 'quota-cache.dismissed';
  const LIMITS = {maxApiCreditItems: 16, maxRenewalItems: 32, maxBytes: 4096, maxOrganizations: 16, maxOrganizationOps: 16};
  const SAVED_FOR = 20000;
  const $ = id => document.getElementById(id);
  // An element of a form not yet in the page, by id.
  const within = (root, id) => root.querySelector('#' + CSS.escape(id));
  const nowSeconds = () => Math.floor(Date.now() / 1000);

  // A node with attributes and children. Attribute values are set as
  // attributes or, for value, checked, disabled and hidden, as properties;
  // listeners go in on. Text is always a text node.
  function h(tag, attributes, ...children) {
    const element = document.createElement(tag);
    for (const [name, value] of Object.entries(attributes || {})) {
      if (value === undefined || value === null || value === false) continue;
      if (name === 'on') { for (const [type, listener] of Object.entries(value)) element.addEventListener(type, listener); continue; }
      if (name === 'value' || name === 'checked' || name === 'disabled' || name === 'hidden') element[name] = value;
      else element.setAttribute(name, value === true ? '' : String(value));
    }
    for (const child of children.flat(Infinity)) {
      if (child === null || child === undefined || child === false || child === '') continue;
      element.append(typeof child === 'string' ? document.createTextNode(child) : child);
    }
    return element;
  }
  const sep = () => h('span', {class: 'sep', 'aria-hidden': 'true'}, '·');
  const setMark = () => h('span', {class: 'mark', 'aria-hidden': 'true'});
  const secondary = (...children) => h('span', {class: 'secondary'}, ...children);
  const muted = text => h('span', {class: 'muted'}, text);
  function pencil() {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'ico'); svg.setAttribute('viewBox', '0 0 12 12'); svg.setAttribute('fill', 'none'); svg.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', 'M8.4 1.6 10.4 3.6 4.2 9.8 1.6 10.4 2.2 7.8Z'); path.setAttribute('stroke', 'currentColor'); path.setAttribute('stroke-width', '1.2'); path.setAttribute('stroke-linejoin', 'round');
    svg.append(path);
    return svg;
  }
  function tick() {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 12 12'); svg.setAttribute('fill', 'none'); svg.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', 'M2.4 6.4 4.9 8.8 9.6 3.4'); path.setAttribute('stroke', 'currentColor'); path.setAttribute('stroke-width', '1.6'); path.setAttribute('stroke-linecap', 'round'); path.setAttribute('stroke-linejoin', 'round');
    svg.append(path);
    return svg;
  }
  const link = (text, onClick, attributes) => h('button', Object.assign({type: 'button', class: 'link', on: {click: onClick}}, attributes || {}), text);
  const editButton = (text, onClick, primary, disabled, id) => h('button', {id, type: 'button', class: primary ? null : 'secondary', disabled, on: {click: onClick}}, primary ? null : pencil(), text);
  // Dismiss for the note remembered under key, which the link carries, so a
  // link kept from an earlier render (settle) always dismisses what it shows.
  const dismissLink = (key, id) => link('Dismiss', event => dismiss(key, event.currentTarget), {id, 'data-dismiss': key});

  // Dismissed notes, remembered per value and per CPA, in this origin's
  // storage or, when that is full, this tab's, like the refusal record.
  function dismissedKeys() {
    for (const store of page.stores()) {
      try {
        const raw = store.getItem(DISMISSED_NAME);
        if (raw === null || raw.length > 32768) continue;
        const record = JSON.parse(raw);
        if (isObject(record) && record.v === 1 && record.root === page.root && Array.isArray(record.keys)) return record.keys.filter(key => typeof key === 'string');
      } catch {}
    }
    return [];
  }
  const dismissed = key => dismissedKeys().includes(key);
  // The note goes, and its Dismiss with it, so focus moves on to the control
  // that followed the note, as Tab from it would have, found again by its id
  // when the render rebuilt it.
  function dismiss(key, from) {
    const keys = dismissedKeys().filter(one => one !== key).concat([key]).slice(-32);
    for (const store of page.stores()) {
      try { store.setItem(DISMISSED_NAME, JSON.stringify({v: 1, root: page.root, keys})); break; } catch {}
    }
    const note = from && document.activeElement === from ? from.closest('.alert') || from : null;
    const after = note ? controlsAfter(note) : [];
    renderAtRest();
    if (!note || (document.activeElement !== null && document.activeElement !== document.body)) return;
    focusOnward(after);
  }
  // Focus on the first of controls that can take it, each found again by its
  // id when a render rebuilt it.
  function focusOnward(controls) {
    for (const control of controls) {
      const element = control.isConnected ? control : control.id ? $(control.id) : null;
      if (element && !element.disabled && element.getClientRects().length > 0) { element.focus(); return; }
    }
  }

  // What the page last read, and each card's own state while it is edited.
  // A card's section is its element's id, key prefixes the ids of its foot's
  // controls, and rest lists where focus goes once it is at rest again: its
  // Edit button, else the first other control it has then.
  let status = null;
  let scrolled = false;
  const card = (section, key, rest) => ({section, key, rest, editing: false, saving: false, failed: false, status: null, saved: null, savedTimer: 0, marks: {}, confirm: null});
  const orgs = Object.assign(card('organizations', 'qc-orgs', ['qc-orgs-edit', 'qc-orgs-add', 'qc-orgs-table']), {draft: emptyOrganizationsDraft(), base: null, confirmRemove: null});
  // gone and revised are what saves learned of the orphans and rows while the
  // card is open (liveOrphans).
  const credits = Object.assign(card('api-credits', 'qc-credits', ['qc-credits-edit', 'qc-credits-table']), {draft: emptyDraft(), overlay: {}, focused: null, gone: new Set(), revised: {}});
  const renewals = Object.assign(card('renewals', 'qc-renewals', ['qc-renewals-edit', 'qc-renewals-table']), {draft: emptyDraft(), overlay: {}, gone: new Set(), revised: {}});
  const importer = {confirming: false, busy: false, status: null, timer: 0};
  const cards = [orgs, credits, renewals];
  const editing = () => cards.some(one => one.editing);

  const creditState = () => (status && isObject(status.credit_state) ? status.credit_state : null);
  const creditDocument = () => { const state = creditState(); return state && isObject(state.api_credits) ? state.api_credits : null; };
  const editorInfo = () => (status && isObject(status.editor) ? status.editor : null);
  const accountsOf = doc => (doc && Array.isArray(doc.accounts) ? doc.accounts : []);
  const creditOrphans = () => creditOrphansOf(creditDocument(), editorInfo());
  const credentialsOf = editor => (editor && Array.isArray(editor.credentials) ? editor.credentials : []);
  const renewalOrphansOf = editor => (editor && Array.isArray(editor.renewal_orphans) ? editor.renewal_orphans : []);
  // The organizations the status lists, overlaid while the card is open by
  // orgs.base, the list it saves against when that is not the status's: the
  // plugin's after a conflict, or the one its renames and removals were made
  // against (organizationsHeld).
  function organizationsInfo() {
    const editor = editorInfo();
    const info = editor && isObject(editor.organizations) ? editor.organizations : {available: false, reason: '', revision: '', max_items: 16, max_ops: 16, items: []};
    return orgs.base ? Object.assign({}, info, orgs.base) : info;
  }
  const limits = editor => ({
    maxApiCreditItems: editor && Number.isInteger(editor.max_api_credit_items) ? editor.max_api_credit_items : LIMITS.maxApiCreditItems,
    maxRenewalItems: editor && Number.isInteger(editor.max_renewal_items) ? editor.max_renewal_items : LIMITS.maxRenewalItems,
    maxBytes: editor && Number.isInteger(editor.max_batch_bytes) ? editor.max_batch_bytes : LIMITS.maxBytes,
  });
  // The file beside the snapshot, named as Quota Cache names it.
  function besideSnapshot(suffix) {
    const path = status && typeof status.cache_path === 'string' ? status.cache_path : '';
    const base = path.split(/[\\/]/).pop() || 'snapshot.json';
    return base.replace(/\.json$/, '') + suffix;
  }
  const LEGACY_UNREADABLE = 'Quota Glance’s settings.json could not be read, so nothing was imported and only config values apply. Fix it or move it aside.';
  // Moving the organizations file aside starts the list over: from the
  // configuration when it lists organizations, else empty. Either drops what
  // was changed here, so fixing the file comes first.
  const organizationsUnreadableText = listed => 'Quota Cache cannot read its organizations file, so the list from its configuration applies and editing it here is unavailable. Fix ' +
    besideSnapshot('.organizations.json') + ' in place, or put back a good copy; the page picks that up within a few seconds. ' + (listed
    ? 'Moving it aside instead moves the configuration’s list in again and drops the organization changes made here: organizations added here leave the list, renames go back to the configuration’s labels, and removed organizations return.'
    : 'Moving it aside instead starts an empty list, and every organization in the file has to be added again.');
  const organizationsUnreadable = () => organizationsInfo().reason === 'organizationsUnreadable';
  function unavailableText(editor) {
    if (!editor || editor.available) return null;
    if (editor.reason === IMPORT_RECORD_UNREADABLE) return importRecordUnreadableText(besideSnapshot('.settings-import.json'));
    return editor.reason === 'legacyUnreadable' ? LEGACY_UNREADABLE : settingsUnreadableText(besideSnapshot('.settings.json'), editor.legacy);
  }
  // What the console's session means for Save (refusalHold): {block},
  // {note}, or null. A lockout that ends while a card is open turns Save back
  // on by itself.
  let releaseTimer = 0;
  function sessionState() {
    const session = page.readSession();
    if (!page.root || !session) return {block: SIGN_IN_FOOT};
    const record = page.refusal(session);
    clearTimeout(releaseTimer);
    if (page.banned(record)) releaseTimer = setTimeout(() => resync(), record.until * 1000 - Date.now() + 1000);
    return refusalHold(record, page.banned);
  }

  // ---------------------------------------------------------------------------
  // Sending

  // One save: a POST of one batch with the console's remembered key, from a
  // button press only and never repeated by the page itself.
  async function send(body) {
    const session = page.readSession();
    if (!page.root || !session) return {kind: 'blocked', reason: 'session'};
    const held = page.refusal(session);
    const hold = refusalHold(held, page.banned);
    if (hold && hold.block) return {kind: 'blocked', reason: 'refused', record: held};
    // A refusal that was not about the key: this press is the one deliberate
    // try that clears it.
    if (held) page.forget();
    let response;
    try {
      response = await fetch(page.root + SETTINGS_PATH, {method: 'POST', headers: {Authorization: ['Bearer', session.key].join(' '), 'Content-Type': 'application/json', Accept: 'application/json'}, body: JSON.stringify(body), mode: 'same-origin', credentials: 'same-origin', cache: 'no-store', redirect: 'error', signal: AbortSignal.timeout(15000)});
    } catch {
      page.unansweredSave();
      return {kind: 'lost'};
    }
    if (response.status === 401 || response.status === 403) {
      // Remember the refusal before anything else runs, then refine its
      // reason from CPA's message, as the status request does.
      let record = page.remember(session, response.status, null);
      let error = null;
      try { const answer = await response.json(); if (isObject(answer) && typeof answer.error === 'string' && answer.error.length <= 512) error = answer.error; } catch {}
      record = page.remember(session, response.status, error);
      page.refusedSave(record);
      return {kind: 'refused', record};
    }
    let answer = null;
    try { answer = page.unescapeAll(await response.json()); } catch {}
    return {kind: 'answer', status: response.status, body: answer, retryAfter: response.headers.get('Retry-After')};
  }

  // ---------------------------------------------------------------------------
  // Shared parts of a card

  // Starts or ends editing a card; the status timer and Refresh view follow.
  function setEditing(target, on) {
    target.editing = on;
    target.status = null;
    target.failed = false;
    target.marks = {};
    target.confirm = null;
    target.gone = new Set();
    target.revised = {};
    if (on) { target.saved = null; clearTimeout(target.savedTimer); }
    page.editingChanged();
  }
  function showSaved(target, text) {
    target.saved = text;
    clearTimeout(target.savedTimer);
    target.savedTimer = setTimeout(() => { target.saved = null; renderAtRest(); }, SAVED_FOR);
  }
  function countText(changes, errors) {
    if (errors > 0) return errors + ' to fix';
    if (changes === 0) return 'no changes';
    return plural(changes, 'change', 'changes');
  }
  // The pinned foot of a card being edited: what its fields mean, then the
  // change count, Cancel and Save; the reason a save failed, or cannot be
  // pressed, or what the next press does after a refusal (note), sits above
  // them. It is built at each full render of the card, which keeps its live
  // regions' nodes (replaceEdit); a keystroke brings it up to date in place
  // (fillFoot). Save submits the card's form, which is
  // novalidate: the page judges every field itself and says why under it, so
  // the browser's own checks, such as a time's bounds or a length counted in
  // UTF-16 units, never stop a Save the page offers.
  function editFoot(target, help, plan, blocked, onCancel, note) {
    const foot = h('div', {class: 'edit-foot'},
      target.confirm ? h('div', {class: 'alert warn confirm', role: 'group', 'aria-labelledby': target.key + '-confirm-question'},
        h('p', {id: target.key + '-confirm-question'}, target.confirm.text),
        h('div', {class: 'edit-acts'},
          h('button', {id: target.key + '-keep-editing', type: 'button', class: 'secondary', on: {click: () => { target.confirm = null; renderAll(); focusFirst(target.key + '-save'); }}}, 'Keep editing'),
          h('button', {id: target.key + '-confirm', type: 'button', on: {click: target.confirm.go}}, target.confirm.label))) : null,
      h('p', {class: 'edit-help'}, help),
      h('div', {class: 'edit-acts'},
        h('span', {class: 'count', 'aria-live': 'polite'}),
        h('button', {id: target.key + '-cancel', type: 'button', class: 'secondary', disabled: target.saving, on: {click: onCancel}}, 'Cancel'),
        h('button', {id: target.key + '-save', type: 'submit', class: 'save'})));
    return fillFoot(foot, target, plan, blocked, note);
  }
  // Brings a foot up to date. The alert and the count are live regions, so
  // each keeps its node and changes its text only when the text changes: a
  // screen reader then announces each change once, not at every keystroke.
  // The alert is added when there is something to say and removed when
  // there is not.
  function fillFoot(foot, target, plan, blocked, note) {
    const off = target.saving || blocked !== null || plan.errors > 0 || plan.changes === 0;
    const notice = blocked || note || (target.status && target.status.text) || '';
    const tone = blocked ? 'error' : note ? 'warn' : target.status ? (target.status.tone === 'bad' ? 'error' : 'warn') : '';
    let shown = foot.querySelector(':scope > .notice');
    if (notice && !shown) { shown = h('div', {id: target.key + '-notice', class: 'notice', role: 'alert', tabindex: '-1'}); foot.prepend(shown); }
    if (!notice && shown) shown.remove();
    if (notice) {
      shown.className = 'notice alert ' + tone;
      if (shown.textContent !== notice) shown.textContent = notice;
    }
    const count = foot.querySelector('.count');
    const counted = countText(plan.changes, plan.errors);
    count.className = 'count' + (plan.errors > 0 ? ' bad' : '');
    if (count.textContent !== counted) count.textContent = counted;
    // While a save is under way Save cannot be pressed again (the save
    // itself checks), but it stays focusable, so the press that started the
    // save keeps focus until its answer moves it (renderAndRefocus).
    const save = foot.querySelector('button.save');
    save.className = 'save' + (off && !target.saving ? ' idle' : '');
    save.disabled = off && !target.saving;
    if (target.saving) save.setAttribute('aria-disabled', 'true');
    else save.removeAttribute('aria-disabled');
    const label = target.saving ? 'Saving…' : target.failed && plan.changes > 0 ? 'Try again' : 'Save';
    if (save.textContent !== label) save.replaceChildren(...(target.saving ? [h('span', {class: 'spin', 'aria-hidden': 'true'}), label] : [label]));
    return foot;
  }
  // A full render of a card being edited, as each press on it makes, builds
  // the card afresh, foot and all. The foot's alert and count still keep
  // their nodes, as fillFoot keeps them on a keystroke: an alert taken out of
  // the page and put back is announced again, though it says the same. So
  // the form, its foot, the foot's row of actions, the alert and the count
  // never leave the page: each takes the attributes of its fresh counterpart,
  // the alert and the count its text where that changed, and everything else
  // in them is the fresh render's. An alert with nothing to say goes, and
  // one where there was none is added.
  function replaceEdit(section, nodes) {
    const form = section.querySelector(':scope > form');
    const foot = form && form.querySelector(':scope > .edit-foot');
    const acts = foot && foot.querySelector(':scope > .edit-acts');
    const next = nodes.find(node => node instanceof HTMLFormElement);
    const nextFoot = next && next.querySelector(':scope > .edit-foot');
    const nextActs = nextFoot && nextFoot.querySelector(':scope > .edit-acts');
    if (!acts || !nextActs) { section.replaceChildren(...nodes); watchFeet(); return; }
    const live = (parent, fresh, selector) => {
      const kept = parent.querySelector(selector);
      const now = fresh.querySelector(selector);
      if (!kept || !now) return [];
      sameAttributes(kept, now);
      if (kept.textContent !== now.textContent) kept.textContent = now.textContent;
      return [[kept, now]];
    };
    placeAround(acts, [...nextActs.childNodes], live(acts, nextActs, ':scope > .count'));
    placeAround(foot, [...nextFoot.childNodes], [...live(foot, nextFoot, ':scope > .notice'), [acts, nextActs]]);
    placeAround(form, [...next.childNodes], [[foot, nextFoot]]);
    placeAround(section, nodes, [[form, next]]);
    for (const [kept, now] of [[acts, nextActs], [foot, nextFoot], [form, next]]) sameAttributes(kept, now);
    watchFeet();
  }
  // The pinned foot covers the bottom of the window, where the browser
  // scrolls a control that focus moves to, so every control of a card being
  // edited keeps --edit-foot clear below it (style.css), and the foot's own
  // are always in view (WCAG 2.4.11). --edit-foot is the tallest foot's
  // height, set on the page's root, whose attributes no render touches. A
  // foot's height changes as it wraps and as its alert comes and goes, so
  // each foot in the page is watched. One taken out of the page stops
  // counting at once, as its size drops to nothing, and stops being watched
  // when a card is next drawn for editing.
  const feet = typeof ResizeObserver === 'function' ? new ResizeObserver(() => {
    let tallest = 0;
    for (const foot of document.querySelectorAll('form > .edit-foot')) tallest = Math.max(tallest, foot.offsetHeight);
    document.documentElement.style.setProperty('--edit-foot', tallest + 'px');
  }) : null;
  function watchFeet() {
    if (!feet) return;
    feet.disconnect();
    for (const foot of document.querySelectorAll('form > .edit-foot')) feet.observe(foot);
  }
  // Gives parent the nodes of children, in order, except that a node kept
  // pairs with one of them stays where it is in the page, standing for it.
  function placeAround(parent, children, kept) {
    const stays = new Map(kept.map(([node, child]) => [child, node]));
    const staying = new Set(stays.values());
    for (const child of [...parent.childNodes]) if (!staying.has(child)) child.remove();
    let last = null;
    for (const child of children) {
      if (stays.has(child)) { last = stays.get(child); continue; }
      if (last) last.after(child); else parent.prepend(child);
      last = child;
    }
  }
  function sameAttributes(element, like) {
    for (const {name} of [...element.attributes]) if (!like.hasAttribute(name)) element.removeAttribute(name);
    for (const {name, value} of [...like.attributes]) if (element.getAttribute(name) !== value) element.setAttribute(name, value);
  }
  function editHead(id, title, target, unsaved) {
    return h('div', {class: 'section-head'},
      h('h2', {id}, title),
      h('span', {class: 'pill' + (target.failed ? ' error' : '')}, target.saving ? 'Saving' : target.failed ? 'Not saved' : 'Editing'),
      unsaved > 0 ? h('span', {class: 'hint'}, unsaved + ' unsaved') : null);
  }
  function savedLine(text) {
    return h('span', {class: 'saved', role: 'status'}, tick(), text);
  }
  // A full render keeps the field the reader was in, by its id.
  function keepFocus(render) {
    const active = document.activeElement;
    const id = active && active.id;
    let selection = null;
    try { if (active && typeof active.selectionStart === 'number') selection = [active.selectionStart, active.selectionEnd]; } catch {}
    render();
    const next = id ? $(id) : null;
    if (next && next !== active) {
      next.focus();
      try { if (selection) next.setSelectionRange(selection[0], selection[1]); } catch {}
    }
  }
  // Brings a card at rest, the import note, or a caption under a field of a
  // card being edited up to date from nodes built afresh, keeping each child
  // that is unchanged: a live region keeps its node, so a screen reader
  // announces an alert once and not at every view update, and a control the
  // reader is on, or is pressing, stays where it was. A changed child is
  // replaced, and focus follows its control by id (keepFocus). Equal nodes
  // may differ only in their listeners, so every listener at rest acts on
  // what its node shows: a key in a data- attribute, or the status read again
  // when pressed. A caption's controls act on the draft as it is when
  // pressed, and on the status the card was last drawn from, which a full
  // render draws again with captions of its own.
  function settle(parent, nodes) {
    keepFocus(() => {
      nodes.forEach((node, i) => {
        const current = parent.childNodes[i];
        if (!current) parent.append(node);
        else if (!current.isEqualNode(node)) parent.replaceChild(node, current);
      });
      while (parent.childNodes.length > nodes.length) parent.lastChild.remove();
    });
  }
  function focusLater(id) {
    const element = $(id);
    if (element) { element.focus(); element.scrollIntoView({block: 'center'}); }
  }
  // Focus on the first of ids that is in the page and can take it. A full
  // render replaces every control of a card being edited (replaceEdit), so
  // each press that changes what the card shows says where focus goes next;
  // keepFocus alone finds only a control that is still there under its id.
  function focusFirst(...ids) {
    for (const id of ids) {
      const element = $(id);
      if (element && !element.disabled && element.getClientRects().length > 0) { element.focus(); return; }
    }
  }
  // The first field of the form in section that can take focus, else its
  // first such button, where focus goes when the card opens: a card may have
  // nothing to type in, as the renewal card whose only row is a saved date
  // for an account no longer in CPA, and the button that opened it is gone.
  function firstControl(section) {
    const form = $(section) && $(section).querySelector('form');
    return form && (form.querySelector('input:not(:disabled)') || form.querySelector('button:not(:disabled)'));
  }
  // Every control after element in the page's order, outside it.
  function controlsAfter(element) {
    return [...document.querySelectorAll('button, input, select, textarea, a[href], [tabindex]:not([tabindex="-1"])')]
      .filter(control => !element.contains(control) && (element.compareDocumentPosition(control) & Node.DOCUMENT_POSITION_FOLLOWING));
  }
  // Where focus goes in a card at rest: the first of its rest controls it
  // has now, else its heading, so focus never drops to the top of the page.
  function focusAtRest(target) {
    focusFirst(...target.rest);
    const section = $(target.section);
    const heading = section && section.querySelector('h2');
    if (heading && !section.contains(document.activeElement)) heading.focus();
  }
  // Renders every card, then puts focus where the reader of target goes
  // next: once the card is at rest, on its Edit button (focusAtRest); while
  // it is still edited, on Save when it can be pressed, else on the foot's
  // notice, which says why not, else on Cancel. A reader who moved on while
  // a save was under way, to anything outside the card, is left where they
  // are.
  function renderAndRefocus(target) {
    const section = $(target.section);
    const active = document.activeElement;
    const elsewhere = active && active !== document.body && !(section && section.contains(active));
    renderAll();
    if (elsewhere) return;
    if (target.editing) focusFirst(target.key + '-save', target.key + '-notice', target.key + '-cancel');
    else focusAtRest(target);
  }
  function box(classes, prefix, input) {
    return h('span', {class: 'box' + classes}, prefix ? h('span', {class: 'pre', 'aria-hidden': 'true'}, prefix) : null, input);
  }
  const boxClasses = state => (state.changed ? ' is-changed' : '') + (state.dropped ? ' is-back' : '') + (state.bad ? ' is-bad' : '') + (state.busy ? ' is-busy' : '');
  // Redraws the box an input sits in, and its aria-invalid from the same
  // state, so a field marked bad is marked for every reader.
  function fillBox(input, kind, state) {
    input.parentElement.className = 'box' + kind + boxClasses(state);
    input.setAttribute('aria-invalid', state.bad ? 'true' : 'false');
  }

  // ---------------------------------------------------------------------------
  // The import note

  function renderImport() {
    const target = $('import-note');
    if (!target) return;
    const editor = editorInfo();
    const note = editor ? importNote(editor.legacy, editor.available === true, editor.reason, nowSeconds(), dismissed) : null;
    const hidden = note === null && importer.status === null;
    const nodes = [];
    if (note) {
      nodes.push(h('span', {class: 'pill' + (note.tone === 'ok' ? ' ok' : ' warn')}, note.pill));
      for (const line of note.lines) nodes.push(h('p', null, line));
      if (note.button && !importer.confirming) nodes.push(h('button', {id: 'qc-import-open', type: 'button', class: 'secondary', disabled: importer.busy || editing(), on: {click: () => { importer.confirming = true; importer.status = null; renderAll(); focusFirst('qc-import-keep'); }}}, note.button));
      if (note.dismiss) nodes.push(dismissLink(note.dismiss, 'qc-import-dismiss'));
      if (note.button && importer.confirming) {
        nodes.push(h('div', {class: 'confirm', role: 'group', 'aria-label': 'Confirm import'},
          h('p', null, importQuestion(storedCounts(creditState(), editor), editor.legacy.file_counts, editor.legacy.file_updated_at)),
          h('div', {class: 'edit-acts'},
            h('button', {id: 'qc-import-keep', type: 'button', class: 'secondary', disabled: importer.busy, on: {click: () => { importer.confirming = false; renderAtRest(); focusFirst('qc-import-open'); }}}, 'Keep what is here'),
            h('button', {id: 'qc-import-go', type: 'button', disabled: importer.busy || editing(), on: {click: runImport}}, importer.busy ? [h('span', {class: 'spin', 'aria-hidden': 'true'}), 'Importing…'] : 'Import'))));
      }
    }
    if (importer.status) nodes.push(h('p', {id: 'qc-import-status', class: importer.status.tone === 'ok' ? 'saved' : 'bad', role: 'status', tabindex: '-1'}, importer.status.text));
    // The note is a live region: it changes only where what it says does.
    settle(target, nodes);
    const className = 'alert moved' + (note && note.tone === 'warn' ? ' warn' : '');
    if (!hidden && target.className !== className) target.className = className;
    if (target.hidden !== hidden) target.hidden = hidden;
  }
  async function runImport() {
    const editor = editorInfo();
    if (!editor || importer.busy || editing()) return;
    importer.busy = true;
    renderAll();
    const outcome = outcomeOf(await send({kind: 'importLegacy', baseRevision: typeof editor.revision === 'string' ? editor.revision : ''}), 'import');
    importer.busy = false;
    importer.confirming = false;
    // What came of it stays as long as a saved line does.
    clearTimeout(importer.timer);
    importer.timer = setTimeout(endImportStatus, SAVED_FOR);
    switch (outcome.kind) {
      case 'saved':
        importer.status = {tone: 'ok', text: 'Imported ' + (countsText(outcome.counts) || 'Quota Glance’s settings') + ' from Quota Glance.'};
        page.reload();
        break;
      case 'conflict':
        importer.status = {tone: 'bad', text: 'Not imported. The settings saved here changed meanwhile; check them, then import again.'};
        page.reload();
        break;
      case 'failed':
        importer.status = {tone: 'bad', text: outcome.code === 'not_editable' ? 'Quota Glance’s settings.json can no longer be read, so nothing was imported.' : outcome.text};
        break;
      default:
        importer.status = {tone: 'bad', text: 'No answer came back, so the import may or may not have happened. Select Refresh view to see which.'};
    }
    // The confirmation is gone, and Import with it: focus goes to what the
    // note offers next, unless the reader moved on meanwhile. After an
    // import that is the result line: the note still offers the import
    // until the status read asked for above arrives, which takes the button
    // away and brings Dismiss, but keeps the line.
    const lost = document.activeElement === null || document.activeElement === document.body;
    renderAll();
    if (lost && outcome.kind === 'saved') focusFirst('qc-import-status', 'qc-import-dismiss');
    else if (lost) focusFirst('qc-import-open', 'qc-import-dismiss');
  }
  // The result line goes once it has stayed as long as a saved line does. A
  // reader still on it moves on to what the note offers then, else to what
  // followed the note.
  function endImportStatus() {
    const note = $('import-note');
    const on = document.activeElement !== null && document.activeElement.id === 'qc-import-status';
    const after = on && note ? controlsAfter(note) : [];
    importer.status = null;
    renderAtRest();
    if (on) focusOnward([$('qc-import-dismiss'), $('qc-import-open'), ...after].filter(Boolean));
  }

  // ---------------------------------------------------------------------------
  // Organizations: the Claude Console organizations, above the API credit
  // card, whose rows they decide

  function organizationNotes(listed) {
    const state = creditState();
    const info = organizationsInfo();
    const notes = [];
    if (info.reason === 'organizationsUnreadable') notes.push(h('div', {class: 'alert error', role: 'alert'}, organizationsUnreadableText(listed)));
    const block = state && isObject(state.organizations) ? state.organizations : null;
    const migration = block && isObject(block.migration) ? block.migration : null;
    const movedAt = migration ? epochOf(migration.at) : null;
    if (movedAt !== null && nowSeconds() - movedAt <= IMPORT_NOTE_DAYS * 86400 && !dismissed('organizations-moved:' + migration.at)) {
      const moved = Number.isInteger(migration.moved) ? migration.moved : 0;
      const skipped = Number.isInteger(migration.skipped) ? migration.skipped : 0;
      notes.push(h('div', {class: 'alert moved', role: 'status'},
        h('span', {class: 'pill ok'}, 'Moved'),
        h('p', null, 'Moved ' + plural(moved, 'organization', 'organizations') + ' here from Quota Cache’s configuration on ' + localDay(movedAt) + (skipped > 0 ? ', ' + skipped + ' skipped because of a problem in the configuration' : '') + '. Edit them here from now on.'),
        dismissLink('organizations-moved:' + migration.at, 'qc-orgs-moved-dismiss')));
    }
    const ignored = block && Number.isInteger(block.config_items_ignored) ? block.config_items_ignored : 0;
    if (ignored > 0 && !dismissed('organizations-config:' + ignored)) {
      notes.push(h('div', {class: 'alert moved', role: 'status'},
        h('span', {class: 'pill'}, 'Config not used'),
        h('p', null, 'Quota Cache’s configuration still lists organizations under claude-api-credits. This page’s list is used instead, so an organization listed only there is not counted until you add it here. You can leave them; only an older Quota Cache reads them.'),
        dismissLink('organizations-config:' + ignored, 'qc-orgs-config-dismiss')));
    }
    return notes;
  }
  function startOrganizations(adding) {
    if (editing()) return;
    orgs.draft = emptyOrganizationsDraft();
    orgs.base = null;
    orgs.confirmRemove = null;
    setEditing(orgs, true);
    renderAll();
    focusLater(adding ? 'qc-org-add-label' : 'qc-org-0-label');
  }
  function cancelOrganizations() {
    orgs.draft = emptyOrganizationsDraft();
    orgs.base = null;
    orgs.confirmRemove = null;
    setEditing(orgs, false);
    renderAndRefocus(orgs);
  }
  function renderOrganizations() {
    const section = $('organizations');
    if (!section) return;
    const editor = editorInfo();
    section.hidden = editor === null;
    if (!editor) return;
    if (orgs.editing) { keepFocus(() => replaceEdit(section, organizationsEdit())); return; }
    const info = organizationsInfo();
    // While the file cannot be read the configuration's list applies, and the
    // card shows that list read-only rather than an empty one.
    const {items, fromConfig} = organizationRows(info, creditDocument());
    const canEdit = info.available === true && !editing();
    const counted = plural(items.length, 'Claude Console organization', 'Claude Console organizations') + (fromConfig ? ' from Quota Cache’s configuration' : '');
    const head = h('div', {class: 'section-head'},
      h('h2', {id: 'organizations-heading', tabindex: '-1'}, 'Organizations'),
      orgs.saved ? savedLine(orgs.saved) : h('span', {class: 'hint'}, items.length ? counted + ' · their API credit is counted below' : 'Claude Console organizations whose API credit Quota Cache counts'),
      info.available === true && items.length ? h('div', {class: 'head-actions'}, editButton('Edit organizations', () => startOrganizations(false), false, !canEdit, 'qc-orgs-edit')) : null);
    let body = null;
    if (items.length) {
      body = h('div', {class: 'card'},
        h('div', {id: 'qc-orgs-table', class: 'table-wrap', tabindex: '0', role: 'region', 'aria-label': 'Claude Console organizations'}, h('table', null,
          h('thead', null, h('tr', null, h('th', {scope: 'col'}, 'Label'), h('th', {scope: 'col'}, 'Organization ID'))),
          h('tbody', null, items.map(item => h('tr', null,
            h('td', {class: 'account'}, item.label),
            h('td', null, item.organization_id ? h('span', {class: 'org-id', title: item.organization_id}, shortOrganizationID(item.organization_id)) : muted('no Organization ID'))))))),
        h('div', {class: 'card-foot'}, h('p', null, fromConfig
          ? 'These come from claude-api-credits in Quota Cache’s configuration while its organizations file cannot be read, and cannot be changed here.'
          : 'Set each organization’s monthly credit, refill date and Console reading in Claude API credits below. Renaming an organization keeps its values. Removing one keeps them as an orphan you can clear there.')));
    } else if (!fromConfig) {
      body = h('div', {class: 'card'}, h('div', {class: 'empty-cta'},
        h('p', null, 'No Console organizations yet. Add one to start counting its API credit.'),
        info.available === true ? h('button', {id: 'qc-orgs-add', type: 'button', disabled: !canEdit, on: {click: () => startOrganizations(true)}}, 'Add organization') : null));
    }
    // Only configured items with an Organization ID move in again.
    settle(section, [head, ...organizationNotes(fromConfig && items.some(item => item.organization_id)), ...(body ? [body] : [])]);
  }
  function organizationsPlanNow() {
    const info = organizationsInfo();
    return organizationsPlan(Array.isArray(info.items) ? info.items : [], orgs.draft, typeof info.revision === 'string' ? info.revision : '',
      {maxItems: Number.isInteger(info.max_items) ? info.max_items : LIMITS.maxOrganizations, maxOps: Number.isInteger(info.max_ops) ? info.max_ops : LIMITS.maxOrganizationOps, maxBytes: limits(editorInfo()).maxBytes});
  }
  function organizationsEdit() {
    const info = organizationsInfo();
    const items = Array.isArray(info.items) ? info.items : [];
    const result = organizationsPlanNow();
    const busy = orgs.saving;
    const capOf = key => {
      const error = result.errors[key] || (orgs.marks[key] && orgs.marks[key].text);
      return error ? h('span', {class: 'cap bad'}, error) : null;
    };
    const rows = items.map((item, index) => {
      const removing = orgs.draft.remove[item.id] === true;
      const typed = orgs.draft.rename[item.id];
      const changed = typed !== undefined && typed.trim() !== item.label;
      const bad = Boolean(result.errors[item.id + ':label'] || orgs.marks[item.id + ':label']);
      const id = 'qc-org-' + index + '-label';
      const input = h('input', {id, type: 'text', value: typed !== undefined ? typed : item.label, autocomplete: 'off', spellcheck: 'false', disabled: removing || busy,
        'aria-label': 'Label for organization ' + item.organization_id, 'aria-describedby': id + '-caps', 'aria-invalid': bad ? 'true' : null,
        on: {input: event => { orgs.draft.rename = Object.assign({}, orgs.draft.rename, {[item.id]: event.target.value}); delete orgs.marks[item.id + ':label']; updateOrganizations(); }}});
      const caps = h('span', {id: id + '-caps', class: 'caps', 'data-row': item.id});
      fillOrganizationCaps(caps, item, result);
      const row = h('div', {class: 'org-edit'},
        h('div', {class: 'edit-grid orgs-grid'},
          h('div', {class: 'field'}, box(boxClasses({changed: changed && !removing, bad, busy}) + (removing ? ' is-removed' : ''), null, input), caps),
          h('div', {class: 'field'}, h('span', {class: 'org-id' + (removing ? ' muted' : '')}, item.organization_id)),
          h('div', {class: 'field row-acts'}, removing ? null : link('Remove', () => { orgs.confirmRemove = item.id; renderAll(); focusLater('qc-org-keep'); }, {id: 'qc-org-' + index + '-remove', disabled: busy, 'aria-expanded': orgs.confirmRemove === item.id ? 'true' : 'false', 'aria-label': 'Remove ' + item.label}))));
      if (orgs.confirmRemove === item.id && !removing) {
        const label = typed !== undefined && typed.trim() ? typed.trim() : item.label;
        row.append(h('div', {class: 'alert warn confirm', role: 'group', 'aria-labelledby': 'qc-org-remove-question'},
          h('p', {id: 'qc-org-remove-question'}, removeQuestion(label, item)),
          h('div', {class: 'edit-acts'},
            h('button', {id: 'qc-org-keep', type: 'button', class: 'secondary', on: {click: () => { orgs.confirmRemove = null; renderAll(); focusFirst('qc-org-' + index + '-remove'); }}}, 'Keep'),
            h('button', {id: 'qc-org-remove', type: 'button', class: 'danger', on: {click: () => {
              orgs.draft.remove = Object.assign({}, orgs.draft.remove, {[item.id]: true});
              orgs.confirmRemove = null;
              renderAll();
              focusFirst('qc-org-' + index + '-undo');
            }}}, 'Remove'))));
      }
      return row;
    });
    const addBad = key => Boolean(result.errors[key] || orgs.marks[key]);
    const addLabel = h('input', {id: 'qc-org-add-label', type: 'text', value: orgs.draft.add.label, autocomplete: 'off', spellcheck: 'false', disabled: busy, 'aria-describedby': 'qc-org-add-label-caps', 'aria-invalid': addBad('add:label') ? 'true' : null,
      on: {input: event => { orgs.draft.add = Object.assign({}, orgs.draft.add, {label: event.target.value}); delete orgs.marks['add:label']; updateOrganizations(); }}});
    const addID = h('input', {id: 'qc-org-add-id', type: 'text', value: orgs.draft.add.organizationId, autocomplete: 'off', spellcheck: 'false', disabled: busy, 'aria-describedby': 'qc-org-add-id-caps', 'aria-invalid': addBad('add:organizationId') ? 'true' : null,
      on: {input: event => { orgs.draft.add = Object.assign({}, orgs.draft.add, {organizationId: event.target.value}); delete orgs.marks['add:organizationId']; updateOrganizations(); }}});
    const max = Number.isInteger(info.max_items) ? info.max_items : LIMITS.maxOrganizations;
    const addRow = h('div', {class: 'org-edit org-add'},
      h('div', {class: 'org-head'}, h('b', null, 'Add an organization'), secondary(max + ' at most')),
      h('div', {class: 'edit-grid orgs-grid'},
        h('div', {class: 'field'}, h('label', {class: 'label', for: 'qc-org-add-label'}, 'Label'),
          box(boxClasses({changed: orgs.draft.add.label !== '', bad: addBad('add:label'), busy}), null, addLabel), h('span', {id: 'qc-org-add-label-caps', class: 'caps', 'data-row': 'add:label'})),
        h('div', {class: 'field'}, h('label', {class: 'label', for: 'qc-org-add-id'}, 'Organization ID'),
          box(' id' + boxClasses({changed: orgs.draft.add.organizationId !== '', bad: addBad('add:organizationId'), busy}), null, addID), h('span', {id: 'qc-org-add-id-caps', class: 'caps', 'data-row': 'add:organizationId'}))));
    const form = h('form', {novalidate: true, class: 'card', 'aria-label': 'Claude Console organizations', 'aria-busy': busy ? 'true' : null, on: {submit: event => { event.preventDefault(); saveOrganizations(); }}},
      items.length ? h('div', {class: 'edit-head orgs-grid', 'aria-hidden': 'true'}, h('span', null, 'Label'), h('span', null, 'Organization ID'), h('span')) : null,
      rows, addRow, h('div', {class: 'foot-slot'}));
    const nodes = [editHead('organizations-heading', 'Organizations', orgs, result.unsaved), form];
    fillOrganizationAddCaps(form, result);
    form.querySelector('.foot-slot').replaceWith(organizationsFoot(result, null));
    return nodes;
  }
  // A label's caption, which keeps every node that did not change (settle),
  // so the window taking focus again (resync) leaves its Undo in place.
  function fillOrganizationCaps(caps, item, result) {
    const key = item.id + ':label';
    const removing = orgs.draft.remove[item.id] === true;
    const typed = orgs.draft.rename[item.id];
    const error = result.errors[key] || (orgs.marks[key] && orgs.marks[key].text);
    if (removing) {
      const index = organizationIndex(item.id);
      settle(caps, [h('span', {class: 'cap'}, h('span', {class: 'dirty'}, 'removing'), sep(), link('Undo', () => {
        const remove = Object.assign({}, orgs.draft.remove); delete remove[item.id]; orgs.draft.remove = remove; renderAll(); focusFirst('qc-org-' + index + '-remove');
      }, {id: 'qc-org-' + index + '-undo', disabled: orgs.saving}))]);
      return;
    }
    const nodes = [];
    if (error) nodes.push(h('span', {class: 'cap bad'}, error));
    if (typed !== undefined && typed.trim() !== item.label) {
      nodes.push(h('span', {class: 'cap'}, h('span', {class: 'dirty'}, 'changed'), sep(), 'was ', h('b', null, item.label), sep(), link('Undo', () => {
        const rename = Object.assign({}, orgs.draft.rename); delete rename[item.id]; orgs.draft.rename = rename; renderAll(); focusLater('qc-org-' + organizationIndex(item.id) + '-label');
      }, {disabled: orgs.saving})));
    }
    settle(caps, nodes);
  }
  const organizationIndex = id => (organizationsInfo().items || []).findIndex(item => item.id === id);
  function fillOrganizationAddCaps(root, result) {
    const labelCaps = root.querySelector('[data-row="add:label"]');
    const idCaps = root.querySelector('[data-row="add:organizationId"]');
    if (!labelCaps || !idCaps) return;
    const message = key => result.errors[key] || (orgs.marks[key] && orgs.marks[key].text);
    settle(labelCaps, [message('add:label') ? h('span', {class: 'cap bad'}, message('add:label')) : h('span', {class: 'cap'}, 'Up to 64 characters, not used by another organization.')]);
    const nodes = [];
    if (message('add:organizationId')) nodes.push(h('span', {class: 'cap bad'}, message('add:organizationId')));
    else if (result.normalized !== null && result.normalized !== orgs.draft.add.organizationId) nodes.push(h('span', {class: 'cap'}, 'Will be saved as ', h('span', {class: 'num'}, result.normalized)));
    if (message('add:')) nodes.push(h('span', {class: 'cap bad'}, message('add:')));
    nodes.push(h('span', {class: 'cap help'}, 'Console → Settings → Organization. It is not a secret.'));
    settle(idCaps, nodes);
  }
  // The organizations card's foot: foot brought up to date, or a new one.
  function organizationsFoot(result, foot) {
    const session = sessionState() || {};
    const blocked = session.block || (result.tooLarge ? 'Too many changes for one save; undo some, save, then make the rest.' : null);
    const plan = {changes: result.changes, errors: result.count};
    if (foot) return fillFoot(foot, orgs, plan, blocked, session.note || null);
    return editFoot(orgs, 'Save applies every change together, or none of them. An Organization ID cannot be changed: a different ID is a different organization, so remove this one and add the other.',
      plan, blocked, cancelOrganizations, session.note || null);
  }
  // On each keystroke only the captions, the boxes and the foot change, so
  // the field being typed in is never rebuilt.
  function updateOrganizations() {
    const section = $('organizations');
    if (!section || !orgs.editing) return;
    const result = organizationsPlanNow();
    const items = organizationsInfo().items || [];
    items.forEach((item, index) => {
      const caps = section.querySelector('[data-row="' + CSS.escape(item.id) + '"]');
      if (caps) fillOrganizationCaps(caps, item, result);
      const input = within(section, 'qc-org-' + index + '-label');
      if (input) {
        const typed = orgs.draft.rename[item.id];
        const removing = orgs.draft.remove[item.id] === true;
        const bad = Boolean(result.errors[item.id + ':label'] || orgs.marks[item.id + ':label']);
        input.setAttribute('aria-invalid', bad ? 'true' : 'false');
        input.parentElement.className = 'box' + boxClasses({changed: typed !== undefined && typed.trim() !== item.label && !removing, bad, busy: orgs.saving}) + (removing ? ' is-removed' : '');
      }
    });
    const label = within(section, 'qc-org-add-label');
    const id = within(section, 'qc-org-add-id');
    if (label) fillBox(label, '', {changed: orgs.draft.add.label !== '', bad: Boolean(result.errors['add:label'] || orgs.marks['add:label']), busy: orgs.saving});
    if (id) fillBox(id, ' id', {changed: orgs.draft.add.organizationId !== '', bad: Boolean(result.errors['add:organizationId'] || orgs.marks['add:organizationId']), busy: orgs.saving});
    fillOrganizationAddCaps(section, result);
    const foot = section.querySelector('.edit-foot');
    if (foot) organizationsFoot(result, foot);
    const hint = section.querySelector('.section-head .hint');
    const head = section.querySelector('.section-head');
    if (hint) hint.textContent = result.unsaved + ' unsaved';
    else if (head && result.unsaved > 0) head.append(h('span', {class: 'hint'}, result.unsaved + ' unsaved'));
    if (hint && result.unsaved === 0) hint.remove();
  }
  async function saveOrganizations() {
    const result = organizationsPlanNow();
    if (result.batch === null || orgs.saving) return;
    orgs.saving = true;
    orgs.status = null;
    orgs.marks = {};
    renderAndRefocus(orgs);
    const outcome = outcomeOf(await send(result.batch));
    orgs.saving = false;
    switch (outcome.kind) {
      case 'saved':
        setEditing(orgs, false);
        orgs.draft = emptyOrganizationsDraft();
        orgs.base = null;
        showSaved(orgs, outcome.unchanged ? 'Already saved. Nothing needed changing.' : 'Saved ' + plural(result.changes, 'change', 'changes') + '. Claude API credits below lists them from the next view update.');
        page.reload();
        break;
      case 'conflict': {
        // The rows become the plugin's list, and the renames and removals
        // made against the old one go, by name; the unsaved add row stays.
        const dropped = organizationEdits(Array.isArray(organizationsInfo().items) ? organizationsInfo().items : [], orgs.draft).map(item => item.label);
        orgs.base = {revision: outcome.revision, items: (outcome.organizations || []).filter(isObject)};
        orgs.draft = organizationsAfterConflict(orgs.draft);
        orgs.status = {tone: 'bad', text: organizationsConflictText(dropped)};
        orgs.failed = orgs.draft.add.label.trim() !== '' || orgs.draft.add.organizationId.trim() !== '';
        break;
      }
      case 'operation': {
        const ref = result.refs[outcome.op];
        const key = ref ? (ref.row === 'add' ? 'add:' + (outcome.field === 'organizationId' ? 'organizationId' : outcome.field === '' ? '' : 'label') : ref.row + ':label') : '';
        if (ref) orgs.marks[key] = {text: outcome.text, tone: 'bad'};
        orgs.status = {tone: 'bad', text: ref ? FIELD_FOOT : outcome.text};
        orgs.failed = true;
        break;
      }
      case 'failed':
        // Without a session, or with a key CPA refused, the foot says so
        // itself from the session, for as long as it holds.
        if (outcome.signIn) break;
        orgs.status = {tone: 'bad', text: outcome.text};
        orgs.failed = true;
        break;
      default:
        orgs.status = {tone: 'unknown', text: 'No answer came back, so this may or may not have saved. Saving again is safe: if it already landed, the page shows the current list instead.'};
        orgs.failed = true;
    }
    renderAndRefocus(orgs);
  }

  // ---------------------------------------------------------------------------
  // Claude API credits: one row per published organization, with its monthly
  // credit, refill date and Console reading, and the orphans to clear

  const settingsOf = account => overlaid(account.settings, credits.overlay[account.id]);
  const creditName = account => account.label || account.id;
  // Why a row is locked. While the organizations file cannot be read
  // (unreadable), organizations cannot be added above either.
  function notEditableText(reason, unreadable) {
    switch (reason) {
      case 'noOrganization': return 'This item in Quota Cache’s configuration has no usable organization-id. ' + (unreadable
        ? 'Organizations can be added above again once Quota Cache can read its organizations file.'
        : 'Add the organization under Organizations above to edit it here.');
      case 'duplicateOrganization': return 'Another item in Quota Cache’s configuration has the same organization-id; edit that one.';
      case 'overLimit': return 'Only the first 16 items in claude-api-credits can be edited here.';
      case 'cacheTooOld': return 'Quota Cache has no Organization ID for this row yet, so it cannot be edited here.';
      default: return 'This organization cannot be edited here.';
    }
  }
  // Whether a Console reading would settle what the row says: it is out, it
  // is counted but incomplete for want of one, or it lacks only a refill
  // date, which a reading stands in for.
  const READING_SETTLES = ['meterStartedLate', 'meterGap', 'readingOnRefillDay', 'claudeCodeRefused'];
  function wantsReading(account) {
    const issues = Array.isArray(account.dataIssues) ? account.dataIssues : [];
    if (account.state === 'out') return true;
    if (account.state === 'needsSettings') return issues.includes('needsRefillDate') && !issues.includes('needsCredit');
    return account.counted === true && READING_SETTLES.some(issue => issues.includes(issue));
  }
  function untilText(epoch, now) {
    const remaining = epoch - now;
    if (remaining <= 0) return 'due';
    if (remaining < 86400) return 'in ' + Math.max(1, Math.round(remaining / 3600)) + 'h';
    return 'in ' + plural(Math.floor(epoch / 86400) - Math.floor(now / 86400), 'day', 'days');
  }
  function statePill(account) {
    const issues = Array.isArray(account.dataIssues) ? account.dataIssues : [];
    switch (account.state) {
      case 'ok': return account.level === 'critical' ? ['Critical', 'error'] : account.level === 'low' ? ['Low', 'warn'] : ['OK', 'ok'];
      case 'stale': return ['Stale', 'warn'];
      case 'out': return ['Out', 'error'];
      case 'needsSettings': return [issues.includes('needsCredit') && issues.includes('needsRefillDate') ? 'Needs credit and date' : issues.includes('needsCredit') ? 'Needs credit' : 'Needs refill date', 'warn'];
      case 'misconfigured': return ['Misconfigured', 'error'];
      case 'cacheTooOld': return ['No figures', ''];
      case 'pending': return ['Pending', ''];
      default: return [account.state || 'Unknown', ''];
    }
  }
  function unusedText(reason, at) {
    switch (reason) {
      case '': return localDateTime(at) + ' · ' + utcStamp(at) + ' · in use';
      case 'beforeRefill': return 'on ' + utcDay(at) + ', before the last refill; not used';
      case 'otherOrganization': return 'for another Organization ID; not used';
      case 'tooOld': return 'over 31 days old; not used';
      case 'future': return 'dated in the future; not used';
      default: return 'on ' + utcDay(at) + '; not used';
    }
  }
  function creditCells(account, now) {
    const s = account.settings || {};
    const credit = account.monthlyCreditSource === 'dashboard' || account.monthlyCreditSource === 'config'
      ? [h('span', {class: 'num'}, account.monthlyCreditText), account.monthlyCreditSource === 'dashboard'
        ? secondary(setMark(), 'entered here · ', s.configMonthlyUsd ? 'config ' + moneyText(s.configMonthlyUsd) : 'not in config')
        : secondary('from config')]
      : [muted('Not set'), s.configMonthlyUsdInvalid ? secondary('the config’s is not a dollar amount') : null];
    let refill;
    if (typeof account.renewsAtEpoch === 'number') {
      const renews = s.renews || s.configRenews || '';
      const startDay = typeof account.cycleStartEpoch === 'number' ? Math.floor(now / 86400) - Math.floor(account.cycleStartEpoch / 86400) : -1;
      // A date entered here carries its mark; one from the config says the
      // day it repeats on, unless the credit refilled today or yesterday.
      const source = account.renewsSource === 'dashboard' ? [setMark(), 'entered here'] : startDay === 0 ? 'refilled today' : startDay === 1 ? 'refilled yesterday' : renews ? 'every ' + ordinal(Number(renews.slice(8, 10))) : null;
      refill = [utcDay(account.renewsAtEpoch), secondary(source, source ? ' · ' : null, untilText(account.renewsAtEpoch, now))];
    } else refill = [muted('Not set'), s.configRenewsInvalid ? secondary('the config’s is not a date') : null];
    const reading = s.reading
      ? [h('span', {class: 'num'}, moneyText(s.reading.remainingUsd)), secondary(unusedText(s.readingUnusedReason || '', s.reading.atEpoch))]
      : [muted('None this cycle')];
    let used;
    if (account.hasEstimate) {
      const out = account.state === 'out';
      const percent = Math.max(0, Math.min(100, 100 - (Number.isFinite(account.remainingPercent) ? account.remainingPercent : 0)));
      const left = out && account.estimateLeftText ? 'out · the estimate had ' + account.estimateLeftText + ' left' : (account.lowerBound ? '≤ ' : '≈ ') + account.leftText + ' left';
      const progress = h('progress', {max: '100', 'aria-label': creditName(account) + ' credit used'});
      progress.value = percent;
      used = [h('span', {class: 'num'}, (account.lowerBound ? '≥ ' : '') + account.usedText + ' of ' + account.monthlyCreditText), progress, secondary(left)];
    } else if (typeof account.spentSinceEpoch === 'number' && account.spentText) used = [h('span', {class: 'num'}, account.spentText + ' metered'), secondary('no credit to count against')];
    else used = [muted('—')];
    const [word, tone] = statePill(account);
    const statusCell = [h('span', {class: 'pill' + (tone ? ' ' + tone : '')}, word), account.issue ? secondary(account.issue) : null];
    const info = editorInfo();
    if (info && info.available === true && s.editable && wantsReading(account)) {
      statusCell.push(h('span', {class: 'secondary'}, link('Enter Console reading', () => enterReading(account.id),
        {id: 'qc-reading-' + account.id, 'data-account': account.id, disabled: editing(), 'aria-label': 'Enter Console reading for ' + creditName(account)})));
    }
    return [credit, refill, reading, used, statusCell];
  }
  function creditsHint(doc) {
    const pool = doc.pool || {};
    const accounts = accountsOf(doc);
    if (!pool.hasEstimate) return noneCountedText(accounts);
    const label = id => { const account = accounts.find(item => item.id === id); return account ? creditName(account) : id; };
    const refill = isObject(pool.nextRefill) ? pool.nextRefill : null;
    return (pool.lowerBound ? '≤ ' : '≈ ') + pool.leftText + ' left of ' + pool.monthlyCreditText + ' across ' + plural(pool.countedCount, 'organization', 'organizations') +
      (refill ? ' · next refill ' + strings(refill.accountIds).map(label).join(', ') + ' ' + utcDay(refill.refillAtEpoch) + ', +' + refill.gainText : '');
  }
  // A Console reading for the account id names, as the status now lists it.
  function enterReading(id) {
    const account = accountsOf(creditDocument()).find(item => item.id === id);
    if (!account || editing()) return;
    startCredits();
    credits.draft = editCredit(credits.draft, {id: account.id, settings: settingsOf(account)}, creditName(account), 'reading', {kind: 'set', amount: '', at: localInputValue(nowSeconds())});
    renderAll();
    const index = accountsOf(creditDocument()).findIndex(item => item.id === account.id);
    focusLater('qc-credit-' + index + '-amount');
  }
  function startCredits() {
    if (editing()) return;
    credits.draft = emptyDraft();
    credits.overlay = {};
    setEditing(credits, true);
    renderAll();
    const first = firstControl('api-credits');
    if (first) first.focus();
  }
  function cancelCredits() {
    credits.draft = emptyDraft();
    credits.overlay = {};
    setEditing(credits, false);
    renderAndRefocus(credits);
  }
  function renderCredits() {
    const section = $('api-credits');
    if (!section) return;
    const editor = editorInfo();
    section.hidden = editor === null;
    if (!editor) return;
    if (credits.editing) { keepFocus(() => replaceEdit(section, creditsEdit())); return; }
    const state = creditState();
    const doc = creditDocument();
    const accounts = accountsOf(doc);
    const orphans = creditOrphans();
    const off = unavailableText(editor);
    const canEdit = editor.available === true && (accounts.some(account => account.settings && account.settings.editable) || orphans.length > 0);
    const counted = doc && doc.pool ? doc.pool.countedCount : 0;
    const editLabel = creditsEditLabel(accounts, counted);
    const head = h('div', {class: 'section-head'},
      h('h2', {id: 'api-credits-heading', tabindex: '-1'}, 'Claude API credits'),
      credits.saved ? savedLine(credits.saved) : h('span', {class: 'hint'}, off && editor.reason !== IMPORT_RECORD_UNREADABLE ? 'Showing values from this plugin’s configuration' : doc ? creditsHint(doc) : state ? 'No organizations yet' : 'Waiting for Quota Cache’s first figures'),
      canEdit ? h('div', {class: 'head-actions'}, editButton(editLabel, startCredits, editLabel === SET_CREDITS, editing(), 'qc-credits-edit')) : null);
    const alerts = off ? [h('div', {class: 'alert error', role: 'alert'}, off)] : [];
    const orphanLines = orphans.map(orphan => h('p', {class: 'orphan'}, 'Saved values for an organization no longer listed under Organizations: ', h('span', {class: 'num'}, orphan.id), canEdit ? ' · clear them under ' + editLabel : ''));
    const heldLines = heldLinesOf(editor);
    const unreadable = organizationsUnreadable();
    if (!doc) {
      settle(section, [head, ...alerts, h('div', {class: 'card'}, h('p', {class: 'empty'}, !state
        ? 'Quota Cache has not worked out its credit figures yet. They appear here shortly after it starts.'
        : unreadable
          ? 'No Claude Console organizations are listed: Quota Cache cannot read its organizations file, and its configuration lists none. Organizations can be added above once it can read that file again.'
          : 'No Claude Console organizations yet. Add one under Organizations above, then set its monthly credit and refill date here.'), orphanLines, heldLines)]);
      return;
    }
    const now = nowSeconds();
    const pool = doc.pool || {};
    const approx = pool.lowerBound ? '≤' : '≈';
    const total = pool.hasEstimate
      ? h('tfoot', null, h('tr', null, h('td', {colspan: '6'},
        h('b', null, (pool.countedCount === pool.accountCount ? 'All ' + plural(pool.accountCount, 'organization', 'organizations') : pool.countedCount + ' of ' + pool.accountCount + ' organizations counted') + ':'),
        ' ' + approx + ' ' + pool.usedText + ' used of ' + pool.monthlyCreditText + ' this cycle · ' + approx + ' ' + pool.leftText + ' left' + (typeof pool.fullAtEpoch === 'number' ? ' · all refilled by ' + utcDay(pool.fullAtEpoch) : ''))))
      : null;
    const table = h('table', null,
      h('thead', null, h('tr', null, ['Organization', 'Monthly credit', 'Refills (UTC)', 'Console reading', 'Credit used (est.)', 'Status'].map(title => h('th', {scope: 'col'}, title)))),
      h('tbody', null, accounts.map(account => {
        const cells = creditCells(account, now);
        return h('tr', null,
          h('td', {class: 'account', title: account.organizationId || null}, creditName(account), secondary(account.organizationId ? shortOrganizationID(account.organizationId) : 'no Organization ID')),
          cells.map(content => h('td', null, content)));
      })),
      total);
    const pricedOn = doc.pricing && /^\d{4}-\d{2}-\d{2}$/.test(doc.pricing.asOf) ? utcDay(dateEpoch(doc.pricing.asOf)) : doc.pricing ? doc.pricing.asOf : '';
    settle(section, [head, ...alerts, h('div', {class: 'card'}, h('div', {id: 'qc-credits-table', class: 'table-wrap', tabindex: '0', role: 'region', 'aria-label': 'Claude API credits by organization'}, table), orphanLines, heldLines,
      h('div', {class: 'card-foot'}, h('p', null, (unreadable
        ? 'You set monthly credits, refill dates and Console readings here. The organizations come from Quota Cache’s configuration until it can read its organizations file again. '
        : 'You set monthly credits, refill dates and Console readings here, and organizations above. ') + (accounts.some(account => account.monthlyCreditSource === 'dashboard' || account.renewsSource === 'dashboard') ? '● marks a value entered here or imported from Quota Glance. ' : '') +
        'Spend is metered from CPA traffic and priced at Anthropic’s list prices' + (pricedOn ? ' of ' + pricedOn : '') + '. Quota Glance shows these figures, and Reset Priority routes by them when its API-credit routing is on.')))]);
  }
  // One line per held value, with nothing to press: clearing one could delete
  // values an organization in the unreadable file still uses.
  function heldLinesOf(editor) {
    return heldCreditsOf(editor).map(row => h('p', {class: 'orphan'}, 'Saved values for an organization Quota Cache’s configuration does not list: ', h('span', {class: 'num'}, row.id),
      ' · kept, and used again once Quota Cache’s organizations file is fixed or a good copy put back'));
  }
  function creditsPlanNow() {
    return creditPlan(accountsOf(creditDocument()), credits.draft, nowSeconds(), settingsOf, {maxItems: limits(editorInfo()).maxApiCreditItems, maxBytes: limits(editorInfo()).maxBytes});
  }
  const markOf = (id, key) => credits.marks[id + ':' + key] || null;
  function editCreditField(account, key, edit, focus) {
    credits.draft = editCredit(credits.draft, {id: account.id, settings: settingsOf(account)}, creditName(account), key, edit);
    // A mark is about the value it was left under; a new one replaces it.
    const prefix = account.id + ':' + key;
    for (const at of Object.keys(credits.marks)) if (at.startsWith(prefix)) delete credits.marks[at];
  }
  function creditsEdit() {
    const doc = creditDocument();
    const accounts = accountsOf(doc);
    const now = nowSeconds();
    const busy = credits.saving;
    const rows = accounts.map((account, index) => {
      const settings = settingsOf(account);
      const name = creditName(account);
      const idOf = part => 'qc-credit-' + index + '-' + part;
      // Every row's fields share their visible labels, so each input is
      // named by its organization first, then its label.
      const named = part => idOf('name') + ' ' + idOf(part);
      const head = h('div', {class: 'org-head'}, h('b', {id: idOf('name')}, name), secondary(account.organizationId || 'no Organization ID'));
      if (credits.gone.has(account.id)) return h('div', {class: 'org-edit'}, head, h('p', {class: 'locked'}, 'Quota Cache no longer offers this organization for editing, so your change to it was dropped. Cancel, then select Refresh view to see why.'));
      if (!settings.editable) return h('div', {class: 'org-edit'}, head, h('p', {class: 'locked'}, notEditableText(settings.notEditableReason, organizationsUnreadable())));
      const row = creditRow(settings, credits.draft.rows[account.id], now);
      const monthly = h('input', {id: idOf('monthly'), type: 'text', inputmode: 'decimal', value: row.monthlyUsd.text, placeholder: 'not set', disabled: busy, spellcheck: 'false', autocomplete: 'off', 'aria-labelledby': named('monthly-label'), 'aria-describedby': idOf('monthly-caps'),
        on: {input: event => { editCreditField(account, 'monthlyUsd', {kind: 'set', text: event.target.value}); updateCredits(); }}});
      const refill = h('input', {id: idOf('refill'), type: 'date', value: row.renews.text, min: '2000-01-01', max: '2099-12-31', disabled: busy, 'aria-labelledby': named('refill-label'), 'aria-describedby': idOf('refill-caps'),
        on: {
          input: event => { editCreditField(account, 'renews', {kind: 'set', text: event.target.value, incomplete: event.target.validity.badInput || undefined}); updateCredits(); },
          focus: () => { credits.focused = account.id; updateCredits(); },
          blur: () => { credits.focused = null; updateCredits(); },
        }});
      return h('div', {class: 'org-edit', 'data-account': account.id},
        head,
        h('div', {class: 'edit-grid'},
          h('div', {class: 'field'}, h('label', {class: 'label', for: idOf('monthly'), id: idOf('monthly-label')}, 'Monthly credit, USD'), box('', '$', monthly), h('span', {id: idOf('monthly-caps'), class: 'caps', 'data-caps': 'monthlyUsd'})),
          h('div', {class: 'field'}, h('label', {class: 'label', for: idOf('refill'), id: idOf('refill-label')}, 'Refills on, UTC'), box('', null, refill), h('span', {id: idOf('refill-caps'), class: 'caps', 'data-caps': 'renews'})),
          readingPart(account, settings, row, index, busy, now)));
    });
    const orphans = liveOrphans(creditOrphans(), credits.gone, credits.revised).map((orphan, index) => {
      const removing = credits.draft.remove[orphan.id] !== undefined;
      const idOf = part => 'qc-credit-orphan-' + index + '-' + part;
      return h('p', {class: 'orphan'}, 'Saved values for an organization no longer listed under Organizations: ', h('span', {class: 'num'}, orphan.id), sep(),
        removing ? [h('span', {class: 'dirty'}, 'removing'), sep(), link('Undo', () => { credits.draft = markRemoved(credits.draft, orphan, false); renderAll(); focusFirst(idOf('remove')); }, {id: idOf('undo'), disabled: busy})]
          : link('Remove', () => { credits.draft = markRemoved(credits.draft, orphan, true); renderAll(); focusFirst(idOf('undo')); }, {id: idOf('remove'), disabled: busy, 'aria-label': 'Remove saved values for ' + orphan.id}));
    });
    const form = h('form', {novalidate: true, class: 'card', 'aria-label': 'Monthly credits, refill dates and Console readings', 'aria-busy': busy ? 'true' : null, on: {submit: event => { event.preventDefault(); saveCredits(); }}},
      rows, orphans, heldLinesOf(editorInfo()), h('div', {class: 'foot-slot'}));
    const plan = creditsPlanNow();
    const nodes = [editHead('api-credits-heading', 'Claude API credits', credits, plan.unsaved), form];
    fillCredits(form, plan);
    return nodes;
  }
  function readingPart(account, settings, row, index, busy, now) {
    const reading = row.reading;
    const idOf = part => 'qc-credit-' + index + '-' + part;
    // Each press says where focus goes next, the first of focus that is
    // there: the link that undoes it, or the reading's first action again.
    const set = (edit, ...focus) => { editCreditField(account, 'reading', edit); renderAll(); focusFirst(...focus); };
    const atRest = [idOf('reading-change'), idOf('reading-add'), idOf('reading-clear')];
    if (reading.edit && reading.edit.kind === 'set') {
      const edit = reading.edit;
      const amount = h('input', {id: idOf('amount'), type: 'text', inputmode: 'decimal', value: reading.amount.text, disabled: busy, spellcheck: 'false', autocomplete: 'off',
        'aria-labelledby': [idOf('name'), idOf('amount-label')].join(' '), 'aria-describedby': idOf('reading-caps'),
        on: {input: event => { editCreditField(account, 'reading', Object.assign({}, credits.draft.rows[account.id].reading, {amount: event.target.value})); updateCredits(); }}});
      const at = h('input', {id: idOf('at'), type: 'datetime-local', value: edit.at, min: localInputValue(now - READING_PICK_AGE), max: localInputValue(now + READING_MAX_AHEAD), disabled: busy,
        'aria-labelledby': [idOf('name'), idOf('reading-label'), idOf('at-label')].join(' '), 'aria-describedby': idOf('reading-caps'),
        on: {input: event => { editCreditField(account, 'reading', Object.assign({}, credits.draft.rows[account.id].reading, {at: event.target.value})); updateCredits(); }}});
      return h('div', {class: 'field'}, h('span', {class: 'label', id: idOf('reading-label')}, 'Console reading'),
        h('div', {class: 'reading-fields'},
          h('div', null, h('label', {class: 'sublabel', for: idOf('amount'), id: idOf('amount-label')}, 'Left in Console'), box(' is-changed', '$', amount)),
          h('div', null, h('label', {class: 'sublabel', for: idOf('at'), id: idOf('at-label')}, 'at, your time'), box(' when is-changed', null, at))),
        h('span', {id: idOf('reading-caps'), class: 'caps', 'data-caps': 'reading'}),
        h('span', {class: 'cap'}, link(reading.stored ? 'Keep the saved one' : 'Cancel', () => set(undefined, ...atRest), {id: idOf('reading-cancel'), disabled: busy, 'aria-label': (reading.stored ? 'Keep the saved one: Console reading for ' : 'Cancel the new Console reading for ') + creditName(account)})),
        h('span', {class: 'cap help'}, 'From Console → Settings → Billing, Promotional credits: the amount left.'));
    }
    const stored = reading.stored;
    const change = () => { set({kind: 'set', amount: stored ? stored.remainingUsd : '', at: localInputValue(stored ? stored.atEpoch : nowSeconds())}); focusLater(idOf('amount')); };
    const add = () => { set({kind: 'set', amount: '', at: localInputValue(nowSeconds())}); focusLater(idOf('amount')); };
    let line;
    if (reading.edit && reading.edit.kind === 'clear') line = [h('span', {class: 'dirty'}, 'cleared'), sep(), link('Undo', () => set(undefined, ...atRest), {id: idOf('reading-undo'), disabled: busy})];
    else if (!stored) line = ['None this cycle', sep(), link('Add', add, {id: idOf('reading-add'), disabled: busy, 'aria-label': 'Add a Console reading for ' + creditName(account)})];
    else {
      line = [h('b', {class: 'num'}, moneyText(stored.remainingUsd)), ' ', reading.unusedReason === '' ? 'at ' + localDateTime(stored.atEpoch) + ' · ' + utcStamp(stored.atEpoch) : unusedText(reading.unusedReason, stored.atEpoch)];
      if (reading.unusedReason === '') line.push(sep(), link('Change', change, {id: idOf('reading-change'), disabled: busy, 'aria-label': 'Change the Console reading for ' + creditName(account)}));
      line.push(sep(), link('Clear', () => set({kind: 'clear'}, idOf('reading-undo')), {id: idOf('reading-clear'), disabled: busy, 'aria-label': 'Clear the Console reading for ' + creditName(account)}));
    }
    return h('div', {class: 'field'}, h('span', {class: 'label'}, 'Console reading'), h('span', {class: 'cap'}, line), h('span', {class: 'caps', 'data-caps': 'reading'}));
  }
  // Under a money or date field: where its value comes from, or what is
  // about to happen to it (Quota Glance's FieldCap).
  function fieldCaps(account, key, state, kind, busy, hint) {
    const shown = value => (kind === 'money' ? moneyText(value) : utcDay(dateEpoch(value)));
    const index = accountsOf(creditDocument()).findIndex(item => item.id === account.id);
    const input = 'qc-credit-' + index + '-' + (key === 'monthlyUsd' ? 'monthly' : 'refill');
    const edit = next => { editCreditField(account, key, next); renderAll(); focusLater(input); };
    const undo = () => link('Undo', () => edit(undefined), {disabled: busy});
    const useConfig = () => link('Use config', () => edit({kind: 'drop'}), {disabled: busy, 'aria-label': 'Use config: ' + (kind === 'money' ? 'monthly credit' : 'refill date') + ' for ' + creditName(account)});
    const mark = markOf(account.id, key);
    const out = [];
    const base = state.stored || state.config;
    if (state.error !== null) {
      out.push(h('span', {class: 'cap bad'}, state.ambiguous
        ? ['A comma is ambiguous. Did you mean ', link(state.ambiguous.decimal, () => edit({kind: 'set', text: state.ambiguous.decimal}), {class: 'link num', 'aria-label': 'Use ' + state.ambiguous.decimal}), ' or ',
          link(state.ambiguous.thousands, () => edit({kind: 'set', text: state.ambiguous.thousands}), {class: 'link num', 'aria-label': 'Use ' + state.ambiguous.thousands}), '?']
        : state.error, sep(), undo()));
      return out;
    }
    if (mark) out.push(h('span', {class: 'cap ' + mark.tone}, mark.text));
    if (state.edit && state.edit.kind === 'drop') {
      out.push(h('span', {class: 'cap'}, h('span', {class: 'dirty'}, state.config ? 'back to config' : 'cleared'), state.config ? [' ', h('b', {class: 'num'}, shown(state.config))] : null, sep(), undo()));
    } else if (state.changed) {
      out.push(h('span', {class: 'cap'}, h('span', {class: 'dirty'}, 'changed'), sep(), base ? ['was ', h('b', {class: 'num'}, shown(base))] : 'was not set', sep(), undo()));
      // A value set here keeps its config note while it is being changed:
      // the reader may want the config's back instead.
      if (state.source === 'dashboard') out.push(h('span', {class: 'cap'}, state.config ? ['config ', h('b', {class: 'num'}, shown(state.config))] : 'not in config', sep(), useConfig()));
    } else if (state.source === 'dashboard') {
      out.push(h('span', {class: 'cap'}, setMark(), 'entered here', sep(), state.config ? ['config ', h('b', {class: 'num'}, shown(state.config))] : 'not in config', sep(), useConfig()));
    } else if (state.source === 'config') {
      out.push(h('span', {class: 'cap'}, 'from config', state.edit !== undefined ? [sep(), undo()] : null));
    } else {
      out.push(h('span', {class: 'cap'}, 'not set', state.configInvalid ? [sep(), kind === 'money' ? 'the config’s is not a dollar amount' : 'the config’s is not a date'] : null));
    }
    if (hint) out.push(hint);
    return out;
  }
  function readingCaps(account, reading) {
    const out = [];
    const edit = reading.edit;
    const mark = markOf(account.id, 'reading.remainingUsd') || markOf(account.id, 'reading.at');
    if (edit && edit.kind === 'set') {
      const index = accountsOf(creditDocument()).findIndex(item => item.id === account.id);
      // The reading as the draft holds it when pressed: the node stays while
      // its time is typed (settle).
      const pick = value => { editCreditField(account, 'reading', Object.assign({}, credits.draft.rows[account.id].reading, {amount: value})); renderAll(); focusLater('qc-credit-' + index + '-amount'); };
      if (reading.at !== null) out.push(h('span', {class: 'cap'}, h('span', {class: 'num'}, '= ' + utcDay(reading.at) + ', ' + utcClock(reading.at))));
      if (reading.amount.error !== null) {
        out.push(h('span', {class: 'cap bad'}, reading.amount.ambiguous
          ? ['A comma is ambiguous. Did you mean ', link(reading.amount.ambiguous.decimal, () => pick(reading.amount.ambiguous.decimal), {class: 'link num'}), ' or ', link(reading.amount.ambiguous.thousands, () => pick(reading.amount.ambiguous.thousands), {class: 'link num'}), '?']
          : reading.amount.error));
      }
      if (reading.atError !== null) out.push(h('span', {class: 'cap bad'}, reading.atError));
      if (reading.onRefillDay) out.push(h('span', {class: 'cap warn'}, 'Today is the refill day. Enter the reading after the new credit shows in Console.'));
    }
    if (mark) out.push(h('span', {class: 'cap ' + mark.tone}, mark.text));
    return out;
  }
  // Fills every caption, box and the foot from the draft; the inputs stay.
  function fillCredits(root, plan) {
    const accounts = accountsOf(creditDocument());
    const now = nowSeconds();
    const busy = credits.saving;
    accounts.forEach((account, index) => {
      const settings = settingsOf(account);
      if (!settings.editable) return;
      const holder = root.querySelector('[data-account="' + CSS.escape(account.id) + '"]');
      if (!holder) return;
      const row = creditRow(settings, credits.draft.rows[account.id], now);
      const refillValue = row.renews.error === null ? (row.renews.edit && row.renews.edit.kind === 'drop' ? row.renews.config : row.renews.desired || row.renews.text) : '';
      const cycle = refillValue ? cycleOn(refillValue, now) : null;
      const rule = refillValue ? refillRule(refillValue) : null;
      const hint = (credits.focused === account.id || row.renews.changed) && (cycle || rule)
        ? h('span', {class: 'cap help'}, rule, rule && cycle ? h('br') : null, cycle ? cycleText(cycle) : null) : null;
      // Captions keep every node that did not change (settle): focus
      // entering or leaving a refill date, which shows or hides its hint,
      // and the window taking focus again (resync) must not take away the
      // link the reader is on or is pressing.
      const caps = key => holder.querySelector('[data-caps="' + key + '"]');
      settle(caps('monthlyUsd'), fieldCaps(account, 'monthlyUsd', row.monthlyUsd, 'money', busy, null));
      settle(caps('renews'), fieldCaps(account, 'renews', row.renews, 'date', busy, hint));
      settle(caps('reading'), readingCaps(account, row.reading));
      // A field is bad when the page refuses its value, or when Quota Cache
      // refused it at the last save; its box and aria-invalid say so alike.
      const refused = key => (markOf(account.id, key) || {}).tone === 'bad';
      const state = (field, key) => ({changed: field.changed, dropped: Boolean(field.edit && field.edit.kind === 'drop'), bad: field.error !== null || refused(key), busy});
      const monthly = within(root, 'qc-credit-' + index + '-monthly');
      const refill = within(root, 'qc-credit-' + index + '-refill');
      if (monthly) fillBox(monthly, '', state(row.monthlyUsd, 'monthlyUsd'));
      if (refill) fillBox(refill, '', state(row.renews, 'renews'));
      const amount = within(root, 'qc-credit-' + index + '-amount');
      const at = within(root, 'qc-credit-' + index + '-at');
      if (amount && row.reading.amount) fillBox(amount, '', {changed: true, bad: row.reading.amount.error !== null || refused('reading.remainingUsd'), busy});
      if (at) fillBox(at, ' when', {changed: true, bad: row.reading.atError !== null || refused('reading.at'), busy});
    });
    const session = sessionState() || {};
    const blocked = session.block || (plan.tooLarge ? 'Too many changes for one save; undo some, save, then make the rest.' : null);
    const foot = root.querySelector('.edit-foot');
    if (foot) { fillFoot(foot, credits, plan, blocked, session.note || null); return; }
    const slot = root.querySelector('.foot-slot');
    const where = organizationsUnreadable()
      ? 'Labels and Organization IDs come from Quota Cache’s configuration until it can read its organizations file again.'
      : 'Labels and Organization IDs are edited under Organizations.';
    if (slot) slot.replaceWith(editFoot(credits, 'A refill date repeats on that day every month, at 00:00 UTC. ' + where, plan, blocked, cancelCredits, session.note || null));
  }
  function updateCredits() {
    const section = $('api-credits');
    if (!section || !credits.editing) return;
    const plan = creditsPlanNow();
    fillCredits(section, plan);
    const head = section.querySelector('.section-head');
    const hint = head && head.querySelector('.hint');
    if (hint && plan.unsaved === 0) hint.remove();
    else if (hint) hint.textContent = plan.unsaved + ' unsaved';
    else if (head && plan.unsaved > 0) head.append(h('span', {class: 'hint'}, plan.unsaved + ' unsaved'));
  }
  // A save while the page still follows Quota Glance's file asks first:
  // saving here ends that. Save anyway answers for that one save, which
  // runs again confirmed, so follow mode that comes back on this page, as
  // after an import, asks again.
  function following() {
    const editor = editorInfo();
    return editor !== null && followsQuotaGlance(editor.legacy);
  }
  // Focus moves to the question's first button, and the question names the
  // group it is in, so a screen reader reads it out.
  function askFollowing(target, go) {
    target.confirm = {text: 'Saving here stops picking up changes made in Quota Glance older than 0.8.0. Save anyway?', label: 'Save anyway', go: () => { target.confirm = null; go(true); }};
    renderAll();
    focusFirst(target.key + '-keep-editing');
  }
  async function saveCredits(confirmed = false) {
    const plan = creditsPlanNow();
    if (plan.batch === null || credits.saving) return;
    if (!confirmed && following()) { askFollowing(credits, saveCredits); return; }
    credits.saving = true;
    credits.status = null;
    credits.marks = {};
    renderAndRefocus(credits);
    const outcome = outcomeOf(await send(plan.batch));
    credits.saving = false;
    const accounts = accountsOf(creditDocument());
    switch (outcome.kind) {
      case 'saved':
        setEditing(credits, false);
        credits.draft = emptyDraft();
        credits.overlay = {};
        showSaved(credits, outcome.unchanged ? 'Already saved. Nothing needed changing.' : 'Saved ' + plural(plan.changes, 'change', 'changes') + '. Quota Glance shows them from its next read of the cache.');
        page.reload();
        break;
      case 'conflict': {
        // Each conflicting row takes what is stored now, and says so under
        // the fields that moved; every other row's draft is kept.
        const marks = {};
        for (const id of outcome.ids) {
          const account = accounts.find(item => item.id === id);
          const current = outcome.current[id];
          if (account && isObject(current)) {
            for (const key of conflictFields(settingsOf(account), current, credits.draft.rows[id])) marks[id + ':' + (key === 'reading' ? 'reading.remainingUsd' : key)] = {text: CONFLICT_FIELD, tone: 'warn'};
            credits.overlay[id] = {settings: current, over: account.settings.revision};
          } else if (!account) orphanAfterConflict(current, credits.gone, credits.revised, id);
        }
        const next = dropConflicts(credits.draft, outcome.ids);
        credits.draft = next.draft;
        credits.marks = marks;
        credits.status = {tone: 'bad', text: outcome.text};
        credits.failed = next.retry;
        break;
      }
      case 'field':
        credits.marks = {[outcome.id + ':' + outcome.field]: {text: outcome.text, tone: 'bad'}};
        credits.status = {tone: 'bad', text: FIELD_FOOT};
        credits.failed = true;
        break;
      case 'failed':
        // Without a session, or with a key CPA refused, the foot says so
        // itself from the session, for as long as it holds.
        if (outcome.signIn) break;
        if (outcome.code === 'not_editable' && outcome.ids.length > 0) {
          // The rows the plugin no longer offers go, by name, so the others
          // can be saved; the held view would never drop them itself.
          const next = dropRefused(credits.draft, outcome.ids, id => id);
          for (const id of outcome.ids) credits.gone.add(id);
          credits.draft = next.draft;
          credits.status = {tone: 'bad', text: refusedRowsText(next.names, next.retry)};
          credits.failed = next.retry;
          break;
        }
        credits.status = {tone: 'bad', text: outcome.text};
        credits.failed = true;
        break;
      default:
        credits.status = {tone: 'unknown', text: outcome.text};
        credits.failed = true;
    }
    renderAndRefocus(credits);
  }

  // ---------------------------------------------------------------------------
  // Claude renewal dates: one row per Claude credential that can take a date,
  // and the dates saved for credentials CPA no longer lists

  function renewalSettingOf(credential) {
    return Object.prototype.hasOwnProperty.call(renewals.overlay, credential.auth_index) ? renewals.overlay[credential.auth_index] : credential.renewal || null;
  }
  const credentialName = credential => credential.name || credential.auth_index;
  function planOf(credential) {
    const entries = status && isObject(status.entries) ? status.entries : {};
    const entry = entries['claude:' + credential.auth_index];
    const plan = entry && (entry.plan || (entry.account_details && entry.account_details.plan));
    if (typeof plan !== 'string' || plan === '') return '';
    return plan.replace(/_/g, ' ').replace(/^./, first => first.toUpperCase());
  }
  function renewalCells(credential, now) {
    const at = credential.renewal_at_epoch;
    const source = credential.renewal_source;
    const period = credential.billing_period === 'annual' ? 'yearly' : credential.billing_period === 'monthly' ? 'monthly' : '';
    const renews = typeof at === 'number' ? [(source === 'estimated' ? '~' : '') + utcDay(at), secondary(untilText(at, now) + (period ? ' · ' + period : ''))] : [muted('—')];
    const estimate = typeof credential.renewal_estimate_at_epoch === 'number' ? credential.renewal_estimate_at_epoch : null;
    let from;
    if (source === 'dashboard') from = [setMark(), 'entered here', estimate !== null ? secondary('estimate ~' + utcDay(estimate)) : null];
    else if (source === 'estimated') from = ['estimated', secondary('from when the subscription started')];
    else if (source === 'reported') from = ['reported', secondary('by the provider')];
    else from = [muted('not known yet')];
    return [renews, from];
  }
  function startRenewals() {
    if (editing()) return;
    renewals.draft = emptyDraft();
    renewals.overlay = {};
    setEditing(renewals, true);
    renderAll();
    const first = firstControl('renewals');
    if (first) { first.focus(); first.scrollIntoView({block: 'center'}); }
  }
  function cancelRenewals() {
    renewals.draft = emptyDraft();
    renewals.overlay = {};
    setEditing(renewals, false);
    renderAndRefocus(renewals);
  }
  function renderRenewals() {
    const section = $('renewals');
    if (!section) return;
    const editor = editorInfo();
    section.hidden = editor === null;
    if (!editor) return;
    if (renewals.editing) { keepFocus(() => replaceEdit(section, renewalsEdit())); return; }
    const credentials = credentialsOf(editor);
    const orphans = renewalOrphansOf(editor);
    const off = unavailableText(editor);
    const canEdit = editor.available === true && (credentials.some(c => c.renewal_editable === true) || orphans.length > 0);
    const now = nowSeconds();
    const head = h('div', {class: 'section-head'},
      h('h2', {id: 'renewals-heading', tabindex: '-1'}, 'Claude renewal dates'),
      renewals.saved ? savedLine(renewals.saved) : h('span', {class: 'hint'}, 'When each Claude subscription renews · Codex reports its own'),
      canEdit ? h('div', {class: 'head-actions'}, editButton('Set renewal dates', startRenewals, false, editing(), 'qc-renewals-edit')) : null);
    const alerts = off ? [h('div', {class: 'alert error', role: 'alert'}, off)] : [];
    if (!credentials.length && !orphans.length) {
      settle(section, [head, ...alerts, h('div', {class: 'card'}, h('p', {class: 'empty'}, 'No Claude subscriptions in CPA yet. They are listed here after Quota Cache’s next scan.'))]);
      return;
    }
    const table = credentials.length ? h('div', {id: 'qc-renewals-table', class: 'table-wrap', tabindex: '0', role: 'region', 'aria-label': 'Claude renewal dates by account'}, h('table', null,
      h('thead', null, h('tr', null, ['Account', 'Plan', 'Renews (UTC)', 'Source'].map(title => h('th', {scope: 'col'}, title)))),
      h('tbody', null, credentials.map(credential => {
        const [renews, from] = renewalCells(credential, now);
        return h('tr', null,
          h('td', {class: 'account', title: credential.auth_index}, credentialName(credential), secondary('…' + credential.auth_index.slice(-12) + (credential.disabled ? ' · disabled in CPA' : ''))),
          h('td', null, planOf(credential) || muted('—')), h('td', null, renews), h('td', null, from));
      })))) : null;
    const orphanLines = orphans.map(orphan => h('p', {class: 'orphan'}, 'Saved renewal date for an account no longer in CPA: ', h('span', {class: 'num'}, shortCredentialID(orphan.id)), canEdit ? ' · remove it under Set renewal dates' : ''));
    settle(section, [head, ...alerts, h('div', {class: 'card'}, table, orphanLines,
      h('div', {class: 'card-foot'}, h('p', null, 'An estimate comes from when the subscription started, because Anthropic does not report the renewal date. A date entered here replaces it and repeats monthly, or yearly on an annual plan.')))]);
  }
  function renewalsPlanNow() {
    return renewalPlan(credentialsOf(editorInfo()), renewals.draft, renewalSettingOf, {maxItems: limits(editorInfo()).maxRenewalItems, maxBytes: limits(editorInfo()).maxBytes});
  }
  function renewalsEdit() {
    const editor = editorInfo();
    const busy = renewals.saving;
    const rows = credentialsOf(editor).map((credential, index) => {
      const name = credentialName(credential);
      const who = h('div', {class: 'field'}, h('span', null, name), h('span', {class: 'cap'}, [planOf(credential), credential.disabled ? 'disabled in CPA' : ''].filter(Boolean).join(' · ') || '…' + credential.auth_index.slice(-12)));
      if (credential.renewal_editable !== true || renewals.gone.has(credential.auth_index)) {
        const [renews, from] = renewalCells(credential, nowSeconds());
        return h('div', {class: 'org-edit'}, h('div', {class: 'edit-grid renew-grid'}, who, h('div', {class: 'field'}, renews), h('div', {class: 'field'}, from)),
          renewals.gone.has(credential.auth_index) ? h('p', {class: 'locked'}, 'Quota Cache no longer offers a renewal date for this account, so your change to it was dropped. Cancel, then select Refresh view to see why.') : null);
      }
      const state = renewalField(renewalSettingOf(credential), renewals.draft.rows[credential.auth_index] && renewals.draft.rows[credential.auth_index].date);
      const input = h('input', {id: 'qc-renewal-' + index, type: 'date', value: state.text, min: '2000-01-01', max: '2099-12-31', disabled: busy, 'aria-label': name + ' renews on, UTC', 'aria-describedby': 'qc-renewal-' + index + '-caps',
        on: {input: event => {
          renewals.draft = editRenewal(renewals.draft, Object.assign({}, credential, {renewal: renewalSettingOf(credential)}), name, {kind: 'set', text: event.target.value, incomplete: event.target.validity.badInput || undefined});
          delete renewals.marks[credential.auth_index];
          updateRenewals();
        }}});
      return h('div', {class: 'org-edit', 'data-credential': credential.auth_index},
        h('div', {class: 'edit-grid renew-grid'}, who, h('div', {class: 'field'}, box('', null, input)), h('div', {class: 'field'}, h('span', {id: 'qc-renewal-' + index + '-caps', class: 'caps', 'data-caps': 'date'}))));
    });
    const orphans = liveOrphans(renewalOrphansOf(editor), renewals.gone, renewals.revised).map((orphan, index) => {
      const removing = renewals.draft.remove[orphan.id] !== undefined;
      const idOf = part => 'qc-renewal-orphan-' + index + '-' + part;
      return h('p', {class: 'orphan'}, 'Saved renewal date for an account no longer in CPA: ', h('span', {class: 'num', title: orphan.id}, shortCredentialID(orphan.id)), sep(),
        removing ? [h('span', {class: 'dirty'}, 'removing'), sep(), link('Undo', () => { renewals.draft = markRemoved(renewals.draft, orphan, false); renderAll(); focusFirst(idOf('remove')); }, {id: idOf('undo'), disabled: busy})]
          : link('Remove', () => { renewals.draft = markRemoved(renewals.draft, orphan, true); renderAll(); focusFirst(idOf('undo')); }, {id: idOf('remove'), disabled: busy, 'aria-label': 'Remove the saved renewal date for ' + shortCredentialID(orphan.id)}));
    });
    const form = h('form', {novalidate: true, class: 'card', 'aria-label': 'Subscription renewal dates', 'aria-busy': busy ? 'true' : null, on: {submit: event => { event.preventDefault(); saveRenewals(); }}},
      rows.length ? h('div', {class: 'edit-head renew-grid', 'aria-hidden': 'true'}, h('span', null, 'Account'), h('span', null, 'Renews on, UTC'), h('span', null, 'Source')) : null,
      rows, orphans, h('div', {class: 'foot-slot'}));
    const plan = renewalsPlanNow();
    const nodes = [editHead('renewals-heading', 'Claude renewal dates', renewals, plan.unsaved), form];
    fillRenewals(form, plan);
    return nodes;
  }
  // Under a renewal date field: where the date comes from, or what is about
  // to happen to it (Quota Glance's RenewalCap).
  function renewalCaps(credential, state, index) {
    const busy = renewals.saving;
    const estimateAt = typeof credential.renewal_estimate_at_epoch === 'number' ? credential.renewal_estimate_at_epoch : credential.renewal_source === 'estimated' && typeof credential.renewal_at_epoch === 'number' ? credential.renewal_at_epoch : null;
    const estimate = estimateAt === null ? null : '~' + utcDay(estimateAt);
    const name = credentialName(credential);
    const edit = next => {
      renewals.draft = editRenewal(renewals.draft, Object.assign({}, credential, {renewal: renewalSettingOf(credential)}), name, next);
      delete renewals.marks[credential.auth_index];
      renderAll();
      focusLater('qc-renewal-' + index);
    };
    const undo = () => link('Undo', () => edit(undefined), {disabled: busy});
    const mark = renewals.marks[credential.auth_index];
    if (state.error !== null) return [h('span', {class: 'cap bad'}, state.error, sep(), undo())];
    const out = mark ? [h('span', {class: 'cap ' + mark.tone}, mark.text)] : [];
    if (state.edit && state.edit.kind === 'drop') out.push(h('span', {class: 'cap'}, h('span', {class: 'dirty'}, 'back to the estimate'), sep(), undo()));
    else if (state.changed) out.push(h('span', {class: 'cap'}, h('span', {class: 'dirty'}, 'changed'), sep(), state.stored ? ['was ', h('b', null, utcDay(dateEpoch(state.stored)))] : estimate ? ['was ', h('b', null, estimate), ', estimated'] : 'was not known', sep(), undo()));
    else if (state.source === 'dashboard') out.push(h('span', {class: 'cap'}, setMark(), 'entered here', estimate ? [sep(), 'estimate ', h('b', null, estimate)] : null, sep(), link('Use estimate', () => edit({kind: 'drop'}), {disabled: busy, 'aria-label': 'Use estimate for ' + name})));
    else if (estimate) out.push(h('span', {class: 'cap'}, 'estimate ', h('b', null, estimate), estimateAt - nowSeconds() > 31 * 86400 ? [sep(), 'yearly plan'] : null));
    else out.push(h('span', {class: 'cap'}, 'not known yet'));
    return out;
  }
  function fillRenewals(root, plan) {
    credentialsOf(editorInfo()).forEach((credential, index) => {
      if (credential.renewal_editable !== true) return;
      const holder = root.querySelector('[data-credential="' + CSS.escape(credential.auth_index) + '"]');
      if (!holder) return;
      const state = renewalField(renewalSettingOf(credential), renewals.draft.rows[credential.auth_index] && renewals.draft.rows[credential.auth_index].date);
      // Kept node by node (settle), so the window taking focus again
      // (resync) leaves Use estimate or Undo in place.
      settle(holder.querySelector('[data-caps="date"]'), renewalCaps(credential, state, index));
      const input = within(root, 'qc-renewal-' + index);
      const mark = renewals.marks[credential.auth_index];
      if (input) fillBox(input, '', {changed: state.changed, dropped: Boolean(state.edit && state.edit.kind === 'drop'), bad: state.error !== null || Boolean(mark && mark.tone === 'bad'), busy: renewals.saving});
    });
    const session = sessionState() || {};
    const blocked = session.block || (plan.tooLarge ? 'Too many changes for one save; undo some, save, then make the rest.' : null);
    const foot = root.querySelector('.edit-foot');
    if (foot) { fillFoot(foot, renewals, plan, blocked, session.note || null); return; }
    const slot = root.querySelector('.foot-slot');
    if (slot) slot.replaceWith(editFoot(renewals, 'The date on claude.ai under Settings, Billing. It repeats monthly, or yearly on an annual plan, and replaces the estimate. Codex reports its own date, so Codex accounts are not listed.', plan, blocked, cancelRenewals, session.note || null));
  }
  function updateRenewals() {
    const section = $('renewals');
    if (!section || !renewals.editing) return;
    const plan = renewalsPlanNow();
    fillRenewals(section, plan);
    const head = section.querySelector('.section-head');
    const hint = head && head.querySelector('.hint');
    if (hint && plan.unsaved === 0) hint.remove();
    else if (hint) hint.textContent = plan.unsaved + ' unsaved';
    else if (head && plan.unsaved > 0) head.append(h('span', {class: 'hint'}, plan.unsaved + ' unsaved'));
  }
  async function saveRenewals(confirmed = false) {
    const plan = renewalsPlanNow();
    if (plan.batch === null || renewals.saving) return;
    if (!confirmed && following()) { askFollowing(renewals, saveRenewals); return; }
    renewals.saving = true;
    renewals.status = null;
    renewals.marks = {};
    renderAndRefocus(renewals);
    const outcome = outcomeOf(await send(plan.batch));
    renewals.saving = false;
    switch (outcome.kind) {
      case 'saved':
        setEditing(renewals, false);
        renewals.draft = emptyDraft();
        renewals.overlay = {};
        showSaved(renewals, outcome.unchanged ? 'Already saved. Nothing needed changing.' : 'Saved ' + plural(plan.changes, 'renewal date', 'renewal dates') + '.');
        page.reload();
        break;
      case 'conflict': {
        const marks = {};
        const listed = new Set(credentialsOf(editorInfo()).map(credential => credential.auth_index));
        for (const id of outcome.ids) {
          const current = outcome.current[id];
          if (!listed.has(id)) { orphanAfterConflict(current, renewals.gone, renewals.revised, id); continue; }
          renewals.overlay[id] = isObject(current) ? current : null;
          marks[id] = {text: CONFLICT_FIELD, tone: 'warn'};
        }
        const next = dropConflicts(renewals.draft, outcome.ids);
        renewals.draft = next.draft;
        renewals.marks = marks;
        renewals.status = {tone: 'bad', text: outcome.text};
        renewals.failed = next.retry;
        break;
      }
      case 'field':
        renewals.marks = {[outcome.id]: {text: outcome.text, tone: 'bad'}};
        renewals.status = {tone: 'bad', text: FIELD_FOOT};
        renewals.failed = true;
        break;
      case 'failed':
        // Without a session, or with a key CPA refused, the foot says so
        // itself from the session, for as long as it holds.
        if (outcome.signIn) break;
        if (outcome.code === 'not_editable' && outcome.ids.length > 0) {
          const next = dropRefused(renewals.draft, outcome.ids, shortCredentialID);
          for (const id of outcome.ids) renewals.gone.add(id);
          renewals.draft = next.draft;
          renewals.status = {tone: 'bad', text: refusedRowsText(next.names, next.retry)};
          renewals.failed = next.retry;
          break;
        }
        renewals.status = {tone: 'bad', text: outcome.text};
        renewals.failed = true;
        break;
      default:
        renewals.status = {tone: 'unknown', text: outcome.text};
        renewals.failed = true;
    }
    renderAndRefocus(renewals);
  }

  // ---------------------------------------------------------------------------
  // Rendering and the page's hooks

  function renderAll() {
    renderImport();
    renderOrganizations();
    renderCredits();
    renderRenewals();
  }
  // The import note and every card at rest, for a timer, a dismissal or the
  // same status drawn again, which change only what those show. A card being
  // edited is not rebuilt: its draft cannot hold a date typed halfway, which
  // a rebuild would clear, and the control the reader is on would lose focus.
  function renderAtRest() {
    renderImport();
    if (!orgs.editing) renderOrganizations();
    if (!credits.editing) renderCredits();
    if (!renewals.editing) renderRenewals();
  }
  // A new status: a card being edited keeps its draft, minus rows the status
  // no longer lists, which are dropped and said so. The Organizations card,
  // whose revision covers its whole list, keeps the list its renames and
  // removals were made against instead, so its Save meets the conflict
  // (organizationsHeld). The same status drawn again, as when the Accounts
  // table's provider filter changes, changes nothing a card shows, so a card
  // being edited is not rebuilt.
  //
  // A status can also take away the control the reader is on in a card at
  // rest, leaving nothing under its id for keepFocus to follow. Saving the
  // first organization replaces Add organization with Edit organizations,
  // and clearing the last orphan of a card with no row to edit takes its
  // Edit button away. A save puts focus on what the card showed before the
  // status it asked for arrives (renderAndRefocus), so focus moves on here,
  // and stays in the card (focusAtRest).
  function render(next) {
    const active = document.activeElement;
    const holder = active ? cards.find(one => !one.editing && $(one.section) && $(one.section).contains(active)) : undefined;
    if (next === status) renderAtRest();
    else {
      const before = organizationsInfo();
      status = next;
      if (credits.editing) {
        const held = reconcileCredits(credits.draft, accountsOf(creditDocument()), creditOrphans());
        credits.draft = held.draft;
        if (held.dropped.length) credits.status = {tone: 'warn', text: held.dropped.join(' ')};
      }
      if (renewals.editing) {
        const held = reconcileRenewals(renewals.draft, credentialsOf(editorInfo()), renewalOrphansOf(editorInfo()));
        renewals.draft = held.draft;
        if (held.dropped.length) renewals.status = {tone: 'warn', text: held.dropped.join(' ')};
      }
      if (!orgs.editing) orgs.base = null;
      else if (orgs.base === null) orgs.base = organizationsHeld(before, organizationsInfo(), orgs.draft);
      renderAll();
    }
    const lost = document.activeElement === null || document.activeElement === document.body;
    if (holder && lost && !active.isConnected) focusAtRest(holder);
    // A link from Quota Glance names a section by its fragment, which exists
    // only once a status has drawn it, and can be scrolled to only once the
    // view around it is shown. Until then it is tried again at each render.
    if (!scrolled && /^#(organizations|api-credits|renewals)$/.test(location.hash)) {
      const target = $(location.hash.slice(1));
      if (target && !target.hidden && target.getClientRects().length > 0) {
        scrolled = true;
        target.scrollIntoView();
      }
    }
  }
  // Leaving with unsaved edits asks first, those still to fix included.
  window.addEventListener('beforeunload', event => {
    const unsaved = (orgs.editing && organizationsPlanNow().unsaved > 0) || (credits.editing && creditsPlanNow().unsaved > 0) || (renewals.editing && renewalsPlanNow().unsaved > 0);
    if (unsaved) { event.preventDefault(); event.returnValue = ''; }
  });
  // Save follows the console's session: signing in again with another key in
  // another tab turns it back on without a reload.
  const resync = () => { if (orgs.editing) updateOrganizations(); if (credits.editing) updateCredits(); if (renewals.editing) updateRenewals(); };
  window.addEventListener('focus', resync);
  window.addEventListener('storage', resync);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) resync(); });

  page.attach(Object.freeze({render, editing, logic}));
})(typeof window === 'object' ? window.QuotaCachePage : undefined);
