# node-red-contrib-matter2mqtt

Node-RED nodes for the [matter2mqtt](../README.md) bridge. Find Matter devices
by their **NodeLabel** and receive readings already converted to engineering
units — 21.37 °C, not 2137.

```
[ matter2mqtt in ]  device: "TEMP Ikea"  ->  msg.payload = 21.37
                                            msg.topic   = TEMP Ikea/1/TemperatureMeasurement/MeasuredValue
                                            msg.matter  = { nodeId: 4, cluster: 1026, raw: 2137, unit: "°C", … }
```

## Install

```bash
cd ~/.node-red
npm install /path/to/matter2mqtt/nodered
```

Restart Node-RED. Configure a **matter2mqtt** broker config node with your MQTT
broker and the bridge's `MQTT_TOPIC_PREFIX` (e.g. `matter`), then import
`examples/measured-values.json` to see the shape of a flow.

## Nodes

| Node | Purpose |
|---|---|
| `matter2mqtt-broker` (config) | one shared MQTT connection and the device registry |
| `matter2mqtt in` | subscribe to one device by name, emit converted readings |
| `matter2mqtt out` | write attributes, invoke cluster commands, issue bridge requests |
| `matter2mqtt device` | presence/descriptor stream, or the whole discovered fabric |

## Discovery by name

The bridge retains `<prefix>/node/<id>/name` for every labelled device, so the
registry fills itself the moment it subscribes — no polling, no query protocol,
and it refills automatically after a broker reconnect.

In the editor the **Device** field is a dropdown of what the fabric has actually
advertised — node id, product and online state included — with a refresh button
for after commissioning or relabelling. It needs a *deployed* broker config
node, since the list comes from live retained topics. Offline devices stay
selectable: a sleeping sensor is a valid target. A device configured earlier but
not currently advertised is kept and marked *not discovered*, so opening the
dialog while the broker is down never silently rewires a flow.

Names are matched **verbatim**, exactly as the bridge publishes them: `TEMP
Ikea` is `TEMP Ikea`, capitals and spaces included. A case-insensitive fallback
and a bare node id also resolve, matching the bridge's own behaviour.

Resolution is continuous, not a one-shot lookup at deploy: a device that is
offline, renamed, or commissioned later binds as soon as its name topic
appears, and the node shows `waiting for "…"` until then.

### Why the numeric tree

Nodes subscribe to `<prefix>/node/<id>/<endpoint>/<cluster>/<attribute>`, not
to the `named/` mirror. The bridge documents the numeric tree as canonical: the
mirror is optional (`MQTT_PUBLISH_NAMES=false`) and is skipped for any
attribute its data model cannot resolve. Subscribing numerically sees every
reading and yields the cluster id that drives the conversion.

## What "Readings only" means

Attribute 0 **of a cluster that defines a MeasuredValue** — not attribute 0 in
general. Every cluster has one: on Identify it is `IdentifyTime`, on Descriptor
it is `DeviceTypeList`, on OnOff it is `OnOff`. Only the clusters in the table
below, plus the concentration clusters, name it `MeasuredValue`.

Boolean sensors have no MeasuredValue at all: a water leak detector or a
contact sensor reports `69` BooleanState/`StateValue`, an occupancy sensor
`1030` OccupancySensing/`Occupancy`. Those are reported too, unconverted, along
with `128` BooleanStateConfiguration's `AlarmsActive`, `AlarmsSuppressed` and
`SensorFault`. Cluster 128's sensitivity attributes are configuration rather
than readings — ask for them with *Selected clusters* (`128/0`).

Concentration clusters (CO₂, PM2.5, TVOC, …) are recognised from the device
descriptor rather than a hardcoded id list, since their ids move between Matter
revisions. They report a float in their own unit and are passed through
unconverted. The match is on `…ConcentrationMeasurement` specifically:
`ElectricalPowerMeasurement` also ends in *Measurement*, but its attribute 0 is
`PowerMode`.

## Unit conversion

**The scaling is fixed by the Matter specification per cluster.** It is not
vendor-specific and cannot be derived from vendor/product info — an IKEA sensor
reports temperature ×100 because it implements cluster 1026, and every
conforming device does the same.

| Cluster | Spec encoding | Conversion | Unit |
|---|---|---|---|
| 1024 IlluminanceMeasurement | `MeasuredValue = 10000 × log₁₀(lux) + 1` | `10^((v−1)/10000)` | lx |
| 1026 TemperatureMeasurement | `MeasuredValue = 100 × °C` | ÷ 100 | °C |
| 1027 PressureMeasurement | `MeasuredValue = 10 × kPa` | ÷ 10 | kPa |
| 1028 FlowMeasurement | `MeasuredValue = 10 × m³/h` | ÷ 10 | m³/h |
| 1029 RelativeHumidityMeasurement | `MeasuredValue = 100 × %` | ÷ 100 | % |

`MinMeasuredValue` and `MaxMeasuredValue` share the encoding and are converted
too. `Tolerance` is not, because it is not log-encoded on the illuminance
cluster.

Any other cluster passes through **untouched**, with `msg.matter.scaled =
false`. That is deliberate: the concentration clusters (CO₂, PM2.5, …) already
report a float in their own unit, and silently dividing an unknown cluster
would be worse than not converting it.

Edge cases:

- `null` — Matter's "unknown" — stays `null`, never 0. Choose *send* or *drop*
  per node.
- Illuminance `0` (too dark to measure) and `0xFFFF` (unknown) both decode to
  `null`, since neither is a reading.
- Division is rounded to the precision the encoding carries, so 2131 gives
  21.31 rather than 21.310000000000002. Override with **Decimals**.
- `msg.matter.raw` always carries the value before conversion.

## Sending

`matter2mqtt out` has three actions. Devices are addressed by name through the
same registry, and cluster/attribute may be given by name **or** number — the
bridge resolves either, so nothing is looked up client-side.

```
Write attribute   -> <prefix>/node/<id>/<ep>/<cluster>/<attr>/set   payload: bare JSON value
Cluster command   -> <prefix>/node/<id>/<ep>/<cluster>/command      payload: {"command":…,"payload":…}
Bridge request    -> <prefix>/bridge/request/<command>              payload: arguments object
```

`msg.device`, `msg.endpoint`, `msg.cluster`, `msg.attribute` and `msg.command`
override the configured fields, so one node can serve a whole flow.

Writes get the **inverse** conversion by default — send 21.5, the device
receives 2150 — using the same spec table, so only the clusters listed above are
affected. In practice those attributes are read-only, so this is mostly
symmetry; turn it off to write exactly what you pass.

**Writes and cluster commands are fire-and-forget.** The bridge does not
acknowledge them, so the node emits nothing; confirm by watching the resulting
attribute report on a `matter2mqtt in` node.

**Bridge requests do produce output.** A `_request_id` is attached and the
matching reply on `<prefix>/bridge/response/<command>` is emitted as
`msg.payload`, with the full envelope in `msg.matter.response`. Replies to other
callers on that shared topic are ignored. A `success: false` reply, or silence
past the timeout, raises a node error a **catch** node can handle — keep the
timeout generous, since commissioning is slow.

```
[inject] -> [matter2mqtt out: request "commission_with_code"] -> [debug]
            msg.payload = {"code":"MT:Y.K90-Q000KA0648G00"}
```

## Tests

```bash
npm test        # node --test, no broker or bridge required
```

Covers the conversion table in both directions, topic parsing (including that
`node/<id>/event/…` is not mistaken for an attribute), name resolution, and the
nodes themselves driven against a fake MQTT client — binding on name arrival,
rename handling, and request/response correlation.
