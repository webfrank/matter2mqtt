'use strict'

const scaling = require('../lib/scale')
const { descriptorClusterName } = require('../lib/registry')

module.exports = function (RED) {
  /**
   * Subscribes to one device, found by NodeLabel, and emits its measurements
   * already converted to engineering units.
   *
   * The device is resolved through the shared registry rather than being
   * hard-wired to a node id, so a device that is renamed, re-commissioned, or
   * simply not present at deploy time is picked up as soon as its retained
   * name topic appears.
   */
  function MatterIn (config) {
    RED.nodes.createNode(this, config)
    const node = this

    const broker = RED.nodes.getNode(config.broker)
    if (!broker) {
      node.status({ fill: 'red', shape: 'ring', text: 'no broker configured' })
      return
    }

    const deviceName = (config.device || '').trim()
    const mode = config.mode || 'measured' // measured | all | custom
    const custom = parseCustom(config.custom)
    const endpointFilter = config.endpoint === '' || config.endpoint === undefined
      ? null
      : Number(config.endpoint)
    const doScale = config.scale !== false
    const precision = config.precision === '' || config.precision === undefined
      ? null
      : Number(config.precision)
    const onNull = config.onNull || 'send' // send | drop

    let unsubscribeAttrs = null
    let boundNodeId = null
    let lastValue = null

    const unbindStatus = broker.onStatus(refresh)
    refresh()

    /**
     * refresh re-resolves the name and moves the subscription if it now points
     * at a different node id. Called on every registry change, so discovery is
     * continuous rather than a one-shot lookup at startup.
     */
    function refresh () {
      const nodeId = broker.registry.lookup(deviceName)

      if (nodeId !== boundNodeId) {
        if (unsubscribeAttrs) unsubscribeAttrs()
        unsubscribeAttrs = null
        boundNodeId = nodeId
        lastValue = null
        if (nodeId) unsubscribeAttrs = broker.subscribeNode(nodeId, onAttribute)
      }
      updateStatus()
    }

    function updateStatus () {
      if (!broker.connected) {
        return node.status({ fill: 'red', shape: 'ring', text: 'broker offline' })
      }
      if (!boundNodeId) {
        return node.status({ fill: 'yellow', shape: 'ring', text: `waiting for "${deviceName}"` })
      }
      const device = broker.registry.devices.get(boundNodeId)
      if (device && device.available === false) {
        return node.status({ fill: 'red', shape: 'dot', text: `node ${boundNodeId} offline` })
      }
      if (lastValue !== null) {
        return node.status({ fill: 'green', shape: 'dot', text: lastValue })
      }
      node.status({ fill: 'green', shape: 'ring', text: `node ${boundNodeId}` })
    }

    function wanted (parsed, clusterHint) {
      if (endpointFilter !== null && parsed.endpoint !== endpointFilter) return false
      switch (mode) {
        case 'all':
          return true
        case 'custom':
          return custom.some(
            (c) => c.cluster === parsed.cluster &&
              (c.attribute === null || c.attribute === parsed.attribute)
          )
        default:
          // Not merely attribute 0: Identify, Descriptor and OnOff all have
          // one, and none of them is a reading.
          return scaling.isReading(parsed.cluster, parsed.attribute, clusterHint)
      }
    }

    function onAttribute (parsed, decoded, device) {
      const clusterHint = descriptorClusterName(device, parsed.endpoint, parsed.cluster)
      if (!wanted(parsed, clusterHint)) return

      const raw = decoded.empty ? null : decoded.json
      const result = doScale
        ? scaling.scale(parsed.cluster, parsed.attribute, raw, { precision })
        : { value: raw, unit: '', scaled: false }

      if (result.value === null && onNull === 'drop') return

      const measured = scaling.isReading(parsed.cluster, parsed.attribute, clusterHint)
      // The descriptor names clusters the conversion table does not cover, so
      // "every attribute" mode still produces readable topics.
      const clusterName = scaling.clusterName(parsed.cluster) || clusterHint
      const attributeName = scaling.attributeName(parsed.cluster, parsed.attribute) ||
        (measured ? 'MeasuredValue' : '')
      const name = (device && device.name) || deviceName

      const msg = {
        topic: [
          name,
          parsed.endpoint,
          clusterName || parsed.cluster,
          attributeName || parsed.attribute,
        ].join('/'),
        payload: result.value,
        matter: {
          nodeId: Number(parsed.nodeId),
          name,
          endpoint: parsed.endpoint,
          cluster: parsed.cluster,
          clusterName,
          attribute: parsed.attribute,
          attributeName,
          raw,
          unit: result.unit,
          scaled: result.scaled,
          available: device ? device.available : null,
        },
      }

      if (measured) {
        lastValue = result.value === null
          ? 'unknown'
          : `${result.value}${result.unit ? ' ' + result.unit : ''}`
        updateStatus()
      }
      node.send(msg)
    }

    node.on('close', function () {
      if (unsubscribeAttrs) unsubscribeAttrs()
      unbindStatus()
      node.status({})
    })
  }

  // "1026, 1029/0, 1024/0" -> [{cluster:1026,attribute:null}, ...]
  function parseCustom (spec) {
    return String(spec || '')
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean)
      .map((s) => {
        const [c, a] = s.split('/')
        return { cluster: Number(c), attribute: a === undefined ? null : Number(a) }
      })
      .filter((c) => Number.isFinite(c.cluster))
  }

  RED.nodes.registerType('matter2mqtt in', MatterIn)
}
