package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

//go:embed model.json
var embeddedModel []byte

// Global attributes exist on every cluster and are not listed per-cluster in
// the SDK dump, so they are merged in at lookup time.
var globalAttributes = map[uint32]string{
	65528: "GeneratedCommandList",
	65529: "AcceptedCommandList",
	65530: "EventList",
	65531: "AttributeList",
	65532: "FeatureMap",
	65533: "ClusterRevision",
}

type clusterDef struct {
	Name       string            `json:"name"`
	Attributes map[string]string `json:"attributes"`
	Commands   []string          `json:"commands"`
}

type modelFile struct {
	Clusters    map[string]clusterDef `json:"clusters"`
	DeviceTypes map[string]string     `json:"device_types"`
}

// Model resolves numeric Matter identifiers to names and back.
type Model struct {
	raw         []byte
	clusters    map[uint32]clusterDef
	deviceTypes map[uint32]string

	clusterByName map[string]uint32            // "OnOff" -> 6
	attrByName    map[uint32]map[string]uint32 // 6 -> {"OnOff": 0}
}

// LoadModel reads an override file if given, otherwise the embedded seed.
func LoadModel(path string) (*Model, error) {
	raw := embeddedModel
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read model %s: %w", path, err)
		}
		raw = b
	}

	var mf modelFile
	if err := json.Unmarshal(raw, &mf); err != nil {
		return nil, fmt.Errorf("parse model: %w", err)
	}

	m := &Model{
		raw:           raw,
		clusters:      make(map[uint32]clusterDef, len(mf.Clusters)),
		deviceTypes:   make(map[uint32]string, len(mf.DeviceTypes)),
		clusterByName: make(map[string]uint32, len(mf.Clusters)),
		attrByName:    make(map[uint32]map[string]uint32, len(mf.Clusters)),
	}

	for idStr, def := range mf.Clusters {
		id64, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			continue
		}
		id := uint32(id64)
		m.clusters[id] = def
		m.clusterByName[strings.ToLower(def.Name)] = id

		rev := make(map[string]uint32, len(def.Attributes)+len(globalAttributes))
		for aidStr, aname := range def.Attributes {
			aid, err := strconv.ParseUint(aidStr, 10, 32)
			if err != nil {
				continue
			}
			rev[strings.ToLower(aname)] = uint32(aid)
		}
		for aid, aname := range globalAttributes {
			rev[strings.ToLower(aname)] = aid
		}
		m.attrByName[id] = rev
	}

	for idStr, name := range mf.DeviceTypes {
		if id, err := strconv.ParseUint(idStr, 10, 32); err == nil {
			m.deviceTypes[uint32(id)] = name
		}
	}
	return m, nil
}

// Raw returns the model file as loaded, for serving to the console.
func (m *Model) Raw() []byte { return m.raw }

func (m *Model) Size() (clusters, deviceTypes int) {
	return len(m.clusters), len(m.deviceTypes)
}

// ClusterName returns the cluster name, or "" when unknown.
func (m *Model) ClusterName(id uint32) string {
	if def, ok := m.clusters[id]; ok {
		return def.Name
	}
	return ""
}

// AttributeName returns the attribute name, or "" when unknown.
func (m *Model) AttributeName(cluster, attr uint32) string {
	if name, ok := globalAttributes[attr]; ok {
		return name
	}
	def, ok := m.clusters[cluster]
	if !ok {
		return ""
	}
	return def.Attributes[strconv.FormatUint(uint64(attr), 10)]
}

func (m *Model) DeviceTypeName(id uint32) string { return m.deviceTypes[id] }

// ResolveCluster accepts either a numeric id or a cluster name.
func (m *Model) ResolveCluster(s string) (uint32, bool) {
	if id, err := strconv.ParseUint(s, 10, 32); err == nil {
		return uint32(id), true
	}
	id, ok := m.clusterByName[strings.ToLower(s)]
	return id, ok
}

// ResolveAttribute accepts either a numeric id or an attribute name.
func (m *Model) ResolveAttribute(cluster uint32, s string) (uint32, bool) {
	if id, err := strconv.ParseUint(s, 10, 32); err == nil {
		return uint32(id), true
	}
	rev, ok := m.attrByName[cluster]
	if !ok {
		return 0, false
	}
	id, ok := rev[strings.ToLower(s)]
	return id, ok
}

// NamePath converts "1/1026/0" to "1/TemperatureMeasurement/MeasuredValue".
// Returns ok=false if either name is unknown, so the caller can skip the
// aliased topic rather than publish a half-numeric path.
func (m *Model) NamePath(path string) (string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 3 {
		return "", false
	}
	cluster, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return "", false
	}
	attr, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return "", false
	}
	cName := m.ClusterName(uint32(cluster))
	aName := m.AttributeName(uint32(cluster), uint32(attr))
	if cName == "" || aName == "" {
		return "", false
	}
	return parts[0] + "/" + cName + "/" + aName, true
}
