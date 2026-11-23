package spatialpick

import (
	"fmt"
	"sort"
)

type GeometryMutation struct {
	NodeID   string    `json:"node_id"`
	Geometry *Geometry `json:"geometry,omitempty"`
}

type GeometryPlan struct {
	BaseRevision     uint64             `json:"base_revision"`
	BaseIndexVersion uint64             `json:"base_index_version"`
	Mutations        []GeometryMutation `json:"mutations"`
	Invalidation     *InvalidationPlan  `json:"invalidation"`
}

func validateRuntimeGeometry(g *Geometry) error {
	if g == nil {
		return coded(CodeSchema, "geometry", "nil geometry")
	}
	if g.ID == "" {
		return &CodedError{Code: CodeSchema, Op: "geometry", GeometryID: g.ID, Field: "geometry.id", Message: "geometry id must not be empty"}
	}
	finiteVec := func(v Vec3, path string) error {
		if !v.IsFinite() {
			return &CodedError{Code: CodeSchema, Op: "geometry", GeometryID: g.ID, Field: path, Message: "coordinate must be finite"}
		}
		return nil
	}
	switch g.Kind {
	case GeometryAABB:
		if err := finiteVec(g.Min, "geometry.min"); err != nil {
			return err
		}
		if err := finiteVec(g.Max, "geometry.max"); err != nil {
			return err
		}
		if g.Min.X > g.Max.X || g.Min.Y > g.Max.Y || g.Min.Z > g.Max.Z {
			return &CodedError{Code: CodeSchema, Op: "geometry", GeometryID: g.ID, Field: "geometry.max", Message: "AABB max must be >= min"}
		}
	case GeometrySphere:
		if err := finiteVec(g.Center, "geometry.center"); err != nil {
			return err
		}
		if !finite(g.Radius) || g.Radius <= 0 {
			return &CodedError{Code: CodeSchema, Op: "geometry", GeometryID: g.ID, Field: "geometry.radius", Message: "radius must be finite and positive"}
		}
	case GeometryMesh:
		if len(g.Vertices) < 3 || len(g.Triangles) == 0 {
			return &CodedError{Code: CodeSchema, Op: "geometry", GeometryID: g.ID, Field: "geometry.vertices", Message: "mesh needs vertices and triangles"}
		}
		for i, v := range g.Vertices {
			if !v.IsFinite() {
				return &CodedError{Code: CodeSchema, Op: "geometry", GeometryID: g.ID, Field: fmt.Sprintf("geometry.vertices[%d]", i), Message: "coordinate must be finite"}
			}
		}
		for i, tri := range g.Triangles {
			for j, idx := range tri {
				if idx < 0 || idx >= len(g.Vertices) {
					return &CodedError{Code: CodeRef, Op: "geometry", GeometryID: g.ID, Field: fmt.Sprintf("geometry.triangles[%d][%d]", i, j), Message: "triangle index out of range"}
				}
			}
		}
	default:
		return &CodedError{Code: CodeSchema, Op: "geometry", GeometryID: g.ID, Field: "geometry.kind", Message: "unknown geometry kind"}
	}
	return nil
}

func cloneGeometry(g *Geometry) *Geometry {
	if g == nil {
		return nil
	}
	c := *g
	c.Vertices = append([]Vec3(nil), g.Vertices...)
	c.Triangles = append([][3]int(nil), g.Triangles...)
	return &c
}

func (e *Engine) PlanGeometryMutations(mutations []GeometryMutation) (*GeometryPlan, error) {
	s := e.currentState()
	plan, _, err := e.buildGeometryPlan(s, mutations)
	return plan, err
}

func (e *Engine) buildGeometryPlan(s *state, mutations []GeometryMutation) (*GeometryPlan, map[string]*Geometry, error) {
	if len(mutations) == 0 {
		return nil, nil, &CodedError{Code: CodeTransaction, Op: "geometry_plan", Field: "mutations", Message: "at least one geometry mutation is required"}
	}
	nextGeometries := make(map[string]*Geometry, len(s.geometries))
	for id, g := range s.geometries {
		nextGeometries[id] = g
	}
	seen := make(map[string]struct{}, len(mutations))
	items := make([]InvalidationItem, 0, len(mutations)*2)
	changed := make([]string, 0, len(mutations))
	clean := make([]GeometryMutation, 0, len(mutations))
	for i, mut := range mutations {
		if _, ok := e.nodeByID[mut.NodeID]; !ok {
			return nil, nil, &CodedError{Code: CodeRef, Op: "geometry_plan", NodeID: mut.NodeID, Field: fmt.Sprintf("mutations[%d].node_id", i), Message: "unknown node"}
		}
		if _, dup := seen[mut.NodeID]; dup {
			return nil, nil, &CodedError{Code: CodeTransaction, Op: "geometry_plan", NodeID: mut.NodeID, Field: fmt.Sprintf("mutations[%d].node_id", i), Message: "duplicate node mutation"}
		}
		seen[mut.NodeID] = struct{}{}
		old := s.geometries[mut.NodeID]
		var g *Geometry
		if mut.Geometry != nil {
			if err := validateRuntimeGeometry(mut.Geometry); err != nil {
				return nil, nil, err
			}
			g = cloneGeometry(mut.Geometry)
		}
		if g == nil {
			delete(nextGeometries, mut.NodeID)
			if old != nil {
				items = append(items, InvalidationItem{ObjectID: indexObject(mut.NodeID, old.ID), Reason: "geometry_remove:" + mut.NodeID})
			}
		} else {
			nextGeometries[mut.NodeID] = g
			if old != nil && old.ID != g.ID {
				items = append(items, InvalidationItem{ObjectID: indexObject(mut.NodeID, old.ID), Reason: "geometry_replace_old:" + mut.NodeID})
			}
			items = append(items, InvalidationItem{ObjectID: indexObject(mut.NodeID, g.ID), Reason: "geometry_add_or_replace:" + mut.NodeID})
		}
		clean = append(clean, GeometryMutation{NodeID: mut.NodeID, Geometry: g})
		changed = append(changed, mut.NodeID)
	}
	ids := make(map[string]string, len(nextGeometries))
	for nodeID, g := range nextGeometries {
		if owner, dup := ids[g.ID]; dup && owner != nodeID {
			return nil, nil, &CodedError{Code: CodeSchema, Op: "geometry_plan", NodeID: nodeID, GeometryID: g.ID, Field: "geometry.id", Message: "geometry id is already used by another node"}
		}
		ids[g.ID] = nodeID
	}
	sort.Strings(changed)
	cacheItems := make([]InvalidationItem, 0, len(changed))
	for _, id := range changed {
		cacheItems = append(cacheItems, InvalidationItem{ObjectID: cacheObject(id), Reason: "geometry_shape:" + id})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ObjectID < items[j].ObjectID })
	items = dedupeInvalidationItems(items)
	plan := &GeometryPlan{
		BaseRevision:     s.revision,
		BaseIndexVersion: s.indexVersion,
		Mutations:        clean,
		Invalidation: &InvalidationPlan{
			BaseRevision:     s.revision,
			BaseIndexVersion: s.indexVersion,
			ChangedNodes:     changed,
			AffectedNodes:    changed,
			CacheItems:       cacheItems,
			IndexItems:       items,
			Reason:           "geometry_mutations",
		},
	}
	return plan, nextGeometries, nil
}

func dedupeInvalidationItems(items []InvalidationItem) []InvalidationItem {
	if len(items) == 0 {
		return items
	}
	out := items[:0]
	for _, item := range items {
		if len(out) == 0 || out[len(out)-1].ObjectID != item.ObjectID {
			out = append(out, item)
		}
	}
	return out
}

func (e *Engine) CommitGeometryPlan(plan *GeometryPlan) (*InvalidationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if plan == nil {
		return nil, coded(CodeTransaction, "commit_geometry", "nil geometry plan")
	}
	if plan.BaseRevision != e.st.revision || plan.BaseIndexVersion != e.st.indexVersion {
		return nil, &CodedError{Code: CodeTransaction, Op: "commit_geometry", ExpectedVersion: plan.BaseRevision, HasExpectedVersion: true, Message: "geometry plan is based on a stale revision"}
	}
	checked, nextGeometries, err := e.buildGeometryPlan(e.st, plan.Mutations)
	if err != nil {
		return nil, err
	}
	if !sameInvalidationManifest(plan.Invalidation, checked.Invalidation) {
		return nil, &CodedError{Code: CodeDependency, Op: "commit_geometry", Missing: missingManifestItems(plan.Invalidation, checked.Invalidation), Message: "geometry invalidation plan is incomplete"}
	}
	next := e.st.clone()
	next.geometries = nextGeometries
	for _, mut := range plan.Mutations {
		next.worldVersions[mut.NodeID]++
	}
	for _, id := range checked.Invalidation.ChangedNodes {
		n := e.nodeByID[id]
		var parent *Derived
		if n.ParentID != "" {
			parent = next.derived[n.ParentID]
			if parent == nil || parent.Failed {
				return nil, &CodedError{Code: CodeTransaction, Op: "commit_geometry", NodeID: id, Message: "parent derived cache unavailable"}
			}
		}
		d, err := BuildDerived(&n, next.locals[id], next.localVersions[id], next.worldVersions[id], parent, next.geometries[id], e.scene.Precision)
		if err != nil {
			return nil, &CodedError{Code: CodeTransaction, Op: "commit_geometry", NodeID: id, Cause: err}
		}
		next.derived[id] = d
	}
	next.revision++
	next.indexVersion++
	changedSet := make(map[string]struct{}, len(checked.Invalidation.ChangedNodes))
	for _, id := range checked.Invalidation.ChangedNodes {
		changedSet[id] = struct{}{}
	}
	next.index = e.buildIndexFromState(next, changedSet)
	next.contentHash = ComputeStateHashWithGeometry(e.baseHash, e.scene.Precision, next.locals, next.appliedBatches, next.geometries)
	e.st = next
	out := checked.Invalidation
	out.BaseRevision = next.revision
	out.BaseIndexVersion = next.indexVersion
	return out, nil
}

func (e *Engine) ApplyGeometryMutations(mutations []GeometryMutation) (*InvalidationPlan, error) {
	plan, err := e.PlanGeometryMutations(mutations)
	if err != nil {
		return nil, err
	}
	return e.CommitGeometryPlan(plan)
}

func (e *Engine) buildIndexFromState(s *state, changed map[string]struct{}) *Index {
	entries := make([]IndexEntry, 0, len(s.geometries))
	oldByNode := make(map[string]IndexEntry, len(s.index.Entries))
	for _, entry := range s.index.Entries {
		oldByNode[entry.NodeID] = entry
	}
	for _, id := range e.nodeIDs {
		g := s.geometries[id]
		if g == nil {
			continue
		}
		if _, isChanged := changed[id]; !isChanged {
			if old, ok := oldByNode[id]; ok && old.GeometryID == g.ID {
				entries = append(entries, old)
				continue
			}
		}
		entries = append(entries, MakeIndexEntry(s.derived[id], g, e.scene.Precision))
	}
	return BuildIndex(entries, s.indexVersion)
}

func sameInvalidationManifest(a, b *InvalidationPlan) bool {
	if a == nil || b == nil {
		return a == b
	}
	return len(missingManifestItems(a, b)) == 0
}

func missingManifestItems(planned, required *InvalidationPlan) []string {
	if planned == nil {
		if required == nil {
			return nil
		}
		var ids []string
		for _, item := range required.CacheItems {
			ids = append(ids, item.ObjectID)
		}
		for _, item := range required.IndexItems {
			ids = append(ids, item.ObjectID)
		}
		return uniqueSorted(ids)
	}
	if required == nil {
		return nil
	}
	have := make(map[string]struct{})
	for _, item := range planned.CacheItems {
		have[item.ObjectID] = struct{}{}
	}
	for _, item := range planned.IndexItems {
		have[item.ObjectID] = struct{}{}
	}
	var missing []string
	for _, item := range required.CacheItems {
		if _, ok := have[item.ObjectID]; !ok {
			missing = append(missing, item.ObjectID)
		}
	}
	for _, item := range required.IndexItems {
		if _, ok := have[item.ObjectID]; !ok {
			missing = append(missing, item.ObjectID)
		}
	}
	return uniqueSorted(missing)
}
