'use strict'

const mqtt = require('mqtt')
const topics = require('../lib/topics')
const { Registry } = require('../lib/registry')

module.exports = function (RED) {
  /**
   * Config node: one MQTT connection and one device registry shared by every
   * node that points at it. Individual nodes never open their own connection,
   * so a flow with twenty sensors still holds a single session to the broker.
   */
  function BrokerNode (config) {
    RED.nodes.createNode(this, config)
    const node = this

    node.brokerUrl = config.brokerUrl
    node.prefix = topics.clean(config.prefix || 'matter')
    node.clientIdPrefix = config.clientIdPrefix || 'node-red-matter2mqtt'
    node.registry = new Registry()
    node.connected = false

    // nodeId -> Set<handler>, plus a set of handlers wanting every device.
    const attributeHandlers = new Map()
    const statusHandlers = new Set()
    const subscribedNodes = new Set()
    const responseHandlers = new Set()

    const clientId = `${node.clientIdPrefix}-${node.id}`
    const options = {
      clientId,
      clean: true,
      reconnectPeriod: 5000,
      connectTimeout: 15000,
      resubscribe: true,
    }
    if (node.credentials && node.credentials.user) {
      options.username = node.credentials.user
      options.password = node.credentials.password
    }

    const client = mqtt.connect(node.brokerUrl, options)
    node.client = client

    client.on('connect', () => {
      node.connected = true
      node.log(`connected to ${node.brokerUrl} (prefix ${node.prefix})`)
      // Retained metadata replays on subscribe, so the registry refills itself
      // on every reconnect without any explicit resynchronisation.
      client.subscribe(topics.nodeMeta(node.prefix), { qos: 0 })
      client.subscribe(topics.bridgeStatus(node.prefix), { qos: 0 })
      for (const nodeId of subscribedNodes) {
        client.subscribe(topics.nodeAttributes(node.prefix, nodeId), { qos: 0 })
      }
      if (responseHandlers.size > 0) {
        client.subscribe(topics.bridgeResponses(node.prefix), { qos: 0 })
      }
      emitStatus()
    })

    client.on('reconnect', () => node.debug('reconnecting'))
    client.on('error', (err) => node.error(`mqtt: ${err.message}`))
    client.on('close', () => {
      if (!node.connected) return
      node.connected = false
      emitStatus()
    })

    client.on('message', (topic, payload) => {
      const parsed = topics.parse(node.prefix, topic)
      if (!parsed) return
      const decoded = topics.decode(payload)

      if (parsed.kind === 'meta') {
        node.registry.applyMeta(parsed.nodeId, parsed.what, decoded)
        emitStatus()
        return
      }
      if (parsed.kind === 'bridge') {
        if (parsed.what === 'availability') node.bridgeOnline = decoded.text === 'online'
        if (parsed.what === 'matter_connected') node.matterConnected = decoded.text === 'true'
        emitStatus()
        return
      }
      if (parsed.kind === 'response') {
        for (const fn of responseHandlers) {
          try {
            fn(parsed.command, decoded)
          } catch (err) {
            node.error(`response handler: ${err.message}`)
          }
        }
        return
      }
      if (parsed.kind !== 'attribute') return

      const handlers = attributeHandlers.get(parsed.nodeId)
      if (!handlers || handlers.size === 0) return
      const device = node.registry.devices.get(parsed.nodeId)
      for (const fn of handlers) {
        try {
          fn(parsed, decoded, device)
        } catch (err) {
          node.error(`handler: ${err.message}`)
        }
      }
    })

    // A rename moves a device to a different name, not a different node id, so
    // subscriptions stay valid - but nodes tracking it by name need to know.
    node.registry.on('renamed', () => emitStatus())

    function emitStatus () {
      for (const fn of statusHandlers) {
        try {
          fn()
        } catch (err) {
          node.error(`status handler: ${err.message}`)
        }
      }
    }

    /** onStatus registers a callback for connection or registry changes. */
    node.onStatus = function (fn) {
      statusHandlers.add(fn)
      return () => statusHandlers.delete(fn)
    }

    /**
     * subscribeNode routes one node's attribute stream to a handler, and
     * returns an unsubscribe function. Subscriptions are reference-counted so
     * two sensor nodes on the same device share one MQTT subscription.
     */
    node.subscribeNode = function (nodeId, fn) {
      let handlers = attributeHandlers.get(nodeId)
      if (!handlers) {
        handlers = new Set()
        attributeHandlers.set(nodeId, handlers)
      }
      handlers.add(fn)

      if (!subscribedNodes.has(nodeId)) {
        subscribedNodes.add(nodeId)
        if (node.connected) {
          client.subscribe(topics.nodeAttributes(node.prefix, nodeId), { qos: 0 })
        }
      }

      return () => {
        handlers.delete(fn)
        if (handlers.size > 0) return
        attributeHandlers.delete(nodeId)
        subscribedNodes.delete(nodeId)
        if (node.connected) {
          client.unsubscribe(topics.nodeAttributes(node.prefix, nodeId))
        }
      }
    }

    /**
     * subscribeResponses routes `bridge/response/<command>` messages to a
     * handler. Subscribed lazily, so a flow that only reads never carries the
     * response tree.
     */
    node.subscribeResponses = function (fn) {
      const first = responseHandlers.size === 0
      responseHandlers.add(fn)
      if (first && node.connected) {
        client.subscribe(topics.bridgeResponses(node.prefix), { qos: 0 })
      }
      return () => {
        responseHandlers.delete(fn)
        if (responseHandlers.size === 0 && node.connected) {
          client.unsubscribe(topics.bridgeResponses(node.prefix))
        }
      }
    }

    /** publish sends one command topic. Commands are never retained. */
    node.publish = function (topic, payload) {
      if (!node.connected) throw new Error('broker offline')
      client.publish(topic, payload, { qos: 0, retain: false })
    }

    node.on('close', function (done) {
      attributeHandlers.clear()
      statusHandlers.clear()
      subscribedNodes.clear()
      responseHandlers.clear()
      client.end(false, {}, () => done())
    })
  }

  RED.nodes.registerType('matter2mqtt-broker', BrokerNode, {
    credentials: {
      user: { type: 'text' },
      password: { type: 'password' },
    },
  })

  // Feeds the device dropdown in the editor. The config node must be deployed
  // and connected, since the list comes from live retained topics.
  RED.httpAdmin.get(
    '/matter2mqtt/:id/devices',
    RED.auth.needsPermission('matter2mqtt-broker.read'),
    function (req, res) {
      const broker = RED.nodes.getNode(req.params.id)
      if (!broker || !broker.registry) {
        return res.json({ connected: false, devices: [] })
      }
      res.json({ connected: broker.connected, devices: broker.registry.list() })
    }
  )
}
