package editorpick

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Snapshot struct {
	Schema           string                    `json:"schema"`
	StructureVersion string                    `json:"structure_version"`
	Precision        PrecisionSpec             `json:"precision"`
	SceneHash        string                    `json:"scene_hash"`
	Revision         int64                     `json:"revision"`
	NodeLocals       map[string]Mat4           `json:"node_locals"`
	NodeGeometry     map[string]int            `json:"node_geometry"`
	Geometries       []Geometry                `json:"geometries"`
	Derived          map[string]*Derived       `json:"derived"`
	WorldGeometry    map[string]*WorldGeometry `json:"world_geometry"`
	Index            map[string]*IndexEntry    `json:"index"`
	GraphEdges       []DependencyEdge          `json:"graph_edges"`
	ContentHash      string                    `json:"content_hash"`
}

// SaveSnapshot atomically writes one complete generation. The snapshot mutex
// serializes writers; a temp file, fsync and rename prevent interleaved or
// partially observed files.
func (e *Engine) SaveSnapshot(path string) error {
	e.snapshotWriteMu.Lock()
	defer e.snapshotWriteMu.Unlock()

	e.mu.RLock()
	snap := &Snapshot{
		Schema:           SnapshotSchemaV1,
		StructureVersion: e.scene.StructureVersion,
		Precision:        e.scene.Precision,
		SceneHash:        e.scene.SourceHash(),
		Revision:         e.revision,
		NodeLocals:       map[string]Mat4{},
		NodeGeometry:     map[string]int{},
		Geometries:       e.scene.Geometries,
		Derived:          e.derived,
		WorldGeometry:    e.worldGeom,
		Index:            e.index,
		GraphEdges:       e.graph.Edges,
	}
	for _, n := range e.scene.Nodes {
		snap.NodeLocals[n.ID] = n.Local
		snap.NodeGeometry[n.ID] = n.GeometryIndex
	}
	hash, err := canonicalHash(snap)
	e.mu.RUnlock()
	if err != nil {
		return err
	}
	snap.ContentHash = hash
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(tmp)
		return writeErr
	}
	if syncErr != nil {
		_ = os.Remove(tmp)
		return syncErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func LoadSnapshot(path string, scene *Scene) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &DomainError{Code: EHash, ObjectID: path, Message: "snapshot unreadable: " + err.Error()}
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, &DomainError{Code: EHash, ObjectID: path, Message: "snapshot JSON is corrupt: " + err.Error()}
	}
	if err := snap.Verify(scene); err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *Snapshot) Verify(scene *Scene) error {
	if s.Schema != SnapshotSchemaV1 {
		return &DomainError{Code: EVersion, ObjectID: "snapshot", Message: "unexpected snapshot schema " + s.Schema}
	}
	if s.StructureVersion != StructureVersion || scene != nil && s.StructureVersion != scene.StructureVersion {
		return &DomainError{Code: EVersion, ObjectID: "structure_version", Message: "snapshot structure version mismatch"}
	}
	if err := s.Precision.Compatible(); err != nil {
		return err
	}
	if scene != nil && s.SceneHash != scene.SourceHash() {
		return &DomainError{Code: EHash, ObjectID: "scene_hash", Message: "snapshot scene content hash mismatch"}
	}
	provided := s.ContentHash
	s.ContentHash = ""
	calculated, err := canonicalHash(s)
	s.ContentHash = provided
	if err != nil {
		return &DomainError{Code: EHash, ObjectID: "content_hash", Message: err.Error()}
	}
	if provided == "" || provided != calculated {
		return &DomainError{Code: EHash, ObjectID: "content_hash", Message: "snapshot content hash mismatch"}
	}
	return nil
}

func (e *Engine) RestoreSnapshot(snap *Snapshot) error {
	if snap == nil {
		return &DomainError{Code: EHash, ObjectID: "snapshot", Message: "nil snapshot"}
	}
	if err := snap.Verify(e.scene); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revision = snap.Revision
	if len(snap.NodeLocals) > 0 {
		for id, local := range snap.NodeLocals {
			if n, ok := e.scene.NodeByID[id]; ok {
				n.Local = local
			}
		}
	}
	if len(snap.NodeGeometry) > 0 {
		for id, idx := range snap.NodeGeometry {
			if n, ok := e.scene.NodeByID[id]; ok {
				n.GeometryIndex = idx
			}
		}
	}
	if len(snap.Geometries) > 0 {
		e.scene.Geometries = snap.Geometries
	}
	e.derived = make(map[string]*Derived, len(snap.Derived))
	for k, v := range snap.Derived {
		e.derived[k] = v
	}
	e.worldGeom = make(map[string]*WorldGeometry, len(snap.WorldGeometry))
	for k, v := range snap.WorldGeometry {
		e.worldGeom[k] = v
	}
	e.index = make(map[string]*IndexEntry, len(snap.Index))
	for k, v := range snap.Index {
		e.index[k] = v
	}
	e.graph = graphFromEdges(snap.GraphEdges)
	e.cache = make(map[string]*queryCacheEntry)
	e.lastReport = &InvalidationReport{Revision: e.revision, Objects: []InvalidObject{{ObjectID: "<snapshot>", Kind: "restore", Reason: "snapshot-restored"}}}
	return nil
}

func graphFromEdges(edges []DependencyEdge) *DepGraph {
	g := NewDepGraph()
	for _, edge := range edges {
		g.AddEdge(edge.From, edge.To, edge.Reason)
	}
	return g
}

type RecoveryNotice struct {
	Discarded bool   `json:"discarded"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
}

// RecoverEngine always starts from a full rebuild. A valid snapshot then
// replaces those caches; any hash/version/precision mismatch discards the
// snapshot and leaves the clean full-rebuild engine in use.
func RecoverEngine(scene *Scene, path string) (*Engine, *RecoveryNotice, error) {
	engine, err := NewEngine(scene)
	if err != nil {
		return nil, nil, err
	}
	snap, loadErr := LoadSnapshot(path, scene)
	if loadErr == nil {
		if restoreErr := engine.RestoreSnapshot(snap); restoreErr == nil {
			return engine, nil, nil
		} else {
			loadErr = restoreErr
		}
	}
	var de *DomainError
	if errors.As(loadErr, &de) {
		return engine, &RecoveryNotice{Discarded: true, Code: de.Code, Message: fmt.Sprintf("snapshot discarded and fully rebuilt: %s", de.Error())}, nil
	}
	return engine, &RecoveryNotice{Discarded: true, Code: EHash, Message: loadErr.Error()}, nil
}
