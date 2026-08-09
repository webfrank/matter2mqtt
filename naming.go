package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// nodeLabelPath is Basic Information (0x0028) attribute 5, NodeLabel: the one
// and only source of a device name.
const nodeLabelPath = "0/40/5"

// Namer derives the named/ topic segment from NodeLabel, and nothing else.
//
// A device with an empty or unset NodeLabel has no name and is simply not
// mirrored into the named/ tree. The canonical numeric tree always carries
// every device regardless, so nothing is ever lost - only the human-readable
// alias is withheld until a label exists.
type Namer struct {
	mu       sync.RWMutex
	resolved map[uint64]string
	byName   map[string]uint64
}

func NewNamer() *Namer {
	return &Namer{
		resolved: make(map[uint64]string),
		byName:   make(map[string]uint64),
	}
}

// Name returns the topic segment for a node. ok is false when the device has
// no usable NodeLabel, meaning it must not be published under named/.
func (n *Namer) Name(nodeID uint64) (string, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	name, ok := n.resolved[nodeID]
	return name, ok
}

// Lookup resolves a name back to a node id, for name-addressed command topics.
func (n *Namer) Lookup(name string) (uint64, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	id, ok := n.byName[strings.ToLower(name)]
	return id, ok
}

// Recompute rebuilds names from the current fabric and reports which nodes
// changed - including those that gained or lost a name - so the caller can
// clear stale retained topics.
func (n *Namer) Recompute(nodes []Node) (changed []uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	base := make(map[uint64]string, len(nodes))
	counts := make(map[string]int, len(nodes))
	for _, node := range nodes {
		name := topicSegment(labelOf(node))
		if name == "" {
			continue // no label: no name, no named/ topics
		}
		base[node.NodeID] = name
		counts[strings.ToLower(name)]++
	}

	next := make(map[uint64]string, len(base))
	byName := make(map[string]uint64, len(base))
	for id, name := range base {
		// Two devices sharing a label would otherwise interleave their state
		// onto one retained topic, so every member of a colliding set is
		// disambiguated by node id. Deterministic regardless of arrival order.
		// Compared case-insensitively, because lookup is.
		if counts[strings.ToLower(name)] > 1 {
			name = name + "-" + strconv.FormatUint(id, 10)
		}
		next[id] = name
		byName[strings.ToLower(name)] = id
	}

	seen := make(map[uint64]struct{}, len(next)+len(n.resolved))
	for id := range next {
		seen[id] = struct{}{}
	}
	for id := range n.resolved {
		seen[id] = struct{}{}
	}
	for id := range seen {
		if n.resolved[id] != next[id] {
			changed = append(changed, id)
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i] < changed[j] })

	n.resolved = next
	n.byName = byName
	return changed
}

// Collisions reports names shared by more than one device before
// disambiguation, so the bridge can warn about them. Keyed by the folded name,
// since names that differ only by case still collide on lookup.
func (n *Namer) Collisions(nodes []Node) map[string][]uint64 {
	groups := map[string][]uint64{}
	for _, node := range nodes {
		if name := topicSegment(labelOf(node)); name != "" {
			key := strings.ToLower(name)
			groups[key] = append(groups[key], node.NodeID)
		}
	}
	for name, ids := range groups {
		if len(ids) < 2 {
			delete(groups, name)
		} else {
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		}
	}
	return groups
}

// labelOf extracts NodeLabel, treating a missing, null, non-string or
// whitespace-only value as absent.
func labelOf(node Node) string {
	raw, ok := node.Attributes[nodeLabelPath]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// maxSegment bounds the topic segment. Matter caps NodeLabel at 32 characters,
// so this only ever trips on a misbehaving device.
const maxSegment = 128

// topicSegment turns a NodeLabel into an MQTT topic segment, keeping the label
// verbatim - case, spaces, punctuation and accents included - so "TEMP Ikea"
// stays "TEMP Ikea".
//
// Only what MQTT cannot carry in a topic name is rewritten: the level separator
// and the two wildcards become a dash, and control characters (including NUL,
// which the spec forbids outright) are dropped.
func topicSegment(in string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(in) {
		if n == maxSegment {
			break
		}
		switch {
		case r == '/' || r == '+' || r == '#':
			b.WriteByte('-')
		case unicode.IsControl(r) || r == unicode.ReplacementChar:
			continue // dropped, and does not count towards the bound
		default:
			b.WriteRune(r)
		}
		n++
	}
	return strings.TrimSpace(b.String())
}
