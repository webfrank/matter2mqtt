'use strict'

module.exports = function (RED) {
  /**
   * Discovery node: reports the fabric as the bridge advertises it.
   *
   * With a device name set it tracks that one device and emits on every
   * presence or descriptor change. Left blank it emits the whole discovered
   * fabric, which is how a flow finds out what exists without hard-coding
   * anything - useful to drive a dashboard or to seed configuration.
   */
  function MatterDevice (config) {
    RED.nodes.createNode(this, config)
    const node = this

    const broker = RED.nodes.getNode(config.broker)
    if (!broker) {
      node.status({ fill: 'red', shape: 'ring', text: 'no broker configured' })
      return
    }

    const deviceName = (config.device || '').trim()
    const emitOnChange = config.emitOnChange !== false
    let lastSignature = null

    const unbind = broker.onStatus(onChange)
    onChange()

    function snapshot () {
      if (!deviceName) {
        return { devices: broker.registry.list(), count: broker.registry.devices.size }
      }
      const nodeId = broker.registry.lookup(deviceName)
      if (!nodeId) return null
      const d = broker.registry.devices.get(nodeId)
      return {
        nodeId: Number(d.nodeId),
        name: d.name,
        available: d.available,
        descriptor: d.descriptor,
      }
    }

    function onChange () {
      const data = snapshot()
      updateStatus(data)
      if (!emitOnChange) return

      // Registry updates arrive per retained topic, so a single device dump
      // produces several callbacks. Only forward genuine changes.
      const signature = JSON.stringify(data)
      if (signature === lastSignature) return
      lastSignature = signature
      if (data === null) return

      node.send({
        topic: deviceName || 'fabric',
        payload: data,
      })
    }

    function updateStatus (data) {
      if (!broker.connected) {
        return node.status({ fill: 'red', shape: 'ring', text: 'broker offline' })
      }
      if (!deviceName) {
        return node.status({ fill: 'green', shape: 'dot', text: `${broker.registry.list().length} devices` })
      }
      if (!data) {
        return node.status({ fill: 'yellow', shape: 'ring', text: `waiting for "${deviceName}"` })
      }
      node.status({
        fill: data.available === false ? 'red' : 'green',
        shape: 'dot',
        text: `node ${data.nodeId} ${data.available === false ? 'offline' : 'online'}`,
      })
    }

    // An input message re-emits the current snapshot on demand.
    node.on('input', function (msg, send, done) {
      const data = snapshot()
      send({ ...msg, topic: deviceName || 'fabric', payload: data })
      done()
    })

    node.on('close', function () {
      unbind()
      node.status({})
    })
  }

  RED.nodes.registerType('matter2mqtt device', MatterDevice)
}
