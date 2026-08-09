'use strict'

/**
 * Matter measurement scaling.
 *
 * The conversion is fixed by the Matter specification per *cluster*, not per
 * vendor and not per device: an IKEA sensor reports temperature x100 because it
 * implements the Temperature Measurement cluster, and every other conforming
 * device does exactly the same. Nothing here can or should be derived from
 * vendor/product info - the cluster id is the whole story.
 *
 *   1024 Illuminance   MeasuredValue = 10000 x log10(lux) + 1
 *   1026 Temperature   MeasuredValue = 100 x degrees Celsius
 *   1027 Pressure      MeasuredValue = 10 x kPa
 *   1028 Flow          MeasuredValue = 10 x m3/h
 *   1029 Humidity      MeasuredValue = 100 x percent
 *
 * Clusters absent from this table are passed through untouched, which is the
 * right default: the concentration clusters (CO2, PM2.5, ...) already report a
 * float in their own unit, and an unknown cluster must not be silently mangled.
 */

// MeasuredValue, MinMeasuredValue and MaxMeasuredValue share one encoding.
// Tolerance (3) is deliberately excluded: it is not log-encoded on the
// illuminance cluster, so treating it like the others would be wrong.
const SCALED_ATTRIBUTES = new Set([0, 1, 2])

const ATTRIBUTE_NAMES = {
  0: 'MeasuredValue',
  1: 'MinMeasuredValue',
  2: 'MaxMeasuredValue',
  3: 'Tolerance',
}

const CLUSTERS = {
  1024: {
    name: 'IlluminanceMeasurement',
    unit: 'lx',
    precision: 2,
    // 0 means "too dark to measure" and 0xFFFF means "unknown", both of which
    // are absence of a reading rather than a number.
    decode: (v) => (v === 0 || v === 0xffff ? null : Math.pow(10, (v - 1) / 10000)),
    encode: (lux) => (lux <= 0 ? 0 : Math.round(10000 * Math.log10(lux) + 1)),
  },
  1026: { name: 'TemperatureMeasurement', unit: '°C', factor: 100, precision: 2 },
  1027: { name: 'PressureMeasurement', unit: 'kPa', factor: 10, precision: 1 },
  1028: { name: 'FlowMeasurement', unit: 'm³/h', factor: 10, precision: 1 },
  1029: { name: 'RelativeHumidityMeasurement', unit: '%', factor: 100, precision: 2 },
}

/** clusterName returns the well-known name of a measurement cluster, or ''. */
function clusterName (cluster) {
  return (CLUSTERS[cluster] || {}).name || ''
}

/** attributeName names the attributes common to every measurement cluster. */
function attributeName (cluster, attribute) {
  return CLUSTERS[cluster] ? ATTRIBUTE_NAMES[attribute] || '' : ''
}

/**
 * definesMeasuredValue reports whether a cluster has a MeasuredValue attribute.
 *
 * Every cluster has an attribute 0, so the attribute id alone means nothing:
 * on Identify it is IdentifyTime, on Descriptor it is DeviceTypeList, on OnOff
 * it is OnOff. Only a cluster from this set names attribute 0 MeasuredValue.
 *
 * The concentration clusters (CO2, PM2.5, TVOC, ...) also expose MeasuredValue
 * - as a float in their own unit, needing no conversion - but their ids vary
 * across Matter revisions, so they are recognised by the name the bridge
 * resolves. The test is deliberately `ConcentrationMeasurement` and not a
 * looser `Measurement` suffix: ElectricalPowerMeasurement would pass that one,
 * and its attribute 0 is PowerMode.
 */
function definesMeasuredValue (cluster, clusterNameHint) {
  if (CLUSTERS[cluster]) return true
  return /ConcentrationMeasurement$/.test(String(clusterNameHint || ''))
}

/**
 * isMeasuredValue reports whether an attribute really is a MeasuredValue.
 * clusterNameHint is the cluster name from the device descriptor, when known.
 */
function isMeasuredValue (cluster, attribute, clusterNameHint) {
  return attribute === 0 && definesMeasuredValue(cluster, clusterNameHint)
}

/**
 * scale converts a raw Matter attribute value into engineering units.
 *
 * It returns {value, unit, scaled}: `scaled` is false when the cluster has no
 * defined conversion, in which case `value` is the input unchanged. A null
 * input - Matter's "unknown" - stays null rather than becoming 0, and a raw
 * value that is not a number (a struct, a list) is passed through as-is.
 */
function scale (cluster, attribute, raw, opts = {}) {
  const def = CLUSTERS[cluster]
  const unit = def ? def.unit : ''

  if (raw === null || raw === undefined) return { value: null, unit, scaled: false }
  if (typeof raw !== 'number' || !Number.isFinite(raw)) {
    return { value: raw, unit, scaled: false }
  }
  if (!def || !SCALED_ATTRIBUTES.has(attribute)) {
    return { value: raw, unit, scaled: false }
  }

  const value = def.decode ? def.decode(raw) : raw / def.factor
  if (value === null) return { value: null, unit, scaled: true }

  const precision = opts.precision === undefined || opts.precision === null
    ? def.precision
    : opts.precision
  return { value: round(value, precision), unit, scaled: true }
}

/**
 * unscale is the inverse of scale, for writing a value back to a device.
 *
 * Matter carries these attributes as integers, so the result is rounded: a
 * device expects 2137, not 21.37. Clusters and attributes outside the table are
 * returned untouched, which covers almost every write in practice - the
 * measurement clusters are read-only, so this matters mainly for symmetry and
 * for test rigs that write MeasuredValue directly.
 */
function unscale (cluster, attribute, value) {
  const def = CLUSTERS[cluster]
  if (value === null || value === undefined) return { value: null, scaled: false }
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    return { value, scaled: false }
  }
  if (!def || !SCALED_ATTRIBUTES.has(attribute)) return { value, scaled: false }

  if (def.encode) return { value: def.encode(value), scaled: true }
  return { value: Math.round(value * def.factor), scaled: true }
}

/** clusterId resolves a measurement cluster name, or a number, to its id. */
function clusterId (nameOrNumber) {
  const s = String(nameOrNumber == null ? '' : nameOrNumber).trim()
  if (s === '') return null
  if (/^\d+$/.test(s)) return Number(s)
  const folded = s.toLowerCase()
  for (const [id, def] of Object.entries(CLUSTERS)) {
    if (def.name.toLowerCase() === folded) return Number(id)
  }
  return null
}

// Dividing by 100 in binary floating point turns 2137 into 21.370000000000001,
// which is ugly in a dashboard and worse in an equality test. Rounding to the
// precision the encoding actually carries loses nothing.
function round (v, digits) {
  if (digits === undefined || digits < 0) return v
  const f = Math.pow(10, digits)
  return Math.round((v + Number.EPSILON) * f) / f
}

module.exports = {
  CLUSTERS,
  SCALED_ATTRIBUTES,
  scale,
  unscale,
  clusterId,
  clusterName,
  attributeName,
  definesMeasuredValue,
  isMeasuredValue,
}
