'use strict'

const test = require('node:test')
const assert = require('node:assert')
const { EventEmitter } = require('node:events')

const fakeClient = new EventEmitter()
fakeClient.published = []
fakeClient.subscribed = []
fakeClient.unsubscribed = []
fakeClient.subscribe = (t) => fakeClient.subscribed.push(t)
fakeClient.unsubscribe = (t) => fakeClient.unsubscribed.push(t)
fakeClient.publish = (topic, payload, opts) => fakeClient.published.push({ topic, payload, opts })
fakeClient.end = (_f, _o, cb) => cb && cb()

const mqttPath = require.resolve('mqtt')
require.cache[mqttPath] = {
  id: mqttPath, filename: mqttPath, loaded: true, exports: { connect: () => fakeClient },
}

function makeRED () {
  const types = {}
  const instances = {}
  const RED = {
    nodes: {
      registerType: (name, ctor) => { types[name] = ctor },
      createNode: (node, config) => {
        Object.setPrototypeOf(
          node,
          Object.assign(Object.create(EventEmitter.prototype), {
            log () {}, warn () {}, error () {}, debug () {}, trace () {},
            status (s) { this._status = s },
            send (m) { this._sent.push(m) },
          })
        )
        EventEmitter.call(node)
        node._sent = []
        node.id = config.id
        node.credentials = {}
      },
      getNode: (id) => instances[id],
    },
    httpAdmin: { get () {} },
    auth: { needsPermission: () => () => {} },
  }
  const create = (type, config) => {
    const node = new types[type](config)
    instances[config.id] = node
    return node
  }
  return { RED, create }
}

function setup (outConfig = {}) {
  fakeClient.published.length = 0
  fakeClient.subscribed.length = 0
  fakeClient.removeAllListeners()

  const { RED, create } = makeRED()
  require('../nodes/matter2mqtt-broker')(RED)
  require('../nodes/matter2mqtt-out')(RED)

  create('matter2mqtt-broker', { id: 'b1', brokerUrl: 'mqtt://x', prefix: 'matter' })
  fakeClient.emit('connect')
  fakeClient.emit('message', 'matter/node/4/name', Buffer.from('TEMP Ikea'))

  const out = create('matter2mqtt out', { id: 'o1', broker: 'b1', ...outConfig })
  return { out, create }
}

// Drives one input message and captures the node's completion callback.
function input (node, msg) {
  const errors = []
  const sent = []
  node.emit(
    'input',
    msg,
    (m) => sent.push(m),
    (err) => { if (err) errors.push(err) }
  )
  return { errors, sent }
}

const lastPublish = () => fakeClient.published[fakeClient.published.length - 1]

test('an attribute write reaches the numeric set topic', () => {
  const { out } = setup({ mode: 'set', device: 'TEMP Ikea', endpoint: '1', cluster: '8', attribute: '17' })
  const { errors } = input(out, { payload: 128 })

  assert.deepStrictEqual(errors, [])
  assert.strictEqual(lastPublish().topic, 'matter/node/4/1/8/17/set')
  assert.strictEqual(lastPublish().payload, '128')
  assert.strictEqual(lastPublish().opts.retain, false)
})

test('cluster and attribute names are passed through for the bridge to resolve', () => {
  const { out } = setup({
    mode: 'set', device: 'TEMP Ikea', endpoint: '1', cluster: 'LevelControl', attribute: 'OnLevel',
  })
  input(out, { payload: 128 })
  assert.strictEqual(lastPublish().topic, 'matter/node/4/1/LevelControl/OnLevel/set')
})

test('a written value gets the inverse conversion', () => {
  const { out } = setup({
    mode: 'set', device: 'TEMP Ikea', endpoint: '1', cluster: '1026', attribute: '0',
  })
  input(out, { payload: 21.5 })
  assert.strictEqual(lastPublish().payload, '2150')

  // …and only where a conversion is defined: OnOff is untouched.
  const { out: plain } = setup({
    mode: 'set', device: 'TEMP Ikea', endpoint: '1', cluster: '6', attribute: '0',
  })
  input(plain, { payload: true })
  assert.strictEqual(lastPublish().payload, 'true')
})

test('conversion on write can be turned off', () => {
  const { out } = setup({
    mode: 'set', device: 'TEMP Ikea', endpoint: '1', cluster: '1026', attribute: '0', scale: false,
  })
  input(out, { payload: 2150 })
  assert.strictEqual(lastPublish().payload, '2150')
})

test('a cluster command is enveloped', () => {
  const { out } = setup({ mode: 'command', device: 'TEMP Ikea', endpoint: '1', cluster: 'OnOff', command: 'Toggle' })
  input(out, {})
  assert.strictEqual(lastPublish().topic, 'matter/node/4/1/OnOff/command')
  assert.deepStrictEqual(JSON.parse(lastPublish().payload), { command: 'Toggle' })

  input(out, { payload: { level: 128, transitionTime: 10 } })
  assert.deepStrictEqual(JSON.parse(lastPublish().payload), {
    command: 'Toggle', payload: { level: 128, transitionTime: 10 },
  })
})

test('a payload that already carries a command is sent unchanged', () => {
  const { out } = setup({ mode: 'command', device: 'TEMP Ikea', endpoint: '1', cluster: '8' })
  input(out, { payload: { command: 'MoveToLevel', payload: { level: 64 } } })
  assert.deepStrictEqual(JSON.parse(lastPublish().payload), {
    command: 'MoveToLevel', payload: { level: 64 },
  })
})

test('message properties override the configured fields', () => {
  const { out } = setup({ mode: 'set', device: 'TEMP Ikea', endpoint: '1', cluster: '8', attribute: '17' })
  input(out, { payload: 5, endpoint: 2, cluster: '6', attribute: '0' })
  assert.strictEqual(lastPublish().topic, 'matter/node/4/2/6/0/set')
})

test('an unknown device is an error, not a silent publish', () => {
  const { out } = setup({ mode: 'set', device: 'Ghost', endpoint: '1', cluster: '6', attribute: '0' })
  const { errors } = input(out, { payload: true })
  assert.strictEqual(fakeClient.published.length, 0)
  assert.match(errors[0].message, /unknown device "Ghost"/)
})

test('a missing endpoint or cluster is refused', () => {
  const { out } = setup({ mode: 'set', device: 'TEMP Ikea', cluster: '6', attribute: '0', endpoint: '' })
  assert.match(input(out, { payload: true }).errors[0].message, /no endpoint/)
})

test('a bridge request is correlated to its reply', () => {
  const { out } = setup({ mode: 'request', command: 'get_nodes', timeout: 5 })
  assert.ok(fakeClient.subscribed.includes('matter/bridge/response/+'))

  const { errors, sent } = input(out, { payload: {} })
  assert.deepStrictEqual(errors, [])

  const published = lastPublish()
  assert.strictEqual(published.topic, 'matter/bridge/request/get_nodes')
  const requestId = JSON.parse(published.payload)._request_id
  assert.ok(requestId, 'a _request_id must be attached')

  // A reply to somebody else's request must not resolve ours.
  fakeClient.emit('message', 'matter/bridge/response/get_nodes',
    Buffer.from(JSON.stringify({ command: 'get_nodes', success: true, result: [], _request_id: 'other' })))
  assert.strictEqual(sent.length, 0)

  fakeClient.emit('message', 'matter/bridge/response/get_nodes',
    Buffer.from(JSON.stringify({
      command: 'get_nodes', success: true, result: [{ node_id: 4 }], _request_id: requestId,
    })))

  assert.strictEqual(sent.length, 1)
  assert.deepStrictEqual(sent[0].payload, [{ node_id: 4 }])
  assert.strictEqual(sent[0].topic, 'get_nodes')
  assert.strictEqual(sent[0].matter.success, true)
})

test('a failed bridge request raises an error instead of emitting', () => {
  const { out } = setup({ mode: 'request', command: 'remove_node', timeout: 5 })
  const { errors, sent } = input(out, { payload: { node_id: 99 } })

  const requestId = JSON.parse(lastPublish().payload)._request_id
  fakeClient.emit('message', 'matter/bridge/response/remove_node',
    Buffer.from(JSON.stringify({
      command: 'remove_node', success: false, error: 'no such node', _request_id: requestId,
    })))

  assert.strictEqual(sent.length, 0)
  assert.match(errors[0].message, /no such node/)
})

test('request arguments are carried through', () => {
  const { out } = setup({ mode: 'request', timeout: 5 })
  input(out, { command: 'commission_with_code', payload: { code: 'MT:Y.K90-Q000KA0648G00' } })

  const body = JSON.parse(lastPublish().payload)
  assert.strictEqual(lastPublish().topic, 'matter/bridge/request/commission_with_code')
  assert.strictEqual(body.code, 'MT:Y.K90-Q000KA0648G00')
})

test('write and command modes never subscribe to the response tree', () => {
  setup({ mode: 'set', device: 'TEMP Ikea', endpoint: '1', cluster: '6', attribute: '0' })
  assert.ok(!fakeClient.subscribed.includes('matter/bridge/response/+'))
})
