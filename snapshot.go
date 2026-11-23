package spatialpick

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

const (
	snapshotFormatVersion = 1
	snapshotVersion       = "spatialpick-snapshot-v1"
)

type SnapshotStatus struct {
	Loaded           bool   `json:"loaded"`
	Rebuilt          bool   `json:"rebuilt"`
	Reason           string `json:"reason,omitempty"`
	SnapshotRevision uint64 `json:"snapshot_revision,omitempty"`
	ContentHash      string `json:"content_hash,omitempty"`
}

type snapshotEnvelope struct {
	FormatVersion int    `json:"format_version"`
	Checksum      string `json:"checksum"`
	Payload       []byte `json:"payload"`
}

type snapshotVersioned struct {
	NodeID  string `json:"node_id"`
	Version uint64 `json:"version"`
}

type snapshotLocal struct {
	NodeID string      `json:"node_id"`
	Matrix FixedMatrix `json:"matrix"`
}

type snapshotGeometry struct {
	NodeID   string    `json:"node_id"`
	Geometry *Geometry `json:"geometry"`
}

type snapshotPayload struct {
	FormatVersion   int                 `json:"format_version"`
	SnapshotVersion string              `json:"snapshot_version"`
	BaseSceneHash   string              `json:"base_scene_hash"`
	StructureHash   string              `json:"structure_hash"`
	SceneVersion    string              `json:"scene_version"`
	Precision       int                 `json:"precision"`
	ContentHash     string              `json:"content_hash"`
	Revision        uint64              `json:"revision"`
	IndexVersion    uint64              `json:"index_version"`
	AppliedBatches  []string            `json:"applied_batches"`
	Locals          []snapshotLocal     `json:"locals"`
	LocalVersions   []snapshotVersioned `json:"local_versions"`
	WorldVersions   []snapshotVersioned `json:"world_versions"`
	Geometries      []snapshotGeometry  `json:"geometries"`
	Derived         []*Derived          `json:"derived"`
	IndexEntries    []IndexEntry        `json:"index_entries"`
	BVH             *BVHNode            `json:"bvh,omitempty"`
	MaxErrorBound   float64             `json:"max_error_bound"`
}

// SaveSnapshot serializes a consistent state pointer and commits it with a
// temp-file + fsync + atomic rename sequence. Writes to one path are serialized
// by an advisory lock, and readers only ever see a complete target file.
func (e *Engine) SaveSnapshot(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.snapshotMu.Lock()
	defer e.snapshotMu.Unlock()

	e.mu.RLock()
	s := e.st
	e.mu.RUnlock()
	payload, err := e.makeSnapshotPayload(s)
	if err != nil {
		return err
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return &CodedError{Code: CodeSchema, Op: "save_snapshot", Message: "encode payload", Cause: err}
	}
	sum := sha256.Sum256(payloadBytes)
	envelope := snapshotEnvelope{
		FormatVersion: snapshotFormatVersion,
		Checksum:      hex.EncodeToString(sum[:]),
		Payload:       payloadBytes,
	}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return &CodedError{Code: CodeSchema, Op: "save_snapshot", Message: "encode envelope", Cause: err}
	}
	data = append(data, '\n')
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeSnapshotAtomic(ctx, path, data)
}

func (e *Engine) makeSnapshotPayload(s *state) (*snapshotPayload, error) {
	p := &snapshotPayload{
		FormatVersion:   snapshotFormatVersion,
		SnapshotVersion: snapshotVersion,
		BaseSceneHash:   e.baseHash,
		StructureHash:   sceneStructureHash(e.scene),
		SceneVersion:    e.scene.SceneVersion,
		Precision:       e.scene.Precision,
		ContentHash:     s.contentHash,
		Revision:        s.revision,
		IndexVersion:    s.indexVersion,
		AppliedBatches:  append([]string(nil), s.appliedBatches...),
		MaxErrorBound:   s.index.MaxErrorBound,
	}
	ids := append([]string(nil), e.nodeIDs...)
	for _, id := range ids {
		p.Locals = append(p.Locals, snapshotLocal{NodeID: id, Matrix: s.locals[id]})
		p.LocalVersions = append(p.LocalVersions, snapshotVersioned{NodeID: id, Version: s.localVersions[id]})
		p.WorldVersions = append(p.WorldVersions, snapshotVersioned{NodeID: id, Version: s.worldVersions[id]})
		p.Derived = append(p.Derived, cloneDerived(s.derived[id]))
		if g := s.geometries[id]; g != nil {
			p.Geometries = append(p.Geometries, snapshotGeometry{NodeID: id, Geometry: g})
		}
	}
	p.IndexEntries = append([]IndexEntry(nil), s.index.Entries...)
	p.BVH = cloneBVH(s.index.Root)
	return p, nil
}

func cloneDerived(d *Derived) *Derived {
	if d == nil {
		return nil
	}
	c := *d
	c.ErrorTrace = append([]TransformStepError(nil), d.ErrorTrace...)
	return &c
}

func cloneBVH(n *BVHNode) *BVHNode {
	if n == nil {
		return nil
	}
	c := *n
	c.Left = cloneBVH(n.Left)
	c.Right = cloneBVH(n.Right)
	return &c
}

func writeSnapshotAtomic(ctx context.Context, path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &CodedError{Code: CodeResource, Op: "save_snapshot", Field: dir, Cause: err}
	}
	lock, err := acquireFileLock(path + ".lock")
	if err != nil {
		return &CodedError{Code: CodeResource, Op: "save_snapshot", Field: path + ".lock", Cause: err}
	}
	defer lock.Release()
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		return &CodedError{Code: CodeResource, Op: "save_snapshot", Field: dir, Cause: err}
	}
	tmpName := tmp.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	published = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// OpenEngineWithSnapshot tries to restore the complete snapshot. Any malformed
// snapshot, checksum mismatch, content/structure/version mismatch or partial
// entry set invalidates the whole snapshot and falls back to a full rebuild.
func OpenEngineWithSnapshot(ctx context.Context, scene *Scene, snapshotPath string) (*Engine, SnapshotStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, SnapshotStatus{}, err
	}
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			e, buildErr := NewEngine(scene)
			return e, SnapshotStatus{Rebuilt: true, Reason: "snapshot_missing"}, buildErr
		}
		return nil, SnapshotStatus{}, &CodedError{Code: CodeCorrupt, Op: "load_snapshot", Field: snapshotPath, Cause: err}
	}
	payload, err := decodeSnapshot(data)
	if err != nil {
		e, buildErr := NewEngine(scene)
		return e, SnapshotStatus{Rebuilt: true, Reason: err.Error()}, buildErr
	}
	e, buildErr := NewEngine(scene)
	if buildErr != nil {
		return nil, SnapshotStatus{}, buildErr
	}
	if err := e.restoreSnapshot(payload); err != nil {
		e, buildErr = NewEngine(scene)
		return e, SnapshotStatus{Rebuilt: true, Reason: err.Error()}, buildErr
	}
	return e, SnapshotStatus{Loaded: true, SnapshotRevision: payload.Revision, ContentHash: payload.ContentHash}, nil
}

func decodeSnapshot(data []byte) (*snapshotPayload, error) {
	var env snapshotEnvelope
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("invalid snapshot envelope: %w", err)
	}
	if env.FormatVersion != snapshotFormatVersion {
		return nil, fmt.Errorf("unsupported snapshot format %d", env.FormatVersion)
	}
	sum := sha256.Sum256(env.Payload)
	if hex.EncodeToString(sum[:]) != env.Checksum {
		return nil, fmt.Errorf("snapshot checksum mismatch")
	}
	var p snapshotPayload
	pdec := json.NewDecoder(bytes.NewReader(env.Payload))
	pdec.DisallowUnknownFields()
	if err := pdec.Decode(&p); err != nil {
		return nil, fmt.Errorf("invalid snapshot payload: %w", err)
	}
	if p.FormatVersion != snapshotFormatVersion || p.SnapshotVersion != snapshotVersion {
		return nil, fmt.Errorf("snapshot version mismatch")
	}
	return &p, nil
}

func (e *Engine) restoreSnapshot(p *snapshotPayload) error {
	if p.BaseSceneHash != e.baseHash {
		return fmt.Errorf("base scene hash mismatch")
	}
	if p.StructureHash != sceneStructureHash(e.scene) {
		return fmt.Errorf("scene structure version mismatch")
	}
	if p.SceneVersion != e.scene.SceneVersion || p.Precision != e.scene.Precision {
		return fmt.Errorf("scene version or precision mismatch")
	}
	locals := make(map[string]FixedMatrix, len(p.Locals))
	for _, l := range p.Locals {
		if _, ok := e.nodeByID[l.NodeID]; !ok {
			return fmt.Errorf("snapshot local references unknown node %s", l.NodeID)
		}
		if !l.Matrix.IsAffine(e.scene.Precision) {
			return fmt.Errorf("snapshot local %s is not affine", l.NodeID)
		}
		if _, dup := locals[l.NodeID]; dup {
			return fmt.Errorf("duplicate snapshot local %s", l.NodeID)
		}
		locals[l.NodeID] = l.Matrix
	}
	if len(locals) != len(e.nodeIDs) {
		return fmt.Errorf("snapshot locals do not cover every node")
	}
	localVersions, err := versionsToMap(p.LocalVersions, e.nodeByID)
	if err != nil {
		return err
	}
	worldVersions, err := versionsToMap(p.WorldVersions, e.nodeByID)
	if err != nil {
		return err
	}
	geometries := make(map[string]*Geometry, len(p.Geometries))
	geometryIDs := make(map[string]string, len(p.Geometries))
	for _, sg := range p.Geometries {
		if _, ok := e.nodeByID[sg.NodeID]; !ok {
			return fmt.Errorf("snapshot geometry references unknown node %s", sg.NodeID)
		}
		if sg.Geometry == nil {
			return fmt.Errorf("snapshot geometry for %s is nil", sg.NodeID)
		}
		if err := validateRuntimeGeometry(sg.Geometry); err != nil {
			return fmt.Errorf("snapshot geometry for %s is invalid: %w", sg.NodeID, err)
		}
		if _, dup := geometries[sg.NodeID]; dup {
			return fmt.Errorf("duplicate snapshot geometry for %s", sg.NodeID)
		}
		if owner, dup := geometryIDs[sg.Geometry.ID]; dup {
			return fmt.Errorf("geometry id %s reused by %s and %s", sg.Geometry.ID, owner, sg.NodeID)
		}
		geometries[sg.NodeID] = sg.Geometry
		geometryIDs[sg.Geometry.ID] = sg.NodeID
	}
	if hash := ComputeStateHashWithGeometry(p.BaseSceneHash, p.Precision, locals, p.AppliedBatches, geometries); hash != p.ContentHash {
		return fmt.Errorf("snapshot content hash mismatch")
	}
	derived := make(map[string]*Derived, len(p.Derived))
	for _, d := range p.Derived {
		if d == nil {
			return fmt.Errorf("nil derived entry")
		}
		if _, ok := e.nodeByID[d.NodeID]; !ok {
			return fmt.Errorf("derived references unknown node %s", d.NodeID)
		}
		if _, dup := derived[d.NodeID]; dup {
			return fmt.Errorf("duplicate derived entry %s", d.NodeID)
		}
		if err := validateDerived(d, e.scene); err != nil {
			return err
		}
		if d.LocalVersion != localVersions[d.NodeID] || d.Version != worldVersions[d.NodeID] {
			return fmt.Errorf("derived version mismatch for %s", d.NodeID)
		}
		derived[d.NodeID] = d
	}
	if len(derived) != len(e.nodeIDs) {
		return fmt.Errorf("derived entries do not cover every node")
	}
	for _, id := range e.topo {
		parentWorld := QuantizedIdentity(e.scene.Precision)
		if p := e.nodeByID[id].ParentID; p != "" {
			parentWorld = derived[p].World
		}
		want, err := MultiplyFixed(parentWorld, locals[id], e.scene.Precision)
		if err != nil {
			return fmt.Errorf("recompute world for %s: %w", id, err)
		}
		if derived[id].World != want {
			return fmt.Errorf("derived world does not match local/parent chain for %s", id)
		}
	}
	index := &Index{
		Version:       p.IndexVersion,
		Entries:       append([]IndexEntry(nil), p.IndexEntries...),
		Root:          cloneBVH(p.BVH),
		MaxErrorBound: p.MaxErrorBound,
	}
	if err := validateRestoredIndex(index, e, derived, geometries); err != nil {
		return err
	}
	e.st = &state{
		locals:         locals,
		localVersions:  localVersions,
		worldVersions:  worldVersions,
		derived:        derived,
		geometries:     geometries,
		index:          index,
		revision:       p.Revision,
		indexVersion:   p.IndexVersion,
		appliedBatches: append([]string(nil), p.AppliedBatches...),
		contentHash:    p.ContentHash,
	}
	return nil
}

func versionsToMap(list []snapshotVersioned, nodes map[string]Node) (map[string]uint64, error) {
	out := make(map[string]uint64, len(list))
	for _, v := range list {
		if _, ok := nodes[v.NodeID]; !ok {
			return nil, fmt.Errorf("snapshot version references unknown node %s", v.NodeID)
		}
		if _, dup := out[v.NodeID]; dup {
			return nil, fmt.Errorf("duplicate snapshot version for %s", v.NodeID)
		}
		out[v.NodeID] = v.Version
	}
	if len(out) != len(nodes) {
		return nil, fmt.Errorf("snapshot versions do not cover every node")
	}
	return out, nil
}

func validateRestoredIndex(idx *Index, e *Engine, derived map[string]*Derived, geometries map[string]*Geometry) error {
	if len(idx.Entries) != len(geometries) {
		return fmt.Errorf("index entry count mismatch")
	}
	seen := make(map[string]struct{}, len(idx.Entries))
	maxBound := 0.0
	for i := range idx.Entries {
		entry := &idx.Entries[i]
		d, ok := derived[entry.NodeID]
		if !ok {
			return fmt.Errorf("index entry references unknown node %s", entry.NodeID)
		}
		if _, dup := seen[entry.NodeID]; dup {
			return fmt.Errorf("duplicate index entry for %s", entry.NodeID)
		}
		seen[entry.NodeID] = struct{}{}
		if entry.World != d.World || entry.Version != d.Version || entry.LocalVersion != d.LocalVersion ||
			entry.Depth != d.Depth || entry.GeometryID != d.GeometryID || entry.Failed != d.Failed ||
			entry.AABB != d.AABB || entry.Sphere != d.Sphere || entry.TransformError != d.TransformError ||
			entry.GeometryError != d.GeometryError {
			return fmt.Errorf("index entry inconsistent with derived cache for %s", entry.NodeID)
		}
		g := geometries[entry.NodeID]
		if g == nil || g.ID != entry.GeometryID || !reflect.DeepEqual(*g, entry.Geometry) {
			return fmt.Errorf("index geometry reference mismatch for %s", entry.NodeID)
		}
		if entry.GeometryError > maxBound {
			maxBound = entry.GeometryError
		}
	}
	if idx.MaxErrorBound != maxBound {
		return fmt.Errorf("index max error bound mismatch")
	}
	leaves := make(map[int]struct{}, len(idx.Entries))
	var walk func(*BVHNode) error
	walk = func(n *BVHNode) error {
		if n == nil {
			return fmt.Errorf("nil BVH node")
		}
		if n.Leaf {
			if n.Entry < 0 || n.Entry >= len(idx.Entries) {
				return fmt.Errorf("BVH leaf index out of range")
			}
			if _, dup := leaves[n.Entry]; dup {
				return fmt.Errorf("duplicate BVH leaf")
			}
			leaves[n.Entry] = struct{}{}
			if !n.Bounds.IsValid() {
				return fmt.Errorf("invalid BVH leaf bounds")
			}
			return nil
		}
		if n.Left == nil || n.Right == nil {
			return fmt.Errorf("BVH internal node missing child")
		}
		if err := walk(n.Left); err != nil {
			return err
		}
		return walk(n.Right)
	}
	if idx.Root != nil {
		if err := walk(idx.Root); err != nil {
			return err
		}
	}
	if len(leaves) != len(idx.Entries) {
		return fmt.Errorf("BVH leaves do not cover index entries")
	}
	return nil
}

func sceneStructureHash(scene *Scene) string {
	h := sha256.New()
	nodes := append([]Node(nil), scene.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	for _, n := range nodes {
		fmt.Fprintf(h, "N|%s|%s|%d\n", n.ID, n.ParentID, n.GeometryIndex)
	}
	for _, g := range scene.Geometries {
		fmt.Fprintf(h, "G|%s|%s|%d|%d\n", g.ID, g.Kind, len(g.Vertices), len(g.Triangles))
	}
	return hex.EncodeToString(h.Sum(nil))
}
