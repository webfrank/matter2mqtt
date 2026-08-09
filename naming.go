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
		name := slug(labelOf(node))
		if name == "" {
			continue // no label: no name, no named/ topics
		}
		base[node.NodeID] = name
		counts[name]++
	}

	next := make(map[uint64]string, len(base))
	byName := make(map[string]uint64, len(base))
	for id, name := range base {
		// Two devices sharing a label would otherwise interleave their state
		// onto one retained topic, so every member of a colliding set is
		// disambiguated by node id. Deterministic regardless of arrival order.
		if counts[name] > 1 {
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
// disambiguation, so the bridge can warn about them.
func (n *Namer) Collisions(nodes []Node) map[string][]uint64 {
	groups := map[string][]uint64{}
	for _, node := range nodes {
		if name := slug(labelOf(node)); name != "" {
			groups[name] = append(groups[name], node.NodeID)
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

// slug produces a safe, stable MQTT topic segment: lowercase ASCII, dashes for
// separators, never a wildcard or level separator.
func slug(in string) string {
	var b strings.Builder
	lastDash := true // suppresses a leading dash
	for _, r := range strings.ToLower(strings.TrimSpace(in)) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			lastDash = false
		case r == '.' || r == ':':
			b.WriteRune(r)
			lastDash = false
		default:
			// Spaces, /, +, #, accents and punctuation all collapse to a dash.
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = strings.Trim(out[:48], "-")
	}
	return out
}
