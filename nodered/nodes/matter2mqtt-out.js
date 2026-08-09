'use strict'

const topics = require('../lib/topics')
const scaling = require('../lib/scale')

module.exports = function (RED) {
  /**
   * Sends to the bridge: attribute writes, cluster commands, and the generic
   * matter-server passthrough.
   *
   * Devices are addressed by NodeLabel through the same registry the input
   * nodes use, so a flow never hard-codes a node id. Cluster and attribute may
   * be given by name or by number - the bridge resolves either on the numeric
   * tree, so nothing has to be looked up here.
   */
  function MatterOut (config) {
    RED.nodes.createNode(this, config)
    const node = this

    const broker = RED.nodes.getNode(config.broker)
    if (!broker) {
      node.status({ fill: 'red', shape: 'ring', text: 'no broker configured' })
      return
    }

    const mode = config.mode || 'set' // set | command | request
    const doScale = config.scale !== false
    const timeout = Number(config.timeout || 60) * 1000

    // request_id -> {send, done, msg, timer}. The bridge echoes _request_id on
    // a topic shared by every caller, which is exactly what it is for.
    const pending = new Map()
    let seq = 0
    let unsubscribeResponses = null

    if (mode === 'request') {
      unsubscribeResponses = broker.subscribeResponses(onResponse)
    }

    const unbindStatus = broker.onStatus(updateStatus)
    updateStatus()

    function updateStatus () {
      if (!broker.connected) {
        return node.status({ fill: 'red', shape: 'ring', text: 'broker offline' })
      }
      node.status({})
    }

    function onResponse (command, decoded) {
      const body = decoded.json
      if (!body || typeof body !== 'object') return
      const entry = pending.get(body._request_id)
      if (!entry) return // another node's request, or another Node-RED instance

      pending.delete(body._request_id)
      clearTimeout(entry.timer)

      const msg = entry.msg
      msg.topic = command
      msg.payload = body.result === undefined ? body : body.result
      msg.matter = { command, success: body.success !== false, response: body }

      if (body.success === false) {
        node.status({ fill: 'red', shape: 'dot', text: `${command} failed` })
        entry.done(new Error(`${command}: ${body.error || 'request failed'}`))
        return
      }
      node.status({ fill: 'green', shape: 'dot', text: command })
      entry.send(msg)
      entry.done()
    }

    node.on('input', function (msg, send, done) {
      try {
        if (mode === 'request') return sendRequest(msg, send, done)
        sendToDevice(msg)
        done()
      } catch (err) {
        node.status({ fill: 'red', shape: 'dot', text: err.message })
        done(err)
      }
    })

    // ---- attribute writes and cluster commands ----

    function sendToDevice (msg) {
      const deviceName = pick(msg.device, config.device)
      const nodeId = broker.registry.lookup(deviceName)
      if (!nodeId) throw new Error(`unknown device "${deviceName}"`)

      const endpoint = pick(msg.endpoint, config.endpoint)
      if (endpoint === '' || endpoint === undefined) throw new Error('no endpoint given')
      const cluster = pick(msg.cluster, config.cluster)
      if (cluster === '' || cluster === undefined) throw new Error('no cluster given')

      if (mode === 'set') {
        const attribute = pick(msg.attribute, config.attribute)
        if (attribute === '' || attribute === undefined) throw new Error('no attribute given')

        const value = encodeValue(cluster, attribute, msg.payload)
        broker.publish(
          topics.setTopic(broker.prefix, nodeId, endpoint, cluster, attribute),
          JSON.stringify(value)
        )
        node.status({ fill: 'green', shape: 'dot', text: `set ${cluster}/${attribute}` })
        return
      }

      const body = commandBody(msg)
      broker.publish(
        topics.commandTopic(broker.prefix, nodeId, endpoint, cluster),
        JSON.stringify(body)
      )
      node.status({ fill: 'green', shape: 'dot', text: body.command })
    }

    /**
     * encodeValue applies the inverse of the read conversion, so a flow writes
     * 21.5 rather than 2150. Only the clusters with a defined conversion are
     * touched; everything else is written exactly as given.
     */
    function encodeValue (cluster, attribute, value) {
      if (!doScale) return value
      const cid = scaling.clusterId(cluster)
      const aid = /^\d+$/.test(String(attribute))
        ? Number(attribute)
        : (String(attribute).toLowerCase() === 'measuredvalue' ? 0 : null)
      if (cid === null || aid === null) return value
      return scaling.unscale(cid, aid, value).value
    }

    function commandBody (msg) {
      const explicit = pick(msg.command, config.command)

      // A payload that already carries a command is passed through, so a flow
      // can build the whole envelope itself.
      if (msg.payload && typeof msg.payload === 'object' && msg.payload.command) {
        return msg.payload
      }
      if (!explicit) throw new Error('no command given')
      if (msg.payload === undefined || msg.payload === null || msg.payload === '') {
        return { command: explicit }
      }
      if (typeof msg.payload === 'object') {
        return { command: explicit, payload: msg.payload }
      }
      return { command: explicit, payload: msg.payload }
    }

    // ---- bridge passthrough ----

    function sendRequest (msg, send, done) {
      const command = pick(msg.command, config.command, msg.topic)
      if (!command) throw new Error('no bridge command given')

      const args = msg.payload && typeof msg.payload === 'object' ? { ...msg.payload } : {}
      const requestId = `${node.id}-${++seq}`
      args._request_id = requestId

      const timer = setTimeout(() => {
        pending.delete(requestId)
        node.status({ fill: 'red', shape: 'dot', text: `${command} timed out` })
        done(new Error(`${command}: no response within ${timeout / 1000}s`))
      }, timeout)
      if (timer.unref) timer.unref()

      pending.set(requestId, { send, done, msg, timer })
      node.status({ fill: 'blue', shape: 'ring', text: command })

      try {
        broker.publish(topics.requestTopic(broker.prefix, command), JSON.stringify(args))
      } catch (err) {
        clearTimeout(timer)
        pending.delete(requestId)
        throw err
      }
    }

    node.on('close', function () {
      for (const entry of pending.values()) clearTimeout(entry.timer)
      pending.clear()
      if (unsubscribeResponses) unsubscribeResponses()
      unbindStatus()
      node.status({})
    })
  }

  // pick returns the first value a message or the config actually supplies,
  // so a per-message override wins over the configured default.
  function pick (...values) {
    for (const v of values) {
      if (v !== undefined && v !== null && v !== '') return v
    }
    return undefined
  }

  RED.nodes.registerType('matter2mqtt out', MatterOut)
}
