# matter2mqtt

A thin, stateless bridge that exposes a Matter fabric over MQTT. It speaks the
python-matter-server WebSocket API on one side and publishes a Zigbee2MQTT-style
topic tree on the other.

Thread mesh is routed by SLZB-06U Thread + OTBR firmware.
docker context 
The Matter protocol work is delegated entirely to
[python-matter-server](https://github.com/home-assistant-libs/python-matter-server)
(the CHIP SDK under the hood). This service holds no Matter state of its own —
it is a protocol adapter, so a crash here costs you a container restart, not a
fabric.

## Why a separate process

Running a Matter controller in-process with your automation runtime means every
Matter-layer exception takes the runtime with it. Splitting on MQTT gives you a
stable, versioned contract: flows consume topics, and the controller
implementation underneath can be replaced without touching them.

## Topic schema

Prefix is configurable via `MQTT_TOPIC_PREFIX` (default `matter`). All examples
below assume `matter`.

### State (published, retained)

| Topic | Payload |
|---|---|
| `matter/bridge/availability` | `online` / `offline` (LWT) |
| `matter/bridge/matter_connected` | `true` / `false` |
| `matter/bridge/info` | matter-server info: sdk version, schema version, fabric id |
| `matter/node/<id>/availability` | `online` / `offline` |
| `matter/node/<id>/descriptor` | vendor/product/serial from Basic Information, endpoint list |
| `matter/node/<id>/<endpoint>/<cluster>/<attribute>` | raw JSON value |
| `matter/named/<label>/<endpoint>/<ClusterName>/<AttributeName>` | same value, keyed by NodeLabel |
| `matter/node/<id>/name` | current name; absent when the device has no NodeLabel |
| `matter/node/<id>/event/<endpoint>/<cluster>/<event>` | event payload (not retained) |

The **numeric tree is canonical**; the `named/` tree is a mirror, published only
when both the cluster and attribute resolve in the data model. A partial model
therefore yields fewer aliases, never a half-numeric path. Set
`MQTT_PUBLISH_NAMES=false` to suppress the mirror entirely and halve the
retained topic count.

```
matter/node/4/1/1026/0                                          ->  2137
matter/named/Kitchen Sensor/1/TemperatureMeasurement/MeasuredValue  ->  2137
```

## Device names

The `named/` tree is keyed by the device's **NodeLabel** (Basic Information
attribute `0/40/5`) and nothing else. There is no local alias store and no
fallback to ProductName or node id.

**A device with no NodeLabel is not published under `named/` at all.** It stays
fully present on the canonical numeric tree, so nothing is lost — only the
human-readable alias is withheld until the device has a label.

This keeps naming honest: the name lives on the device, survives
re-commissioning, and is the same label Apple Home, Google Home and every other
ecosystem shows.

### Naming a device

Write NodeLabel to the device:

```bash
mosquitto_pub -t 'matter/node/9/0/40/5/set' -m '"Ingresso"'
```

Or use the **Device label** field in the web console. Either way the change
takes effect immediately: the device reports the new label back, the bridge
recomputes, clears the retained topics under the old name, and republishes under
the new one. Clearing the label removes the device from `named/` again.

The label is used **verbatim** as the topic segment — case, spaces, punctuation
and accents all preserved. `TEMP Ikea` stays `TEMP Ikea`, not `temp-ikea`.

Only what MQTT cannot carry in a topic name is rewritten: `/`, `+` and `#`
become `-`, and control characters are dropped. So `Temp/Humidity #1` becomes
`Temp-Humidity -1`. A label left with nothing after that counts as no label.

`matter/node/<id>/name` carries the current name for id-to-name mapping,
and is cleared when a device has no label.

### Duplicate labels

Two devices sharing a label would otherwise interleave their state onto one
retained topic, so **every** member of a colliding set is suffixed with its node
id — `Sensor-4` and `Sensor-9`, with neither keeping the bare `Sensor`. The
result is deterministic regardless of discovery order, and the bridge logs a
warning naming the devices involved. Give them distinct labels to clear it.

### Addressing by name

Command topics accept a name or a node id anywhere in the `named/` tree:

```
matter/named/Salotto/1/OnOff/command       <-  {"command":"Toggle"}
matter/named/4/1/OnOff/command             <-  {"command":"Toggle"}
```

Numeric IDs are stable across spec revisions; names are not. Automate against
the numeric tree, and use `named/` for dashboards, debugging, and
`mosquitto_sub` by hand.

The `descriptor` topic carries the decoded endpoint map, read from the
Descriptor cluster on each endpoint:

```json
{
  "node_id": 4,
  "name": "Kitchen Sensor",
  "named": true,
  "vendor_name": "IKEA of Sweden",
  "product_name": "Vallhorn",
  "endpoints": {
    "1": {
      "device_types": [{"id": 770, "name": "TemperatureSensor"}],
      "clusters": {"29": "Descriptor", "1026": "TemperatureMeasurement"}
    }
  }
}
```

## Node-RED

`nodered/` holds a companion npm package, `node-red-contrib-matter2mqtt`, that
consumes this bridge over MQTT: devices are found by NodeLabel, and readings
arrive already converted — 21.37 °C rather than 2137. Attribute writes, cluster
commands and the `bridge/request` passthrough (with response correlation) are
covered too. See [nodered/README.md](nodered/README.md).

The conversion is fixed by the Matter specification **per cluster**, not per
vendor: 1026 Temperature and 1029 RelativeHumidity are ×100, 1027 Pressure and
1028 Flow are ×10, and 1024 Illuminance is `10000 × log10(lux) + 1`. Nothing
about it can be derived from vendor or product info, and clusters outside that
list are passed through untouched.

## Web console

An embedded single-page console ships in the binary — no build step, no CDN, no
network access needed at runtime. Default `http://127.0.0.1:8099`.

It covers the full matter-server surface:

- **Add a device** — pairing code or QR payload, or PIN + discriminator for
  on-network commissioning when another commissioner has already joined the
  device to your Thread mesh.
- **Fabric list** — every node with live availability.
- **Device detail** — attributes grouped by endpoint and cluster with decoded
  names, live-updating as reports arrive, inline attribute writes, and a cluster
  command sender.
- **Per-device actions** — share (open a commissioning window for another
  admin), re-interview, ping, remove.
- **Thread credentials** — store the operational dataset.
- **Run a command** — generic passthrough to any matter-server command, so
  anything not surfaced by a button is still one text field away.

### Security

The console can commission and remove devices, so it binds to **loopback only**
by default. Startup **fails** if `HTTP_ADDR` is non-loopback and no credentials
are set — bind it to `127.0.0.1` and reach it over SSH, or set
`HTTP_USERNAME` / `HTTP_PASSWORD` for HTTP basic auth. There is no TLS; put it
behind a reverse proxy if it needs to leave the host.

Set `HTTP_ENABLED=false` to run headless.

### API

The UI is a client of a small JSON API, usable directly:

| Route | Purpose |
|---|---|
| `GET /api/status` | connection state, matter-server info, node count |
| `GET /api/nodes` | fabric snapshot with decoded endpoints |
| `GET /api/model` | the data model, as served to the UI |
| `GET /api/events` | server-sent events: attribute updates, node changes, status |
| `POST /api/command` | `{"command":"...","args":{...}}` — generic passthrough |
| `POST /api/write` | `{"node_id","endpoint","cluster","attribute","value"}` |
| `POST /api/invoke` | `{"node_id","endpoint","cluster","command","payload"}` |

`cluster` and `attribute` accept names or numbers. Commands that fail return
`502` with matter-server's own error text preserved, not a generic message.

## Data model

Cluster, attribute and device-type names come from `model.json`, embedded into
the binary at build time. The shipped file is a **partial seed** covering common
clusters. Generate the complete, SDK-exact map from your own controller:

```bash
docker exec -i matter-server python3 - < tools/dump-model.py > model.json
```

The `-i` matters: without it docker does not forward stdin, python reads
nothing, and you get an empty file. If you would rather not depend on that,
copy the script in instead:

```bash
docker cp tools/dump-model.py matter-server:/tmp/dump-model.py
docker exec matter-server python3 /tmp/dump-model.py > model.json
```

Either way it prints a summary to stderr — cluster, attribute and device-type
counts — so a truncated result is obvious immediately, and it exits non-zero
with an explanation rather than emitting empty JSON. Add `--debug` to see which
discovery strategy was used.

Run it inside the matter-server container so names match precisely the CHIP SDK
build your controller uses, and regenerate after any upgrade that bumps the SDK
version. Either rebuild to re-embed it, or mount it and set
`MATTER_MODEL_PATH=/model.json` to override the embedded copy at runtime.

Global attributes (`FeatureMap`, `ClusterRevision`, `AttributeList`, ...) are
built in and resolve for every cluster, including ones absent from the model.

### Commands (subscribed)

**Write an attribute** — payload is the bare JSON value:

```
matter/node/4/1/8/17/set                        <-  128
matter/named/4/1/LevelControl/OnLevel/set       <-  128
```

**Invoke a cluster command:**

```
matter/node/4/1/6/command                       <-  {"command":"Toggle"}
matter/named/4/1/OnOff/command                  <-  {"command":"Toggle"}
matter/node/4/1/8/command                       <-  {"command":"MoveToLevel","payload":{"level":128,"transitionTime":10}}
```

Both trees accept both forms — cluster and attribute segments are resolved by
name *or* number on either, and names are case-insensitive. Unknown numeric IDs
pass straight through, so an incomplete model never blocks a write. A bare
string payload (`Toggle`) is also accepted.

**Bridge operations** — generic passthrough to any matter-server command:

```
matter/bridge/request/<command>   <-  <args as JSON object>
matter/bridge/response/<command>  ->  {"command":..., "success":true, "result":...}
```

Because it is a passthrough, new matter-server API surface works without a code
change here. Useful ones:

```bash
# Push your Thread dataset into the controller (hex TLV from your OTBR)
mosquitto_pub -t matter/bridge/request/set_thread_dataset \
  -m '{"dataset":"0e08000000000001000..."}'

# Commission a device — matter-server does BLE + Thread join itself
mosquitto_pub -t matter/bridge/request/commission_with_code \
  -m '{"code":"MT:Y.K90-Q000KA0648G00"}'

# Reopen a commissioning window for another admin
mosquitto_pub -t matter/bridge/request/open_commissioning_window \
  -m '{"node_id":4}'

mosquitto_pub -t matter/bridge/request/remove_node -m '{"node_id":4}'
mosquitto_pub -t matter/bridge/request/get_nodes   -m '{}'
```

Add `"_request_id":"abc"` to any request and it is echoed in the response, for
correlating concurrent calls on a shared response topic.

Removing a node clears all of its retained topics.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `MATTER_WS_URL` | `ws://127.0.0.1:5580/ws` | |
| `MATTER_CALL_TIMEOUT` | `60s` | commissioning is slow; keep this generous |
| `MQTT_BROKER` | `tcp://127.0.0.1:1883` | `ssl://` for TLS |
| `MQTT_USERNAME` / `MQTT_PASSWORD` | empty | |
| `MQTT_CLIENT_ID` | `matter2mqtt` | |
| `MQTT_TOPIC_PREFIX` | `matter` | |
| `MQTT_QOS` | `0` | |
| `MQTT_RETAIN` | `true` | retain attribute state |
| `MQTT_PUBLISH_NAMES` | `true` | mirror attributes under `named/` |
| `MATTER_MODEL_PATH` | empty | override the embedded `model.json` |
| `HTTP_ENABLED` | `true` | serve the web console |
| `HTTP_ADDR` | `127.0.0.1:8099` | non-loopback requires credentials |
| `HTTP_USERNAME` / `HTTP_PASSWORD` | empty | enables HTTP basic auth |
| `LOG_LEVEL` | `info` | `debug` logs unhandled event types |

## Build and run

```bash
go mod tidy      # generates go.sum - commit the result
go test ./...
go build -o matter2mqtt .
```

Or `docker compose up -d --build`.

`go mod tidy` is not optional on a fresh clone. Since Go 1.18 `go mod download`
populates the module cache but no longer records module *content* hashes, so a
`go.sum` produced by download alone is incomplete and the build fails with
`missing go.sum entry for module providing package ...`. The Dockerfile runs
`go mod tidy` for this reason; committing `go.sum` makes that step a no-op and
pins the dependency tree.

## Operational notes

**Host networking is required for matter-server**, not for this bridge. Matter
discovery is mDNS on 5353 plus link-local/ULA IPv6; a Docker bridge network
carries neither. Note that `network_mode: host` is unavailable for Docker Swarm
services, so the Matter half of the stack has to run as a plain container.

**IPv6 must actually route.** For Matter-over-Thread the border router
advertises an off-mesh-routable prefix via RA; the host running matter-server
must accept it:

```bash
ip -6 route                           # expect a route toward the OTBR's prefix
sysctl net.ipv6.conf.all.accept_ra    # 1, or 2 if forwarding is enabled
```

**Reconnect behaviour.** The websocket session is disposable: on drop, all nodes
are marked `offline`, `matter_connected` goes `false`, and the bridge redials
with exponential backoff capped at 60s. On reconnect it re-issues
`start_listening` and republishes the full node dump, so retained state
self-heals. MQTT reconnects independently via paho's auto-reconnect.

**Event backpressure.** If MQTT publishing stalls, events are dropped rather
than blocking the websocket read pump — losing an update is preferable to losing
the connection. Raise the `Events` buffer in `matterws.go` if you run a very
chatty fabric.

**Schema drift.** The WebSocket message shapes (`start_listening`,
`attribute_updated` tuples, `device_command` args) are pinned to the
matter-server schema version reported in `bridge/info`. If you upgrade
matter-server across a major schema bump, check that topic first — decoding is
tolerant, so a shape change shows up as missing data rather than a crash.
