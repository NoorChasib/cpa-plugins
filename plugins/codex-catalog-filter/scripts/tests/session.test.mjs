// The Codex Models page's console session reader, run as shipped: app.js is
// loaded unchanged into a stub page at CPA's resource route, with local
// storage holding what the official Management Center stores, and the test
// watches whether the page's first request carries the remembered key or the
// page asks to sign in instead. fetch only records its arguments, so no
// request leaves.
//
// The go test in internal/plugin runs this file when node is on PATH.
// Run directly: node --test plugins/codex-catalog-filter/scripts/tests/

import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import {test} from 'node:test'
import vm from 'node:vm'

const script = readFileSync(new URL('../../internal/plugin/pageassets/app.js', import.meta.url), 'utf8')

const HOST = '127.0.0.1:8317'
const USER_AGENT = 'Mozilla/5.0 (X11; Linux x86_64) TestUA/1'
const KEY = 'test-management-key-0123456789'
const STATE_URL = 'http://' + HOST + '/v0/management/plugins/codex-catalog-filter/state'

// Remembered sessions exactly as the official Management Center stores them
// under cli-proxy-auth, produced by upstream's own src/utils/encryption.ts
// (obfuscateData) at host 127.0.0.1:8317 with the user agent above: the V2*
// values by tag v1.27.2 (commit f154fa9380825c4fe94bc577a9abaa7e505f8875),
// V1 by v1.26.7, the last release that wrote enc::v1::. Every vector encodes
// PLAINTEXT except where noted.
const PLAINTEXT = '{"state":{"apiBase":"http://127.0.0.1:8317","managementKey":"test-management-key-0123456789","rememberPassword":true,"serverVersion":"8.0.53","serverBuildDate":null},"version":0}'
const V2 = 'enc::v2::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSAAgPHQEZEgITV1ldUFAGAQxDBDkKAVsXQwQMXgNIDxQHW10WCAYbBkhGFg1CQlBVVkgEBAAWCQwcDENfVVZcVQYeOUwDARgXC0lDSh1fAgBOVxpfSAUAESMXF14aGwFQW0VdUgEcAh0SAhJdVEhOVkN1FgUFSTQTGx1bFw8FBUEKSUADDEhJGgoNV0hVUA=='
// The same plaintext, written by a console served at localhost:8317.
const V2_OTHER_HOST = 'enc::v2::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSXVVbUl1fTV9WV1ldUFAGAQxDBDkKAVsXQwQMXgNIDxQHW10WCAYbBkhGFg1CQlBVVkhZWVRZVUpDUQZfVVZcVQYeOUwDARgXC0lDSh1fAgBOVxpfSAUAESMXF14aGwFQW0VdUlxBVlJORE0AEUhOVkN1FgUFSTQTGx1bFw8FBUEKSUADDEhJGgoNV0hVUA=='
// rememberPassword false with the key still present, so only the remember
// rule refuses it:
// {"state":{"apiBase":"http://127.0.0.1:8317","managementKey":"test-management-key-0123456789","rememberPassword":false,"serverVersion":"8.0.53","serverBuildDate":null},"version":0}
const V2_REMEMBER_OFF_WITH_KEY = 'enc::v2::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSAAgPHQEZEgITV1ldUFAGAQxDBDkKAVsXQwQMXgNIDxQHW10WCAYbBkhGFg1CQlBVVkgEBAAWCQwcDENfVVZcVQYeOUwDARgXC0lDSg9MGxYHWUtJXwETBgckAF8AHQAcQ11HRB8CGRsDDBwMQl9KRVRFIRkAQRQ2DgwcD1seHEEbGE5XH19IAAwMG1BfHQ4='
// rememberPassword false as the console itself persists it, without the key:
// {"state":{"apiBase":"http://127.0.0.1:8317","rememberPassword":false,"serverVersion":"8.0.53","serverBuildDate":null},"version":0}
const V2_REMEMBER_OFF = 'enc::v2::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSAAgPHQEZEgITSF1eVFoBCRt9EQEcDxZfBVJTSxYJERBFGEkWFxUQADNIAQcGHQ9FX14JHAcABR0SAhNJXUFHUhEuHEQcFisZDUhDSgdYGwkfWUtMXwEWChocRxdDCQ=='
const V1 = 'enc::v1::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSAAgPHQEZEgITV1ldUFAZIAoUHScJGA0PDERFWywcXFpOLQ4LGB1ODBVdOk8ZGRFmVkdBY3YXCEFAS18VHwoVG0gTIAheBBINBw0YAAcXFhBeR14WBhkXEzEADkJbWEASFBIWHwoWBgIVUG8cHxsaCRNtQEdcRGw5RVQZGiIcAhkFDFpOUy1FXUZOdl9DCQ=='

// Opens the page with these local storage entries and reports what it did.
function open(entries, {host = HOST, userAgent = USER_AGENT} = {}) {
  const elements = new Map()
  const element = () => ({addEventListener() {}, replaceChildren() {}, setAttribute() {}, append() {}})
  const fetches = []
  const storage = new Map(Object.entries(entries))
  const context = vm.createContext({
    location: {protocol: 'http:', host, origin: 'http://' + host, pathname: '/v0/resource/plugins/codex-catalog-filter/settings'},
    document: {getElementById: id => elements.get(id) || elements.set(id, element()).get(id), createElement: element},
    localStorage: {getItem: name => (storage.has(name) ? storage.get(name) : null)},
    navigator: {userAgent},
    addEventListener() {}, confirm: () => false,
    fetch: (...args) => { fetches.push(args); return new Promise(() => {}) },
    setTimeout: () => 0, clearTimeout() {},
    TextEncoder, TextDecoder, atob, URL, Headers, AbortController,
  })
  context.window = context
  vm.runInContext(script, context)
  const notice = elements.get('notice')
  return {fetches, notice: notice ? notice.textContent : ''}
}

function recovered(page) {
  assert.equal(page.fetches.length, 1, 'the page sent its state request')
  const [url, init] = page.fetches[0]
  assert.equal(url, STATE_URL)
  assert.equal(init.headers.Authorization, 'Bearer ' + KEY)
}
function signInAsked(page) {
  assert.equal(page.fetches.length, 0, 'the page sent a request without a usable session')
  assert.match(page.notice, /No compatible remembered console session/)
}

// Builds a console entry from raw bytes, for inputs upstream cannot produce.
// Pinned to upstream below, so it encodes exactly as encryption.ts does.
function obfuscate(prefix, bytes, salt) {
  const key = new TextEncoder().encode(salt)
  return prefix + btoa(String.fromCharCode(...bytes.map((byte, i) => byte ^ key[i % key.length])))
}
const V2_SALT = 'cli-proxy-api-webui::secure-storage|' + HOST

test('the obfuscate helper reproduces upstream v1.27.2 and v1.26.7 byte for byte', () => {
  const plain = new TextEncoder().encode(PLAINTEXT)
  assert.equal(obfuscate('enc::v2::', plain, V2_SALT), V2)
  assert.equal(obfuscate('enc::v1::', plain, V2_SALT + '|' + USER_AGENT), V1)
})

test('a session saved by Management Center v1.27+ (enc::v2::) is recovered', () => {
  recovered(open({'cli-proxy-auth': V2}))
})

test('enc::v2:: is keyed by the host alone, so another user agent still recovers it', () => {
  recovered(open({'cli-proxy-auth': V2}, {userAgent: 'Mozilla/5.0 (Macintosh) Other/2'}))
})

test('enc::v2:: written under another host is not recovered', () => {
  // Same plaintext and API base as V2, which is recovered above: only the
  // host the console ran on differs.
  signInAsked(open({'cli-proxy-auth': V2_OTHER_HOST}))
  // And V2 itself, opened on another host.
  signInAsked(open({'cli-proxy-auth': V2}, {host: 'localhost:8317'}))
})

test('enc::v2:: with rememberPassword false is not recovered', () => {
  signInAsked(open({'cli-proxy-auth': V2_REMEMBER_OFF_WITH_KEY}))
  signInAsked(open({'cli-proxy-auth': V2_REMEMBER_OFF}))
})

test('a session saved by Management Center before v1.27 (enc::v1::) is still recovered', () => {
  recovered(open({'cli-proxy-auth': V1}))
})

test('enc::v1:: stays keyed by the user agent', () => {
  signInAsked(open({'cli-proxy-auth': V1}, {userAgent: 'Mozilla/5.0 (Macintosh) Other/2'}))
})

test('an unknown prefix is not decoded and gives no session', () => {
  signInAsked(open({'cli-proxy-auth': 'enc::v3::' + V2.slice(9)}))
})

test('enc::v2:: decoding is strict UTF-8', () => {
  // A session the reader would accept, except one byte that is not UTF-8. A
  // lenient decoder would turn it into U+FFFD and recover the key.
  const plain = new TextEncoder().encode(PLAINTEXT)
  const at = PLAINTEXT.indexOf('8.0.53') + 5
  assert.equal(PLAINTEXT[at], '3')
  plain[at] = 0xff
  signInAsked(open({'cli-proxy-auth': obfuscate('enc::v2::', plain, V2_SALT)}))
})

test('an unreadable enc::v2:: entry is authoritative: legacy entries are not revived', () => {
  const legacy = {isLoggedIn: 'true', apiBase: JSON.stringify('http://' + HOST), managementKey: KEY}
  recovered(open(legacy))
  signInAsked(open({...legacy, 'cli-proxy-auth': V2_OTHER_HOST}))
  signInAsked(open({...legacy, 'cli-proxy-auth': V2_REMEMBER_OFF}))
})
