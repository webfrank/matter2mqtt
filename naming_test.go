package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func labelled(id uint64, label string) Node {
	attrs := map[string]json.RawMessage{
		"0/40/3": json.RawMessage(`"SomeProduct"`), // must never be used as a name
	}
	if label != "" {
		attrs[nodeLabelPath] = json.RawMessage(`"` + label + `"`)
	}
	return Node{NodeID: id, Available: true, Attributes: attrs}
}

func TestTopicSegmentKeepsTheLabelVerbatim(t *testing.T) {
	cases := map[string]string{
		"TEMP Ikea":              "TEMP Ikea",
		"Kitchen Sensor":         "Kitchen Sensor",
		"  Salotto  ":            "Salotto",
		"già-caldo":              "già-caldo",
		"UPPER_case_Name":        "UPPER_case_Name",
		"v1.2:sensor":            "v1.2:sensor",
		"___":                    "___",
		"Temp/Humidity #1":       "Temp-Humidity -1",
		"a+b#c/d":                "a-b-c-d",
		"tab\there":              "tabhere",
		strings.Repeat("x", 200): strings.Repeat("x", 128),
	}
	for in, want := range cases {
		if got := topicSegment(in); got != want {
			t.Errorf("topicSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTopicSegmentNeverProducesWildcardsOrSeparators(t *testing.T) {
	for _, in := range []string{"a/b", "a+b", "a#b", "café/+#", "a\x00b"} {
		if got := topicSegment(in); strings.ContainsAny(got, "/+#\x00") {
			t.Fatalf("topicSegment(%q) = %q contains an MQTT-unsafe character", in, got)
		}
	}
}

func TestNameComesOnlyFromNodeLabel(t *testing.T) {
	n := NewNamer()
	n.Recompute([]Node{
		labelled(1, "Kitchen Sensor"),
		labelled(2, ""), // has ProductName, but no label
	})

	if name, ok := n.Name(1); !ok || name != "Kitchen Sensor" {
		t.Errorf("node 1 = %q ok=%v, want the NodeLabel verbatim", name, ok)
	}
	// No fallback to ProductName, and no node-<id> fallback either.
	if name, ok := n.Name(2); ok {
		t.Errorf("node 2 should have no name, got %q", name)
	}
	if name, ok := n.Name(999); ok {
		t.Errorf("unknown node should have no name, got %q", name)
	}
}

func TestLabelEdgeCasesCountAsAbsent(t *testing.T) {
	// The last case is a label made only of control characters: MQTT cannot
	// carry them, so nothing is left to name the device with.
	ctrl, _ := json.Marshal(string([]rune{1, 2}))
	for _, raw := range []string{`""`, `"   "`, `null`, `123`, `{"a":1}`, string(ctrl)} {
		n := NewNamer()
		n.Recompute([]Node{{NodeID: 1, Attributes: map[string]json.RawMessage{
			nodeLabelPath: json.RawMessage(raw),
		}}})
		if name, ok := n.Name(1); ok {
			t.Errorf("label %s should count as absent, got %q", raw, name)
		}
	}
}

func TestCollisionsAreDisambiguatedDeterministically(t *testing.T) {
	nodes := []Node{labelled(4, "Sensor"), labelled(9, "Sensor"), labelled(2, "Unique")}

	a := NewNamer()
	a.Recompute(nodes)
	b := NewNamer()
	b.Recompute([]Node{nodes[2], nodes[1], nodes[0]}) // reversed arrival order

	for _, id := range []uint64{2, 4, 9} {
		an, _ := a.Name(id)
		bn, _ := b.Name(id)
		if an != bn {
			t.Fatalf("node %d: %q vs %q - name depends on arrival order", id, an, bn)
		}
	}
	if n4, _ := a.Name(4); n4 != "Sensor-4" {
		t.Fatalf("node 4 = %q", n4)
	}
	if n9, _ := a.Name(9); n9 != "Sensor-9" {
		t.Fatalf("node 9 = %q", n9)
	}
	if n2, _ := a.Name(2); n2 != "Unique" {
		t.Fatalf("non-colliding node was suffixed: %q", n2)
	}

	if got := a.Collisions(nodes); len(got) != 1 || len(got["sensor"]) != 2 {
		t.Fatalf("collision report = %+v", got)
	}
}

func TestLabelsDifferingOnlyByCaseCollide(t *testing.T) {
	// Lookup folds case, so "Sensor" and "sensor" would resolve to the same
	// node id. They must be treated as a collision and suffixed.
	n := NewNamer()
	n.Recompute([]Node{labelled(4, "Sensor"), labelled(9, "sensor")})

	if got, _ := n.Name(4); got != "Sensor-4" {
		t.Fatalf("node 4 = %q", got)
	}
	if got, _ := n.Name(9); got != "sensor-9" {
		t.Fatalf("node 9 = %q", got)
	}
	if id, ok := n.Lookup("SENSOR-9"); !ok || id != 9 {
		t.Fatalf("lookup = %d %v", id, ok)
	}
}

func TestRecomputeReportsGainAndLossOfName(t *testing.T) {
	n := NewNamer()
	nodes := []Node{labelled(1, "Old"), labelled(2, "")}
	n.Recompute(nodes)

	// Label changed.
	nodes[0] = labelled(1, "New")
	if changed := n.Recompute(nodes); len(changed) != 1 || changed[0] != 1 {
		t.Fatalf("rename not reported: %v", changed)
	}
	// Label gained: node 2 enters the named tree.
	nodes[1] = labelled(2, "Fresh")
	if changed := n.Recompute(nodes); len(changed) != 1 || changed[0] != 2 {
		t.Fatalf("gaining a label not reported: %v", changed)
	}
	// Label lost: node 1 must be reported so its topics get cleared.
	nodes[0] = labelled(1, "")
	changed := n.Recompute(nodes)
	if len(changed) != 1 || changed[0] != 1 {
		t.Fatalf("losing a label not reported: %v", changed)
	}
	if _, ok := n.Name(1); ok {
		t.Fatal("node 1 should have no name after losing its label")
	}
}

func TestLookupIsReversibleAndCaseInsensitive(t *testing.T) {
	n := NewNamer()
	n.Recompute([]Node{labelled(7, "Kitchen Sensor"), labelled(8, "")})

	if id, ok := n.Lookup("Kitchen Sensor"); !ok || id != 7 {
		t.Fatalf("lookup = %d %v", id, ok)
	}
	if id, ok := n.Lookup("kitchen sensor"); !ok || id != 7 {
		t.Fatalf("lookup should be case-insensitive: %d %v", id, ok)
	}
	if _, ok := n.Lookup("someproduct"); ok {
		t.Fatal("ProductName must not be addressable")
	}
}

func TestUnlabelledDeviceGetsNoNamedTopics(t *testing.T) {
	m, _ := LoadModel("")
	b := NewBridge(&Config{TopicPrefix: "m", RetainState: true, PublishNames: true}, m)

	unlabelled := labelled(9, "")
	unlabelled.Attributes["1/1026/0"] = json.RawMessage(`2137`)
	b.cacheNode(unlabelled)
	b.namer.Recompute(b.Nodes())
	b.publishNode(unlabelled)

	b.topicsMu.Lock()
	defer b.topicsMu.Unlock()
	numeric := 0
	for topic := range b.topics[9] {
		if strings.Contains(topic, "/named/") {
			t.Fatalf("unlabelled device published under named/: %s", topic)
		}
		numeric++
	}
	if numeric == 0 {
		t.Fatal("numeric topics missing - the device must still be fully published")
	}
}

func TestLabelledDeviceGetsNamedTopics(t *testing.T) {
	m, _ := LoadModel("")
	b := NewBridge(&Config{TopicPrefix: "m", RetainState: true, PublishNames: true}, m)

	n := labelled(4, "Kitchen Sensor")
	n.Attributes["1/1026/0"] = json.RawMessage(`2137`)
	b.cacheNode(n)
	b.namer.Recompute(b.Nodes())
	b.publishNode(n)

	want := "m/named/Kitchen Sensor/1/TemperatureMeasurement/MeasuredValue"
	b.topicsMu.Lock()
	defer b.topicsMu.Unlock()
	if _, ok := b.topics[4][want]; !ok {
		t.Fatalf("expected %s among %d tracked topics", want, len(b.topics[4]))
	}
}

func TestSpacedAndMixedCaseLabelReachesTheTopicVerbatim(t *testing.T) {
	m, _ := LoadModel("")
	b := NewBridge(&Config{TopicPrefix: "m", RetainState: true, PublishNames: true}, m)

	n := labelled(4, "TEMP Ikea")
	n.Attributes["1/1026/0"] = json.RawMessage(`2137`)
	b.cacheNode(n)
	b.namer.Recompute(b.Nodes())
	b.publishNode(n)

	want := "m/named/TEMP Ikea/1/TemperatureMeasurement/MeasuredValue"
	b.topicsMu.Lock()
	defer b.topicsMu.Unlock()
	if _, ok := b.topics[4][want]; !ok {
		t.Fatalf("expected %s among %d tracked topics", want, len(b.topics[4]))
	}
}

func TestResolveNodeAcceptsNameOrID(t *testing.T) {
	m, _ := LoadModel("")
	b := NewBridge(&Config{TopicPrefix: "m"}, m)
	b.cacheNode(labelled(4, "Kitchen Sensor"))
	b.namer.Recompute(b.Nodes())

	if id, err := b.resolveNode("Kitchen Sensor"); err != nil || id != 4 {
		t.Fatalf("by name: %d %v", id, err)
	}
	if id, err := b.resolveNode("4"); err != nil || id != 4 {
		t.Fatalf("by id: %d %v", id, err)
	}
	if _, err := b.resolveNode("ghost"); err == nil {
		t.Fatal("unknown name should error")
	}
}

func TestClearNamedTopicsLeavesNumericTreeIntact(t *testing.T) {
	m, _ := LoadModel("")
	b := NewBridge(&Config{TopicPrefix: "m", RetainState: true, PublishNames: true}, m)

	n := labelled(4, "Old Name")
	n.Attributes["1/1026/0"] = json.RawMessage(`2137`)
	b.cacheNode(n)
	b.namer.Recompute(b.Nodes())
	b.publishNode(n)

	b.clearNamedTopics(4)

	b.topicsMu.Lock()
	defer b.topicsMu.Unlock()
	for topic := range b.topics[4] {
		if strings.Contains(topic, "/named/") {
			t.Fatalf("named topic survived the clear: %s", topic)
		}
	}
	if len(b.topics[4]) == 0 {
		t.Fatal("numeric topics were cleared too")
	}
}
