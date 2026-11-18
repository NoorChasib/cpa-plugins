// The OpenAI API credit's lane on the page: the organization as OpenAI's
// responses name it, checked as Quota Cache checks it; the batches its two
// cards send; its words, which never name Console; which of its cards a
// status shows; and the organizations seen in traffic it offers to add.
//
// editor.js is loaded alone, as in editor.test.mjs, against a stub page that
// has no session, so nothing it does can send a request. The cards are drawn
// into a small stand-in for the document: enough of the DOM for the editor's
// own calls, and nothing more.
//
// Run: node --test plugins/quota-cache/scripts/tests/

import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import {describe, test} from 'node:test'
import vm from 'node:vm'

const assets = new URL('../../internal/plugin/pageassets/', import.meta.url)
const editorSource = readFileSync(new URL('editor.js', assets), 'utf8')
// The published OpenAI credit state Quota Cache's own tests compute.
const golden = JSON.parse(readFileSync(new URL('../../internal/credits/testdata/golden/degraded-states.openai-credits.json', import.meta.url), 'utf8'))

// ---------------------------------------------------------------------------
// A document for the editor

// One compound of a selector: a tag, classes, [attr="value"], #id and
// :not(:disabled), or :scope. Anything else is refused, so a selector the
// editor starts to use is never matched loosely.
function compound(text) {
  if (text === ':scope') return {scope: true}
  const part = {tag: null, classes: [], attrs: [], id: null, enabled: false}
  const pattern = /[a-z]+|\.[\w-]+|#[\w-]+|\[([\w-]+)="([^"]*)"\]|:not\(:disabled\)/giy
  let match
  while (pattern.lastIndex < text.length && (match = pattern.exec(text))) {
    const token = match[0]
    if (token[0] === '.') part.classes.push(token.slice(1))
    else if (token[0] === '#') part.id = token.slice(1)
    else if (token[0] === '[') part.attrs.push([match[1], match[2]])
    else if (token[0] === ':') part.enabled = true
    else part.tag = token.toUpperCase()
  }
  if (!match) throw new Error('unsupported selector ' + text)
  return part
}
// A selector as steps, each with the combinator before it: ' ' or '>'.
function steps(selector) {
  const out = []
  let combinator = ' '
  // Whitespace inside [attr="value"] belongs to the value.
  for (const token of selector.trim().match(/(?:\[[^\]]*\]|[^\s[])+/g)) {
    if (token === '>') { combinator = '>'; continue }
    out.push({combinator, part: compound(token)})
    combinator = ' '
  }
  return out
}

class FakeText {
  constructor(text) { this.data = String(text); this.parentNode = null }
  get textContent() { return this.data }
  isEqualNode(other) { return other instanceof FakeText && other.data === this.data }
  remove() { if (this.parentNode) this.parentNode.removeChild(this) }
}
class FakeElement {
  constructor(tag) { this.tagName = tag.toUpperCase(); this.attrs = new Map(); this.childNodes = []; this.parentNode = null; this.listeners = {}; this.hidden = false; this.disabled = false }
  setAttribute(name, value) { this.attrs.set(name, String(value)) }
  getAttribute(name) { return this.attrs.has(name) ? this.attrs.get(name) : null }
  hasAttribute(name) { return this.attrs.has(name) }
  removeAttribute(name) { this.attrs.delete(name) }
  get attributes() { return [...this.attrs].map(([name, value]) => ({name, value})) }
  get id() { return this.getAttribute('id') || '' }
  get parentElement() { return this.parentNode }
  get className() { return this.getAttribute('class') || '' }
  set className(value) { this.setAttribute('class', value) }
  get classes() { return this.className.split(/\s+/).filter(Boolean) }
  addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener) }
  click() { for (const listener of this.listeners.click || []) listener({currentTarget: this, preventDefault() {}}) }
  adopt(node) { const child = typeof node === 'string' ? new FakeText(node) : node; if (child.parentNode) child.parentNode.removeChild(child); child.parentNode = this; return child }
  append(...nodes) { for (const node of nodes) this.childNodes.push(this.adopt(node)) }
  prepend(...nodes) { this.childNodes.unshift(...nodes.map(node => this.adopt(node))) }
  after(...nodes) { const parent = this.parentNode; const at = parent.childNodes.indexOf(this) + 1; parent.childNodes.splice(at, 0, ...nodes.map(node => parent.adopt(node))) }
  removeChild(node) { const at = this.childNodes.indexOf(node); if (at >= 0) this.childNodes.splice(at, 1); node.parentNode = null; return node }
  replaceChild(next, old) { const child = this.adopt(next); this.childNodes[this.childNodes.indexOf(old)] = child; old.parentNode = null; return old }
  replaceWith(node) { this.parentNode.replaceChild(node, this) }
  replaceChildren(...nodes) { for (const child of [...this.childNodes]) this.removeChild(child); this.append(...nodes) }
  remove() { if (this.parentNode) this.parentNode.removeChild(this) }
  get lastChild() { return this.childNodes[this.childNodes.length - 1] || null }
  get textContent() { return this.childNodes.map(child => child.textContent).join('') }
  set textContent(text) { this.replaceChildren(String(text)) }
  isEqualNode(other) {
    if (!(other instanceof FakeElement) || other.tagName !== this.tagName || other.attrs.size !== this.attrs.size || other.childNodes.length !== this.childNodes.length) return false
    for (const [name, value] of this.attrs) if (other.getAttribute(name) !== value) return false
    return this.childNodes.every((child, i) => child.isEqualNode(other.childNodes[i]))
  }
  contains(node) { for (let at = node; at; at = at.parentNode) if (at === this) return true; return false }
  get isConnected() { let at = this; while (at.parentNode) at = at.parentNode; return at.tagName === 'BODY' }
  getClientRects() { return this.isConnected && !this.hidden ? [{}] : [] }
  focus() {}
  scrollIntoView() {}
  get descendants() { return this.childNodes.filter(child => child instanceof FakeElement).flatMap(child => [child, ...child.descendants]) }
  matches(part, scope) {
    if (part.scope) return this === scope
    return (!part.tag || part.tag === this.tagName) && part.classes.every(name => this.classes.includes(name)) &&
      part.attrs.every(([name, value]) => this.getAttribute(name) === value) && (!part.id || this.id === part.id) && (!part.enabled || !this.disabled)
  }
  querySelectorAll(selector) {
    const chain = steps(selector)
    const fits = (element, i) => {
      if (!element.matches(chain[i].part, this)) return false
      if (i === 0) return chain[0].part.scope || this.contains(element)
      if (chain[i].combinator === '>') return !!element.parentNode && fits(element.parentNode, i - 1)
      for (let at = element.parentNode; at; at = at.parentNode) if (fits(at, i - 1)) return true
      return false
    }
    return this.descendants.filter(element => fits(element, chain.length - 1))
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null }
  // What a reader sees: hidden elements and their text left out.
  get shownText() { return this.hidden ? '' : this.childNodes.map(child => (child instanceof FakeText ? child.data : child.shownText)).join('') }
}
class FakeForm extends FakeElement {}

// The page's sections as index.html has them, the editor loaded against a
// page with no session, and what it attached.
function loadEditor() {
  const body = new FakeElement('body')
  for (const id of ['organizations', 'api-credits', 'renewals', 'openai-organizations', 'openai-api-credits']) {
    const section = new FakeElement('section')
    section.setAttribute('id', id)
    section.hidden = true
    body.append(section)
  }
  const byId = id => body.descendants.find(element => element.id === id) || null
  const document = {
    body, activeElement: null, hidden: false, documentElement: {style: {setProperty() {}}},
    getElementById: byId, querySelectorAll: selector => body.querySelectorAll(selector),
    createElement: tag => (tag === 'form' ? new FakeForm(tag) : new FakeElement(tag)), createElementNS: (namespace, tag) => new FakeElement(tag),
    createTextNode: text => new FakeText(text), addEventListener() {},
  }
  let hooks = null
  const fetches = []
  const page = {
    root: null, readSession: () => null, refusal: () => null, banned: () => false, forget() {}, stores: () => [],
    editingChanged() {}, reload() {}, refusedSave() {}, unansweredSave() {}, remember() {}, unescapeAll: value => value,
    attach(attached) { hooks = attached },
  }
  const context = vm.createContext({
    window: {QuotaCachePage: page, addEventListener() {}}, document, location: {hash: ''}, CSS: {escape: text => text}, HTMLFormElement: FakeForm,
    fetch: (...args) => { fetches.push(args); return new Promise(() => {}) }, setTimeout: () => 0, clearTimeout() {},
    TextEncoder, Intl, Date, BigInt, Math, JSON, Object, Array, Number, String, RegExp, Set, Map, Promise, console,
  })
  vm.runInContext(editorSource, context)
  assert.ok(hooks && hooks.logic, 'editor.js attaches itself')
  return {hooks, byId, fetches}
}

// ---------------------------------------------------------------------------
// The statuses

// The editor keys as the status route answers them, with no organization.
const editorKey = extra => ({available: true, reason: '', revision: '', max_api_credit_items: 16, max_renewal_items: 0, max_batch_bytes: 4096,
  credentials: [], renewal_orphans: [], api_credit_orphans: [], held_api_credits: [],
  organizations: {available: true, reason: '', revision: '', max_items: 16, max_ops: 16, items: []}, ...extra})
// A status of a Quota Cache before the OpenAI API credit: no openai_* key.
const claudeOnly = () => ({cache_path: '/data/snapshot.json', credit_state: null, editor: editorKey({max_renewal_items: 32})})
const SEEN = 'user-AbCdEfGhIjKlMnOpQrStUvWx'
// The OpenAI meter file while no OpenAI organization is listed: watching,
// with one organization seen.
const watchingMeter = () => ({schema: 1, since: '2026-11-17T09:00:00Z', started_at: '2026-11-17T09:00:00Z', flushed_at: '2026-11-17T10:00:00Z', received: 12, counted: 0,
  gaps: [], organizations: {}, unlinked: [{organization_id: SEEN, first_seen_at: '2026-11-17T09:30:00Z', last_seen_at: '2026-11-17T09:40:00Z', requests: 7}], auths: {}, auth_ids: {}})
const watching = meter => ({...claudeOnly(), openai_credits: null, openai_editor: editorKey(), ...(meter ? {openai_meter: meter} : {})})
// The published OpenAI state with its three organizations listed.
function published() {
  const items = golden.api_credits.accounts.map(account => ({id: account.id, label: account.label, organization_id: account.organizationId}))
  return {...claudeOnly(), openai_credits: golden, openai_editor: editorKey({revision: '5', organizations: {available: true, reason: '', revision: '3', max_items: 16, max_ops: 16, items}})}
}

const L = loadEditor().hooks.logic
const OPENAI = L.PROVIDERS.openai

// ---------------------------------------------------------------------------
// The logic

describe('normalizeOpenAIOrganization, the port of client.NormalizeOpenAIOrganization', () => {
  // The cases of client/openai_test.go's TestNormalizeOpenAIOrganization.
  const longest = 'acme-' + 'x'.repeat(59)
  test('trimmed, its case kept, and stable', () => {
    for (const [input, want] of [
      ['user-AbCdEfGhIjKlMnOpQrStUvWx', 'user-AbCdEfGhIjKlMnOpQrStUvWx'], ['acme-x1wegt', 'acme-x1wegt'], ['  acme-x1wegt\n', 'acme-x1wegt'],
      ['\tUser-AbCdEfGhIjKlMnOpQrStUvWx ', 'User-AbCdEfGhIjKlMnOpQrStUvWx'], ['ACME-X1WEGT', 'ACME-X1WEGT'], ['acme.corp_eu-1', 'acme.corp_eu-1'],
      ['a', 'a'], ['0', '0'], [longest, longest],
      // An Organization ID has the shape too; the card warns about it instead.
      ['org-AbCdEfGhIjKlMnOpQrStUvWx', 'org-AbCdEfGhIjKlMnOpQrStUvWx'],
    ]) {
      assert.equal(L.normalizeOpenAIOrganization(input), want, JSON.stringify(input))
      assert.equal(L.normalizeOpenAIOrganization(want), want, 'what it returns passes unchanged')
    }
    assert.equal(longest.length, 64)
  })

  test('refused', () => {
    for (const bad of ['', '   ', '\n', longest + 'x', '-acme', '.acme', '_acme', 'acme corp', 'acme\tcorp', 'acme\ncorp', 'acme\x00', 'acmé-x1wegt', 'ａcme-x1wegt',
      'acme/x1wegt', 'acme:x1wegt', 'acme+x1wegt', 'acme"x1wegt', 'acme,acme-x1wegt']) {
      assert.equal(L.normalizeOpenAIOrganization(bad), null, JSON.stringify(bad))
    }
  })
})

describe('the OpenAI cards\' batches', () => {
  const orgLimits = {maxItems: 16, maxOps: 16, maxBytes: 4096}
  const draftWith = add => ({...L.emptyOrganizationsDraft(), add})
  const items = [{id: 'oai-dd79fc440924', label: 'Pro', organization_id: 'user-AbCdEfGhIjKlMnOpQrStUvWx'}]

  test('an organizations batch has its own kind, and keeps the organization as typed, trimmed', () => {
    const plan = L.organizationsPlan(items, draftWith({label: 'Business', organizationId: ' Acme-X1wegt '}), '3', orgLimits, L.OPENAI_RULES)
    assert.deepEqual(JSON.parse(JSON.stringify(plan.batch)), {kind: 'openaiOrganizations', baseRevision: '3', ops: [{op: 'add', label: 'Business', organizationId: 'Acme-X1wegt'}]})
    assert.equal(plan.normalized, 'Acme-X1wegt')
    // Claude's rules are still the default.
    assert.equal(L.organizationsPlan([], draftWith({label: 'x', organizationId: '00000000-0000-4000-8000-0000000000AA'}), '', orgLimits).batch.kind, 'organizations')
  })

  test('two organizations that differ only in case are two; the same one is a duplicate', () => {
    const differ = L.organizationsPlan(items, draftWith({label: 'Other', organizationId: 'USER-AbCdEfGhIjKlMnOpQrStUvWx'}), '3', orgLimits, L.OPENAI_RULES)
    assert.equal(differ.count, 0)
    const same = L.organizationsPlan(items, draftWith({label: 'Other', organizationId: ' user-AbCdEfGhIjKlMnOpQrStUvWx'}), '3', orgLimits, L.OPENAI_RULES)
    assert.match(same.errors['add:organizationId'], /already listed/)
  })

  test('an organization OpenAI could not name is said in OpenAI\'s words', () => {
    const bad = L.organizationsPlan([], draftWith({label: 'x', organizationId: 'acme corp'}), '', orgLimits, L.OPENAI_RULES)
    assert.equal(bad.errors['add:organizationId'], L.OPENAI_RULES.idMessage)
    assert.match(L.OPENAI_RULES.idMessage, /^Enter the organization as OpenAI’s responses name it: 1 to 64 letters/)
    // A Claude Organization ID is not an OpenAI organization's shape, nor the reverse.
    assert.notEqual(L.organizationsPlan([], draftWith({label: 'x', organizationId: 'user-AbCd'}), '', orgLimits).count, 0)
  })

  test('an API credit batch has its own kind, and sixteen full rows fit 4096 bytes', () => {
    const settings = {editable: true, notEditableReason: '', revision: '18446744073709551615', monthlyUsd: '', renews: '', reading: null, readingUnusedReason: ''}
    const accounts = Array.from({length: 16}, (_, i) => ({id: 'oai-' + i.toString(16).padStart(12, '0'), label: 'o' + i, settings}))
    const now = Date.parse('2026-10-11T12:00:00Z') / 1000
    let draft = L.emptyDraft()
    for (const account of accounts) {
      draft = L.editCredit(draft, account, account.label, 'monthlyUsd', {kind: 'set', text: '1234567.89'})
      draft = L.editCredit(draft, account, account.label, 'renews', {kind: 'set', text: '2026-09-29'})
      draft = L.editCredit(draft, account, account.label, 'reading', {kind: 'set', amount: '1234567.89', at: L.localInputValue(now - 3600)})
    }
    const plan = L.creditPlan(accounts, draft, now, account => account.settings, {maxItems: 16, maxBytes: 4096}, OPENAI.creditsKind)
    assert.equal(plan.tooLarge, false)
    assert.equal(plan.batch.kind, 'openaiApiCredits')
    assert.ok(new TextEncoder().encode(JSON.stringify(plan.batch)).length < 4096)
    assert.equal(L.creditPlan(accounts, draft, now, account => account.settings, {maxItems: 16, maxBytes: 4096}).batch.kind, 'apiCredits', 'Claude\'s kind is still the default')
  })
})

describe('the OpenAI API credit\'s words', () => {
  const settings = {editable: true, revision: '', monthlyUsd: '', renews: '', reading: null, readingUnusedReason: ''}
  const now = Date.parse('2026-10-11T12:00:00Z') / 1000

  test('a reading is read from API Platform', () => {
    const at = L.localInputValue(now - 3600)
    assert.equal(L.readingField(settings, {kind: 'set', amount: '', at}, {now, words: OPENAI.words}).amount.error, 'Enter the amount API Platform shows.')
    assert.equal(L.readingField(settings, {kind: 'set', amount: 'lots', at}, {now, words: OPENAI.words}).amount.error, 'Enter the amount API Platform shows, like 143.20.')
    assert.equal(L.creditRow(settings, {reading: {kind: 'set', amount: 'lots', at}}, now, OPENAI.words).reading.amount.error, OPENAI.words.amount)
    // Without words, Claude's: Console.
    assert.equal(L.readingField(settings, {kind: 'set', amount: '', at}, {now}).amount.error, 'Enter the amount Console shows.')
    assert.equal(L.readingField(settings, {kind: 'set', amount: 'lots', at}, {now}).amount.error, L.READING_AMOUNT_MESSAGE)
  })

  test('what a save comes to is said as before for every code both lanes share', () => {
    assert.equal(L.outcomeOf({kind: 'answer', status: 400, body: {error: 'invalid_reading_amount', id: 'oai-a', field: 'reading.remainingUsd'}}).text, L.READING_AMOUNT_MESSAGE)
    assert.equal(L.saveErrorText('settings_unavailable'), 'Quota Cache cannot read its saved settings, so nothing was saved.')
    assert.equal(L.saveErrorText('organizations_unavailable'), 'Quota Cache cannot read its organizations file, so nothing was saved.')
    assert.equal(L.saveErrorText('conflict'), L.CONFLICT_FOOT)
    assert.equal(L.outcomeOf({kind: 'answer', status: 400, body: {error: 'invalid_request'}}).text, 'Quota Cache could not read this save, so nothing was saved. Reload the page and try again.')
  })

  test('no OpenAI word names Console, Anthropic or Claude', () => {
    const words = []
    const collect = value => {
      if (typeof value === 'string') words.push(value)
      else if (typeof value === 'function') { words.push(value(true), value(false), value('Nov 18'), value('org-AbCdEfGhIjKlMnOpQrStUvWx')) }
      else if (value && typeof value === 'object') Object.values(value).forEach(collect)
    }
    collect(OPENAI)
    words.push(L.OPENAI_RULES.idMessage)
    for (const text of words.filter(word => typeof word === 'string')) assert.doesNotMatch(text, /Console|Anthropic|Claude/, text)
  })

  test('the organization’s help says to pick one seen in traffic only while one is', () => {
    assert.match(OPENAI.idHelp(true), / Pick one seen in traffic below, or type it\.$/)
    assert.doesNotMatch(OPENAI.idHelp(false), /seen in traffic/)
    assert.match(OPENAI.idHelp(false), /^What OpenAI’s openai-organization response header names\. It is not the org-… Organization ID, and not a secret\.$/)
  })

  test('an Organization ID typed for an OpenAI organization is warned about, never refused', () => {
    assert.match(OPENAI.idWarning(' org-AbCdEfGhIjKlMnOpQrStUvWx '), /^This looks like the Organization ID from API Platform settings\./)
    for (const fine of ['user-AbCdEfGhIjKlMnOpQrStUvWx', 'acme-x1wegt', 'org-short', '']) assert.equal(OPENAI.idWarning(fine), null, fine)
    assert.equal(L.PROVIDERS.claude.idWarning('org-AbCdEfGhIjKlMnOpQrStUvWx'), null)
  })
})

describe('seen in traffic', () => {
  const unlinked = (organizationId, requests, reason = 'notConfigured') => ({organizationId, firstSeenEpoch: 1, lastSeenEpoch: 1789009200, requests, reason})

  test('the published state\'s own list while there is one, those not listed only', () => {
    const state = {api_credits: {unlinked: [unlinked('acme-x1wegt', 3), unlinked('gone-team', 2, 'dormant'), unlinked('user-ZyXwVuTsRqPoNmLkJiHgFeDc', 7)]}}
    assert.deepEqual(JSON.parse(JSON.stringify(L.seenInTraffic(state, watchingMeter(), [{organization_id: 'acme-x1wegt'}]))),
      [{organizationId: 'user-ZyXwVuTsRqPoNmLkJiHgFeDc', requests: 7, lastSeenEpoch: 1789009200}])
  })

  test('while the lane publishes nothing, the meter file\'s list', () => {
    const seen = L.seenInTraffic(null, watchingMeter(), [])
    assert.deepEqual(JSON.parse(JSON.stringify(seen)), [{organizationId: SEEN, requests: 7, lastSeenEpoch: Date.parse('2026-11-17T09:40:00Z') / 1000}])
    assert.equal(L.seenInTraffic({api_credits: null}, watchingMeter(), []).length, 1, 'a state that lists no organization has no list of its own')
    assert.equal(L.seenInTraffic(null, watchingMeter(), [{organization_id: SEEN}]).length, 0, 'one just added is listed already')
    assert.equal(L.seenInTraffic(null, null, []).length, 0)
    assert.equal(L.seenInTraffic(null, {unlinked: [{organization_id: SEEN}]}, [])[0].lastSeenEpoch, null, 'no last-seen time')
  })
})

// ---------------------------------------------------------------------------
// The cards

describe('which OpenAI cards a status shows', () => {
  test('a status without the openai_* keys shows no OpenAI card', () => {
    const {hooks, byId} = loadEditor()
    hooks.render(claudeOnly())
    assert.equal(byId('openai-organizations').hidden, true)
    assert.equal(byId('openai-api-credits').hidden, true)
    assert.equal(byId('openai-organizations').childNodes.length, 0)
    assert.equal(byId('organizations').hidden, false, 'Claude\'s cards are drawn')
    assert.equal(byId('api-credits').hidden, false)
  })

  test('watching, with nothing listed: the organizations card offers what was seen, and the credit card waits', () => {
    const {hooks, byId, fetches} = loadEditor()
    hooks.render(watching(watchingMeter()))
    const card = byId('openai-organizations')
    assert.equal(card.hidden, false)
    assert.equal(byId('openai-api-credits').hidden, true, 'nothing published, no orphan, nothing held')
    const text = card.shownText
    for (const want of ['OpenAI organizations', OPENAI.orgsEmpty[0], OPENAI.orgsEmpty[1], 'Seen in traffic', SEEN, '7 requests', 'last seen', 'Add organization']) assert.ok(text.includes(want), want)
    assert.ok(!text.includes('Nothing seen yet.'))
    assert.match(OPENAI.orgsEmpty[0], /^No OpenAI API organizations yet\. Send a request through the openai-compatibility entry that uses your OpenAI API key: /)
    assert.doesNotMatch(card.shownText, /Console/)
    // Add opens the card with the organization on its add row, and its help
    // says to pick one seen in traffic.
    card.querySelector('[aria-label="Add ' + SEEN + '"]').click()
    assert.equal(hooks.editing(), true)
    assert.equal(byId('qc-oai-org-add-id').value, SEEN)
    assert.equal(byId('qc-oai-org-add-label').value, '')
    assert.ok(byId('qc-oai-org-add-id-caps').textContent.includes('Pick one seen in traffic below, or type it.'))
    assert.equal(card.querySelector('form').getAttribute('aria-label'), 'OpenAI API organizations')
    assert.equal(fetches.length, 0, 'opening a card sends nothing')
  })

  test('a picked organization’s empty label is not marked to fix until it is typed in or a save is tried', () => {
    for (const touch of ['typed', 'save']) {
      const {hooks, byId, fetches} = loadEditor()
      hooks.render(watching(watchingMeter()))
      const card = byId('openai-organizations')
      card.querySelector('[aria-label="Add ' + SEEN + '"]').click()
      const label = byId('qc-oai-org-add-label')
      const caps = () => byId('qc-oai-org-add-label-caps').textContent
      assert.notEqual(label.getAttribute('aria-invalid'), 'true', touch)
      assert.doesNotMatch(caps(), /Enter a label/, touch)
      assert.ok(card.querySelector('.edit-foot').textContent.includes('1 to fix'), 'Save stays held off')
      if (touch === 'typed') {
        for (const value of ['x', '']) { label.value = value; label.listeners.input[0]({target: label}) }
      } else {
        card.querySelector('form').listeners.submit[0]({preventDefault() {}})
      }
      assert.equal(byId('qc-oai-org-add-label').getAttribute('aria-invalid'), 'true', touch)
      assert.match(caps(), /Enter a label of 1 to 64/, touch)
      assert.equal(fetches.length, 0, touch)
    }
  })

  test('watching, with nothing seen yet', () => {
    const {hooks, byId} = loadEditor()
    for (const status of [watching(null), watching({...watchingMeter(), unlinked: []})]) {
      hooks.render(status)
      const text = byId('openai-organizations').shownText
      assert.ok(text.includes('Seen in traffic') && text.includes('Nothing seen yet.'), text)
    }
    byId('qc-oai-orgs-add').click()
    assert.ok(!byId('qc-oai-org-add-id-caps').textContent.includes('seen in traffic'), 'nothing to pick')
  })

  test('published: the organizations, the one seen, and the credits in API Platform\'s words', () => {
    const {hooks, byId} = loadEditor()
    hooks.render(published())
    const orgs = byId('openai-organizations').shownText
    for (const want of ['3 OpenAI API organizations', 'Organization in responses', 'Pro', 'user-AbCd…UvWx', 'acme-x1wegt', 'Seen in traffic', 'user-ZyXwVuTsRqPoNmLkJiHgFeDc', '7 requests']) assert.ok(orgs.includes(want), want)
    const credits = byId('openai-api-credits')
    assert.equal(credits.hidden, false)
    const text = credits.shownText
    for (const want of ['OpenAI API credits', 'API Platform reading', 'Enter API Platform reading', golden.api_credits.accounts[1].issue, 'oai-c3a38df85b55',
      'no longer listed under OpenAI organizations', 'OpenAI’s list prices of Nov 18', 'Quota Glance 0.11.0 or newer shows these figures; Reset Priority does not route on them.']) {
      assert.ok(text.includes(want), want)
    }
    assert.doesNotMatch(byId('openai-organizations').shownText + text, /Console|Anthropic/)
    assert.ok(credits.querySelector('[aria-label="Enter API Platform reading for Business"]'))
    assert.equal(credits.querySelector('.table-wrap').getAttribute('aria-label'), 'OpenAI API credits by organization')
    // Claude's cards are untouched by the OpenAI state.
    assert.ok(byId('organizations').shownText.includes('No Console organizations yet. Add one to start counting its API credit.'))
  })

  test('editing the OpenAI API credits: what to enter under an empty field, and a reading in API Platform\'s words', () => {
    const {hooks, byId, fetches} = loadEditor()
    hooks.render(published())
    const card = byId('openai-api-credits')
    card.querySelectorAll('button').find(button => button.textContent === 'Set credits and dates' || button.textContent === 'Edit credits and dates').click()
    assert.ok(card.querySelector('form'), 'the card opened')
    // Pro has its monthly credit and refill date; Side project has neither.
    assert.equal(byId('qc-oai-credit-0-name').textContent, 'Pro')
    assert.ok(!byId('qc-oai-credit-0-monthly-caps').textContent.includes(OPENAI.monthlyHelp) && !byId('qc-oai-credit-0-refill-caps').textContent.includes(OPENAI.refillHelp))
    assert.equal(byId('qc-oai-credit-2-name').textContent, 'Side project')
    assert.ok(byId('qc-oai-credit-2-monthly-caps').textContent.includes(OPENAI.monthlyHelp) && byId('qc-oai-credit-2-refill-caps').textContent.includes(OPENAI.refillHelp))
    byId('qc-oai-credit-1-reading-add').click()
    const amount = byId('qc-oai-credit-1-amount')
    amount.value = 'lots'
    amount.listeners.input[0]({target: amount})
    assert.ok(byId('qc-oai-credit-1-reading-caps').textContent.includes('Enter the amount API Platform shows, like 143.20.'))
    assert.ok(card.textContent.includes('Left in API Platform') && card.textContent.includes(OPENAI.readingHelp))
    assert.equal(card.querySelector('form').getAttribute('aria-label'), 'Monthly credits, refill dates and API Platform readings')
    assert.doesNotMatch(card.textContent, /Console/)
    assert.equal(fetches.length, 0)
  })

  test('the OpenAI API credit card shows saved values with no organization listed', () => {
    const {hooks, byId} = loadEditor()
    const status = watching(null)
    status.openai_editor.api_credit_orphans = [{id: 'oai-c3a38df85b55', monthlyUsd: '100', renews: '', hasReading: false, revision: '2', updatedAtEpoch: 1}]
    hooks.render(status)
    const text = byId('openai-api-credits').shownText
    assert.ok(!byId('openai-api-credits').hidden && text.includes('oai-c3a38df85b55') && text.includes('no longer listed under OpenAI organizations'), text)
  })
})
