package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"inputkernel/internal/model"
	"inputkernel/internal/sampleio"
)

const SnapshotSchemaVersion = 1

type CacheEntryView struct {
	NodeID       string    `json:"node_id"`
	Kind         NodeKind  `json:"kind"`
	CacheKey     string    `json:"cache_key"`
	ParamsHash   string    `json:"params_hash"`
	Dependencies []string  `json:"dependencies"`
	Value        NodeValue `json:"value"`
}

type Snapshot struct {
	SchemaVersion         int              `json:"schema_version"`
	ScenarioID            string           `json:"scenario_id"`
	EventHash             string           `json:"event_hash"`
	ConfigHash            string           `json:"config_hash"`
	FixedPrecisionVersion string           `json:"fixed_precision_version"`
	BindingVersion        string           `json:"binding_version"`
	CacheHash             string           `json:"cache_hash"`
	Nodes                 []CacheEntryView `json:"nodes"`
}

type SnapshotStore struct {
	Dir string
	mu  sync.Mutex
}

var snapshotProcessWriteMu sync.Mutex

func NewSnapshotStore(dir string) *SnapshotStore {
	return &SnapshotStore{Dir: dir}
}

func (s *SnapshotStore) Path(scenarioID string) string {
	return filepath.Join(s.Dir, scenarioID+".snapshot.json")
}

func (s *SnapshotStore) Save(snapshot *Snapshot) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshotProcessWriteMu.Lock()
	defer snapshotProcessWriteMu.Unlock()
	if snapshot == nil {
		return "", model.Schema("snapshot", "", "snapshot is nil")
	}
	if snapshot.SchemaVersion != SnapshotSchemaVersion {
		return "", model.Version("unsupported snapshot schema", snapshot.SchemaVersion)
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", &model.Error{Code: model.CodeStore, Detail: err.Error()}
	}
	path := s.Path(snapshot.ScenarioID)
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return "", &model.Error{Code: model.CodeStore, Detail: err.Error()}
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(s.Dir, "."+snapshot.ScenarioID+"-*.tmp")
	if err != nil {
		return "", &model.Error{Code: model.CodeStore, Detail: err.Error()}
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", &model.Error{Code: model.CodeStore, Detail: err.Error()}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", &model.Error{Code: model.CodeStore, Detail: err.Error()}
	}
	if err := tmp.Close(); err != nil {
		return "", &model.Error{Code: model.CodeStore, Detail: err.Error()}
	}
	if err := atomicReplace(tmpName, path); err != nil {
		return "", &model.Error{Code: model.CodeStore, Detail: err.Error()}
	}
	keep = true
	if dir, err := os.Open(s.Dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return path, nil
}

func (s *SnapshotStore) Load(scenarioID string) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.Path(scenarioID))
	if err != nil {
		return nil, &model.Error{Code: model.CodeStore, Detail: err.Error()}
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var snapshot Snapshot
	if err := dec.Decode(&snapshot); err != nil {
		return nil, model.Version("snapshot decode failed", err.Error())
	}
	if snapshot.SchemaVersion != SnapshotSchemaVersion {
		return nil, model.Version("snapshot schema version mismatch", snapshot.SchemaVersion)
	}
	return &snapshot, nil
}

func (rt *Runtime) MakeSnapshot() (*Snapshot, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.cache) != len(rt.graph.Nodes) {
		return nil, &model.Error{Code: model.CodeStore, Detail: "cache is incomplete; run a full replay first"}
	}
	snapshot := &Snapshot{
		SchemaVersion:         SnapshotSchemaVersion,
		ScenarioID:            rt.scenario.ID,
		EventHash:             rt.graph.EventHash,
		ConfigHash:            sampleio.ConfigHash(rt.scenario),
		FixedPrecisionVersion: rt.scenario.FixedPrecisionVersion,
		BindingVersion:        rt.scenario.BindingVersion,
	}
	for _, id := range rt.graph.Topo {
		entry := rt.cache[id]
		if entry == nil {
			return nil, &model.Error{Code: model.CodeStore, Detail: "missing cache node " + id}
		}
		copyEntry := CacheEntryView{
			NodeID: entry.NodeID, Kind: entry.Kind, CacheKey: entry.CacheKey,
			ParamsHash:   entry.ParamsHash,
			Dependencies: append([]string(nil), entry.Dependencies...),
			Value:        cloneNodeValue(entry.Value),
		}
		snapshot.Nodes = append(snapshot.Nodes, copyEntry)
	}
	snapshot.CacheHash = hashSnapshotNodes(snapshot.Nodes)
	return snapshot, nil
}

func (rt *Runtime) RestoreSnapshot(snapshot *Snapshot) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if snapshot == nil {
		return model.Version("snapshot is nil", "snapshot")
	}
	if err := rt.validateSnapshotLocked(snapshot); err != nil {
		return err
	}
	nextCache := make(map[string]*CacheEntry, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		nextCache[node.NodeID] = &CacheEntry{
			NodeID: node.NodeID, Kind: node.Kind, CacheKey: node.CacheKey,
			ParamsHash:   node.ParamsHash,
			Dependencies: append([]string(nil), node.Dependencies...),
			Value:        cloneNodeValue(node.Value),
		}
	}
	unmapped, err := computeUnmapped(rt.scenario, rt.contexts)
	if err != nil {
		return err
	}
	rt.cache = nextCache
	rt.unmapped = unmapped
	return nil
}

func (rt *Runtime) ValidateSnapshot(snapshot *Snapshot) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.validateSnapshotLocked(snapshot)
}

func (rt *Runtime) validateSnapshotLocked(snapshot *Snapshot) error {
	issues := make([]string, 0)
	if snapshot.SchemaVersion != SnapshotSchemaVersion {
		issues = append(issues, fmt.Sprintf("snapshot_schema_version expected=%d actual=%d", SnapshotSchemaVersion, snapshot.SchemaVersion))
	}
	if snapshot.ScenarioID != rt.scenario.ID {
		issues = append(issues, fmt.Sprintf("scenario_id expected=%s actual=%s", rt.scenario.ID, snapshot.ScenarioID))
	}
	if snapshot.EventHash != rt.graph.EventHash {
		issues = append(issues, fmt.Sprintf("event_hash expected=%s actual=%s", rt.graph.EventHash, snapshot.EventHash))
	}
	if expected := sampleio.ConfigHash(rt.scenario); snapshot.ConfigHash != expected {
		issues = append(issues, fmt.Sprintf("config_hash expected=%s actual=%s", expected, snapshot.ConfigHash))
	}
	if snapshot.FixedPrecisionVersion != rt.scenario.FixedPrecisionVersion {
		issues = append(issues, fmt.Sprintf("fixed_precision_version expected=%s actual=%s", rt.scenario.FixedPrecisionVersion, snapshot.FixedPrecisionVersion))
	}
	if snapshot.BindingVersion != rt.scenario.BindingVersion {
		issues = append(issues, fmt.Sprintf("binding_version expected=%s actual=%s", rt.scenario.BindingVersion, snapshot.BindingVersion))
	}
	if expected := hashSnapshotNodes(snapshot.Nodes); snapshot.CacheHash != expected {
		issues = append(issues, fmt.Sprintf("cache_hash expected=%s actual=%s", expected, snapshot.CacheHash))
	}
	byID := map[string]CacheEntryView{}
	for _, node := range snapshot.Nodes {
		if _, exists := byID[node.NodeID]; exists {
			issues = append(issues, "duplicate_snapshot_node="+node.NodeID)
		}
		if _, ok := rt.graph.Nodes[node.NodeID]; !ok {
			issues = append(issues, "unexpected_snapshot_node="+node.NodeID)
		}
		byID[node.NodeID] = node
	}
	upstream := map[string]string{}
	for _, id := range rt.graph.Topo {
		node := rt.graph.Nodes[id]
		expectedKey := rt.graph.CacheKey(rt.scenario, id, upstream)
		upstream[id] = expectedKey
		entry, ok := byID[id]
		if !ok {
			issues = append(issues, "missing_snapshot_node="+id)
			continue
		}
		if entry.Kind != node.Kind {
			issues = append(issues, fmt.Sprintf("node_kind_mismatch=%s expected=%s actual=%s", id, node.Kind, entry.Kind))
		}
		if entry.ParamsHash != node.ParamsHash {
			issues = append(issues, "node_params_hash_mismatch="+id)
		}
		if entry.CacheKey != expectedKey {
			issues = append(issues, "node_cache_key_mismatch="+id)
		}
		if !equalStrings(entry.Dependencies, node.Dependencies) {
			issues = append(issues, "node_dependency_mismatch="+id)
		}
	}
	if len(issues) > 0 {
		sort.Strings(issues)
		return model.Version("snapshot invalidated as a whole", issues)
	}
	return nil
}

func hashSnapshotNodes(nodes []CacheEntryView) string {
	data, _ := json.Marshal(nodes)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneNodeValue(v NodeValue) NodeValue {
	out := NodeValue{
		AxisObservations: append([]AxisObservation(nil), v.AxisObservations...),
		Digest:           v.Digest,
	}
	if v.Action != nil {
		payload := *v.Action
		payload.Outputs = append([]ActionOutput(nil), v.Action.Outputs...)
		payload.AxisSamples = append([]AxisSample(nil), v.Action.AxisSamples...)
		payload.TimingChecks = append([]TimingCheck(nil), v.Action.TimingChecks...)
		out.Action = &payload
	}
	return out
}
