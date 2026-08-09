'use strict'

const test = require('node:test')
const assert = require('node:assert')
const topics = require('../lib/topics')

const P = 'matter' // a multi-level prefix, the harder case

test('attribute topics are parsed numerically', () => {
  assert.deepStrictEqual(topics.parse(P, `${P}/node/4/1/1026/0`), {
    kind: 'attribute', nodeId: '4', endpoint: 1, cluster: 1026, attribute: 0,
  })
})

test('per-node metadata is recognised', () => {
  assert.deepStrictEqual(topics.parse(P, `${P}/node/4/name`), { kind: 'meta', nodeId: '4', what: 'name' })
  assert.deepStrictEqual(topics.parse(P, `${P}/node/4/availability`), { kind: 'meta', nodeId: '4', what: 'availability' })
  assert.deepStrictEqual(topics.parse(P, `${P}/node/4/descriptor`), { kind: 'meta', nodeId: '4', what: 'descriptor' })
})

test('node events are not mistaken for attributes', () => {
  // node/<id>/event/<ep>/<cluster>/<event> has one segment more.
  assert.strictEqual(topics.parse(P, `${P}/node/4/event/1/6/0`), null)
})

test('the named mirror is ignored, since the numeric tree is canonical', () => {
  assert.strictEqual(topics.parse(P, `${P}/named/TEMP Ikea/1/TemperatureMeasurement/MeasuredValue`), null)
})

test('foreign topics and foreign prefixes are rejected', () => {
  assert.strictEqual(topics.parse(P, 'other/node/4/1/1026/0'), null)
  assert.strictEqual(topics.parse(P, 'matterx/node/4/1/1026/0'), null)
  assert.strictEqual(topics.parse(P, `${P}/node/notanumber/1/1026/0`), null)
})

test('bridge status topics are classified', () => {
  assert.deepStrictEqual(topics.parse(P, `${P}/bridge/availability`), { kind: 'bridge', what: 'availability' })
})

test('subscription filters are built around the prefix', () => {
  assert.strictEqual(topics.nodeMeta(P), 'matter/node/+/+')
  assert.strictEqual(topics.nodeAttributes(P, '4'), 'matter/node/4/+/+/+')
  assert.strictEqual(topics.nodeMeta('/matter/'), 'matter/node/+/+')
})

test('payloads decode as JSON, or as text when they are bare strings', () => {
  assert.deepStrictEqual(topics.decode(Buffer.from('2137')), { text: '2137', json: 2137, empty: false })
  // name and availability are published as bare strings, not JSON.
  assert.deepStrictEqual(topics.decode(Buffer.from('TEMP Ikea')), {
    text: 'TEMP Ikea', json: undefined, empty: false,
  })
  assert.deepStrictEqual(topics.decode(Buffer.from('online')), {
    text: 'online', json: undefined, empty: false,
  })
  // An empty retained payload is a deletion, not a value.
  assert.strictEqual(topics.decode(Buffer.from('')).empty, true)
  assert.strictEqual(topics.decode(Buffer.from('null')).json, null)
})
