package spatialpick

import (
	"sort"
)

type DependencyEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

type InvalidationItem struct {
	ObjectID string `json:"object_id"`
	Reason   string `json:"reason"`
}

type InvalidationPlan struct {
	BaseRevision     uint64             `json:"base_revision"`
	BaseIndexVersion uint64             `json:"base_index_version"`
	ChangedNodes     []string           `json:"changed_nodes"`
	AffectedNodes    []string           `json:"affected_nodes"`
	CacheItems       []InvalidationItem `json:"cache_items"`
	IndexItems       []InvalidationItem `json:"index_items"`
	Reason           string             `json:"reason"`
}

func cacheObject(nodeID string) string { return "cache:" + nodeID }

func indexObject(nodeID, geometryID string) string {
	return "index:" + nodeID + "/" + geometryID
}

func (e *Engine) buildDependencyGraph(geometries map[string]*Geometry) []DependencyEdge {
	var edges []DependencyEdge
	for _, id := range e.nodeIDs {
		n := e.nodeByID[id]
		edges = append(edges, DependencyEdge{
			From: "local:" + id, To: cacheObject(id), Kind: "local_transform", Reason: "local transform contributes to world cache",
		})
		if n.ParentID != "" {
			edges = append(edges, DependencyEdge{
				From: cacheObject(n.ParentID), To: cacheObject(id), Kind: "world_parent", Reason: "parent world transform contributes to child world cache",
			})
		}
		if g := geometries[id]; g != nil {
			gid := g.ID
			edges = append(edges, DependencyEdge{
				From: "geometry:" + gid, To: cacheObject(id), Kind: "geometry_bounds", Reason: "world AABB, bounding sphere and geometry error derive from geometry shape",
			})
			edges = append(edges, DependencyEdge{
				From: cacheObject(id), To: indexObject(id, gid), Kind: "geometry_world", Reason: "index entry contains world bounds and fixed world transform",
			})
			edges = append(edges, DependencyEdge{
				From: "geometry:" + gid, To: indexObject(id, gid), Kind: "geometry_shape", Reason: "index entry borrows immutable geometry data",
			})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].Kind < edges[j].Kind
	})
	return edges
}

// Dependencies returns a stable snapshot of the explicit dependency graph.
func (e *Engine) Dependencies() []DependencyEdge {
	e.mu.RLock()
	geometries := e.st.geometries
	e.mu.RUnlock()
	return e.buildDependencyGraph(geometries)
}

func (e *Engine) affectedClosure(ids []string) ([]string, []string, error) {
	changed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := e.nodeByID[id]; !ok {
			return nil, nil, &CodedError{Code: CodeRef, Op: "invalidation", NodeID: id, Field: "node_id", Message: "unknown node"}
		}
		changed[id] = struct{}{}
	}
	affected := make(map[string]struct{}, len(changed))
	var visit func(string)
	visit = func(id string) {
		if _, seen := affected[id]; seen {
			return
		}
		affected[id] = struct{}{}
		for _, child := range e.children[id] {
			visit(child)
		}
	}
	for id := range changed {
		visit(id)
	}
	changedList := sortedStringsFromSet(changed)
	affectedList := sortedStringsFromSet(affected)
	return changedList, affectedList, nil
}

func (e *Engine) PlanTransformInvalidation(nodeIDs []string, reason string) (*InvalidationPlan, error) {
	s := e.currentState()
	changed, affected, err := e.affectedClosure(nodeIDs)
	if err != nil {
		return nil, err
	}
	p := &InvalidationPlan{
		BaseRevision:     s.revision,
		BaseIndexVersion: s.indexVersion,
		ChangedNodes:     changed,
		AffectedNodes:    affected,
		Reason:           reason,
	}
	changedSet := make(map[string]struct{}, len(changed))
	for _, id := range changed {
		changedSet[id] = struct{}{}
	}
	for _, id := range affected {
		r := "ancestor_transform:" + ancestorChanged(id, changedSet, e.nodeByID)
		if _, ok := changedSet[id]; ok {
			r = "local_transform:" + id
		}
		p.CacheItems = append(p.CacheItems, InvalidationItem{ObjectID: cacheObject(id), Reason: r})
		if g := s.geometries[id]; g != nil {
			p.IndexItems = append(p.IndexItems, InvalidationItem{
				ObjectID: indexObject(id, g.ID),
				Reason:   r,
			})
		}
	}
	sort.Slice(p.CacheItems, func(i, j int) bool { return p.CacheItems[i].ObjectID < p.CacheItems[j].ObjectID })
	sort.Slice(p.IndexItems, func(i, j int) bool { return p.IndexItems[i].ObjectID < p.IndexItems[j].ObjectID })
	return p, nil
}

func ancestorChanged(nodeID string, changed map[string]struct{}, nodes map[string]Node) string {
	cur := nodeID
	for cur != "" {
		if _, ok := changed[cur]; ok {
			return cur
		}
		cur = nodes[cur].ParentID
	}
	return ""
}

func (e *Engine) validatePlan(plan *InvalidationPlan, changes map[string]FixedMatrix, geometries map[string]*Geometry) error {
	if plan == nil {
		return coded(CodeDependency, "apply", "nil invalidation plan")
	}
	requiredIDs := make([]string, 0, len(changes))
	for id := range changes {
		if _, ok := e.nodeByID[id]; !ok {
			return &CodedError{Code: CodeRef, Op: "apply", NodeID: id, Field: "changes", Message: "unknown changed node"}
		}
		requiredIDs = append(requiredIDs, id)
	}
	_, actualAffected, err := e.affectedClosure(requiredIDs)
	if err != nil {
		return err
	}
	actual := make(map[string]struct{}, len(actualAffected))
	for _, id := range actualAffected {
		actual[id] = struct{}{}
	}
	plannedNodes := make(map[string]struct{}, len(plan.AffectedNodes))
	for _, id := range plan.AffectedNodes {
		plannedNodes[id] = struct{}{}
	}
	var missing []string
	for _, id := range actualAffected {
		if _, ok := plannedNodes[id]; !ok {
			missing = append(missing, cacheObject(id))
			if g := geometries[id]; g != nil {
				missing = append(missing, indexObject(id, g.ID))
			}
		}
	}
	plannedCache := make(map[string]struct{}, len(plan.CacheItems))
	for _, item := range plan.CacheItems {
		plannedCache[item.ObjectID] = struct{}{}
	}
	for id := range actual {
		if _, ok := plannedCache[cacheObject(id)]; !ok {
			missing = append(missing, cacheObject(id))
			if g := geometries[id]; g != nil {
				missing = append(missing, indexObject(id, g.ID))
			}
		}
	}
	plannedIndex := make(map[string]struct{}, len(plan.IndexItems))
	for _, item := range plan.IndexItems {
		plannedIndex[item.ObjectID] = struct{}{}
	}
	for id := range actual {
		if g := geometries[id]; g != nil {
			obj := indexObject(id, g.ID)
			if _, ok := plannedIndex[obj]; !ok {
				missing = append(missing, obj)
			}
		}
	}
	if len(missing) > 0 {
		missing = uniqueSorted(missing)
		return &CodedError{
			Code: CodeDependency, Op: "apply", Missing: missing,
			Message: "invalidation plan omits derived objects that depend on the changed nodes",
		}
	}
	changedSet := make(map[string]struct{}, len(plan.ChangedNodes))
	for _, id := range plan.ChangedNodes {
		changedSet[id] = struct{}{}
	}
	for id := range changes {
		if _, ok := changedSet[id]; !ok {
			return &CodedError{Code: CodeDependency, Op: "apply", NodeID: id, Missing: []string{cacheObject(id)}, Message: "changed node is not declared in invalidation plan"}
		}
	}
	return nil
}

func uniqueSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
