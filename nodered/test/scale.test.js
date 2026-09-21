'use strict'

const test = require('node:test')
const assert = require('node:assert')
const { scale, unscale, clusterId, clusterName, attributeName, isMeasuredValue, isReading } = require('../lib/scale')

test('temperature and humidity are divided by 100', () => {
  // The reading an IKEA VALLHORN actually publishes.
  assert.deepStrictEqual(scale(1026, 0, 2137), { value: 21.37, unit: '°C', scaled: true })
  assert.deepStrictEqual(scale(1029, 0, 4550), { value: 45.5, unit: '%', scaled: true })
})

test('negative temperatures survive the conversion', () => {
  assert.strictEqual(scale(1026, 0, -1250).value, -12.5)
})

test('binary floating point noise is rounded away', () => {
  // 2131 / 100 is 21.310000000000002 in IEEE 754.
  assert.strictEqual(scale(1026, 0, 2131).value, 21.31)
})

test('pressure and flow are divided by 10', () => {
  assert.strictEqual(scale(1027, 0, 1013).value, 101.3)
  assert.strictEqual(scale(1028, 0, 125).value, 12.5)
})

test('illuminance is decoded from its logarithmic encoding', () => {
  // 10000 * log10(100) + 1 = 20001
  assert.strictEqual(scale(1024, 0, 20001).value, 100)
  // 0 is "too dark to measure" and 0xFFFF is "unknown": neither is a reading.
  assert.strictEqual(scale(1024, 0, 0).value, null)
  assert.strictEqual(scale(1024, 0, 0xffff).value, null)
})

test('min and max share the encoding of MeasuredValue', () => {
  assert.strictEqual(scale(1026, 1, -4000).value, -40)
  assert.strictEqual(scale(1026, 2, 12500).value, 125)
})

test('tolerance is left alone, since it is not log-encoded on illuminance', () => {
  assert.deepStrictEqual(scale(1024, 3, 5), { value: 5, unit: 'lx', scaled: false })
})

test('unknown clusters pass through untouched', () => {
  // 1037 is CarbonDioxideConcentrationMeasurement: already a float in ppm.
  assert.deepStrictEqual(scale(1037, 0, 812.5), { value: 812.5, unit: '', scaled: false })
  assert.deepStrictEqual(scale(6, 0, true), { value: true, unit: '', scaled: false })
})

test('null stays null rather than becoming zero', () => {
  assert.deepStrictEqual(scale(1026, 0, null), { value: null, unit: '°C', scaled: false })
  assert.deepStrictEqual(scale(1026, 0, undefined), { value: null, unit: '°C', scaled: false })
})

test('non-numeric payloads are not divided', () => {
  const list = [1, 2, 3]
  assert.strictEqual(scale(1026, 0, list).value, list)
  assert.strictEqual(scale(1026, 0, 'warm').value, 'warm')
})

test('precision can be overridden per node', () => {
  assert.strictEqual(scale(1026, 0, 2137, { precision: 1 }).value, 21.4)
  assert.strictEqual(scale(1026, 0, 2137, { precision: 0 }).value, 21)
})

test('unscale is the inverse, and yields the integer a device expects', () => {
  assert.deepStrictEqual(unscale(1026, 0, 21.5), { value: 2150, scaled: true })
  assert.deepStrictEqual(unscale(1029, 0, 45.5), { value: 4550, scaled: true })
  assert.deepStrictEqual(unscale(1027, 0, 101.3), { value: 1013, scaled: true })
  // A value that cannot be represented exactly is rounded, not truncated.
  assert.strictEqual(unscale(1026, 0, 21.375).value, 2138)
  assert.strictEqual(unscale(1026, 0, -12.5).value, -1250)
  // Round-trips through the logarithmic encoding.
  assert.strictEqual(scale(1024, 0, unscale(1024, 0, 100).value).value, 100)
})

test('unscale leaves undefined clusters and non-numbers alone', () => {
  assert.deepStrictEqual(unscale(6, 0, true), { value: true, scaled: false })
  assert.deepStrictEqual(unscale(1026, 3, 50), { value: 50, scaled: false })
  assert.deepStrictEqual(unscale(1026, 0, null), { value: null, scaled: false })
  assert.deepStrictEqual(unscale(1026, 0, 'warm'), { value: 'warm', scaled: false })
})

test('clusterId accepts a name or a number, and rejects anything else', () => {
  assert.strictEqual(clusterId('TemperatureMeasurement'), 1026)
  assert.strictEqual(clusterId('temperaturemeasurement'), 1026)
  assert.strictEqual(clusterId('1026'), 1026)
  assert.strictEqual(clusterId(1026), 1026)
  assert.strictEqual(clusterId('OnOff'), null) // no conversion defined
  assert.strictEqual(clusterId(''), null)
})

test('names cover the measurement clusters', () => {
  assert.strictEqual(clusterName(1026), 'TemperatureMeasurement')
  assert.strictEqual(attributeName(1026, 0), 'MeasuredValue')
  assert.strictEqual(clusterName(6), '')
  assert.strictEqual(attributeName(6, 0), '')
  assert.ok(isMeasuredValue(1026, 0))
  assert.ok(!isMeasuredValue(1026, 1))
})

test('MeasuredValue requires a cluster that defines one', () => {
  // Attribute 0 exists on every cluster; only a measurement cluster names it
  // MeasuredValue.
  assert.ok(!isMeasuredValue(3, 0)) // Identify.IdentifyTime
  assert.ok(!isMeasuredValue(29, 0)) // Descriptor.DeviceTypeList
  assert.ok(!isMeasuredValue(6, 0)) // OnOff.OnOff
  assert.ok(!isMeasuredValue(40, 5)) // BasicInformation.NodeLabel

  // Concentration clusters are recognised by the name from the descriptor…
  assert.ok(isMeasuredValue(1066, 0, 'Pm25ConcentrationMeasurement'))
  // …and the test is tight enough to exclude the electrical clusters, whose
  // attribute 0 is PowerMode.
  assert.ok(!isMeasuredValue(144, 0, 'ElectricalPowerMeasurement'))
  assert.ok(!isMeasuredValue(1066, 1, 'Pm25ConcentrationMeasurement'))
})

test('state clusters are readings, unconverted', () => {
  // A water leak detector reports BooleanState/StateValue, not a MeasuredValue.
  assert.ok(!isMeasuredValue(69, 0))
  assert.ok(isReading(69, 0))
  assert.ok(!isReading(69, 1))
  assert.ok(isReading(1030, 0)) // OccupancySensing.Occupancy
  assert.strictEqual(clusterName(69), 'BooleanState')
  assert.strictEqual(attributeName(69, 0), 'StateValue')
  assert.deepStrictEqual(scale(69, 0, true), { value: true, unit: '', scaled: false })

  // BooleanStateConfiguration: alarms and fault are readings, the sensitivity
  // settings next to them are configuration.
  assert.ok(isReading(128, 3))
  assert.ok(isReading(128, 7))
  assert.ok(!isReading(128, 0))
  assert.strictEqual(attributeName(128, 3), 'AlarmsActive')
})
