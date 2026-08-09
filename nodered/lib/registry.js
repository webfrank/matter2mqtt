'use strict'

const { EventEmitter } = require('node:events')

/**
 * Registry is the device-discovery-by-name index.
 *
 * The bridge retains `<prefix>/node/<id>/name`, so a fresh subscriber is handed
 * the whole fabric immediately - no polling and no query protocol. Renaming a
 * device updates that topic and clearing its NodeLabel empties it, which is why
 * a name is never cached beyond the retained message that produced it.
 *
 * Names are used verbatim, exactly as the bridge publishes them: "TEMP Ikea"
 * stays "TEMP Ikea", spaces, capitals and accents included. Lookup prefers an
 * exact match and falls back to a case-insensitive one, mirroring the bridge's
 * own name resolution.
 */
class Registry extends EventEmitter {
  constructor () {
    super()
    /** @type {Map<string, {nodeId:string,name:string,available:boolean|null,descriptor:object|null}>} */
    this.devices = new Map()
  }

  device (nodeId) {
    let d = this.devices.get(nodeId)
    if (!d) {
      d = { nodeId, name: '', available: null, descriptor: null }
      this.devices.set(nodeId, d)
    }
    return d
  }

  /**
   * applyMeta folds one retained metadata message into the index and reports
   * whether the name mapping changed, so callers can re-resolve subscriptions.
   */
  applyMeta (nodeId, what, decoded) {
    const d = this.device(nodeId)
    let renamed = false

    switch (what) {
      case 'name': {
        // An empty retained payload is the bridge deleting the topic: the
        // device lost its NodeLabel and is no longer addressable by name.
        const name = decoded.empty ? '' : decoded.text
        renamed = name !== d.name
        d.name = name
        break
      }
      case 'availability':
        d.available = decoded.text === 'online'
        break
      case 'descriptor':
        if (decoded.json && typeof decoded.json === 'object') d.descriptor = decoded.json
        break
      default:
        return false
    }

    this.emit('change', d, { renamed })
    if (renamed) this.emit('renamed', d)
    return renamed
  }

  remove (nodeId) {
    const d = this.devices.get(nodeId)
    if (!d) return
    this.devices.delete(nodeId)
    this.emit('removed', d)
  }

  /** lookup resolves a device name, or a bare node id, to a node id. */
  lookup (name) {
    const wanted = String(name == null ? '' : name).trim()
    if (wanted === '') return null
    if (/^\d+$/.test(wanted) && this.devices.has(wanted)) return wanted

    for (const d of this.devices.values()) {
      if (d.name === wanted) return d.nodeId
    }
    const folded = wanted.toLowerCase()
    for (const d of this.devices.values()) {
      if (d.name && d.name.toLowerCase() === folded) return d.nodeId
    }
    return null
  }

  /** list returns every *named* device, ordered by node id, for the editor. */
  list () {
    return [...this.devices.values()]
      .filter((d) => d.name !== '')
      .sort((a, b) => Number(a.nodeId) - Number(b.nodeId))
      .map((d) => ({
        nodeId: d.nodeId,
        name: d.name,
        available: d.available,
        vendor: (d.descriptor || {}).vendor_name || '',
        product: (d.descriptor || {}).product_name || '',
        clusters: measurementClusters(d.descriptor),
      }))
  }

  clear () {
    this.devices.clear()
    this.emit('cleared')
  }
}

// measurementClusters summarises which endpoints carry which clusters, so the
// editor can show what a device actually reports.
function measurementClusters (descriptor) {
  const out = []
  const endpoints = (descriptor || {}).endpoints || {}
  for (const [ep, info] of Object.entries(endpoints)) {
    for (const [id, name] of Object.entries((info || {}).clusters || {})) {
      out.push({ endpoint: Number(ep), cluster: Number(id), name })
    }
  }
  return out
}

/**
 * descriptorClusterName looks up a cluster's name in a device's descriptor.
 *
 * The bridge resolves these names through its own data model, so a device that
 * has been interviewed tells us what each numeric cluster on each endpoint
 * actually is. Returns '' when the descriptor has not arrived yet or the
 * bridge's model does not know the cluster.
 */
function descriptorClusterName (device, endpoint, cluster) {
  const endpoints = ((device || {}).descriptor || {}).endpoints || {}
  const info = endpoints[String(endpoint)]
  if (!info || !info.clusters) return ''
  return info.clusters[String(cluster)] || ''
}

module.exports = { Registry, descriptorClusterName }
