package spatialpick

import (
	"context"
	"fmt"
)

type ThreePathResult struct {
	Incremental ResultBundle
	FullRebuild ResultBundle
	Resume      ResultBundle
	Checkpoint  int
}

// NewEngineFromLocals constructs all derived data and the spatial index from a
// complete local-transform map. It is the final materialization step used by
// the full-rebuild verification path.
func NewEngineFromLocals(scene *Scene, locals map[string]FixedMatrix, appliedBatches []string) (*Engine, error) {
	e, err := NewEngine(scene)
	if err != nil {
		return nil, err
	}
	if len(locals) != len(e.nodeIDs) {
		return nil, &CodedError{Code: CodeSchema, Op: "full_rebuild", Message: "local transform map does not cover every node"}
	}
	s := &state{
		locals:         make(map[string]FixedMatrix, len(locals)),
		localVersions:  make(map[string]uint64, len(locals)),
		worldVersions:  make(map[string]uint64, len(locals)),
		derived:        make(map[string]*Derived, len(locals)),
		geometries:     make(map[string]*Geometry),
		indexVersion:   1,
		revision:       uint64(len(appliedBatches)),
		appliedBatches: append([]string(nil), appliedBatches...),
	}
	for _, id := range e.nodeIDs {
		m, ok := locals[id]
		if !ok || !m.IsAffine(scene.Precision) {
			return nil, &CodedError{Code: CodeSchema, Op: "full_rebuild", NodeID: id, Field: "local_matrix", Message: "missing or invalid local transform"}
		}
		s.locals[id] = m
		s.localVersions[id] = 0
		s.worldVersions[id] = 1
		if n := e.nodeByID[id]; n.GeometryIndex >= 0 {
			s.geometries[id] = cloneGeometry(&scene.Geometries[n.GeometryIndex])
		}
	}
	for _, id := range e.topo {
		n := e.nodeByID[id]
		var parent *Derived
		if n.ParentID != "" {
			parent = s.derived[n.ParentID]
		}
		g := s.geometries[id]
		d, err := BuildDerived(&n, s.locals[id], 0, 1, parent, g, scene.Precision)
		if err != nil {
			return nil, err
		}
		s.derived[id] = d
	}
	var entries []IndexEntry
	for _, id := range e.nodeIDs {
		if g := s.geometries[id]; g != nil {
			entries = append(entries, MakeIndexEntry(s.derived[id], g, scene.Precision))
		}
	}
	s.index = BuildIndex(entries, 1)
	s.contentHash = ComputeStateHashWithGeometry(e.baseHash, scene.Precision, s.locals, s.appliedBatches, s.geometries)
	e.st = s
	return e, nil
}

// RunThreePaths runs the same edit sequence through incremental commits, a
// process-restart checkpoint, and a full derived rebuild. It returns E_DIVERGE
// at the first differing object/data item.
func RunThreePaths(ctx context.Context, scene *Scene, checkpoint int, snapshotPath string) (*ThreePathResult, error) {
	if checkpoint < 0 || checkpoint > len(scene.EditSequence) {
		return nil, &CodedError{Code: CodeParameter, Op: "three_paths", Field: "checkpoint", Message: "checkpoint is outside the edit sequence"}
	}
	incremental, err := NewEngine(scene)
	if err != nil {
		return nil, err
	}
	for _, batch := range scene.EditSequence {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := incremental.ApplyPivotBatch(batch); err != nil {
			return nil, err
		}
	}

	working, err := NewEngine(scene)
	if err != nil {
		return nil, err
	}
	for i := 0; i < checkpoint; i++ {
		if _, err := working.ApplyPivotBatch(scene.EditSequence[i]); err != nil {
			return nil, err
		}
	}
	if err := working.SaveSnapshot(ctx, snapshotPath); err != nil {
		return nil, err
	}
	resumed, status, err := OpenEngineWithSnapshot(ctx, scene, snapshotPath)
	if err != nil {
		return nil, err
	}
	if !status.Loaded {
		return nil, fmt.Errorf("resume path did not load snapshot: %s", status.Reason)
	}
	for i := checkpoint; i < len(scene.EditSequence); i++ {
		if _, err := resumed.ApplyPivotBatch(scene.EditSequence[i]); err != nil {
			return nil, err
		}
	}

	finalLocals := make(map[string]FixedMatrix)
	for _, id := range incremental.nodeIDs {
		d, err := incremental.GetDerived(id, 0)
		if err != nil {
			return nil, err
		}
		finalLocals[id] = incremental.st.locals[id]
		_ = d
	}
	full, err := NewEngineFromLocals(scene, finalLocals, incremental.AppliedBatches())
	if err != nil {
		return nil, err
	}
	out := &ThreePathResult{
		Incremental: incremental.Bundle(),
		FullRebuild: full.Bundle(),
		Resume:      resumed.Bundle(),
		Checkpoint:  checkpoint,
	}
	if err := CompareBundles(&out.Incremental, &out.FullRebuild); err != nil {
		var ce *CodedError
		if IsCoded(err, &ce) {
			ce.Message = "incremental and full rebuild diverge"
		}
		return nil, err
	}
	if err := CompareBundles(&out.Incremental, &out.Resume); err != nil {
		var ce *CodedError
		if IsCoded(err, &ce) {
			ce.Message = "incremental and resumed paths diverge"
		}
		return nil, err
	}
	return out, nil
}
