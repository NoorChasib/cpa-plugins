// The Model Picker page's per-client logic and save flow, run as shipped:
// app.js is loaded unchanged into a fake page built from index.html's own
// element ids (an id the script uses but the page lacks makes it throw), with
// fetch answered by a stand-in for CPA and the plugin. Nothing leaves.
//
// session.test.mjs imports this file, because the go test in internal/plugin
// runs that file alone. Run directly:
//   node --test plugins/codex-catalog-filter/scripts/tests/session.test.mjs

import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import {test} from 'node:test'
import vm from 'node:vm'
import {models as realModels, cases as realCases} from './agy-parity-fixture.mjs'

const script = readFileSync(new URL('../../internal/plugin/pageassets/app.js', import.meta.url), 'utf8')
const html = readFileSync(new URL('../../internal/plugin/pageassets/index.html', import.meta.url), 'utf8')

const HOST = '127.0.0.1:8317'
const KEY = 'test-management-key-0123456789'
const ROOT = 'http://' + HOST
const STATE_URL = ROOT + '/v0/management/plugins/codex-catalog-filter/state'
const CONFIG_URL = ROOT + '/v0/management/plugins/codex-catalog-filter/config'
const AGY_PATH = '/v0/resource/plugins/codex-catalog-filter/agy/models'
const SECTIONS_KEY = 'codex-catalog-filter:open-sections'
// A remembered session in the console's older separate entries, which the
// reader accepts (see session.test.mjs for every stored form).
const SESSION = {isLoggedIn: 'true', apiBase: JSON.stringify(ROOT), managementKey: KEY}

// Every element of index.html that has an id, with its tag and attributes.
function pageIds() {
  const ids = new Map()
  for (const [, tag, attributes] of html.matchAll(/<([a-z0-9]+)\b([^>]*)>/gi)) {
    const attrs = {}
    for (const [, name, value] of attributes.matchAll(/([a-z-]+)(?:="([^"]*)")?/gi)) attrs[name] = value ?? ''
    if (!('id' in attrs)) continue
    assert.ok(!ids.has(attrs.id), 'index.html repeats id ' + attrs.id)
    ids.set(attrs.id, {tag, attrs})
  }
  return ids
}

class FakeElement {
  constructor(tag, attrs = {}) {
    this.tagName = tag.toUpperCase()
    this.attributes = {...attrs}
    this.children = []
    this.listeners = new Map()
    this.textContent = ''
    this.className = attrs.class || ''
    this.hidden = 'hidden' in attrs
    this.disabled = 'disabled' in attrs
    this.open = 'open' in attrs
    this.checked = false
    this.value = ''
    this.type = attrs.type || ''
    this.focused = false
  }
  addEventListener(type, listener) { this.listeners.set(type, [...(this.listeners.get(type) || []), listener]) }
  fire(type) { for (const listener of this.listeners.get(type) || []) listener({type, target: this, preventDefault() {}}) }
  setAttribute(name, value) { this.attributes[name] = String(value) }
  getAttribute(name) { return name in this.attributes ? this.attributes[name] : null }
  append(...nodes) { this.children.push(...nodes) }
  replaceChildren(...nodes) { this.children = nodes; if (nodes.length === 0) this.textContent = '' }
  focus() { this.focused = true }
  get text() { return this.textContent + this.children.map((child) => child.text).join('') }
}

function find(root, predicate) {
  if (predicate(root)) return root
  for (const child of root.children) {
    const found = find(child, predicate)
    if (found) return found
  }
  return null
}

// Values made inside the page's realm have that realm's prototypes; compare
// them as plain data.
const plain = (value) => JSON.parse(JSON.stringify(value))
const json = (status, value) => new Response(JSON.stringify(value), {status, headers: {'Content-Type': 'application/json; charset=utf-8'}})
// Lets every pending promise and the page's short timers run.
const settle = async () => { for (let i = 0; i < 100; i++) await new Promise((resolve) => setImmediate(resolve)) }

// Opens the page. respond(request) answers each fetch; storage is the
// browser's local storage for this origin.
function openPage({respond = () => new Promise(() => {}), storage = SESSION, failingStorage = false} = {}) {
  const ids = pageIds()
  const elements = new Map()
  const document = {
    getElementById(id) {
      if (!ids.has(id)) return null
      if (!elements.has(id)) elements.set(id, new FakeElement(ids.get(id).tag, ids.get(id).attrs))
      return elements.get(id)
    },
    createElement: (tag) => new FakeElement(tag),
  }
  const store = new Map(Object.entries(storage))
  const requests = []
  const context = vm.createContext({
    location: {protocol: 'http:', host: HOST, origin: ROOT, pathname: '/v0/resource/plugins/codex-catalog-filter/settings'},
    document,
    localStorage: {
      getItem(name) {
        if (failingStorage && name === SECTIONS_KEY) throw new Error('SecurityError')
        return store.has(name) ? store.get(name) : null
      },
      setItem(name, value) {
        if (failingStorage) throw new Error('QuotaExceededError')
        store.set(name, String(value))
      },
    },
    navigator: {userAgent: 'Mozilla/5.0 TestUA/1'},
    addEventListener() {}, confirm: () => true,
    fetch: async (url, init) => {
      const request = {url, method: init.method, body: init.body === undefined ? undefined : JSON.parse(init.body), authorization: init.headers.Authorization}
      requests.push(request)
      return respond(request)
    },
    // The page waits 500 ms between checks after saving; run those at once.
    // Its 15 s request timeout never fires.
    setTimeout: (callback, ms) => { if (ms <= 1000) Promise.resolve().then(callback); return 0 },
    clearTimeout() {},
    TextEncoder, TextDecoder, atob, URL, Headers, AbortController,
  })
  context.window = context
  vm.runInContext(script, context)
  const el = (id) => document.getElementById(id)
  const toggle = (prefix, client, id) => find(el(prefix + '-rows'), (e) => e.tagName === 'INPUT' && e.getAttribute('aria-label') === 'Show ' + id + ' in ' + client)
  return {el, requests, store, toggle, logic: vm.runInContext('ModelPickerLogic', context), notice: () => el('notice').textContent}
}

const logic = openPage({storage: {}}).logic

function agyModel(id, extra = {}) {
  return {id, display_name: id, label: id, enabled: false, switched: false, non_text: false, hidden_by_agy: false, builtin: false, tokens: 'none', in_list: false, position: 0, slot: '', skipped: false, ...extra}
}
function claudeModel(id, extra = {}) {
  return {id, display_name: id, enabled: true, switched: false, ...extra}
}

// A /state response as the plugin builds it, with overrides per client.
function makeState({codex = {}, agy = {}, claude = {}} = {}) {
  return {
    catalog_path: '/v0/resource/plugins/codex-catalog-filter/models',
    seen_at: '2026-11-19T10:00:00Z',
    saved: true,
    new_models: 'enabled',
    action: 'remove',
    models: [{slug: 'gpt-6-sol', display_name: 'GPT 6 Sol', visibility: 'list', enabled: true, switched: false}, {slug: 'grok-4.7', display_name: 'Grok 4.7', visibility: 'list', enabled: true, switched: false}],
    switches: {},
    ...codex,
    version: '0.2.0',
    antigravity: {
      config_key: 'antigravity', configured: true, error: '', using: 'config', new_models: 'disabled', switches: {}, models_source: 'client', saved: true,
      path: AGY_PATH, default_model: '', title_model: '', fetched_at: '2026-11-19T10:00:00Z', served_at: '2026-11-19T10:00:00Z', served_from: 'cpa', last_error: '', last_error_at: null,
      models: [agyModel('gemini-4-argon-high', {builtin: true, builtin_enum: 1417, tokens: 'builtin'}), agyModel('gemini-4-neon-xhigh', {tokens: 'clamped', max_output_tokens: 65536, output_token_limit: 1048576, thinking_budget: 32768}), agyModel('gemini-3.1-flash-lite', {builtin: true, builtin_enum: 1050}), agyModel('gemini-3.5-flash-lite'), agyModel('gpt-6-sol'), agyModel('grok-tts', {non_text: true})],
      preview: null,
      ...agy,
    },
    claude_code: {
      config_key: 'claude-code', configured: false, error: '', using: 'config', new_models: 'enabled', switches: {}, models_source: 'client', saved: true,
      filter: true, seen_at: '2026-11-19T10:00:00Z', lists: 2, rewritten: 1, intercept_errors: 0,
      models: [claudeModel('claude-opus-5-5'), claudeModel('gpt-6-sol', {cloaked_id: 'claude-fable-5-dd-gpt-6-sol'})],
      ...claude,
    },
  }
}

// CPA's PATCH (shallow merge, null deletes a key) followed by the plugin's
// reconfigure, reduced to what /state reports.
function applyPatch(state, body) {
  const next = structuredClone(state)
  if ('models' in body) next.switches = body.models ?? {}
  if ('new-models' in body) next.new_models = body['new-models'] ?? 'enabled'
  if ('action' in body) next.action = body.action ?? 'remove'
  if ('antigravity' in body) {
    const s = body.antigravity ?? {}
    Object.assign(next.antigravity, {switches: s.models ?? {}, new_models: s['new-models'] ?? 'disabled', default_model: s['default-model'] ?? '', title_model: s['title-model'] ?? ''})
  }
  if ('claude-code' in body) {
    const s = body['claude-code'] ?? {}
    Object.assign(next.claude_code, {switches: s.models ?? {}, new_models: s['new-models'] ?? 'enabled', filter: s.filter ?? true})
  }
  return next
}

// A stand-in server: GET state answers server.state; PATCH applies the body.
function server(initial) {
  const stand = {state: initial, patches: []}
  stand.respond = ({url, method, body}) => {
    if (url === STATE_URL && method === 'GET') return json(200, stand.state)
    if (url === CONFIG_URL && method === 'PATCH') {
      stand.patches.push(body)
      stand.state = applyPatch(stand.state, body)
      return json(200, {status: 'ok'})
    }
    return json(404, {error: 'not_found'})
  }
  return stand
}

const agyConfig = ({switches = {}, new_models = 'disabled', default_model = '', title_model = ''} = {}) =>
  logic.base('antigravity', {antigravity: {switches, new_models, default_model, title_model}})

test('the page’s Antigravity preview matches the plugin’s for CPA’s real list', () => {
  assert.equal(realModels.length, 89)
  for (const {name, config, want} of realCases) {
    const preview = logic.agyPreview(realModels, agyConfig(config))
    assert.deepEqual(plain({
      enabled: realModels.filter((m) => logic.isEnabled('antigravity', m, agyConfig(config))).map((m) => m.id),
      entries: preview.entries.map((e) => e.id + ':' + e.slot),
      skipped: preview.skipped,
      slots_used: preview.slotsUsed,
      default_model_id: preview.defaultId,
      title_model_id: preview.titleId,
      fallback: preview.fallback,
      warnings: preview.warnings,
    }), plain(want), name)
  }
})

test('Antigravity selection: a switch wins, then the default and title models, then non-text models stay off', () => {
  const tts = agyModel('grok-tts', {non_text: true})
  const sol = agyModel('gpt-6-sol')
  const on = (model, config) => logic.isEnabled('antigravity', model, agyConfig(config))
  assert.equal(on(sol, {}), false, 'new-models defaults to disabled for agy')
  assert.equal(on(sol, {new_models: 'enabled'}), true)
  assert.equal(on(tts, {new_models: 'enabled'}), false, 'non-text models are off without a switch')
  assert.equal(on(tts, {switches: {'grok-tts': true}}), true, 'a switch turns a non-text model on')
  assert.equal(on(sol, {default_model: 'gpt-6-sol'}), true, 'the default model is on without a switch')
  assert.equal(on(tts, {title_model: 'grok-tts'}), true, 'the title model is on without a switch')
  assert.equal(on(sol, {default_model: 'gpt-6-sol', switches: {'gpt-6-sol': false}}), false, 'an explicit switch beats the default choice')
})

test('slots: built-in ids need none unless their enum is taken, image models none, and ids past 32 are left out in id order', () => {
  // Listed out of order, as CPA does; agy receives them by id.
  const list = [
    ...Array.from({length: 33}, (_, i) => agyModel('zz-custom-' + String(32 - i).padStart(2, '0'))),
    agyModel('gemini-4-argon-medium', {builtin: true, builtin_enum: 1418}),
    agyModel('gemini-3.1-flash-image', {non_text: true, hidden_by_agy: true}),
    agyModel('gemini-4-argon', {builtin: true, builtin_enum: 1418}),
  ]
  const preview = logic.agyPreview(list, agyConfig({new_models: 'enabled', switches: {'gemini-3.1-flash-image': true}}))
  assert.deepEqual(plain(preview.entries.slice(0, 4).map((e) => e.id + ':' + e.slot)), ['gemini-3.1-flash-image:image', 'gemini-4-argon:builtin', 'gemini-4-argon-medium:placeholder', 'zz-custom-00:placeholder'])
  assert.equal(preview.slotsUsed, 32)
  assert.deepEqual(plain(preview.skipped), ['zz-custom-31', 'zz-custom-32'], 'the duplicate enum took the first slot, so two customs are left out')
  assert.equal(preview.defaultId, 'gemini-4-argon', 'an image model is never the default')
  assert.equal(preview.positions.get('zz-custom-30'), 34)
  assert.equal(preview.positions.has('zz-custom-31'), false)
  assert.match(preview.warnings.join('\n'), /^2 switched-on models are left out/)
})

test('order: by id, the default first; a flash-lite title model moves ahead of other flash-lite ids only', () => {
  const list = ['gpt-6-sol', 'gemini-3.5-flash-lite', 'gemini-3.8-flash', 'gemini-3.1-flash-lite', 'grok-4.7'].map((id) => agyModel(id))
  const byId = ['gemini-3.1-flash-lite', 'gemini-3.5-flash-lite', 'gemini-3.8-flash', 'gpt-6-sol', 'grok-4.7']
  const ids = (config) => logic.agyPreview(list, agyConfig({new_models: 'enabled', ...config})).entries.map((e) => e.id)
  assert.deepEqual(plain(ids({})), byId, 'CPA’s order is not kept: it changes on every reload')
  assert.deepEqual(plain(ids({default_model: 'grok-4.7', title_model: 'gemini-3.5-flash-lite'})), ['grok-4.7', 'gemini-3.5-flash-lite', 'gemini-3.1-flash-lite', 'gemini-3.8-flash', 'gpt-6-sol'])
  const notLite = logic.agyPreview(list, agyConfig({new_models: 'enabled', title_model: 'grok-4.7'}))
  assert.deepEqual(plain(notLite.entries.map((e) => e.id)), byId, 'a title model without flash-lite is not moved')
  assert.equal(notLite.titleId, 'gemini-3.1-flash-lite')
  assert.match(notLite.warnings.join('\n'), /grok-4\.7 is not used for titles/)
  const missing = logic.agyPreview(list, agyConfig({new_models: 'enabled', default_model: 'gone'}))
  assert.equal(missing.defaultId, 'gemini-3.1-flash-lite')
  assert.match(missing.warnings.join('\n'), /The default model gone is not in the list, so gemini-3\.1-flash-lite is the default/)
  // Ids compare as Go compares strings: by code point, so '-' sorts before '.'.
  assert.deepEqual(plain(logic.agyPreview(['gemini-3.1-pro', 'gemini-3-pro', 'Z', 'a'].map((id) => agyModel(id)), agyConfig({new_models: 'enabled'})).entries.map((e) => e.id)), ['Z', 'a', 'gemini-3-pro', 'gemini-3.1-pro'])
})

test('only image models switched on: the Gemini text models are served, as the plugin does', () => {
  const list = [agyModel('gemini-3.1-flash-image', {non_text: true, hidden_by_agy: true}), agyModel('gemini-4-neon'), agyModel('gpt-6-sol')]
  const preview = logic.agyPreview(list, agyConfig({switches: {'gemini-3.1-flash-image': true}}))
  assert.equal(preview.fallback, true)
  assert.deepEqual(plain(preview.entries.map((e) => e.id)), ['gemini-4-neon'])
  assert.match(preview.warnings[0], /^Every switched-on model is an image model agy hides/)
})

test('nothing switched on serves CPA’s Gemini text models, then any text model, never an empty list', () => {
  const list = [agyModel('gpt-6-sol'), agyModel('gemini-4-neon'), agyModel('gemini-tts', {non_text: true})]
  const gemini = logic.agyPreview(list, agyConfig())
  assert.equal(gemini.fallback, true)
  assert.deepEqual(plain(gemini.entries.map((e) => e.id)), ['gemini-4-neon'])
  const other = logic.agyPreview([agyModel('gpt-6-sol'), agyModel('grok-tts', {non_text: true})], agyConfig())
  assert.deepEqual(plain(other.entries.map((e) => e.id)), ['gpt-6-sol'])
  assert.equal(logic.agyPreview([agyModel('grok-tts', {non_text: true})], agyConfig()).entries.length, 0)
})

test('a switch is pending only while it differs from what applies without it', () => {
  const config = agyConfig({switches: {'gpt-6-sol': false}})
  const pending = logic.emptyPending()
  const sol = agyModel('gpt-6-sol')
  const neon = agyModel('gemini-4-neon')
  logic.setSwitch('antigravity', pending, config, sol, true)
  logic.setSwitch('antigravity', pending, config, neon, true)
  assert.deepEqual(plain([...pending.switches]), [['gpt-6-sol', true], ['gemini-4-neon', true]])
  logic.setSwitch('antigravity', pending, config, sol, false)
  logic.setSwitch('antigravity', pending, config, neon, false)
  assert.equal(logic.dirty(pending), false, 'switching back leaves nothing to save')
  logic.setSetting(pending, config, 'newModels', 'enabled')
  logic.setSwitch('antigravity', pending, config, neon, true)
  assert.equal(pending.switches.size, 0, 'already on under the pending new-models')
})

test('PATCH bodies name one client: a complete subtree for Antigravity and Claude Code, Codex as before', () => {
  const codexState = makeState({codex: {switches: {'grok-4.7': false}}})
  const codexPending = logic.emptyPending()
  codexPending.switches.set('gpt-6-sol', false)
  assert.deepEqual(plain(logic.patchBody('codex', logic.apply(logic.base('codex', codexState), codexPending), codexPending)), {models: {'gpt-6-sol': false, 'grok-4.7': false}})
  codexPending.settings.set('newModels', 'disabled')
  assert.deepEqual(plain(logic.patchBody('codex', logic.apply(logic.base('codex', codexState), codexPending), codexPending)), {models: {'gpt-6-sol': false, 'grok-4.7': false}, 'new-models': 'disabled'})

  const agyState = makeState({agy: {switches: {b: false, a: true}, default_model: 'a'}})
  const agyPending = logic.emptyPending()
  agyPending.settings.set('titleModel', 'gemini-3.1-flash-lite')
  const agyBody = logic.patchBody('antigravity', logic.apply(logic.base('antigravity', agyState), agyPending), agyPending)
  assert.deepEqual(plain(agyBody), {antigravity: {models: {a: true, b: false}, 'new-models': 'disabled', 'default-model': 'a', 'title-model': 'gemini-3.1-flash-lite'}})
  assert.equal(JSON.stringify(agyBody.antigravity.models), '{"a":true,"b":false}', 'switches are sent sorted')
  agyPending.settings.set('defaultModel', '')
  assert.deepEqual(plain(Object.keys(logic.patchBody('antigravity', logic.apply(logic.base('antigravity', agyState), agyPending), agyPending).antigravity)), ['models', 'new-models', 'title-model'], 'an unset default is omitted')

  const claudePending = logic.emptyPending()
  claudePending.settings.set('filter', false)
  assert.deepEqual(plain(logic.patchBody('claude', logic.apply(logic.base('claude', makeState()), claudePending), claudePending)), {'claude-code': {filter: false, models: {}, 'new-models': 'enabled'}})
})

test('model ids are data: __proto__ and constructor round-trip as switches', () => {
  const state = JSON.parse('{"antigravity":{"switches":{"__proto__":true,"constructor":false},"new_models":"disabled","default_model":"","title_model":""}}')
  const config = logic.base('antigravity', state)
  assert.equal(config.switches.get('__proto__'), true)
  assert.equal(logic.isEnabled('antigravity', agyModel('toString'), config), false)
  const body = JSON.stringify(logic.patchBody('antigravity', config, logic.emptyPending()))
  assert.equal(body, '{"antigravity":{"models":{"__proto__":true,"constructor":false},"new-models":"disabled"}}')
})

test('a save counts as applied only with the exact switches and settings and no error', () => {
  const state = makeState({agy: {switches: {a: true}, default_model: 'a'}})
  const config = logic.base('antigravity', state)
  assert.equal(logic.applied('antigravity', state, config), true)
  assert.equal(logic.applied('antigravity', makeState({agy: {switches: {a: true}, default_model: 'a', error: 'antigravity: unknown key'}}), config), false)
  assert.equal(logic.applied('antigravity', makeState({agy: {switches: {a: true, b: true}, default_model: 'a'}}), config), false)
  assert.equal(logic.applied('antigravity', makeState({agy: {switches: {a: true}}}), config), false)
  assert.equal(logic.applied('claude', makeState({claude: {filter: false}}), logic.base('claude', makeState())), false)
})

test('carrying changes onto a newer saved state drops those it already holds', () => {
  const older = logic.base('claude', makeState())
  const pending = logic.emptyPending()
  const models = makeState().claude_code.models
  logic.setSwitch('claude', pending, older, models[0], false)
  logic.setSwitch('claude', pending, older, models[1], false)
  logic.setSetting(pending, older, 'filter', false)
  const newer = logic.base('claude', makeState({claude: {switches: {'claude-opus-5-5': false}}}))
  logic.normalize('claude', pending, newer, models)
  assert.deepEqual(plain([...pending.switches]), [['gpt-6-sol', false]])
  assert.deepEqual(plain([...pending.settings]), [['filter', false]])
})

test('the page renders every client from index.html’s own elements', async () => {
  const stand = server(makeState())
  const page = openPage({respond: stand.respond})
  await settle()
  assert.deepEqual(page.requests.map((r) => r.method + ' ' + r.url), ['GET ' + STATE_URL])
  assert.equal(page.requests[0].authorization, 'Bearer ' + KEY)
  assert.equal(page.el('workspace').hidden, false)
  assert.equal(page.notice(), '')
  assert.equal(page.el('codex-setup').textContent, 'model_catalog_url = "' + ROOT + '/v0/resource/plugins/codex-catalog-filter/models"')
  assert.deepEqual(JSON.parse(page.el('agy-setup').textContent), {models: {listSource: ROOT + AGY_PATH}})
  assert.equal(page.el('claude-setup').textContent, 'CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1')
  assert.equal(page.el('codex-rows').children.length, 2)
  assert.equal(page.el('agy-rows').children.length, 6)
  assert.equal(page.el('claude-rows').children.length, 2)
  // Nothing switched on: CPA's Gemini text models are served instead.
  assert.equal(page.el('agy-slots').textContent, '2 of 32 slots')
  assert.match(page.el('agy-warnings').text, /No model is switched on/)
  assert.match(page.el('agy-rows').text, /Output limit fixed: 65,536 \(CPA: 1,048,576\)/)
  assert.match(page.el('agy-rows').text, /Effort kept: thinking budget 32,768/)
  assert.match(page.el('claude-rows').text, /Claude Code sees claude-fable-5-dd-gpt-6-sol/)
  for (const prefix of ['codex', 'agy', 'claude']) {
    assert.equal(page.el(prefix + '-save').disabled, true, prefix + ' has nothing to save')
    assert.equal(page.el(prefix + '-dirty').hidden, true)
  }
  // Chips say a model has no switch, and agy's non-text models stay off.
  assert.match(page.el('codex-rows').text, /No switch: on/)
  assert.match(page.el('agy-rows').text, /No switch: off \(not a text model\)/)
  assert.doesNotMatch(page.el('agy-rows').text + page.el('codex-rows').text, /Default: o/)
  // The Default and Title selectors list text models by id.
  const options = page.el('agy-default').children.map((o) => o.value)
  assert.deepEqual(options, ['', 'gemini-3.1-flash-lite', 'gemini-3.5-flash-lite', 'gemini-4-argon-high', 'gemini-4-neon-xhigh', 'gpt-6-sol'])
  assert.deepEqual(page.el('agy-title').children.map((o) => o.value), ['', 'gemini-3.1-flash-lite', 'gemini-3.5-flash-lite'])
})

test('Enable shown leaves agy’s non-text models alone unless the filter names them', async () => {
  const stand = server(makeState())
  const page = openPage({respond: stand.respond})
  await settle()
  page.el('agy-enable-shown').fire('click')
  assert.equal(page.toggle('agy', 'Antigravity', 'gpt-6-sol').checked, true)
  assert.equal(page.toggle('agy', 'Antigravity', 'grok-tts').checked, false)
  assert.match(page.notice(), /left 1 Antigravity models that do not answer in text/)
  page.el('agy-search').value = 'tts'
  page.el('agy-search').fire('input')
  page.el('agy-enable-shown').fire('click')
  assert.equal(page.toggle('agy', 'Antigravity', 'grok-tts').checked, true, 'a filtered bulk switch is deliberate')
  // Codex and Claude Code have no such rule.
  page.el('claude-disable-shown').fire('click')
  page.el('claude-enable-shown').fire('click')
  assert.equal(page.el('claude-dirty').hidden, true)
})

test('a rejected client value can be replaced by saving, with nothing changed', async () => {
  const stand = server(makeState({agy: {error: 'antigravity: unknown key "bogus"', using: 'last-good', switches: {'gpt-6-sol': true}, default_model: 'gpt-6-sol'}}))
  const respond = (request) => {
    if (request.method === 'PATCH') stand.state.antigravity.error = ''
    return stand.respond(request)
  }
  const page = openPage({respond})
  await settle()
  assert.match(page.el('agy-error').textContent, /Save this section to replace it with the settings shown here\.$/)
  assert.equal(page.el('agy-dirty').hidden, true)
  assert.equal(page.el('agy-save').disabled, false, 'Save replaces the rejected value')
  assert.equal(page.el('agy-discard').disabled, true)
  assert.equal(page.el('claude-save').disabled, true)
  page.el('agy-save').fire('click')
  await settle()
  assert.deepEqual(stand.patches, [{antigravity: {models: {'gpt-6-sol': true}, 'new-models': 'disabled', 'default-model': 'gpt-6-sol'}}])
  assert.match(page.notice(), /^Saved Antigravity\./)
  assert.equal(page.el('agy-error').hidden, true)
  assert.equal(page.el('agy-save').disabled, true)
})

test('a success notice goes once anything changes', async () => {
  const stand = server(makeState())
  const page = openPage({respond: stand.respond})
  await settle()
  const sol = page.toggle('agy', 'Antigravity', 'gpt-6-sol')
  sol.checked = true
  sol.fire('change')
  page.el('agy-save').fire('click')
  await settle()
  assert.match(page.notice(), /^Saved Antigravity\./)
  const neon = page.toggle('agy', 'Antigravity', 'gemini-4-neon-xhigh')
  neon.checked = true
  neon.fire('change')
  assert.equal(page.notice(), '')
  assert.equal(page.el('notice').hidden, true)
})

test('the slot counter and order follow switches live, before saving', async () => {
  const many = Array.from({length: 33}, (_, i) => agyModel('custom-' + String(i).padStart(2, '0')))
  const stand = server(makeState({agy: {models: [agyModel('gemini-4-argon-high', {builtin: true, builtin_enum: 1417}), ...many]}}))
  const page = openPage({respond: stand.respond})
  await settle()
  page.el('agy-enable-shown').fire('click')
  assert.equal(page.el('agy-slots').textContent, '32 of 32 slots')
  assert.equal(page.el('agy-slots').className, 'pill warn')
  assert.match(page.el('agy-warnings').text, /1 switched-on models are left out/)
  assert.match(page.el('agy-rows').text, /Left out: over 32 slots/)
  page.el('agy-default').value = 'custom-32'
  page.el('agy-default').fire('change')
  assert.equal(page.el('agy-order-list').children[0].children[0].textContent, 'custom-32', 'the default model leads the order')
  assert.match(page.el('agy-roles').textContent, /^Default: custom-32 /)
  assert.equal(page.el('agy-dirty').hidden, false)
  assert.equal(page.el('agy-save').disabled, false)
  assert.equal(page.requests.length, 1, 'changes send nothing until Save')
})

test('saving Antigravity re-reads the state and merges onto newer settings from another tab', async () => {
  const stand = server(makeState({agy: {switches: {'gpt-6-sol': true}}}))
  const page = openPage({respond: stand.respond})
  await settle()
  // Another tab saves after this one loaded.
  stand.state = applyPatch(stand.state, {antigravity: {models: {'gpt-6-sol': true, 'gemini-4-argon-high': true}, 'new-models': 'disabled', 'default-model': 'gemini-4-argon-high'}})
  const neon = page.toggle('agy', 'Antigravity', 'gemini-4-neon-xhigh')
  neon.checked = true
  neon.fire('change')
  assert.equal(page.toggle('agy', 'Antigravity', 'gemini-4-neon-xhigh').focused, true, 'focus stays on the switch after the table is redrawn')
  page.el('agy-title').value = 'gemini-3.5-flash-lite'
  page.el('agy-title').fire('change')
  page.el('agy-save').fire('click')
  await settle()
  assert.deepEqual(page.requests.map((r) => r.method), ['GET', 'GET', 'PATCH', 'GET'], 'load, re-read, save, confirm')
  assert.deepEqual(stand.patches, [{antigravity: {
    models: {'gemini-4-argon-high': true, 'gemini-4-neon-xhigh': true, 'gpt-6-sol': true},
    'new-models': 'disabled',
    'default-model': 'gemini-4-argon-high',
    'title-model': 'gemini-3.5-flash-lite',
  }}])
  assert.match(page.notice(), /^Saved Antigravity\. .*Changes saved elsewhere since this page loaded were kept\.$/)
  assert.equal(page.el('agy-dirty').hidden, true)
  assert.equal(page.el('agy-save').disabled, true)
  assert.equal(page.el('agy-default').value, 'gemini-4-argon-high', 'the page now shows the merged settings')
})

test('saving one client sends only its key and keeps the other clients’ unsaved changes', async () => {
  const stand = server(makeState({codex: {switches: {'grok-4.7': false}}}))
  const page = openPage({respond: stand.respond})
  await settle()
  const claude = page.toggle('claude', 'Claude Code', 'gpt-6-sol')
  claude.checked = false
  claude.fire('change')
  const codex = page.toggle('codex', 'Codex', 'gpt-6-sol')
  codex.checked = false
  codex.fire('change')
  page.el('codex-save').fire('click')
  await settle()
  assert.deepEqual(stand.patches, [{models: {'gpt-6-sol': false, 'grok-4.7': false}}])
  assert.equal(page.el('codex-dirty').hidden, true)
  assert.equal(page.el('claude-dirty').hidden, false, 'Claude Code’s change is still pending')
  assert.equal(page.toggle('claude', 'Claude Code', 'gpt-6-sol').checked, false)
  page.el('claude-save').fire('click')
  await settle()
  assert.deepEqual(stand.patches[1], {'claude-code': {filter: true, models: {'gpt-6-sol': false}, 'new-models': 'enabled'}})
  assert.equal(page.el('claude-dirty').hidden, true)
})

test('the Claude Code filter switch saves the whole claude-code subtree', async () => {
  const stand = server(makeState({claude: {switches: {'claude-opus-5-5': true}}}))
  const page = openPage({respond: stand.respond})
  await settle()
  page.el('claude-filter').checked = false
  page.el('claude-filter').fire('change')
  assert.match(page.el('claude-warning').textContent, /The filter is off/)
  page.el('claude-save').fire('click')
  await settle()
  assert.deepEqual(stand.patches, [{'claude-code': {filter: false, models: {'claude-opus-5-5': true}, 'new-models': 'enabled'}}])
  assert.equal(page.el('claude-summary').textContent, 'Filter off')
})

test('choosing a switched-off model as default switches it on', async () => {
  const stand = server(makeState({agy: {switches: {'gpt-6-sol': false}}}))
  const page = openPage({respond: stand.respond})
  await settle()
  page.el('agy-default').value = 'gpt-6-sol'
  page.el('agy-default').fire('change')
  assert.equal(page.toggle('agy', 'Antigravity', 'gpt-6-sol').checked, true)
  page.el('agy-save').fire('click')
  await settle()
  assert.deepEqual(stand.patches, [{antigravity: {models: {'gpt-6-sol': true}, 'new-models': 'disabled', 'default-model': 'gpt-6-sol'}}])
})

test('a rejected key stops requests: no PATCH and no retry', async () => {
  const stand = server(makeState())
  let reads = 0
  const page = openPage({respond: (request) => (request.method === 'GET' && ++reads > 1 ? json(401, {error: 'invalid management key'}) : stand.respond(request))})
  await settle()
  const sol = page.toggle('agy', 'Antigravity', 'gpt-6-sol')
  sol.checked = true
  sol.fire('change')
  page.el('agy-save').fire('click')
  await settle()
  assert.deepEqual(page.requests.map((r) => r.method), ['GET', 'GET'])
  assert.equal(stand.patches.length, 0)
  assert.match(page.notice(), /rejected/)
  assert.equal(page.el('workspace').hidden, true)
  page.el('agy-save').fire('click')
  page.el('codex-save').fire('click')
  await settle()
  assert.equal(page.requests.length, 2, 'nothing is sent after a rejected key until Refresh')
})

test('a value the plugin rejects is reported, after a bounded number of checks', async () => {
  const stand = server(makeState())
  const respond = (request) => {
    const answer = stand.respond(request)
    if (request.method === 'PATCH') stand.state.antigravity.error = 'antigravity: models lists more than 4096 switches'
    return answer
  }
  const page = openPage({respond})
  await settle()
  const sol = page.toggle('agy', 'Antigravity', 'gpt-6-sol')
  sol.checked = true
  sol.fire('change')
  page.el('agy-save').fire('click')
  await settle()
  assert.equal(page.requests.length, 1 + 1 + 1 + 10, 'load, re-read, PATCH, then ten checks')
  assert.match(page.notice(), /the plugin rejected them: antigravity: models lists more than 4096 switches/)
  assert.equal(page.el('agy-dirty').hidden, false, 'the change is kept to try again')
})

test('a client section the page cannot read is shown as unavailable; the others still work', async () => {
  const stand = server({...makeState(), claude_code: {config_key: 'claude-code', models: 'nope'}})
  const page = openPage({respond: stand.respond})
  await settle()
  assert.equal(page.el('claude-content').hidden, true)
  assert.equal(page.el('claude-unusable').hidden, false)
  assert.equal(page.el('codex-content').hidden, false)
  assert.equal(page.el('codex-rows').children.length, 2)
  page.el('claude-save').fire('click')
  await settle()
  assert.equal(page.requests.length, 1)
})

test('a rejected client value shows what still applies', async () => {
  const stand = server(makeState({agy: {error: 'antigravity: unknown key', using: 'last-good'}}))
  const page = openPage({respond: stand.respond})
  await settle()
  assert.equal(page.el('agy-error').hidden, false)
  assert.match(page.el('agy-error').textContent, /^The plugin rejected the antigravity value in CPA’s configuration: antigravity: unknown key\. Antigravity uses the last valid settings/)
})

test('open sections are remembered per browser, and failing storage changes nothing else', async () => {
  const stand = server(makeState())
  const page = openPage({respond: stand.respond, storage: {...SESSION, [SECTIONS_KEY]: '{"antigravity":false}'}})
  await settle()
  assert.equal(page.el('codex-section').open, true)
  assert.equal(page.el('agy-section').open, false)
  page.el('codex-section').open = false
  page.el('codex-section').fire('toggle')
  assert.deepEqual(JSON.parse(page.store.get(SECTIONS_KEY)), {codex: false, antigravity: false, claude: true})

  const failing = openPage({respond: server(makeState()).respond, failingStorage: true})
  await settle()
  assert.equal(failing.requests.length, 1, 'the session still loads')
  failing.el('agy-section').open = false
  assert.doesNotThrow(() => failing.el('agy-section').fire('toggle'))
  assert.equal(failing.el('workspace').hidden, false)
})
