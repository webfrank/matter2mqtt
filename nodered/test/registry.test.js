'use strict'

const test = require('node:test')
const assert = require('node:assert')
const { Registry } = require('../lib/registry')

const decoded = (text) => ({ text, json: safeJSON(text), empty: text === '' })
const safeJSON = (s) => { try { return JSON.parse(s) } catch { return undefined } }

test('a device is discovered by its verbatim label', () => {
  const r = new Registry()
  r.applyMeta('4', 'name', decoded('TEMP Ikea'))

  assert.strictEqual(r.lookup('TEMP Ikea'), '4')
  // The bridge folds case when resolving names, so this does too.
  assert.strictEqual(r.lookup('temp ikea'), '4')
  // But the stored name keeps its original form.
  assert.strictEqual(r.devices.get('4').name, 'TEMP Ikea')
  assert.strictEqual(r.lookup('temp-ikea'), null)
})

test('a node id resolves directly, once the node is known', () => {
  const r = new Registry()
  assert.strictEqual(r.lookup('4'), null)
  r.applyMeta('4', 'name', decoded('Salotto'))
  assert.strictEqual(r.lookup('4'), '4')
})

test('an empty retained name means the label was cleared', () => {
  const r = new Registry()
  r.applyMeta('4', 'name', decoded('Old'))
  assert.strictEqual(r.lookup('Old'), '4')

  const renamed = r.applyMeta('4', 'name', decoded(''))
  assert.strictEqual(renamed, true)
  assert.strictEqual(r.lookup('Old'), null)
  assert.deepStrictEqual(r.list(), []) // unnamed devices are not listed
})

test('renaming is reported so subscriptions can be re-resolved', () => {
  const r = new Registry()
  const seen = []
  r.on('renamed', (d) => seen.push(d.name))

  assert.strictEqual(r.applyMeta('4', 'name', decoded('Old')), true)
  assert.strictEqual(r.applyMeta('4', 'name', decoded('Old')), false) // idempotent replay
  assert.strictEqual(r.applyMeta('4', 'name', decoded('New')), true)
  assert.deepStrictEqual(seen, ['Old', 'New'])
})

test('availability and descriptor enrich the entry', () => {
  const r = new Registry()
  r.applyMeta('4', 'name', decoded('Kitchen'))
  r.applyMeta('4', 'availability', decoded('online'))
  r.applyMeta('4', 'descriptor', decoded(JSON.stringify({
    vendor_name: 'IKEA of Sweden',
    product_name: 'Vallhorn',
    endpoints: { 1: { clusters: { 1026: 'TemperatureMeasurement', 1029: 'RelativeHumidityMeasurement' } } },
  })))

  const [d] = r.list()
  assert.strictEqual(d.available, true)
  assert.strictEqual(d.product, 'Vallhorn')
  assert.deepStrictEqual(d.clusters, [
    { endpoint: 1, cluster: 1026, name: 'TemperatureMeasurement' },
    { endpoint: 1, cluster: 1029, name: 'RelativeHumidityMeasurement' },
  ])

  r.applyMeta('4', 'availability', decoded('offline'))
  assert.strictEqual(r.list()[0].available, false)
})

test('the listing is ordered by node id', () => {
  const r = new Registry()
  r.applyMeta('9', 'name', decoded('Nine'))
  r.applyMeta('4', 'name', decoded('Four'))
  r.applyMeta('11', 'name', decoded('Eleven'))
  assert.deepStrictEqual(r.list().map((d) => d.nodeId), ['4', '9', '11'])
})
