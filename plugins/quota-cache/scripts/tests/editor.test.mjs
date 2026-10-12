// The page editor's rules: what a reader types, checked as Quota Cache will
// check it; the cycle a refill date gives; the batch a Save sends and its
// byte cap; what the page says of every answer; conflict merging; the
// organizations card's rules; and the one decoding of escaped management
// strings.
//
// app.js and editor.js are loaded exactly as status_page.go joins them, into
// one script, in a context with a stub page: no browser is needed, and no
// request can leave.
//
// Run: node --test plugins/quota-cache/scripts/tests/

import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import {describe, test} from 'node:test'
import vm from 'node:vm'

const assets = new URL('../../internal/plugin/pageassets/', import.meta.url)
const script = readFileSync(new URL('app.js', assets), 'utf8') + '\n' + readFileSync(new URL('editor.js', assets), 'utf8')

// The page as the scripts see it at load: elements that take any property,
// a location that is not CPA's resource route, so app.js sends nothing, and
// storage that holds nothing.
function load() {
  const element = () => new Proxy({}, {get: (target, key) => (key in target ? target[key] : () => {}), set: (target, key, value) => { target[key] = value; return true }})
  const elements = new Map()
  const fetches = []
  const storage = {getItem: () => null, setItem() {}, removeItem() {}}
  const context = vm.createContext({
    addEventListener() {}, location: {pathname: '/elsewhere', protocol: 'http:', origin: 'http://127.0.0.1', host: '127.0.0.1', hash: ''},
    document: {getElementById: id => elements.get(id) || elements.set(id, element()).get(id), createElement: element, addEventListener() {}, hidden: false},
    localStorage: storage, sessionStorage: storage, navigator: {userAgent: 'node'},
    setInterval: () => 0, setTimeout: () => 0, clearTimeout() {}, fetch: (...args) => { fetches.push(args); return new Promise(() => {}) },
    TextEncoder, TextDecoder, URL, Headers: class { constructor(init) { this.init = init } get(name) { return this.init[name] } }, AbortSignal, Intl, Date, BigInt, Math, JSON, Object, Array, Number, String, RegExp, Set, Map, Promise, Proxy, console,
  })
  context.window = context
  vm.runInContext(script, context)
  const page = context.QuotaCachePage
  assert.ok(page, 'app.js defines its namespace')
  return {page, fetches}
}

const {page, fetches} = load()
// The editor's logic: editor.js alone, against a namespace that records what
// attaches to it.
function editorLogic() {
  let editor = null
  const context = vm.createContext({
    window: {QuotaCachePage: {attach: hooks => { editor = hooks }}}, TextEncoder, Intl, Date, BigInt, Math, JSON, Object, Array, Number, String, RegExp, Set, Map, console,
  })
  vm.runInContext(readFileSync(new URL('editor.js', assets), 'utf8'), context)
  assert.ok(editor && editor.logic, 'editor.js attaches its logic')
  return editor.logic
}
const L = editorLogic()

const utc = text => Date.parse(text) / 1000
const limits = {maxItems: 16, maxBytes: 4096}

test('a refusal of a save from a card being edited says to save again, since Refresh view is off', () => {
  const proxy = {reason: 'other', status: 403, until: null}
  assert.match(page.refusalMessage(proxy), /refused the status request \(HTTP 403\).+Select Refresh view to try once more\./)
  assert.match(page.refusalMessage(proxy, 'save'), /refused the save \(HTTP 403\).+Save again to try once more\./)
  assert.doesNotMatch(page.refusalMessage(proxy, 'save'), /Refresh view/)
  assert.match(page.refusalMessage({reason: 'refused', status: 401, until: null}, 'save'), /“Remember password” ticked, then save again\./)
  assert.match(page.refusalMessage({reason: 'banned', status: 403, until: 1}, 'save'), /should be over\. Save again to try once more\./)
  assert.equal(typeof page.forget, 'function', 'the editor can clear a refusal on a deliberate press')
})

test('loading the page sends nothing: there is no session and no CPA route', () => {
  assert.equal(fetches.length, 0)
  assert.equal(Object.isFrozen(page), true, 'the one namespace is frozen')
})

describe('unescapeHost: CPA escapes management strings below schema 6', () => {
  test('decodes the five entities, &amp; last, only while the last status said strings_escaped', () => {
    const status = page.decodeStatus({strings_escaped: true, label: 'o&#39;neil &amp; co', nested: [{sentence: 'Quota Cache&#39;s meter &lt;full&gt; &#34;now&#34;'}], twice: '&amp;#39;', key: 1})
    assert.equal(status.label, "o'neil & co")
    assert.equal(status.nested[0].sentence, 'Quota Cache\'s meter <full> "now"')
    assert.equal(status.twice, '&#39;')
    assert.equal(status.key, 1)
    // Answers until the next status are decoded the same way.
    assert.equal(page.unescapeHost('a &amp; b'), 'a & b')
    assert.deepEqual(page.unescapeAll({error: 'x', ids: ['&#39;']}), {error: 'x', ids: ["'"]})
  })

  test('leaves text alone on a raw host', () => {
    const status = page.decodeStatus({strings_escaped: false, label: 'Tom &amp; Jerry&#39;s'})
    assert.equal(status.label, 'Tom &amp; Jerry&#39;s')
    assert.equal(page.unescapeHost('&lt;b&gt;'), '&lt;b&gt;')
    page.decodeStatus({label: '&amp;'})
    assert.equal(page.unescapeHost('&amp;'), '&amp;', 'a status without the flag is raw')
  })

  test('never escapes keys, and a __proto__ key stays data', () => {
    const status = page.decodeStatus(JSON.parse('{"strings_escaped":true,"entries":{"claude:a&amp;b":"&amp;"},"__proto__":{"polluted":"&amp;"}}'))
    assert.deepEqual(Object.keys(status.entries), ['claude:a&amp;b'])
    assert.equal(status.entries['claude:a&amp;b'], '&')
    assert.equal(Object.getPrototypeOf(status), Object.prototype)
    assert.equal(status.polluted, undefined)
    page.decodeStatus({strings_escaped: false})
  })
})

describe('amounts (readMoney)', () => {
  test('what the page sends, and what it asks about', () => {
    for (const [input, value] of [['200', '200'], ['$200', '200'], [' 200 ', '200'], ['260.5', '260.5'], ['260.50', '260.50'], ['$1,200.50', '1200.50'], ['1,200', '1200'], ['0', '0'], ['$ 1,234,567.89', '1234567.89']]) {
      assert.deepEqual({...L.readMoney(input)}, {kind: 'ok', value}, input)
    }
    assert.deepEqual({...L.readMoney('12,50')}, {kind: 'ambiguous', decimal: '12.50', thousands: '1,250.00'})
    assert.deepEqual({...L.readMoney('1,2')}, {kind: 'ambiguous', decimal: '1.20', thousands: '12.00'})
    for (const input of ['200.123', '-1', '1e3', '12345678', '.5', '5.', '1,2345', 'abc', '$']) assert.notEqual(L.readMoney(input).kind, 'ok', input)
    assert.equal(L.readMoney('').kind, 'empty')
  })

  test('printed as money, and compared by cents, from the string alone', () => {
    assert.equal(L.moneyText('1200.5'), '$1,200.50')
    assert.equal(L.moneyText('007'), '$7.00')
    assert.equal(L.sameAmount('200', '200.00'), true)
    assert.equal(L.sameAmount('260.5', '260.05'), false)
  })
})

describe('dates (readDate)', () => {
  test('a day that exists, in 2000 to 2099', () => {
    assert.deepEqual({...L.readDate('2026-10-29')}, {kind: 'ok', value: '2026-10-29'})
    assert.deepEqual({...L.readDate('2028-02-29')}, {kind: 'ok', value: '2028-02-29'})
    assert.equal(L.readDate('2026-11-31').message, 'November has 30 days.')
    assert.equal(L.readDate('2026-02-30').message, 'February 2026 has 28 days.')
    assert.equal(L.readDate('1999-12-31').message, 'Enter a date between 2000 and 2099.')
    assert.equal(L.readDate('2100-01-01').message, 'Enter a date between 2000 and 2099.')
    assert.equal(L.readDate('2026-1-5').message, 'Enter a full date.')
    assert.equal(L.readDate('', true).message, 'Enter a full date.')
    assert.equal(L.readDate('').kind, 'empty')
  })
})

describe('cycleOn, the port of client.CreditCycleAt', () => {
  // The cycle vectors client/apicredit_test.go's TestCreditCycleAt uses, its
  // date-only rows: the page reads only the YYYY-MM-DD form, which is what
  // the published state carries.
  const vectors = [
    ['2026-10-29', '2026-10-09T12:00:00Z', '2026-09-29', '2026-10-29'],
    ['2026-10-29', '2026-10-29T00:00:00Z', '2026-10-29', '2026-11-29'],
    ['2026-10-31', '2026-11-15T00:00:00Z', '2026-10-31', '2026-11-30'],
    ['2026-10-31', '2027-03-01T00:00:00Z', '2027-02-28', '2027-03-31'],
    ['2026-10-31', '2028-03-01T00:00:00Z', '2028-02-29', '2028-03-31'],
    ['2026-01-30', '2028-02-29T12:00:00Z', '2028-02-29', '2028-03-30'],
    ['2026-01-01', '2026-12-31T23:59:00Z', '2026-12-01', '2027-01-01'],
    ['2026-10-29', '2026-10-30T01:00:00Z', '2026-10-29', '2026-11-29'],
    ['2026-10-29', '2026-10-28T23:59:59Z', '2026-09-29', '2026-10-29'],
    ['2026-09-10', '2026-09-10T04:00:00Z', '2026-09-10', '2026-10-10'],
    ['2031-03-29', '2026-10-09T12:00:00Z', '2026-09-29', '2026-10-29'],
    ['2024-01-29', '2026-10-09T12:00:00Z', '2026-09-29', '2026-10-29'],
    ['2026-01-01', '2027-01-01T00:00:00Z', '2027-01-01', '2027-02-01'],
    ['2026-10-31', '2026-12-01T00:00:00Z', '2026-11-30', '2026-12-31'],
    ['2026-10-31', '2027-02-27T23:00:00Z', '2027-01-31', '2027-02-28'],
    ['2026-01-30', '2027-02-27T12:00:00Z', '2027-01-30', '2027-02-28'],
    ['2026-01-29', '2027-03-01T00:00:00Z', '2027-02-28', '2027-03-29'],
    ['2026-01-29', '2028-02-29T00:00:00Z', '2028-02-29', '2028-03-29'],
    ['2026-12-31', '2027-01-05T00:00:00Z', '2026-12-31', '2027-01-31'],
  ]
  for (const [renews, at, start, end] of vectors) {
    test(`${renews} at ${at}`, () => {
      assert.deepEqual({...L.cycleOn(renews, utc(at))}, {start: utc(`${start}T00:00:00Z`), end: utc(`${end}T00:00:00Z`)})
    })
  }

  test('a date the plugin would not accept has no cycle', () => {
    assert.equal(L.cycleOn('2026-02-30', utc('2026-10-09T12:00:00Z')), null)
    assert.equal(L.cycleOn('', 0), null)
  })

  test('the notes under the refill date being edited', () => {
    assert.equal(L.cycleText(L.cycleOn('2026-10-22', utc('2026-10-09T12:00:00Z'))), 'this cycle Sep 22 – Oct 22')
    assert.equal(L.refillRule('2026-09-20'), 'Refills on the 20th of every month at 00:00 UTC')
    assert.equal(L.refillRule('2026-10-31'), 'Refills on the 31st, or on the last day of shorter months.')
    assert.equal(L.refillRule('2026-10-29'), 'Refills on the 29th, or on the last day of shorter months.')
    assert.deepEqual([1, 2, 3, 4, 11, 12, 13, 21, 22, 23].map(L.ordinal), ['1st', '2nd', '3rd', '4th', '11th', '12th', '13th', '21st', '22nd', '23rd'])
  })
})

// A published account's settings block, as credit_state carries it. Quota
// Cache leaves the config fields empty: the values are the ones entered here.
const settings = extra => ({editable: true, notEditableReason: '', revision: '', monthlyUsd: '', configMonthlyUsd: '', configMonthlyUsdInvalid: false, renews: '', configRenews: '', configRenewsInvalid: false, reading: null, readingUnusedReason: '', updatedAtEpoch: null, ...extra})
const NOW = utc('2026-10-11T12:00:00Z')

describe('Console readings: the window and the refill', () => {
  const context = {now: NOW, renews: '2026-09-29'}
  test('a new reading from 48 hours back to 5 minutes ahead, taken since the refill', () => {
    const at = NOW - 3600
    const reading = L.readingField(settings(), {kind: 'set', amount: '$143.20', at: L.localInputValue(at)}, context)
    assert.deepEqual([reading.atError, reading.amount.error, reading.changed], [null, null, true])
    assert.equal(reading.desired.remainingUsd, '143.20')
    assert.equal(reading.desired.at, L.rfc3339(Math.floor(at / 60) * 60))
    const old = L.readingField(settings(), {kind: 'set', amount: '1', at: L.localInputValue(NOW - L.READING_MAX_AGE - 120)}, context)
    assert.equal(old.atError, L.READING_TIME_MESSAGE)
    const ahead = L.readingField(settings(), {kind: 'set', amount: '1', at: L.localInputValue(NOW + L.READING_MAX_AHEAD + 120)}, context)
    assert.equal(ahead.atError, L.READING_TIME_MESSAGE)
    const edge = L.readingField(settings(), {kind: 'set', amount: '1', at: L.localInputValue(NOW + 240)}, context)
    assert.equal(edge.atError, null, 'four minutes ahead is inside the five allowed')
  })

  test('before the refill, on the refill day, and a stored one sent back as it was', () => {
    const before = L.readingField(settings(), {kind: 'set', amount: '1', at: L.localInputValue(utc('2026-10-10T12:00:00Z'))}, {now: NOW, renews: '2026-10-11'})
    assert.match(before.atError, /^The credit refilled on Oct 11, after this time/)
    const refillDay = L.readingField(settings(), {kind: 'set', amount: '1', at: L.localInputValue(NOW - 600)}, {now: NOW, renews: '2026-10-11'})
    assert.equal(refillDay.onRefillDay, true)
    const stored = {remainingUsd: '180.00', atEpoch: NOW - 7 * 86400, enteredAtEpoch: NOW - 7 * 86400}
    const kept = L.readingField(settings({reading: stored}), {kind: 'set', amount: '180.00', at: L.localInputValue(stored.atEpoch)}, context)
    assert.deepEqual([kept.atError, kept.changed], [null, false], 'a week-old reading resent unchanged is not judged by the clock again')
    assert.deepEqual([L.readingField(settings({reading: stored}), {kind: 'clear'}, context).desired, L.readingField(settings({reading: stored}), {kind: 'clear'}, context).changed], [null, true])
  })
})

describe('fields and the API credit batch', () => {
  const account = (id, extra) => ({id, label: id, settings: settings(extra)})
  const accounts = [account('org-aaaaaaaaaaaa'), account('org-bbbbbbbbbbbb', {revision: '4', monthlyUsd: '260.50'}), account('org-cccccccccccc', {editable: false, notEditableReason: 'settingsUnreadable'})]
  const of = a => a.settings

  test('untouched keeps the value entered here; typing it back is no change; Clear sends null', () => {
    const none = L.creditField(settings(), undefined)
    assert.deepEqual([none.text, none.source, none.desired, none.changed], ['', 'none', null, false])
    const entered = L.creditField(settings({monthlyUsd: '200'}), undefined)
    assert.deepEqual([entered.text, entered.source, entered.desired, entered.changed], ['200', 'dashboard', '200', false])
    assert.deepEqual([L.creditField(settings({monthlyUsd: '200'}), {kind: 'set', text: '$200.00'}).changed], [false])
    const cleared = L.creditField(settings({monthlyUsd: '260.50'}), {kind: 'drop'})
    assert.deepEqual([cleared.text, cleared.desired, cleared.changed], ['', null, true])
    // A configured value an older Quota Cache published is never the field's.
    const configured = L.creditField(settings({configMonthlyUsd: '200', configRenews: '2026-09-29'}), undefined)
    assert.deepEqual([configured.text, configured.source, configured.desired], ['', 'none', null])
    assert.deepEqual([L.refillField(settings({configRenews: '2026-09-29'}), undefined).source], ['none'])
    assert.equal(L.creditField(settings(), {kind: 'set', text: '-1'}).error, L.AMOUNT_MESSAGE)
    assert.equal(L.creditField(settings(), {kind: 'set', text: '12,50'}).error, 'A comma is ambiguous. Did you mean 12.50 or 1,250.00?')
    assert.equal(L.refillField(settings(), {kind: 'set', text: '2026-11-31'}).error, 'November has 30 days.')
  })

  test('one batch: changed editable rows with their revisions, then orphans cleared', () => {
    let draft = L.emptyDraft()
    draft = L.editCredit(draft, accounts[1], 'bravo', 'monthlyUsd', {kind: 'set', text: '275'})
    draft = L.editCredit(draft, accounts[0], 'alpha', 'monthlyUsd', {kind: 'drop'})
    draft = L.markRemoved(draft, {id: 'org-7c1e09a4d2b3', revision: '2'}, true)
    const plan = L.creditPlan(accounts, draft, NOW, of, limits)
    assert.equal(plan.changes, 2, 'Clear on a value not set is no change')
    assert.deepEqual(JSON.parse(JSON.stringify(plan.batch)), {kind: 'apiCredits', items: [
      {id: 'org-bbbbbbbbbbbb', baseRevision: '4', monthlyUsd: '275', renews: null, reading: null},
      {id: 'org-7c1e09a4d2b3', baseRevision: '2', monthlyUsd: null, renews: null, reading: null},
    ]})
    const undone = L.editCredit(draft, accounts[1], 'bravo', 'monthlyUsd', undefined)
    assert.equal(undone.rows['org-bbbbbbbbbbbb'], undefined, 'undoing the last edit drops the row and its revision')
  })

  test('a locked row is never sent, and a field to fix sends nothing', () => {
    const locked = L.editCredit(L.emptyDraft(), accounts[2], 'charlie', 'monthlyUsd', {kind: 'set', text: '5'})
    assert.equal(L.creditPlan(accounts, locked, NOW, of, limits).batch, null)
    const bad = L.editCredit(L.emptyDraft(), accounts[0], 'alpha', 'monthlyUsd', {kind: 'set', text: 'about 30'})
    const plan = L.creditPlan(accounts, bad, NOW, of, limits)
    assert.deepEqual([plan.errors, plan.batch], [1, null])
  })

  test('an edit still to fix is unsaved, so leaving asks first; typed back, it is not', () => {
    const unsaved = edits => {
      let draft = L.emptyDraft()
      for (const [key, edit] of edits) draft = L.editCredit(draft, accounts[1], 'bravo', key, edit)
      const plan = L.creditPlan(accounts, draft, NOW, of, limits)
      return [plan.changes, plan.errors, plan.unsaved]
    }
    assert.deepEqual(unsaved([['monthlyUsd', {kind: 'set', text: '1,50'}]]), [0, 1, 1], 'an ambiguous comma')
    assert.deepEqual(unsaved([['monthlyUsd', {kind: 'set', text: 'abc'}]]), [0, 1, 1], 'not an amount')
    assert.deepEqual(unsaved([['monthlyUsd', {kind: 'set', text: ''}]]), [0, 1, 1], 'a stored value emptied')
    assert.deepEqual(unsaved([['renews', {kind: 'set', text: '', incomplete: true}]]), [0, 1, 1], 'a date half typed')
    assert.deepEqual(unsaved([['reading', {kind: 'set', amount: 'x', at: ''}]]), [1, 2, 1], 'a reading with both parts to fix is one edit')
    assert.deepEqual(unsaved([['monthlyUsd', {kind: 'set', text: '$260.50'}]]), [0, 0, 0], 'typed back')
    const unset = account('org-dddddddddddd')
    const emptied = L.creditPlan([unset], L.editCredit(L.emptyDraft(), unset, 'delta', 'monthlyUsd', {kind: 'set', text: ''}), NOW, of, limits)
    assert.deepEqual([emptied.changes, emptied.errors, emptied.unsaved], [0, 0, 0], 'emptied with nothing set')
    assert.deepEqual(unsaved([['monthlyUsd', {kind: 'set', text: '275'}], ['renews', {kind: 'set', text: '2026-13-01'}]]), [1, 1, 2])
    const removed = L.markRemoved(L.emptyDraft(), {id: 'org-7c1e09a4d2b3', revision: '2'}, true)
    assert.equal(L.creditPlan(accounts, removed, NOW, of, limits).unsaved, 1, 'an orphan marked for removal')

    const credential = {auth_index: '5f2b8c41d09e7a36', renewal_editable: true, renewal: {date: '2026-09-10', revision: '3', updatedAtEpoch: 1}}
    const renewal = edit => {
      const plan = L.renewalPlan([credential], L.editRenewal(L.emptyDraft(), credential, 'x', edit), c => c.renewal, {maxItems: 32, maxBytes: 4096})
      return [plan.changes, plan.errors, plan.unsaved]
    }
    assert.deepEqual(renewal({kind: 'set', text: '', incomplete: true}), [0, 1, 1], 'a renewal date half typed')
    assert.deepEqual(renewal({kind: 'set', text: '2026-09-10'}), [0, 0, 0], 'a renewal date typed back')
    assert.deepEqual(renewal({kind: 'set', text: '2026-09-11'}), [1, 0, 1])
  })

  test('the batch is held to the plugin limits: 4096 bytes and the item count', () => {
    const many = Array.from({length: 17}, (_, i) => account('org-' + String(i).padStart(12, '0')))
    let draft = L.emptyDraft()
    for (const one of many) draft = L.editCredit(draft, one, one.id, 'monthlyUsd', {kind: 'set', text: '1'})
    assert.equal(L.creditPlan(many, draft, NOW, of, limits).tooLarge, true, 'seventeen rows')
    assert.equal(L.creditPlan(many.slice(0, 16), draft, NOW, of, limits).tooLarge, false, 'sixteen rows fit')
    assert.equal(L.creditPlan(many.slice(0, 16), draft, NOW, of, {maxItems: 16, maxBytes: 600}).tooLarge, true, 'past the bytes')
    let renewals = L.emptyDraft()
    const credentials = Array.from({length: 33}, (_, i) => ({auth_index: i.toString(16).padStart(16, '0'), renewal_editable: true, renewal: null}))
    for (const c of credentials) renewals = L.editRenewal(renewals, c, c.auth_index, {kind: 'set', text: '2026-10-29'})
    assert.equal(L.renewalPlan(credentials, renewals, c => c.renewal, {maxItems: 32, maxBytes: 4096}).tooLarge, true)
  })

  test('a renewal date: Use estimate sends null, a stored date keeps its revision', () => {
    const credential = {auth_index: '5f2b8c41d09e7a36', renewal_editable: true, renewal: {date: '2026-09-10', revision: '3', updatedAtEpoch: 1}}
    const draft = L.editRenewal(L.emptyDraft(), credential, 'x', {kind: 'drop'})
    assert.deepEqual(JSON.parse(JSON.stringify(L.renewalPlan([credential], draft, c => c.renewal, {maxItems: 32, maxBytes: 4096}).batch)),
      {kind: 'renewals', items: [{id: '5f2b8c41d09e7a36', baseRevision: '3', date: null}]})
    const orphan = L.markRemoved(L.emptyDraft(), {id: '9c1e00000000a1a1', revision: '2'}, true)
    assert.deepEqual(JSON.parse(JSON.stringify(L.renewalPlan([], orphan, c => c.renewal, {maxItems: 32, maxBytes: 4096}).batch)).items, [{id: '9c1e00000000a1a1', baseRevision: '2', date: null}])
    assert.equal(L.shortCredentialID('9c1e00000000a1a1'), '9c1e…a1a1')
  })

  test('the edit button, which the orphan lines name: Set until an organization is counted or has a credit', () => {
    const none = {id: 'org-cccccccccccc', settings: {monthlyUsd: '', configMonthlyUsd: ''}}
    assert.equal(L.creditsEditLabel([none], 0), 'Set credits and dates', 'an orphan beside organizations with no credit names this button')
    assert.equal(L.creditsEditLabel([], 0), 'Edit credits and dates', 'with only orphans')
    assert.equal(L.creditsEditLabel([none], 1), 'Edit credits and dates')
    assert.equal(L.creditsEditLabel([none, {id: 'org-dddddddddddd', settings: {monthlyUsd: '200'}}], 0), 'Edit credits and dates')
    assert.equal(L.creditsEditLabel([none, {id: 'org-dddddddddddd', settings: {monthlyUsd: '', configMonthlyUsd: '200'}}], 0), 'Set credits and dates', 'a configured credit is no credit')
  })

  test('none counted: the head says to set credits and dates only when every organization lacks one', () => {
    const remedy = '2 organizations · none counted yet: set each one’s monthly credit and refill date'
    const needs = {state: 'needsSettings', dataIssues: ['needsRefillDate']}
    const pendingUnset = {state: 'pending', dataIssues: ['meterMissing', 'needsCredit', 'needsRefillDate']}
    assert.equal(L.noneCountedText([needs, pendingUnset]), remedy)
    // A meter that has not saved, or cannot be read, leaves organizations
    // with both values set pending: the remedy is the meter's, which each
    // row's issue names.
    const pendingSet = {state: 'pending', dataIssues: ['meterMissing']}
    assert.equal(L.noneCountedText([pendingSet, {state: 'pending', dataIssues: ['orgNotCounted']}]), '2 organizations · none counted yet')
    assert.equal(L.noneCountedText([needs, pendingSet]), '2 organizations · none counted yet', 'one of them waits on something else')
  })
})

describe('a new Console reading is judged by the refill date its row keeps', () => {
  // The plugin checks a new reading against the row's refill date, the one
  // entered here or typed now, and against nothing else.
  const stored = {id: 'org-eeeeeeeeeeee', label: 'echo', settings: settings({revision: '6', renews: '2026-10-11'})}
  const unset = {id: 'org-dddddddddddd', label: 'delta', settings: settings({configRenews: '2026-10-11'})}
  const of = a => a.settings
  const at = utc('2026-10-10T12:00:00Z')
  const reading = {kind: 'set', amount: '150', at: L.localInputValue(at)}
  const planOf = (account, edits) => {
    let draft = L.emptyDraft()
    for (const [key, edit] of edits) draft = L.editCredit(draft, account, account.label, key, edit)
    return L.creditPlan([account], draft, NOW, of, limits)
  }

  test('the row’s own new date, before the one entered here, takes the reading in its cycle', () => {
    const plan = planOf(stored, [['renews', {kind: 'set', text: '2026-10-05'}], ['reading', reading]])
    assert.equal(plan.errors, 0)
    assert.deepEqual(JSON.parse(JSON.stringify(plan.batch)).items, [{id: 'org-eeeeeeeeeeee', baseRevision: '6', monthlyUsd: null, renews: '2026-10-05', reading: {remainingUsd: '150', at: '2026-10-10T12:00:00Z'}}])
  })

  test('a date entered here and left as it is refuses a reading from before it, and flags the refill day', () => {
    const before = planOf(stored, [['reading', reading]])
    assert.deepEqual([before.errors, before.batch], [1, null])
    const row = L.creditRow(stored.settings, {reading}, NOW)
    assert.match(row.reading.atError, /^The credit refilled on Oct 11, after this time/)
    const sameDay = L.creditRow(stored.settings, {reading: {kind: 'set', amount: '150', at: L.localInputValue(NOW - 600)}}, NOW)
    assert.deepEqual([sameDay.reading.atError, sameDay.reading.onRefillDay], [null, true])
  })

  test('with no date, nothing refuses the reading: not a configured date, and not one cleared', () => {
    const plan = planOf(unset, [['reading', reading]])
    assert.equal(plan.errors, 0)
    assert.deepEqual(JSON.parse(JSON.stringify(plan.batch)).items, [{id: 'org-dddddddddddd', baseRevision: '', monthlyUsd: null, renews: null, reading: {remainingUsd: '150', at: '2026-10-10T12:00:00Z'}}])
    const cleared = L.creditRow(stored.settings, {renews: {kind: 'drop'}, reading}, NOW)
    assert.deepEqual([cleared.reading.atError, cleared.renews.desired], [null, null])
  })
})

describe('a 409 not_editable', () => {
  test('drops the refused rows by name and keeps the rest to save', () => {
    const draft = {rows: {'org-a': {baseRevision: '1', name: 'alpha', monthlyUsd: {kind: 'set', text: '5'}}, 'org-b': {baseRevision: '2', name: 'bravo', monthlyUsd: {kind: 'set', text: '6'}}}, remove: {'org-c': '3'}}
    const one = L.dropRefused(draft, ['org-b'], id => id)
    assert.deepEqual(Object.keys(one.draft.rows), ['org-a'])
    assert.deepEqual({...one.draft.remove}, {'org-c': '3'})
    assert.deepEqual([...one.names], ['bravo'])
    assert.equal(one.retry, true)
    assert.equal(L.refusedRowsText(one.names, one.retry), 'Not saved. bravo can no longer be edited here, so its change was dropped. Your other changes are kept.')
    // An orphan removal is named by its id; with nothing left, nothing is said to be kept.
    const all = L.dropRefused(draft, ['org-a', 'org-b', 'org-c'], id => 'id ' + id)
    assert.deepEqual([...all.names], ['alpha', 'bravo', 'id org-c'])
    assert.equal(all.retry, false)
    assert.equal(L.refusedRowsText(all.names, all.retry), 'Not saved. alpha, bravo and id org-c can no longer be edited here, so their changes were dropped.')
    // The next Save sends only what is left.
    const plan = L.creditPlan([{id: 'org-a', settings: {editable: true, revision: '1', monthlyUsd: '', configMonthlyUsd: '', renews: '', configRenews: '', reading: null}}], one.draft, NOW, account => account.settings, limits)
    assert.deepEqual([...plan.batch.items.map(item => item.id)], ['org-a', 'org-c'])
    assert.equal(L.refusedRowsText([], false), L.saveErrorText('not_editable'), 'ids the draft does not hold')
  })
})

describe('a 409 conflict', () => {
  test('drops only the conflicting rows, and retries only while one is left', () => {
    const draft = {rows: {a: {baseRevision: '1'}, b: {baseRevision: '2'}}, remove: {c: '3'}}
    const one = L.dropConflicts(draft, ['a'])
    assert.deepEqual(Object.keys(one.draft.rows), ['b'])
    assert.deepEqual({...one.draft.remove}, {c: '3'})
    assert.equal(one.retry, true)
    assert.equal(L.dropConflicts(draft, ['a', 'b', 'c']).retry, false)
  })

  test('an orphan another device cleared goes; one stored again is marked at its new revision', () => {
    const orphans = [{id: 'org-a', revision: '3'}, {id: 'org-b', revision: '4'}, {id: 'org-c', revision: '5'}]
    const gone = new Set()
    const revised = {}
    L.orphanAfterConflict({revision: ''}, gone, revised, 'org-a')
    L.orphanAfterConflict(null, gone, revised, 'org-c')
    L.orphanAfterConflict({revision: '13', monthlyUsd: '50'}, gone, revised, 'org-b')
    const live = L.liveOrphans(orphans, gone, revised)
    assert.deepEqual(JSON.parse(JSON.stringify(live.map(orphan => [orphan.id, orphan.revision]))), [['org-b', '13']])
    // Marked for removal again, it sends the revision stored now.
    const draft = L.markRemoved(L.emptyDraft(), live[0], true)
    assert.equal(L.creditPlan([], draft, NOW, s => s, limits).batch.items[0].baseRevision, '13')
    assert.equal(orphans[1].revision, '4', 'the status itself is left as it was')
  })

  test('marks the fields that moved and those the reader changed', () => {
    const before = settings({monthlyUsd: '260.50', renews: '2026-09-20'})
    const current = settings({monthlyUsd: '280', renews: '2026-09-20', revision: '5'})
    assert.deepEqual([...L.conflictFields(before, current, undefined)], ['monthlyUsd'])
    assert.deepEqual([...L.conflictFields(before, current, {reading: {kind: 'clear'}})], ['monthlyUsd', 'reading'])
    assert.equal(L.newerRevision('10', '9'), true)
    assert.equal(L.newerRevision('', '0'), false)
  })

  test('the merged block shows until the published one moves on, even a row another device cleared', () => {
    const published = settings({revision: '12', monthlyUsd: '260.50'})
    // Cleared elsewhere: nothing is stored, so the conflict's revision is "".
    const cleared = {settings: settings({revision: '', monthlyUsd: '', editable: false}), over: '12'}
    const shown = L.overlaid(published, cleared)
    assert.deepEqual([shown.revision, shown.monthlyUsd, shown.editable], ['', '', true], 'the published verdict on editing is kept')
    assert.equal(L.overlaid(settings({revision: ''}), cleared).revision, '', 'the published block caught up')
    assert.equal(L.overlaid(settings({revision: '15', monthlyUsd: '300'}), cleared).monthlyUsd, '300', 'a later save elsewhere is newer news')
    const newer = {settings: settings({revision: '14', monthlyUsd: '280'}), over: '12'}
    assert.equal(L.overlaid(published, newer).monthlyUsd, '280')
    assert.equal(L.overlaid(settings({revision: '14', monthlyUsd: '280', readingUnusedReason: 'tooOld'}), newer).readingUnusedReason, 'tooOld', 'the same revision published brings its verdicts')
    assert.equal(L.overlaid(published, undefined), published)
    // The next save of a cleared row starts from nothing stored.
    const draft = L.editCredit(L.emptyDraft(), {id: 'org-a', settings: L.overlaid(published, cleared)}, 'alpha', 'monthlyUsd', {kind: 'set', text: '300'})
    assert.equal(draft.rows['org-a'].baseRevision, '')
  })

  test('the answer becomes a conflict with the current blocks', () => {
    const outcome = L.outcomeOf({kind: 'answer', status: 409, body: {error: 'conflict', revision: '5', conflicts: ['a'], current: {a: {monthlyUsd: '280'}}}})
    assert.deepEqual([outcome.kind, [...outcome.ids], outcome.current.a.monthlyUsd, outcome.text], ['conflict', ['a'], '280', L.CONFLICT_FOOT])
    const organizations = L.outcomeOf({kind: 'answer', status: 409, body: {error: 'conflict', revision: '4', organizations: [{id: 'org-1', label: 'team'}]}})
    assert.equal(organizations.organizations[0].label, 'team')
  })

  test('the organizations card keeps its unsaved add row and drops the rest', () => {
    const draft = {rename: {'org-1': 'x'}, remove: {'org-2': true}, add: {label: 'team-2', organizationId: 'abc'}}
    assert.deepEqual(JSON.parse(JSON.stringify(L.organizationsAfterConflict(draft))), {rename: {}, remove: {}, add: {label: 'team-2', organizationId: 'abc'}})
  })
})

describe('organizations: the rules the save route applies to an organizations batch', () => {
  const items = [
    {id: 'org-000000000001', label: 'personal-1', organization_id: '00000000-0000-4000-8000-000000000001'},
    {id: 'org-000000000002', label: 'personal-2', organization_id: '00000000-0000-4000-8000-000000000002'},
  ]
  const orgLimits = {maxItems: 16, maxOps: 16, maxBytes: 4096}
  const draftWith = extra => ({...L.emptyOrganizationsDraft(), ...extra})

  test('NormalizeOrganizationID, ported: trimmed, lower-cased, never the nil UUID', () => {
    assert.equal(L.normalizeOrganizationID('  00000000-0000-4000-8000-0000000000AA \n'), '00000000-0000-4000-8000-0000000000aa')
    for (const bad of ['', '00000000-0000-0000-0000-000000000000', '00000000000040008000000000000001', '00000000-0000-4000-8000-00000000000g', '{00000000-0000-4000-8000-000000000001}', '0000 0000-0000-4000-8000-000000000001']) {
      assert.equal(L.normalizeOrganizationID(bad), null, bad)
    }
  })

  test('labels: 1 to 64 printable characters with no surrounding spaces', () => {
    assert.equal(L.validOrganizationLabel('team'), true)
    assert.equal(L.validOrganizationLabel("o'neil & co"), true)
    assert.equal(L.validOrganizationLabel('é'.repeat(64)), true)
    assert.equal(L.validOrganizationLabel('a'.repeat(65)), false)
    // Characters, not UTF-16 units: 64 emoji are 128 units and still a label.
    assert.equal(L.validOrganizationLabel('\u{1F600}'.repeat(64)), true)
    assert.equal(L.validOrganizationLabel('\u{1F600}'.repeat(65)), false)
    for (const bad of ['', ' team', 'team ', 'te\tam', 'te‍am', 'te am']) assert.equal(L.validOrganizationLabel(bad), false, JSON.stringify(bad))
  })

  test('one batch: removes, then renames, then the add, with the ID as it will be saved', () => {
    const draft = draftWith({rename: {'org-000000000001': ' personal-one '}, remove: {'org-000000000002': true}, add: {label: 'personal-2', organizationId: ' 00000000-0000-4000-8000-0000000000AA'}})
    const plan = L.organizationsPlan(items, draft, '4', orgLimits)
    assert.equal(plan.count, 0)
    assert.equal(plan.changes, 3)
    assert.equal(plan.normalized, '00000000-0000-4000-8000-0000000000aa')
    assert.deepEqual(JSON.parse(JSON.stringify(plan.batch)), {kind: 'organizations', baseRevision: '4', ops: [
      {op: 'remove', id: 'org-000000000002'},
      {op: 'rename', id: 'org-000000000001', label: 'personal-one'},
      {op: 'add', label: 'personal-2', organizationId: '00000000-0000-4000-8000-0000000000aa'},
    ]}, 'a removed label is free again for the add, as the plugin applies the ops in order')
    assert.deepEqual([...plan.refs.map(ref => ref.row)], ['org-000000000002', 'org-000000000001', 'add'])
  })

  test('a rename typed back is no change; an empty add row is nothing', () => {
    const plan = L.organizationsPlan(items, draftWith({rename: {'org-000000000001': 'personal-1 '}}), '4', orgLimits)
    assert.deepEqual([plan.changes, plan.unsaved, plan.batch], [0, 0, null])
  })

  test('an edit still to fix is unsaved, as on the other cards', () => {
    const plan = L.organizationsPlan(items, draftWith({rename: {'org-000000000001': ''}, add: {label: 'x', organizationId: 'nope'}}), '4', orgLimits)
    assert.deepEqual([plan.changes, plan.count, plan.unsaved], [2, 2, 2])
  })

  test('every rule the plugin applies is said under its field first', () => {
    const errors = draft => ({...L.organizationsPlan(items, draftWith(draft), '4', orgLimits).errors})
    assert.match(errors({rename: {'org-000000000001': 'PERSONAL-2'}})['org-000000000001:label'], /already has this label/)
    assert.match(errors({rename: {'org-000000000001': ''}})['org-000000000001:label'], /^Enter a label/)
    assert.match(errors({add: {label: 'Personal-1', organizationId: '00000000-0000-4000-8000-000000000003'}})['add:label'], /already has this label/)
    assert.match(errors({add: {label: 'x', organizationId: '00000000-0000-4000-8000-000000000001'}})['add:organizationId'], /already listed/)
    assert.match(errors({add: {label: 'x', organizationId: 'not an id'}})['add:organizationId'], /^Enter the Organization ID/)
    assert.match(errors({add: {label: '', organizationId: '00000000-0000-4000-8000-000000000003'}})['add:label'], /^Enter a label/)
    const full = Array.from({length: 16}, (_, i) => ({id: 'org-' + i, label: 'o' + i, organization_id: '00000000-0000-4000-8000-' + String(i + 1).padStart(12, '0')}))
    assert.match(L.organizationsPlan(full, draftWith({add: {label: 'x', organizationId: '00000000-0000-4000-8000-0000000000ff'}}), '4', orgLimits).errors['add:'], /^16 organizations at most/)
    const freed = L.organizationsPlan(full, draftWith({remove: {'org-0': true}, add: {label: 'x', organizationId: '00000000-0000-4000-8000-0000000000ff'}}), '4', orgLimits)
    assert.equal(freed.count, 0, 'a removal in the same batch makes room')
  })

  test('more ops than the plugin takes, or past 4096 bytes, is too large', () => {
    const many = Array.from({length: 17}, (_, i) => ({id: 'org-' + i, label: 'o' + i, organization_id: '00000000-0000-4000-8000-' + String(i + 1).padStart(12, '0')}))
    const remove = Object.fromEntries(many.map(item => [item.id, true]))
    assert.equal(L.organizationsPlan(many, draftWith({remove}), '4', orgLimits).tooLarge, true)
    assert.equal(L.organizationsPlan(many, draftWith({remove: {'org-0': true}}), '4', {...orgLimits, maxBytes: 20}).tooLarge, true)
  })

  test('a newer status keeps the list renames and removals were made against, so Save meets the conflict', () => {
    const before = {revision: '4', items}
    const after = {revision: '5', items: [{...items[0], label: 'theirs'}, items[1]]}
    const renamed = draftWith({rename: {'org-000000000001': 'mine'}})
    assert.deepEqual(JSON.parse(JSON.stringify(L.organizationsHeld(before, after, renamed))), {revision: '4', items: JSON.parse(JSON.stringify(items))})
    const removed = draftWith({remove: {'org-000000000002': true}})
    assert.equal(L.organizationsHeld(before, after, removed).revision, '4')
    assert.equal(L.organizationsHeld(before, {...after, revision: '4'}, renamed), null, 'the same revision moved nothing')
    assert.equal(L.organizationsHeld(before, after, L.emptyOrganizationsDraft()), null, 'nothing typed yet: the newer list, with no conflict to meet')
    assert.equal(L.organizationsHeld(before, after, draftWith({rename: {'org-000000000001': 'personal-1 '}})), null, 'a rename typed back is no edit')
    assert.equal(L.organizationsHeld(before, after, draftWith({add: {label: 'x', organizationId: '00000000-0000-4000-8000-0000000000aa'}})), null, 'an add is judged against the newer list')
    // The batch then goes out at the revision the rename was made against.
    const held = L.organizationsHeld(before, after, renamed)
    assert.equal(L.organizationsPlan(held.items, renamed, held.revision, orgLimits).batch.baseRevision, '4')
  })

  test('the conflict names the renames and removals it dropped', () => {
    assert.equal(L.organizationsConflictText([]), 'Not saved. Another device changed the organizations first. The list now shows theirs; check it, then save again.')
    assert.equal(L.organizationsConflictText(['personal-1']), 'Not saved. Another device changed the organizations first. The list now shows theirs, so your change to personal-1 was dropped; check it, then make it again.')
    assert.equal(L.organizationsConflictText(['personal-1', 'personal-2']), 'Not saved. Another device changed the organizations first. The list now shows theirs, so your changes to personal-1 and personal-2 were dropped; check it, then make them again.')
  })

  test('the ID shortened at rest to its first 8 and last 4 characters', () => {
    assert.equal(L.shortOrganizationID('00000000-0000-4000-8000-000000000001'), '00000000…0001')
  })

  test('Remove says what an add brings back', () => {
    assert.equal(L.removeQuestion('team'), 'Remove team? Its amounts and readings entered here stay as an orphan you can clear below, and adding the same Organization ID again brings them back.')
  })

  test('the notes: the configuration ignored, and an unreadable file', () => {
    assert.equal(L.configIgnoredText(5), 'Quota Cache’s configuration still lists 5 organizations under claude-api-credits. This version doesn’t read them: add each one here with its label and Organization ID. ' +
      'You can leave the configuration as it is; only an older Quota Cache reads it.')
    assert.match(L.configIgnoredText(1), /^Quota Cache’s configuration still lists 1 organization under claude-api-credits\. This version doesn’t read it: /)
    assert.equal(L.organizationsUnreadableText('snapshot.organizations.json'), 'Quota Cache cannot read its organizations file, so no organization is counted and editing them here is unavailable. ' +
      'Fix the file’s owner or permissions, or move snapshot.organizations.json aside and add the organizations again. The page picks that up within a few seconds.')
  })

  test('the plugin naming an op by index lands under that op', () => {
    const outcome = L.outcomeOf({kind: 'answer', status: 400, body: {error: 'duplicate_label', op: 1, field: 'label'}})
    assert.deepEqual([outcome.kind, outcome.op, outcome.field], ['operation', 1, 'label'])
    assert.match(outcome.text, /already has this label/)
  })
})

describe('every answer the save route gives has copy', () => {
  test('each plugin code is said in its own words', () => {
    const generic = L.saveErrorText('', 500)
    for (const code of L.PLUGIN_CODES) {
      const text = L.FIELD_CODES.has(code) || L.OPERATION_CODES.has(code) ? L.fieldMessage(code) : L.saveErrorText(code, 400)
      assert.ok(text && text !== generic && text !== 'Check this value.', code)
    }
    for (const code of ['cache_stopped', 'cross_site', 'unsupported_media_type', 'invalid_request', 'settings_unavailable', 'organizations_unavailable', 'not_editable', 'conflict', 'too_many_writes', 'settings_full', 'settings_unwritable',
      'invalid_monthly_usd', 'invalid_renews', 'invalid_date', 'invalid_reading_amount', 'invalid_reading_time', 'reading_before_refill', 'invalid_label', 'duplicate_label', 'invalid_organization_id', 'duplicate_organization', 'too_many_organizations', 'unknown_id']) {
      assert.ok(L.PLUGIN_CODES.has(code), code)
    }
  })

  test('429 says when to try again, from Retry-After', () => {
    const outcome = L.outcomeOf({kind: 'answer', status: 429, body: {error: 'too_many_writes'}, retryAfter: '60'})
    assert.equal(outcome.text, 'Too many saves in the last minute, so nothing was saved. Try again in a minute.')
    assert.equal(L.retryText('300'), 'in 5 minutes')
    assert.equal(L.retryText(null), 'in a minute')
  })

  test('answers that are not the plugin, and answers lost', () => {
    assert.equal(L.outcomeOf({kind: 'lost'}).kind, 'lost')
    assert.equal(L.outcomeOf({kind: 'answer', status: 200, body: null}).kind, 'lost', 'a 200 without its body is an answer lost')
    assert.equal(L.outcomeOf({kind: 'answer', status: 502, body: null}).kind, 'lost')
    assert.match(L.outcomeOf({kind: 'answer', status: 404, body: null}).text, /did not offer the save route/)
    assert.match(L.outcomeOf({kind: 'answer', status: 400, body: {error: 'whatever'}}).text, /refused the save \(HTTP 400\)/)
    const saved = L.outcomeOf({kind: 'answer', status: 200, body: {ok: true, unchanged: false, revision: '7', settings: {a: null}}})
    assert.deepEqual([saved.kind, saved.unchanged, saved.revision], ['saved', false, '7'])
    const field = L.outcomeOf({kind: 'answer', status: 400, body: {error: 'invalid_reading_time', id: 'org-a', field: 'reading.at'}})
    assert.deepEqual([field.kind, field.id, field.field, field.text], ['field', 'org-a', 'reading.at', L.READING_TIME_REFUSED])
    // Quota Cache judges the time by CPA's clock, which can refuse a time
    // this device's clock passed, so its refusal names that clock.
    assert.notEqual(L.READING_TIME_REFUSED, L.READING_TIME_MESSAGE)
    assert.match(L.READING_TIME_REFUSED, /48 hours ago to 5 minutes ahead by CPA’s clock/)
    const locked = L.outcomeOf({kind: 'answer', status: 409, body: {error: 'not_editable', id: 'a', ids: ['a', 'b']}})
    assert.deepEqual([locked.kind, [...locked.ids]], ['failed', ['a', 'b']])
  })

  test('nothing sent, or a key refused: the reason, and no other try', () => {
    assert.equal(L.outcomeOf({kind: 'blocked', reason: 'session'}).text, L.SIGN_IN_FOOT)
    const refused = L.outcomeOf({kind: 'refused', record: {reason: 'refused', status: 401}})
    assert.equal(refused.signIn, true)
    assert.match(refused.text, /^CPA refused the remembered management key, so nothing was saved/)
    assert.match(L.outcomeOf({kind: 'blocked', reason: 'refused', record: {reason: 'banned', until: NOW + 600, status: 403}}).text, /refusing management requests from this address until about/)
  })

  test('a refused key keeps Save off; a refusal that was not about the key leaves one deliberate try', () => {
    const running = () => true
    const over = () => false
    assert.equal(L.refusalHold(null, running), null)
    const key = L.refusalHold({reason: 'refused', status: 401}, over)
    assert.match(key.block, /Save stays off until the console holds a different key\. Only Refresh view, once no card is open, sends this key again\.$/)
    assert.doesNotMatch(key.block, /never sends/, 'Refresh view sends a refused key again')
    assert.equal(key.note, undefined)
    const ban = {reason: 'banned', until: NOW + 600, status: 403}
    assert.match(L.refusalHold(ban, running).block, /until about .+ Save turns back on then\.$/)
    const ended = L.refusalHold(ban, over)
    assert.equal(ended.block, undefined, 'a lockout that is over leaves Save on')
    assert.match(ended.note, /^CPA’s lockout of this address after repeated failed sign-ins should be over\. .+ Select Save to try once more\.$/)
    const proxy = L.refusalHold({reason: 'other', status: 403}, over)
    assert.match(proxy.note, /^Something in front of CPA refused the save \(HTTP 403\).+ Select Save to try once more\.$/)
    assert.doesNotMatch(proxy.note, /different key/)
  })
})

describe('saved values', () => {
  test('an unreadable settings file: put back a good copy, or move it aside and enter the values again', () => {
    assert.equal(L.settingsUnreadableText('snapshot.settings.json'), 'Quota Cache cannot read its saved settings, so no value saved here applies. To keep the values saved here, put back a good copy of snapshot.settings.json. ' +
      'Moving it aside instead starts Quota Cache empty, and every value saved here has to be entered again.')
  })

  test('values held while the organizations file cannot be read are saved here, and no orphans', () => {
    const editor = {credentials: [], renewal_orphans: [], api_credit_orphans: [], held_api_credits: [{id: 'org-a', monthlyUsd: '200', renews: '', hasReading: true}]}
    assert.deepEqual([...L.heldCreditsOf(editor).map(row => row.id)], ['org-a'])
    assert.equal(L.creditOrphansOf({orphans: []}, editor).length, 0)
    assert.equal(L.heldCreditsOf({}).length, 0)
  })

  test('with no organization listed, the orphans are the editor\'s', () => {
    const editor = {credentials: [], renewal_orphans: [], api_credit_orphans: [{id: 'org-a', monthlyUsd: '200', renews: '2026-10-29', hasReading: true}]}
    assert.deepEqual(L.creditOrphansOf({orphans: [{id: 'org-b'}]}, editor).map(orphan => orphan.id), ['org-b'], 'api_credits carries its own orphans while it is published')
    assert.deepEqual(L.creditOrphansOf(null, editor).map(orphan => orphan.id), ['org-a'])
    assert.equal(L.creditOrphansOf(null, null).length, 0)
  })
})
