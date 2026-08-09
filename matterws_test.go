package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeServer mimics python-matter-server closely enough to exercise framing:
// server-info first frame, message_id correlation, and pushed events.
func fakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()

		_ = c.WriteJSON(map[string]any{
			"fabric_id": 1, "compressed_fabric_id": 2, "schema_version": 11,
			"sdk_version": "2024.11.0", "bluetooth_enabled": true,
		})

		for {
			var cmd wsCommand
			if err := c.ReadJSON(&cmd); err != nil {
				return
			}
			switch cmd.Command {
			case "start_listening":
				_ = c.WriteJSON(map[string]any{
					"message_id": cmd.MessageID,
					"result": []map[string]any{{
						"node_id": 4, "available": true,
						"attributes": map[string]any{"1/6/0": true, "0/40/3": "Sensor"},
					}},
				})
				_ = c.WriteJSON(map[string]any{
					"event": "attribute_updated",
					"data":  []any{4, "1/1029/0", 2137},
				})
			case "disconnect":
				return // drop the socket without replying
			case "boom":
				code := 3
				_ = c.WriteJSON(map[string]any{
					"message_id": cmd.MessageID, "error_code": code, "details": "no such node",
				})
			default:
				_ = c.WriteJSON(map[string]any{"message_id": cmd.MessageID, "result": cmd.Args})
			}
		}
	}))
}

func TestClientHandshakeCallAndEvents(t *testing.T) {
	srv := fakeServer(t)
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mc, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer mc.Close()

	if mc.Info.SDKVersion != "2024.11.0" || !mc.Info.BluetoothEnabled {
		t.Fatalf("server info not parsed: %+v", mc.Info)
	}

	raw, err := mc.Call(ctx, "start_listening", nil)
	if err != nil {
		t.Fatalf("start_listening: %v", err)
	}
	var nodes []Node
	if err := json.Unmarshal(raw, &nodes); err != nil {
		t.Fatalf("decode nodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].NodeID != 4 || !nodes[0].Available {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
	if string(nodes[0].Attributes["1/6/0"]) != "true" {
		t.Fatalf("attribute not preserved as raw JSON: %s", nodes[0].Attributes["1/6/0"])
	}

	select {
	case ev := <-mc.Events:
		if ev.Name != "attribute_updated" {
			t.Fatalf("unexpected event %q", ev.Name)
		}
		var tuple []json.RawMessage
		if err := json.Unmarshal(ev.Data, &tuple); err != nil || len(tuple) != 3 {
			t.Fatalf("bad tuple: %s", ev.Data)
		}
		var path string
		_ = json.Unmarshal(tuple[1], &path)
		if path != "1/1029/0" || string(tuple[2]) != "2137" {
			t.Fatalf("bad attribute event: path=%s value=%s", path, tuple[2])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}

	// Concurrent calls must not cross-deliver responses.
	type res struct {
		out string
		err error
	}
	ch := make(chan res, 4)
	for i := 0; i < 4; i++ {
		i := i
		go func() {
			r, err := mc.Call(ctx, "echo", map[string]any{"n": i})
			ch <- res{string(r), err}
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		r := <-ch
		if r.err != nil {
			t.Fatalf("echo failed: %v", r.err)
		}
		seen[r.out] = true
	}
	if len(seen) != 4 {
		t.Fatalf("responses got crossed: %v", seen)
	}
}

func TestCallSurfacesServerError(t *testing.T) {
	srv := fakeServer(t)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mc, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer mc.Close()

	if _, err := mc.Call(ctx, "boom", nil); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "no such node") {
		t.Fatalf("error detail lost: %v", err)
	}
}

func TestSessionEndsWhenServerDrops(t *testing.T) {
	srv := fakeServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mc, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Ask the fake server to hang up. The call itself will fail; what we care
	// about is that the session tears down and Events closes.
	go func() { _, _ = mc.Call(ctx, "disconnect", nil) }()

	select {
	case <-mc.Done():
		if mc.Err() == nil {
			t.Fatal("expected a session error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session did not terminate after server dropped")
	}
	if _, ok := <-mc.Events; ok {
		t.Fatal("Events channel should be closed")
	}
}

func TestTopicParts(t *testing.T) {
	m, err := LoadModel("")
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	b := NewBridge(&Config{TopicPrefix: "vapp/matter"}, m)
	got := b.topicParts("vapp/matter/node/4/1/6/0/set")
	want := []string{"node", "4", "1", "6", "0", "set"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestModelLookups(t *testing.T) {
	m, err := LoadModel("")
	if err != nil {
		t.Fatalf("load embedded model: %v", err)
	}
	if n, _ := m.Size(); n == 0 {
		t.Fatal("embedded model has no clusters")
	}

	if got := m.ClusterName(1026); got != "TemperatureMeasurement" {
		t.Fatalf("cluster 1026 = %q", got)
	}
	if got := m.AttributeName(1026, 0); got != "MeasuredValue" {
		t.Fatalf("attribute 1026/0 = %q", got)
	}
	// Global attributes must resolve for every cluster, including unknown ones.
	if got := m.AttributeName(64999, 65533); got != "ClusterRevision" {
		t.Fatalf("global attribute = %q", got)
	}
	if got := m.DeviceTypeName(770); got != "TemperatureSensor" {
		t.Fatalf("device type 770 = %q", got)
	}

	named, ok := m.NamePath("1/1026/0")
	if !ok || named != "1/TemperatureMeasurement/MeasuredValue" {
		t.Fatalf("NamePath = %q ok=%v", named, ok)
	}
	// Unknown clusters must not produce a half-numeric alias.
	if _, ok := m.NamePath("1/64999/0"); ok {
		t.Fatal("expected NamePath to refuse an unknown cluster")
	}
}

func TestResolveAcceptsNamesAndNumbers(t *testing.T) {
	m, err := LoadModel("")
	if err != nil {
		t.Fatalf("load model: %v", err)
	}

	for _, in := range []string{"6", "OnOff", "onoff"} {
		if id, ok := m.ResolveCluster(in); !ok || id != 6 {
			t.Fatalf("ResolveCluster(%q) = %d ok=%v", in, id, ok)
		}
	}
	for _, in := range []string{"17", "OnLevel", "onlevel"} {
		if id, ok := m.ResolveAttribute(8, in); !ok || id != 17 {
			t.Fatalf("ResolveAttribute(8, %q) = %d ok=%v", in, id, ok)
		}
	}
	// Numeric ids must pass through even when the model does not know them,
	// so an incomplete model never blocks a write.
	if id, ok := m.ResolveCluster("64999"); !ok || id != 64999 {
		t.Fatalf("unknown numeric cluster rejected: %d %v", id, ok)
	}
	if _, ok := m.ResolveCluster("NoSuchCluster"); ok {
		t.Fatal("expected unknown cluster name to fail")
	}
}

func TestDescribeEndpoints(t *testing.T) {
	m, err := LoadModel("")
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	b := NewBridge(&Config{TopicPrefix: "vapp/matter"}, m)

	n := Node{NodeID: 4, Attributes: map[string]json.RawMessage{
		"1/29/0": json.RawMessage(`[{"deviceType": 770, "revision": 2}]`),
		"1/29/1": json.RawMessage(`[29, 1026, 64999]`),
	}}
	eps := b.describeEndpoints(n)

	ep1, ok := eps["1"]
	if !ok {
		t.Fatalf("endpoint 1 missing: %+v", eps)
	}
	if len(ep1.DeviceTypes) != 1 || ep1.DeviceTypes[0].ID != 770 || ep1.DeviceTypes[0].Name != "TemperatureSensor" {
		t.Fatalf("device types: %+v", ep1.DeviceTypes)
	}
	if ep1.Clusters["1026"] != "TemperatureMeasurement" {
		t.Fatalf("cluster names: %+v", ep1.Clusters)
	}
	// Unknown cluster ids are still listed, just without a name.
	if name, present := ep1.Clusters["64999"]; !present || name != "" {
		t.Fatalf("unknown cluster handling: %q present=%v", name, present)
	}
}
