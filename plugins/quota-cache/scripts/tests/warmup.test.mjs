import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import {test} from 'node:test'
import vm from 'node:vm'

const script = readFileSync(new URL('../../internal/plugin/pageassets/warmup.js', import.meta.url), 'utf8')
function fixture({session = {key: 'fixture-key'}, refused = null, status = 202} = {}) {
  const elements = new Map()
  const element = () => ({append() {}, replaceChildren() {}, addEventListener(event, handler) { this[event] = handler }})
  const get = id => elements.get(id) || elements.set(id, element()).get(id)
  const calls = []
  let remembered = refused
  let rejected = 0
  const page = {
    root: 'http://fixture', readSession: () => session, refusal: () => remembered,
    node: element, remember: () => (remembered = {reason: 'refused'}), refusedSave: () => rejected++,
    unansweredSave() {}, reload() {}
  }
  const context = vm.createContext({window: {QuotaCachePage: page}, document: {getElementById: get, createTextNode: value => value},
    fetch: async (...args) => { calls.push(args); return {status} }, AbortSignal, Map, Date, Boolean, Object})
  vm.runInContext(script, context)
  const render = (enabled = true) => context.window.QuotaCacheWarmup.render({warmup: {enabled, model: 'claude-future-model', effort: 'low', hours: '09:00-17:00', timezone: 'UTC', accounts: []}})
  return {get, calls, render, rejected: () => rejected}
}

test('warm-up sends nothing without a session, after a refusal, or while disabled', async () => {
  for (const options of [{session: null}, {refused: {reason: 'refused'}}]) {
    const f = fixture(options); f.render(); await f.get('warm-all').click()
    assert.equal(f.calls.length, 0)
  }
  const f = fixture(); f.render(false); await f.get('warm-all').click()
  assert.equal(f.calls.length, 0)
})

test('Warm all now sends one authenticated management POST from a press', async () => {
  const f = fixture(); f.render()
  assert.equal(f.calls.length, 0)
  assert.match(f.get('warmup-config').textContent, /claude-future-model/)
  await f.get('warm-all').click()
  assert.equal(f.calls.length, 1)
  const [url, options] = f.calls[0]
  assert.equal(url, 'http://fixture/v0/management/plugins/quota-cache/warmup')
  assert.equal(options.method, 'POST')
  assert.equal(options.headers.Authorization, 'Bearer fixture-key')
})

test('a refused warm-up is remembered before any later press', async () => {
  for (const status of [401, 403]) {
    const f = fixture({status}); f.render(); await f.get('warm-all').click(); await f.get('warm-all').click()
    assert.equal(f.calls.length, 1)
    assert.equal(f.rejected(), 1)
  }
})
