'use strict'

/**
 * Topic helpers for the matter2mqtt tree.
 *
 * Everything is parsed against the *numeric* tree, which the bridge documents
 * as canonical: the named/ mirror is optional (MQTT_PUBLISH_NAMES=false turns
 * it off) and is skipped for any attribute the bridge's data model cannot
 * resolve. Subscribing numerically means this package sees every reading, and
 * gets the cluster id it needs for scaling without a name lookup.
 *
 * The prefix may itself contain slashes ("matter"), so segments are only
 * ever counted after stripping it.
 */

/** clean normalises a configured prefix: no leading or trailing slash. */
function clean (prefix) {
  return String(prefix || '').replace(/^\/+|\/+$/g, '')
}

const topics = {
  clean,

  /** nodeMeta matches the retained per-node metadata: name, availability, descriptor. */
  nodeMeta: (prefix) => `${clean(prefix)}/node/+/+`,

  /** nodeAttributes matches every attribute of one node: <endpoint>/<cluster>/<attribute>. */
  nodeAttributes: (prefix, nodeId) => `${clean(prefix)}/node/${nodeId}/+/+/+`,

  bridgeStatus: (prefix) => `${clean(prefix)}/bridge/+`,

  bridgeResponses: (prefix) => `${clean(prefix)}/bridge/response/+`,

  /** setTopic writes one attribute; the payload is the bare JSON value. */
  setTopic: (prefix, nodeId, endpoint, cluster, attribute) =>
    `${clean(prefix)}/node/${nodeId}/${endpoint}/${cluster}/${attribute}/set`,

  /** commandTopic invokes a cluster command on one endpoint. */
  commandTopic: (prefix, nodeId, endpoint, cluster) =>
    `${clean(prefix)}/node/${nodeId}/${endpoint}/${cluster}/command`,

  /** requestTopic is the generic matter-server passthrough. */
  requestTopic: (prefix, command) => `${clean(prefix)}/bridge/request/${command}`,

  /** strip removes the prefix, returning null when the topic is foreign. */
  strip (prefix, topic) {
    const p = clean(prefix)
    if (!topic.startsWith(p + '/')) return null
    return topic.slice(p.length + 1)
  },

  /**
   * parse classifies one message topic. Returns null when it belongs to a tree
   * this package does not consume (the named/ mirror, node events).
   *
   *   node/<id>/name|availability|descriptor          -> {kind:'meta'}
   *   node/<id>/<endpoint>/<cluster>/<attribute>      -> {kind:'attribute'}
   *   bridge/<what>                                   -> {kind:'bridge'}
   *   bridge/response/<command>                       -> {kind:'response'}
   */
  parse (prefix, topic) {
    const rest = topics.strip(prefix, topic)
    if (rest === null) return null
    const parts = rest.split('/')

    if (parts[0] === 'bridge') {
      if (parts.length === 2) return { kind: 'bridge', what: parts[1] }
      if (parts.length === 3 && parts[1] === 'response') {
        return { kind: 'response', command: parts[2] }
      }
      return null
    }
    if (parts[0] !== 'node') return null

    const nodeId = parts[1]
    if (!/^\d+$/.test(nodeId)) return null

    if (parts.length === 3) {
      const what = parts[2]
      if (what !== 'name' && what !== 'availability' && what !== 'descriptor') return null
      return { kind: 'meta', nodeId, what }
    }
    // Attribute paths are exactly <endpoint>/<cluster>/<attribute>; the 6-part
    // node/<id>/event/... topics fall through here and are ignored.
    if (parts.length === 5 && parts.every((p, i) => i < 2 || /^\d+$/.test(p))) {
      return {
        kind: 'attribute',
        nodeId,
        endpoint: Number(parts[2]),
        cluster: Number(parts[3]),
        attribute: Number(parts[4]),
      }
    }
    return null
  },

  /**
   * decode parses a payload. Attribute values and descriptors are JSON, while
   * name and availability are published as bare strings, so a JSON parse
   * failure means "take it literally" rather than "bad message".
   */
  decode (buf) {
    const s = buf.toString('utf8')
    if (s === '') return { text: '', json: undefined, empty: true }
    try {
      return { text: s, json: JSON.parse(s), empty: false }
    } catch {
      return { text: s, json: undefined, empty: false }
    }
  },
}

module.exports = topics
