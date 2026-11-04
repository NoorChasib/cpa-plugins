// The page's console session reader, run as shipped: app.js, editor.js and
// warmup.js are joined as status_page.go joins them and loaded at CPA's
// resource route, with local storage holding what the official Management
// Center stores. Each case checks what readSession recovers and whether the
// page's first status request carries that key. fetch only records its
// arguments, so no request leaves.
//
// Run: node --test plugins/quota-cache/scripts/tests/

import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import {test} from 'node:test'
import vm from 'node:vm'

const assets = new URL('../../internal/plugin/pageassets/', import.meta.url)
const script = ['app.js', 'editor.js', 'warmup.js'].map(name => readFileSync(new URL(name, assets), 'utf8')).join('\n')

const HOST = '127.0.0.1:8317'
const USER_AGENT = 'Mozilla/5.0 (X11; Linux x86_64) TestUA/1'
const KEY = 'test-management-key-0123456789'
const BASE = 'http://' + HOST
const STATUS_URL = BASE + '/v0/management/plugins/quota-cache/status'

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
  const element = () => new Proxy({}, {get: (target, key) => (key in target ? target[key] : () => {}), set: (target, key, value) => { target[key] = value; return true }})
  const elements = new Map()
  const fetches = []
  const local = new Map(Object.entries(entries))
  const tab = new Map()
  const store = map => ({getItem: name => (map.has(name) ? map.get(name) : null), setItem: (name, value) => { map.set(name, String(value)) }, removeItem: name => { map.delete(name) }})
  const context = vm.createContext({
    addEventListener() {}, location: {pathname: '/v0/resource/plugins/quota-cache/status', protocol: 'http:', origin: 'http://' + host, host, hash: ''},
    document: {getElementById: id => elements.get(id) || elements.set(id, element()).get(id), createElement: element, createTextNode: value => value, addEventListener() {}, hidden: false},
    localStorage: store(local), sessionStorage: store(tab), navigator: {userAgent},
    setInterval: () => 0, setTimeout: () => 0, clearTimeout() {}, fetch: (...args) => { fetches.push(args); return new Promise(() => {}) },
    TextEncoder, TextDecoder, atob, URL, Headers, AbortSignal, Intl, console,
  })
  context.window = context
  vm.runInContext(script, context)
  const page = context.QuotaCachePage
  assert.ok(page, 'app.js defines its namespace')
  return {page, fetches}
}

function recovered({page, fetches}) {
  assert.deepEqual({...page.readSession()}, {key: KEY, base: BASE})
  assert.equal(fetches.length, 1, 'the page sent its status request')
  const [url, init] = fetches[0]
  assert.equal(url, STATUS_URL)
  assert.equal(init.headers.Authorization, 'Bearer ' + KEY)
}
function noSession({page, fetches}) {
  assert.equal(page.readSession(), null)
  assert.equal(fetches.length, 0, 'the page sent a request without a usable session')
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
  noSession(open({'cli-proxy-auth': V2_OTHER_HOST}))
  // And V2 itself, opened on another host.
  noSession(open({'cli-proxy-auth': V2}, {host: 'localhost:8317'}))
})

test('enc::v2:: with rememberPassword false is not recovered', () => {
  noSession(open({'cli-proxy-auth': V2_REMEMBER_OFF_WITH_KEY}))
  noSession(open({'cli-proxy-auth': V2_REMEMBER_OFF}))
})

test('a session saved by Management Center before v1.27 (enc::v1::) is still recovered', () => {
  recovered(open({'cli-proxy-auth': V1}))
})

test('enc::v1:: stays keyed by the user agent', () => {
  noSession(open({'cli-proxy-auth': V1}, {userAgent: 'Mozilla/5.0 (Macintosh) Other/2'}))
})

test('an unknown prefix is not decoded and gives no session', () => {
  noSession(open({'cli-proxy-auth': 'enc::v3::' + V2.slice(9)}))
})

test('enc::v2:: decoding is strict UTF-8', () => {
  // A session the reader would accept, except one byte that is not UTF-8. A
  // lenient decoder would turn it into U+FFFD and recover the key.
  const plain = new TextEncoder().encode(PLAINTEXT)
  const at = PLAINTEXT.indexOf('8.0.53') + 5
  assert.equal(PLAINTEXT[at], '3')
  plain[at] = 0xff
  noSession(open({'cli-proxy-auth': obfuscate('enc::v2::', plain, V2_SALT)}))
})

test('an unreadable enc::v2:: entry is authoritative: legacy entries are not revived', () => {
  const legacy = {isLoggedIn: 'true', apiBase: JSON.stringify(BASE), managementKey: KEY}
  recovered(open(legacy))
  noSession(open({...legacy, 'cli-proxy-auth': V2_OTHER_HOST}))
  noSession(open({...legacy, 'cli-proxy-auth': V2_REMEMBER_OFF}))
})
