package editorpick

import (
	"math"
	"sort"
	"strings"
)

type PivotOp struct {
	Type   string  `json:"type"`
	Vector Vec3    `json:"vector,omitempty"`
	Axis   string  `json:"axis,omitempty"`
	Angle  float64 `json:"angle,omitempty"`
}

type PivotBatch struct {
	Selection []string  `json:"selection"`
	Pivot     Vec3      `json:"pivot"`
	Ops       []PivotOp `json:"ops"`
}

type WorkingCopy struct {
	baseRevision int64
	scene        *Scene
	changed      map[string]bool
	committed    bool
}

func (e *Engine) NewWorkingCopy() *WorkingCopy {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return &WorkingCopy{baseRevision: e.revision, scene: e.scene.clone(), changed: make(map[string]bool)}
}

func (w *WorkingCopy) Scene() *Scene { return w.scene }

func (e *Engine) ApplyPivotBatch(batch PivotBatch) (*InvalidationReport, error) {
	wc := e.NewWorkingCopy()
	if err := wc.ApplyPivot(batch); err != nil {
		return nil, err
	}
	return e.CommitWorkingCopy(wc)
}

func (w *WorkingCopy) ApplyPivot(batch PivotBatch) error {
	if w.committed {
		return newTxn("working copy has already been committed")
	}
	if len(batch.Selection) == 0 {
		return newTxn("pivot batch selection must not be empty")
	}
	if len(batch.Ops) == 0 {
		return newTxn("pivot batch must contain at least one operation")
	}
	for _, op := range batch.Ops {
		if err := validatePivotOp(op); err != nil {
			return err
		}
	}
	selected := make(map[string]bool, len(batch.Selection))
	for _, id := range batch.Selection {
		if _, ok := w.scene.NodeByID[id]; !ok {
			return newTxn("selected node %q does not exist", id)
		}
		selected[id] = true
	}
	targets := effectivePivotTargets(w.scene, selected)
	if len(targets) == 0 {
		return newTxn("pivot selection has no effective transform targets")
	}

	// A private pre-image gives all-or-nothing semantics inside the working
	// copy as well. The committed Engine is not touched until Commit.
	before := w.scene.clone()
	newChanged := make(map[string]bool)
	for _, id := range targets {
		newChanged[id] = true
	}
	for _, op := range batch.Ops {
		delta, err := pivotDelta(batch.Pivot, op)
		if err != nil {
			w.scene = before
			return newTxn("cannot build pivot transform: %v", err)
		}
		// Depth order is not strictly required for independent effective
		// roots, but makes parent-current calculations deterministic.
		sort.SliceStable(targets, func(i, j int) bool {
			return w.scene.NodeByID[targets[i]].Depth < w.scene.NodeByID[targets[j]].Depth
		})
		for _, id := range targets {
			node := w.scene.NodeByID[id]
			world, err := computeWorldMatrix(w.scene, id)
			if err != nil {
				w.scene = before
				return newTxn("%s: %v", id, err)
			}
			newWorld, err := delta.Mul(world)
			if err != nil {
				w.scene = before
				return newTxn("%s: fixed point overflow", id)
			}
			newLocal := newWorld
			if node.ParentID != "" {
				parentWorld, err := computeWorldMatrix(w.scene, node.ParentID)
				if err != nil {
					w.scene = before
					return newTxn("%s: %v", id, err)
				}
				parentInverse, err := parentWorld.Inverse()
				if err != nil {
					w.scene = before
					return newTxn("%s: parent transform is singular", node.ParentID)
				}
				newLocal, err = parentInverse.Mul(newWorld)
				if err != nil {
					w.scene = before
					return newTxn("%s: local transform overflow", id)
				}
			}
			node.Local = newLocal
		}
	}
	for id := range newChanged {
		w.changed[id] = true
	}
	return nil
}

func validatePivotOp(op PivotOp) error {
	switch op.Type {
	case "translate":
		return nil
	case "rotate":
		if op.Axis != "x" && op.Axis != "y" && op.Axis != "z" {
			return newTxn("rotate op has invalid axis %q", op.Axis)
		}
		if !isFinite(op.Angle) {
			return newTxn("rotate angle must be finite")
		}
		return nil
	case "scale":
		if op.Vector.X == 0 || op.Vector.Y == 0 || op.Vector.Z == 0 {
			return newTxn("scale factors must be non-zero and invertible")
		}
		return nil
	default:
		return newTxn("unknown pivot operation type %q", op.Type)
	}
}

func effectivePivotTargets(scene *Scene, selected map[string]bool) []string {
	var out []string
	for id := range selected {
		ancestorSelected := false
		n := scene.NodeByID[id]
		for n != nil && n.ParentID != "" {
			if selected[n.ParentID] {
				ancestorSelected = true
				break
			}
			n = scene.NodeByID[n.ParentID]
		}
		if !ancestorSelected {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func pivotDelta(pivot Vec3, op PivotOp) (Mat4, error) {
	switch op.Type {
	case "translate":
		return TranslationMat4(op.Vector), nil
	case "rotate":
		r := AxisRotationMat4(op.Axis, op.Angle)
		t := TranslationMat4(pivot)
		back := TranslationMat4(Vec3{-pivot.X, -pivot.Y, -pivot.Z})
		m, err := t.Mul(r)
		if err != nil {
			return Mat4{}, err
		}
		return m.Mul(back)
	case "scale":
		s := ScaleMat4(op.Vector)
		t := TranslationMat4(pivot)
		back := TranslationMat4(Vec3{-pivot.X, -pivot.Y, -pivot.Z})
		m, err := t.Mul(s)
		if err != nil {
			return Mat4{}, err
		}
		return m.Mul(back)
	default:
		return Mat4{}, newTxn("unknown op %q", op.Type)
	}
}

func computeWorldMatrix(scene *Scene, nodeID string) (Mat4, error) {
	var chain []*Node
	id := nodeID
	for id != "" {
		n, ok := scene.NodeByID[id]
		if !ok {
			return Mat4{}, newRef(id, "node not found while computing world matrix")
		}
		chain = append(chain, n)
		id = n.ParentID
	}
	world := IdentityMat4()
	for i := len(chain) - 1; i >= 0; i-- {
		var err error
		world, err = world.Mul(chain[i].Local)
		if err != nil {
			return Mat4{}, err
		}
	}
	return world, nil
}

func (e *Engine) CommitWorkingCopy(w *WorkingCopy) (*InvalidationReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if w.committed {
		return nil, newTxn("working copy has already been committed")
	}
	if w.baseRevision != e.revision {
		return nil, newTxn("working copy is based on stale revision %d (current %d)", w.baseRevision, e.revision)
	}
	changed := make([]string, 0, len(w.changed))
	for id := range w.changed {
		changed = append(changed, id)
	}
	sort.Strings(changed)
	report, err := e.commitTransformsLocked(w.scene, changed)
	if err != nil {
		return nil, err
	}
	w.committed = true
	return report, nil
}

func (e *Engine) commitTransformsLocked(next *Scene, changed []string) (*InvalidationReport, error) {
	changes := make([]Change, 0, len(changed))
	for _, id := range changed {
		changes = append(changes, Change{ObjectID: localObject(id), Reason: "local-transform-edit"})
	}
	return e.commitChangesLocked(next, changes)
}

func (e *Engine) commitChangesLocked(next *Scene, changes []Change) (*InvalidationReport, error) {
	e.revision++
	affected := e.graph.InvalidateChanges(changes)
	report := &InvalidationReport{Revision: e.revision, Objects: affected}

	newDerived := make(map[string]*Derived, len(e.derived))
	for k, v := range e.derived {
		newDerived[k] = v
	}
	newWorld := make(map[string]*WorldGeometry, len(e.worldGeom))
	for k, v := range e.worldGeom {
		newWorld[k] = v
	}
	newIndex := make(map[string]*IndexEntry, len(e.index))
	for k, v := range e.index {
		newIndex[k] = v
	}
	newCache := make(map[string]*queryCacheEntry, len(e.cache))
	for k, v := range e.cache {
		newCache[k] = v
	}

	invalidWorld := map[string]bool{}
	invalidEntry := map[string]bool{}
	for _, obj := range affected {
		switch obj.Kind {
		case "world":
			invalidWorld[strings.TrimPrefix(obj.ObjectID, "world:")] = true
		case "entry":
			invalidEntry[strings.TrimPrefix(obj.ObjectID, "entry:")] = true
		case "query":
			key := strings.TrimPrefix(obj.ObjectID, "query:")
			delete(newCache, key)
		}
	}

	oldScene := e.scene
	e.scene = next
	order, err := e.topoOrderLocked()
	if err != nil {
		e.scene = oldScene
		e.revision--
		return nil, err
	}
	for _, n := range order {
		if !invalidWorld[n.ID] {
			continue
		}
		var parent *Derived
		if n.ParentID != "" {
			parent = newDerived[n.ParentID]
			if parent == nil {
				e.scene = oldScene
				e.revision--
				return nil, newSchema(n.ID, "incremental parent cache missing")
			}
		}
		d, wg, entry, err := computeNode(next, e.revision, n, parent)
		if err != nil {
			e.scene = oldScene
			e.revision--
			return nil, &DomainError{Code: ETxn, NodeID: n.ID, Message: "commit failed; scene rolled back: " + err.Error()}
		}
		newDerived[n.ID] = d
		if wg != nil {
			newWorld[n.ID] = wg
		} else {
			delete(newWorld, n.ID)
		}
		newIndex[n.ID] = entry
	}
	for i := len(order) - 1; i >= 0; i-- {
		id := order[i].ID
		if invalidEntry[id] {
			// Temporarily install maps so shared recompute helper can read
			// children from the new generation.
			e.index = newIndex
			e.scene = next
			e.recomputeSubtreeLocked(id)
		}
	}

	e.derived = newDerived
	e.worldGeom = newWorld
	e.index = newIndex
	e.cache = newCache
	e.lastReport = report
	return report, nil
}

// Ensure math is referenced even if future edits remove direct trigonometry use.
var _ = math.Pi
