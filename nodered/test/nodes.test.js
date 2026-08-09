'use strict'

const test = require('node:test')
const assert = require('node:assert')
const { EventEmitter } = require('node:events')

// Replace the mqtt module before the broker node requires it, so the whole
// package can be exercised without a broker, a bridge, or a network.
const fakeClient = new EventEmitter()
fakeClient.subscribed = []
fakeClient.unsubscribed = []
fakeClient.subscribe = (t) => fakeClient.subscribed.push(t)
fakeClient.unsubscribe = (t) => fakeClient.unsubscribed.push(t)
fakeClient.end = (_force, _opts, cb) => cb && cb()

const mqttPath = require.resolve('mqtt')
require.cache[mqttPath] = {
  id: mqttPath,
  filename: mqttPath,
  loaded: true,
  exports: { connect: () => fakeClient },
}

// A minimal stand-in for the Node-RED runtime: enough of the API that the
// nodes exercise, and nothing else.
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
        node.name = config.name
        node.credentials = config.credentials || {}
      },
      getNode: (id) => instances[id],
    },
    httpAdmin: { get () {} },
    auth: { needsPermission: () => (req, res, next) => next() },
  }
  const create = (type, config) => {
    const node = new types[type](config)
    instances[config.id] = node
    return node
  }
  return { RED, create }
}

function setup (inConfig = {}) {
  fakeClient.subscribed.length = 0
  fakeClient.unsubscribed.length = 0
  fakeClient.removeAllListeners()

  const { RED, create } = makeRED()
  require('../nodes/matter2mqtt-broker')(RED)
  require('../nodes/matter2mqtt-in')(RED)
  require('../nodes/matter2mqtt-device')(RED)

  const broker = create('matter2mqtt-broker', {
    id: 'b1', brokerUrl: 'mqtt://localhost:1883', prefix: 'matter',
  })
  const sensor = create('matter2mqtt in', {
    id: 's1', broker: 'b1', device: 'TEMP Ikea', mode: 'measured', onNull: 'send', ...inConfig,
  })
  fakeClient.emit('connect')
  return { broker, sensor, create, RED }
}

const publish = (topic, payload) =>
  fakeClient.emit('message', topic, Buffer.from(String(payload)))

test('the broker subscribes to the retained metadata tree on connect', () => {
  setup()
  assert.ok(fakeClient.subscribed.includes('matter/node/+/+'))
  assert.ok(fakeClient.subscribed.includes('matter/bridge/+'))
})

test('a sensor binds to its device once the retained name arrives', () => {
  const { sensor } = setup()
  assert.match(sensor._status.text, /waiting for "TEMP Ikea"/)

  publish('matter/node/4/name', 'TEMP Ikea')

  assert.ok(fakeClient.subscribed.includes('matter/node/4/+/+/+'))
  assert.match(sensor._status.text, /node 4/)
})

test('a reading is converted and annotated', () => {
  const { sensor } = setup()
  publish('matter/node/4/name', 'TEMP Ikea')
  publish('matter/node/4/availability', 'online')
  publish('matter/node/4/1/1026/0', '2137')

  assert.strictEqual(sensor._sent.length, 1)
  const msg = sensor._sent[0]
  assert.strictEqual(msg.payload, 21.37)
  assert.strictEqual(msg.topic, 'TEMP Ikea/1/TemperatureMeasurement/MeasuredValue')
  assert.deepStrictEqual(msg.matter, {
    nodeId: 4,
    name: 'TEMP Ikea',
    endpoint: 1,
    cluster: 1026,
    clusterName: 'TemperatureMeasurement',
    attribute: 0,
    attributeName: 'MeasuredValue',
    raw: 2137,
    unit: '°C',
    scaled: true,
    available: true,
  })
  assert.strictEqual(sensor._status.text, '21.37 °C')
})

test('humidity from the same device comes through the same node', () => {
  const { sensor } = setup()
  publish('matter/node/4/name', 'TEMP Ikea')
  publish('matter/node/4/1/1029/0', '4550')

  assert.strictEqual(sensor._sent[0].payload, 45.5)
  assert.strictEqual(sensor._sent[0].matter.unit, '%')
})

test('by default only MeasuredValue is reported', () => {
  const { sensor } = setup()
  publish('matter/node/4/name', 'TEMP Ikea')
  publish('matter/node/4/1/1026/2', '12500') // MaxMeasuredValue
  publish('matter/node/4/0/40/5', '"TEMP Ikea"') // NodeLabel
  assert.strictEqual(sensor._sent.length, 0)

  publish('matter/node/4/1/1026/0', '2137')
  assert.strictEqual(sensor._sent.length, 1)
})

test('attribute 0 of a non-measurement cluster is not a MeasuredValue', () => {
  // Every cluster has an attribute 0. On Identify it is IdentifyTime and on
  // Descriptor it is DeviceTypeList - neither is a reading.
  const { sensor } = setup({ endpoint: '1' })
  publish('matter/node/4/name', 'TEMP Ikea')

  publish('matter/node/4/1/3/0', '0') // Identify.IdentifyTime
  publish('matter/node/4/1/29/0', '[{"deviceType":770,"revision":2}]') // Descriptor.DeviceTypeList
  publish('matter/node/4/1/6/0', 'true') // OnOff.OnOff
  assert.deepStrictEqual(sensor._sent, [])

  publish('matter/node/4/1/1026/0', '2137')
  assert.strictEqual(sensor._sent.length, 1)
  assert.strictEqual(sensor._sent[0].payload, 21.37)
})

test('concentration clusters are recognised through the descriptor', () => {
  const { sensor } = setup()
  publish('matter/node/4/name', 'TEMP Ikea')
  publish('matter/node/4/descriptor', JSON.stringify({
    endpoints: {
      1: {
        clusters: {
          1066: 'Pm25ConcentrationMeasurement',
          144: 'ElectricalPowerMeasurement',
        },
      },
    },
  }))

  // Already a float in µg/m³: reported, but not divided.
  publish('matter/node/4/1/1066/0', '12.5')
  assert.strictEqual(sensor._sent.length, 1)
  assert.strictEqual(sensor._sent[0].payload, 12.5)
  assert.strictEqual(sensor._sent[0].matter.scaled, false)
  assert.strictEqual(sensor._sent[0].topic, 'TEMP Ikea/1/Pm25ConcentrationMeasurement/MeasuredValue')

  // ElectricalPowerMeasurement also ends in "Measurement", but its attribute 0
  // is PowerMode, so a looser name test would wrongly let it through.
  publish('matter/node/4/1/144/0', '1')
  assert.strictEqual(sensor._sent.length, 1)
})

test('every-attribute mode names clusters from the descriptor', () => {
  const { sensor } = setup({ mode: 'all' })
  publish('matter/node/4/name', 'TEMP Ikea')
  publish('matter/node/4/descriptor', JSON.stringify({
    endpoints: { 1: { clusters: { 3: 'Identify' } } },
  }))

  publish('matter/node/4/1/3/0', '0')
  assert.strictEqual(sensor._sent[0].topic, 'TEMP Ikea/1/Identify/0')
  assert.strictEqual(sensor._sent[0].matter.clusterName, 'Identify')
})

test('other devices on the same broker are not delivered', () => {
  const { sensor } = setup()
  publish('matter/node/4/name', 'TEMP Ikea')
  publish('matter/node/9/name', 'Salotto')
  publish('matter/node/9/1/1026/0', '1900')
  assert.strictEqual(sensor._sent.length, 0)
})

test('a rename moves the subscription and stops the old stream', () => {
  const { sensor } = setup()
  publish('matter/node/4/name', 'TEMP Ikea')
  assert.ok(fakeClient.subscribed.includes('matter/node/4/+/+/+'))

  publish('matter/node/4/name', 'Corridoio') // relabelled on the device
  assert.ok(fakeClient.unsubscribed.includes('matter/node/4/+/+/+'))
  assert.match(sensor._status.text, /waiting for "TEMP Ikea"/)

  publish('matter/node/7/name', 'TEMP Ikea') // the label moved to node 7
  assert.ok(fakeClient.subscribed.includes('matter/node/7/+/+/+'))
  publish('matter/node/7/1/1026/0', '2000')
  assert.strictEqual(sensor._sent[0].payload, 20)
})

test('unknown readings can be dropped instead of forwarded as null', () => {
  const { sensor } = setup({ onNull: 'drop' })
  publish('matter/node/4/name', 'TEMP Ikea')
  publish('matter/node/4/1/1026/0', 'null')
  assert.strictEqual(sensor._sent.length, 0)
})

test('conversion can be turned off, leaving the raw value', () => {
  const { sensor } = setup({ scale: false })
  publish('matter/node/4/name', 'TEMP Ikea')
  publish('matter/node/4/1/1026/0', '2137')
  assert.strictEqual(sensor._sent[0].payload, 2137)
  assert.strictEqual(sensor._sent[0].matter.raw, 2137)
})

test('an endpoint filter excludes the other endpoints', () => {
  const { sensor } = setup({ endpoint: '2' })
  publish('matter/node/4/name', 'TEMP Ikea')
  publish('matter/node/4/1/1026/0', '2137')
  assert.strictEqual(sensor._sent.length, 0)
  publish('matter/node/4/2/1026/0', '2200')
  assert.strictEqual(sensor._sent[0].payload, 22)
})

test('the device node reports presence and the whole fabric', () => {
  const { create } = setup()
  const one = create('matter2mqtt device', { id: 'd1', broker: 'b1', device: 'TEMP Ikea' })
  const all = create('matter2mqtt device', { id: 'd2', broker: 'b1', device: '' })

  publish('matter/node/4/name', 'TEMP Ikea')
  publish('matter/node/4/availability', 'online')

  const last = one._sent[one._sent.length - 1]
  assert.strictEqual(last.payload.nodeId, 4)
  assert.strictEqual(last.payload.available, true)

  const fabric = all._sent[all._sent.length - 1]
  assert.strictEqual(fabric.payload.count, 1)
  assert.strictEqual(fabric.payload.devices[0].name, 'TEMP Ikea')
})

test('two sensors on one device share a single mqtt subscription', () => {
  const { create } = setup()
  create('matter2mqtt in', { id: 's2', broker: 'b1', device: 'TEMP Ikea', mode: 'measured' })
  publish('matter/node/4/name', 'TEMP Ikea')

  const subs = fakeClient.subscribed.filter((t) => t === 'matter/node/4/+/+/+')
  assert.strictEqual(subs.length, 1)
})
