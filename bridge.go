package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Basic Information cluster (0x0028) attribute ids we surface as a descriptor.
var basicInfoAttrs = map[string]string{
	"1":  "vendor_name",
	"2":  "vendor_id",
	"3":  "product_name",
	"4":  "product_id",
	"5":  "node_label",
	"6":  "location",
	"7":  "hardware_version",
	"8":  "hardware_version_string",
	"9":  "software_version",
	"10": "software_version_string",
	"15": "serial_number",
}

type Bridge struct {
	cfg   *Config
	model *Model
	namer *Namer
	mqtt  mqtt.Client

	mcMu sync.RWMutex
	mc   *MatterClient

	// Live view of the fabric, kept in sync from the event stream so the web
	// console can render without re-querying matter-server.
	nodesMu sync.RWMutex
	nodes   map[uint64]*Node
	info    ServerInfo

	// Console event subscribers (server-sent events).
	subsMu sync.Mutex
	subs   map[chan []byte]struct{}

	// Retained topics we have published per node, so they can be cleared
	// when a node is removed from the fabric.
	topicsMu sync.Mutex
	topics   map[uint64]map[string]struct{}
}

func NewBridge(cfg *Config, model *Model) *Bridge {
	return &Bridge{
		cfg:    cfg,
		model:  model,
		namer:  NewNamer(),
		nodes:  make(map[uint64]*Node),
		subs:   make(map[chan []byte]struct{}),
		topics: make(map[uint64]map[string]struct{}),
	}
}

// ---------- console support ----------

type BridgeStatus struct {
	MQTTConnected   bool       `json:"mqtt_connected"`
	MatterConnected bool       `json:"matter_connected"`
	Broker          string     `json:"broker"`
	TopicPrefix     string     `json:"topic_prefix"`
	NodeCount       int        `json:"node_count"`
	Server          ServerInfo `json:"server"`
}

func (b *Bridge) Status() BridgeStatus {
	b.mcMu.RLock()
	connected := b.mc != nil
	b.mcMu.RUnlock()

	b.nodesMu.RLock()
	defer b.nodesMu.RUnlock()
	return BridgeStatus{
		MQTTConnected:   b.mqtt != nil && b.mqtt.IsConnectionOpen(),
		MatterConnected: connected,
		Broker:          b.cfg.MQTTBroker,
		TopicPrefix:     b.cfg.TopicPrefix,
		NodeCount:       len(b.nodes),
		Server:          b.info,
	}
}

// Nodes returns a snapshot of the fabric, ordered by node id.
func (b *Bridge) Nodes() []Node {
	b.nodesMu.RLock()
	defer b.nodesMu.RUnlock()

	out := make([]Node, 0, len(b.nodes))
	for _, n := range b.nodes {
		copyNode := *n
		copyNode.Attributes = make(map[string]json.RawMessage, len(n.Attributes))
		for k, v := range n.Attributes {
			copyNode.Attributes[k] = v
		}
		out = append(out, copyNode)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// Endpoints exposes the decoded Descriptor view for one node.
func (b *Bridge) Endpoints(n Node) map[string]endpointInfo { return b.describeEndpoints(n) }

func (b *Bridge) subscribeConsole() chan []byte {
	ch := make(chan []byte, 64)
	b.subsMu.Lock()
	b.subs[ch] = struct{}{}
	b.subsMu.Unlock()
	return ch
}

func (b *Bridge) unsubscribeConsole(ch chan []byte) {
	b.subsMu.Lock()
	delete(b.subs, ch)
	b.subsMu.Unlock()
}

// broadcast fans an event out to console clients, dropping for slow ones.
func (b *Bridge) broadcast(kind string, payload any) {
	msg, err := json.Marshal(map[string]any{"type": kind, "data": payload})
	if err != nil {
		return
	}
	b.subsMu.Lock()
	defer b.subsMu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Invoke runs a matter-server command, surfacing the error directly. The MQTT
// path wraps this in a response envelope; the console uses it as-is.
func (b *Bridge) Invoke(ctx context.Context, command string, args map[string]any) (json.RawMessage, error) {
	b.mcMu.RLock()
	mc := b.mc
	b.mcMu.RUnlock()
	if mc == nil {
		return nil, fmt.Errorf("matter-server not connected")
	}
	return mc.Call(ctx, command, args)
}

func (b *Bridge) Model() *Model { return b.model }

// ---------- lifecycle ----------

func (b *Bridge) Run(ctx context.Context) error {
	if err := b.connectMQTT(ctx); err != nil {
		return err
	}
	defer func() {
		b.publish(b.t("bridge/availability"), []byte("offline"), true)
		b.mqtt.Disconnect(500)
	}()

	backoff := time.Second
	for ctx.Err() == nil {
		err := b.session(ctx)
		if ctx.Err() != nil {
			break
		}
		slog.Warn("matter session ended, reconnecting", "err", err, "in", backoff)
		b.publish(b.t("bridge/matter_connected"), []byte("false"), true)
		b.markAllUnavailable()
		b.broadcast("status", b.Status())

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
	}
	return ctx.Err()
}

func (b *Bridge) connectMQTT(ctx context.Context) error {
	opts := mqtt.NewClientOptions().
		AddBroker(b.cfg.MQTTBroker).
		SetClientID(b.cfg.MQTTClientID).
		SetUsername(b.cfg.MQTTUsername).
		SetPassword(b.cfg.MQTTPassword).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30*time.Second).
		SetConnectRetry(true).
		SetConnectRetryInterval(5*time.Second).
		SetWill(b.t("bridge/availability"), "offline", b.cfg.MQTTQoS, true)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		slog.Info("mqtt connected", "broker", b.cfg.MQTTBroker)
		b.publish(b.t("bridge/availability"), []byte("online"), true)
		b.subscribeCommands(c)
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		slog.Warn("mqtt connection lost", "err", err)
	})

	b.mqtt = mqtt.NewClient(opts)
	tok := b.mqtt.Connect()
	select {
	case <-tok.Done():
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := tok.Error(); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	return nil
}

// session runs one websocket connection to matter-server until it dies.
func (b *Bridge) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	mc, err := Dial(dialCtx, b.cfg.MatterWSURL)
	cancel()
	if err != nil {
		return err
	}
	defer mc.Close()

	b.mcMu.Lock()
	b.mc = mc
	b.mcMu.Unlock()
	defer func() {
		b.mcMu.Lock()
		b.mc = nil
		b.mcMu.Unlock()
	}()

	slog.Info("matter-server connected",
		"sdk", mc.Info.SDKVersion,
		"schema", mc.Info.SchemaVersion,
		"bluetooth", mc.Info.BluetoothEnabled,
		"thread_credentials_set", mc.Info.ThreadCredentialsSet)

	if info, err := json.Marshal(mc.Info); err == nil {
		b.publish(b.t("bridge/info"), info, true)
	}

	callCtx, callCancel := context.WithTimeout(ctx, b.cfg.MatterCallTimeout)
	raw, err := mc.Call(callCtx, "start_listening", nil)
	callCancel()
	if err != nil {
		return fmt.Errorf("start_listening: %w", err)
	}

	var nodes []Node
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return fmt.Errorf("decode node list: %w", err)
	}
	b.nodesMu.Lock()
	b.info = mc.Info
	b.nodes = make(map[uint64]*Node, len(nodes))
	b.nodesMu.Unlock()

	for _, n := range nodes {
		b.cacheNode(n)
	}
	b.namer.Recompute(nodes)
	for name, ids := range b.namer.Collisions(nodes) {
		slog.Warn("devices share a NodeLabel, disambiguating with node ids",
			"label", name, "nodes", ids,
			"fix", "give each device a distinct NodeLabel (attribute 0/40/5)")
	}
	unnamed := 0
	for _, n := range nodes {
		if _, ok := b.namedKey(n.NodeID); !ok {
			unnamed++
		}
		b.publishNode(n)
	}
	slog.Info("initial node dump published", "nodes", len(nodes),
		"without_nodelabel", unnamed)
	if unnamed > 0 {
		slog.Info("devices without a NodeLabel are not mirrored under named/",
			"count", unnamed, "fix", "write attribute 0/40/5 to name them")
	}
	b.publish(b.t("bridge/matter_connected"), []byte("true"), true)
	b.broadcast("status", b.Status())

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-mc.Events:
			if !ok {
				return mc.Err()
			}
			b.handleEvent(ev)
		}
	}
}

// ---------- inbound: matter -> mqtt ----------

func (b *Bridge) handleEvent(ev Event) {
	switch ev.Name {
	case "attribute_updated":
		// data is a 3-tuple: [node_id, "endpoint/cluster/attribute", value]
		var tuple []json.RawMessage
		if err := json.Unmarshal(ev.Data, &tuple); err != nil || len(tuple) != 3 {
			slog.Warn("malformed attribute_updated", "data", string(ev.Data))
			return
		}
		var nodeID uint64
		var path string
		if json.Unmarshal(tuple[0], &nodeID) != nil || json.Unmarshal(tuple[1], &path) != nil {
			slog.Warn("malformed attribute_updated header", "data", string(ev.Data))
			return
		}
		b.cacheAttribute(nodeID, path, tuple[2])
		if path == nodeLabelPath {
			// The device was (re)named: republish it under the new key.
			b.refreshNames()
		}
		b.publishAttribute(nodeID, path, tuple[2])
		b.broadcast("attribute", map[string]any{
			"node_id": nodeID, "path": path, "value": json.RawMessage(nonNil(tuple[2])),
		})

	case "node_added", "node_updated":
		var n Node
		if err := json.Unmarshal(ev.Data, &n); err != nil {
			slog.Warn("malformed node payload", "event", ev.Name, "err", err)
			return
		}
		b.cacheNode(n)
		b.refreshNames()
		b.publishNode(n)
		b.broadcast("node", n)

	case "node_removed":
		var nodeID uint64
		if err := json.Unmarshal(ev.Data, &nodeID); err != nil {
			return
		}
		slog.Info("node removed from fabric", "node", nodeID)
		b.clearNode(nodeID)
		b.nodesMu.Lock()
		delete(b.nodes, nodeID)
		b.nodesMu.Unlock()
		b.refreshNames() // a removal can resolve a name collision
		b.broadcast("node_removed", nodeID)

	case "node_event":
		var ne NodeEvent
		if err := json.Unmarshal(ev.Data, &ne); err != nil {
			return
		}
		topic := fmt.Sprintf("%s/node/%d/event/%d/%d/%d", b.cfg.TopicPrefix, ne.NodeID, ne.EndpointID, ne.ClusterID, ne.EventID)
		b.publish(topic, nonNil(ne.Data), false)
		b.broadcast("node_event", ne)

	case "server_shutdown":
		slog.Warn("matter-server announced shutdown")

	default:
		slog.Debug("unhandled event", "event", ev.Name)
	}
}

func (b *Bridge) publishNode(n Node) {
	avail := "offline"
	if n.Available {
		avail = "online"
	}
	b.publishTracked(n.NodeID, fmt.Sprintf("%s/node/%d/availability", b.cfg.TopicPrefix, n.NodeID), []byte(avail), true)

	name, named := b.namedKey(n.NodeID)
	// An empty retained payload deletes the topic, so a device that loses its
	// label stops advertising a name rather than keeping a stale one.
	b.publishTracked(n.NodeID, fmt.Sprintf("%s/node/%d/name", b.cfg.TopicPrefix, n.NodeID),
		[]byte(name), true)

	desc := map[string]any{
		"node_id":           n.NodeID,
		"name":              name,
		"named":             named,
		"is_bridge":         n.IsBridge,
		"date_commissioned": n.DateCommissioned,
		"last_interview":    n.LastInterview,
	}

	for path, val := range n.Attributes {
		b.publishAttribute(n.NodeID, path, val)

		parts := strings.Split(path, "/")
		if len(parts) != 3 {
			continue
		}
		// Basic Information lives on endpoint 0, cluster 40 (0x0028).
		if parts[0] == "0" && parts[1] == "40" {
			if name, ok := basicInfoAttrs[parts[2]]; ok {
				var v any
				if json.Unmarshal(val, &v) == nil {
					desc[name] = v
				}
			}
		}
	}
	desc["endpoints"] = b.describeEndpoints(n)

	if payload, err := json.Marshal(desc); err == nil {
		b.publishTracked(n.NodeID, fmt.Sprintf("%s/node/%d/descriptor", b.cfg.TopicPrefix, n.NodeID), payload, true)
	}
}

func (b *Bridge) publishAttribute(nodeID uint64, path string, value json.RawMessage) {
	if strings.Count(path, "/") != 2 {
		slog.Warn("unexpected attribute path", "node", nodeID, "path", path)
		return
	}
	payload := nonNil(value)
	topic := fmt.Sprintf("%s/node/%d/%s", b.cfg.TopicPrefix, nodeID, path)
	b.publishTracked(nodeID, topic, payload, b.cfg.RetainState)

	// Human-readable mirror. Skipped silently when either name is unknown, so
	// an incomplete model never yields a half-numeric topic.
	if b.cfg.PublishNames {
		if key, named := b.namedKey(nodeID); named {
			if path, ok := b.model.NamePath(path); ok {
				alias := fmt.Sprintf("%s/named/%s/%s", b.cfg.TopicPrefix, key, path)
				b.publishTracked(nodeID, alias, payload, b.cfg.RetainState)
			}
		}
	}
}

// endpointInfo summarises what a device exposes on one endpoint.
type endpointInfo struct {
	DeviceTypes []deviceTypeInfo  `json:"device_types,omitempty"`
	Clusters    map[string]string `json:"clusters,omitempty"`
}

type deviceTypeInfo struct {
	ID   uint32 `json:"id"`
	Name string `json:"name,omitempty"`
}

// describeEndpoints reads the Descriptor cluster (0x001D = 29) on every
// endpoint: attribute 0 is DeviceTypeList, attribute 1 is ServerList.
func (b *Bridge) describeEndpoints(n Node) map[string]endpointInfo {
	out := map[string]endpointInfo{}

	for path, raw := range n.Attributes {
		parts := strings.Split(path, "/")
		if len(parts) != 3 || parts[1] != "29" {
			continue
		}
		ep := parts[0]
		info := out[ep]

		switch parts[2] {
		case "0": // DeviceTypeList
			var entries []map[string]json.Number
			if json.Unmarshal(raw, &entries) != nil {
				continue
			}
			for _, e := range entries {
				// Field naming varies across serialisations; accept the
				// common spellings rather than guessing one.
				for _, key := range []string{"deviceType", "device_type", "0"} {
					if v, ok := e[key]; ok {
						if id, err := strconv.ParseUint(v.String(), 10, 32); err == nil {
							info.DeviceTypes = append(info.DeviceTypes, deviceTypeInfo{
								ID:   uint32(id),
								Name: b.model.DeviceTypeName(uint32(id)),
							})
						}
						break
					}
				}
			}
		case "1": // ServerList
			var ids []uint32
			if json.Unmarshal(raw, &ids) != nil {
				continue
			}
			if info.Clusters == nil {
				info.Clusters = map[string]string{}
			}
			for _, id := range ids {
				info.Clusters[strconv.FormatUint(uint64(id), 10)] = b.model.ClusterName(id)
			}
		default:
			continue
		}
		out[ep] = info
	}
	return out
}

// ---------- outbound: mqtt -> matter ----------

func (b *Bridge) subscribeCommands(c mqtt.Client) {
	subs := map[string]mqtt.MessageHandler{
		b.t("node/+/+/+/+/set"):    b.onWriteAttribute,
		b.t("node/+/+/+/command"):  b.onDeviceCommand,
		b.t("named/+/+/+/+/set"):   b.onWriteAttribute,
		b.t("named/+/+/+/command"): b.onDeviceCommand,
		b.t("bridge/request/+"):    b.onBridgeRequest,
	}
	for filter, handler := range subs {
		if tok := c.Subscribe(filter, b.cfg.MQTTQoS, handler); tok.Wait() && tok.Error() != nil {
			slog.Error("subscribe failed", "filter", filter, "err", tok.Error())
		} else {
			slog.Info("subscribed", "filter", filter)
		}
	}
}

// resolveTarget turns topic segments into numeric ids. Cluster and attribute
// may be given either numerically or by name, so the numeric and named topic
// trees share one code path.
func (b *Bridge) resolveTarget(nodeStr, epStr, clusterStr string) (nodeID uint64, endpoint int, cluster uint32, err error) {
	nodeID, err = b.resolveNode(nodeStr)
	if err != nil {
		return 0, 0, 0, err
	}
	endpoint, err = strconv.Atoi(epStr)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("bad endpoint %q", epStr)
	}
	cluster, ok := b.model.ResolveCluster(clusterStr)
	if !ok {
		return 0, 0, 0, fmt.Errorf("unknown cluster %q", clusterStr)
	}
	return nodeID, endpoint, cluster, nil
}

// resolveNode accepts a numeric node id or a friendly name, so the named/ tree
// is addressable by either.
func (b *Bridge) resolveNode(s string) (uint64, error) {
	if id, err := strconv.ParseUint(s, 10, 64); err == nil {
		return id, nil
	}
	if id, ok := b.namer.Lookup(s); ok {
		return id, nil
	}
	return 0, fmt.Errorf("unknown node %q", s)
}

// <prefix>/{node|named}/<id or name>/<endpoint>/<cluster>/<attribute>/set
// payload: the bare JSON value to write
func (b *Bridge) onWriteAttribute(_ mqtt.Client, msg mqtt.Message) {
	parts := b.topicParts(msg.Topic())
	if len(parts) != 6 {
		return
	}
	nodeID, endpoint, cluster, err := b.resolveTarget(parts[1], parts[2], parts[3])
	if err != nil {
		slog.Warn("ignoring write", "topic", msg.Topic(), "err", err)
		return
	}
	attr, ok := b.model.ResolveAttribute(cluster, parts[4])
	if !ok {
		slog.Warn("ignoring write: unknown attribute", "topic", msg.Topic(), "attribute", parts[4])
		return
	}

	var value any
	if err := json.Unmarshal(msg.Payload(), &value); err != nil {
		// Tolerate unquoted strings so `mosquitto_pub -m on` behaves sanely.
		value = string(msg.Payload())
	}
	go b.call("write_attribute", map[string]any{
		"node_id":        nodeID,
		"attribute_path": fmt.Sprintf("%d/%d/%d", endpoint, cluster, attr),
		"value":          value,
	}, "")
}

// <prefix>/{node|named}/<id>/<endpoint>/<cluster>/command
// payload: {"command":"Toggle","payload":{...}}  or  "Toggle"
func (b *Bridge) onDeviceCommand(_ mqtt.Client, msg mqtt.Message) {
	parts := b.topicParts(msg.Topic())
	if len(parts) != 5 {
		return
	}
	nodeID, endpoint, cluster, err := b.resolveTarget(parts[1], parts[2], parts[3])
	if err != nil {
		slog.Warn("ignoring command", "topic", msg.Topic(), "err", err)
		return
	}

	var req struct {
		Command string         `json:"command"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(msg.Payload(), &req); err != nil || req.Command == "" {
		var name string
		if json.Unmarshal(msg.Payload(), &name) != nil {
			name = strings.TrimSpace(string(msg.Payload()))
		}
		if name == "" {
			slog.Warn("device command without a command name", "topic", msg.Topic())
			return
		}
		req.Command = name
	}
	if req.Payload == nil {
		req.Payload = map[string]any{}
	}

	go b.call("device_command", map[string]any{
		"node_id":      nodeID,
		"endpoint_id":  endpoint,
		"cluster_id":   cluster,
		"command_name": req.Command,
		"payload":      req.Payload,
	}, "")
}

// matter/bridge/request/<matter-server-command>
// payload: the args object, e.g. {"code":"34970112332"}
// Generic passthrough: any command the server exposes works without a code
// change here. Result is echoed on matter/bridge/response/<command>.
func (b *Bridge) onBridgeRequest(_ mqtt.Client, msg mqtt.Message) {
	parts := b.topicParts(msg.Topic())
	if len(parts) != 3 {
		return
	}
	command := parts[2]

	args := map[string]any{}
	if len(strings.TrimSpace(string(msg.Payload()))) > 0 {
		if err := json.Unmarshal(msg.Payload(), &args); err != nil {
			b.publish(b.t("bridge/response/"+command),
				mustJSON(map[string]any{"success": false, "error": "payload must be a JSON object"}), false)
			return
		}
	}
	// Optional client-supplied correlation id, echoed back in the response.
	reqID, _ := args["_request_id"].(string)
	delete(args, "_request_id")

	go func() {
		result := b.call(command, args, reqID)
		b.publish(b.t("bridge/response/"+command), result, false)

		// A removed node leaves stale retained topics behind.
		if command == "remove_node" {
			if id, ok := toUint64(args["node_id"]); ok {
				b.clearNode(id)
			}
		}
	}()
}

// call executes a matter-server command and returns a JSON response envelope.
func (b *Bridge) call(command string, args map[string]any, reqID string) []byte {
	env := map[string]any{"command": command}
	if reqID != "" {
		env["_request_id"] = reqID
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.MatterCallTimeout)
	defer cancel()

	result, err := b.Invoke(ctx, command, args)
	if err != nil {
		slog.Error("command failed", "command", command, "err", err)
		env["success"] = false
		env["error"] = err.Error()
		return mustJSON(env)
	}
	slog.Info("command ok", "command", command)
	env["success"] = true
	env["result"] = json.RawMessage(nonNil(result))
	return mustJSON(env)
}

// ---------- helpers ----------

func (b *Bridge) t(suffix string) string { return b.cfg.TopicPrefix + "/" + suffix }

func (b *Bridge) Namer() *Namer { return b.namer }

// namedKey is the segment used in the named/ tree. ok is false when the device
// has no NodeLabel, in which case nothing is published under named/.
func (b *Bridge) namedKey(nodeID uint64) (string, bool) {
	return b.namer.Name(nodeID)
}

// refreshNames recomputes names and republishes any node whose name changed,
// clearing the retained topics published under the old name first. Called
// whenever the fabric or a NodeLabel changes.
func (b *Bridge) refreshNames() {
	nodes := b.Nodes()
	changed := b.namer.Recompute(nodes)

	for name, ids := range b.namer.Collisions(nodes) {
		slog.Warn("devices share a NodeLabel, disambiguating with node ids",
			"label", name, "nodes", ids,
			"fix", "give each device a distinct NodeLabel (attribute 0/40/5)")
	}
	if len(changed) == 0 {
		return
	}

	for _, id := range changed {
		b.clearNamedTopics(id)
	}
	byID := make(map[uint64]Node, len(nodes))
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	for _, id := range changed {
		if n, ok := byID[id]; ok {
			name, named := b.namedKey(id)
			slog.Info("device name changed", "node", id, "name", name, "published", named)
			b.publishNode(n)
			b.broadcast("node", n)
		}
	}
}

func (b *Bridge) cacheNode(n Node) {
	b.nodesMu.Lock()
	defer b.nodesMu.Unlock()
	copyNode := n
	copyNode.Attributes = make(map[string]json.RawMessage, len(n.Attributes))
	for k, v := range n.Attributes {
		copyNode.Attributes[k] = v
	}
	b.nodes[n.NodeID] = &copyNode
}

func (b *Bridge) cacheAttribute(nodeID uint64, path string, value json.RawMessage) {
	b.nodesMu.Lock()
	defer b.nodesMu.Unlock()
	n, ok := b.nodes[nodeID]
	if !ok {
		return
	}
	if n.Attributes == nil {
		n.Attributes = map[string]json.RawMessage{}
	}
	n.Attributes[path] = append(json.RawMessage(nil), value...)
}

// topicParts strips the configured prefix and splits the remainder.
func (b *Bridge) topicParts(topic string) []string {
	trimmed := strings.TrimPrefix(topic, b.cfg.TopicPrefix+"/")
	return strings.Split(trimmed, "/")
}

func (b *Bridge) publish(topic string, payload []byte, retain bool) {
	if b.mqtt == nil || !b.mqtt.IsConnectionOpen() {
		return
	}
	b.mqtt.Publish(topic, b.cfg.MQTTQoS, retain, payload)
}

// publishTracked records retained topics so they can be cleared on removal.
func (b *Bridge) publishTracked(nodeID uint64, topic string, payload []byte, retain bool) {
	if retain {
		b.topicsMu.Lock()
		if b.topics[nodeID] == nil {
			b.topics[nodeID] = make(map[string]struct{})
		}
		b.topics[nodeID][topic] = struct{}{}
		b.topicsMu.Unlock()
	}
	b.publish(topic, payload, retain)
}

// clearNode wipes every retained topic belonging to a node by publishing an
// empty payload, which is how MQTT deletes a retained message.
func (b *Bridge) clearNode(nodeID uint64) {
	b.topicsMu.Lock()
	topics := b.topics[nodeID]
	delete(b.topics, nodeID)
	b.topicsMu.Unlock()

	for topic := range topics {
		b.publish(topic, []byte{}, true)
	}
}

// clearNamedTopics removes retained topics under the named/ tree for one node,
// used when its name changes so the old path does not linger on the broker.
func (b *Bridge) clearNamedTopics(nodeID uint64) {
	prefix := b.cfg.TopicPrefix + "/named/"
	b.topicsMu.Lock()
	var stale []string
	for topic := range b.topics[nodeID] {
		if strings.HasPrefix(topic, prefix) {
			stale = append(stale, topic)
		}
	}
	for _, topic := range stale {
		delete(b.topics[nodeID], topic)
	}
	b.topicsMu.Unlock()

	for _, topic := range stale {
		b.publish(topic, []byte{}, true)
	}
}

func (b *Bridge) markAllUnavailable() {
	b.topicsMu.Lock()
	ids := make([]uint64, 0, len(b.topics))
	for id := range b.topics {
		ids = append(ids, id)
	}
	b.topicsMu.Unlock()

	for _, id := range ids {
		b.publish(fmt.Sprintf("%s/node/%d/availability", b.cfg.TopicPrefix, id), []byte("offline"), true)
	}
}

func nonNil(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	return raw
}

func mustJSON(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"success":false,"error":"response encoding failed"}`)
	}
	return out
}

func toUint64(v any) (uint64, bool) {
	switch n := v.(type) {
	case float64:
		return uint64(n), true
	case json.Number:
		i, err := n.Int64()
		return uint64(i), err == nil
	case string:
		i, err := strconv.ParseUint(n, 10, 64)
		return i, err == nil
	}
	return 0, false
}
