package editorpick

import "sort"

type Rect struct {
	MinX Fixed `json:"min_x"`
	MinY Fixed `json:"min_y"`
	MaxX Fixed `json:"max_x"`
	MaxY Fixed `json:"max_y"`
}

type BoxQuery struct {
	ID   string `json:"id"`
	Rect Rect   `json:"rect"`
}

type BoxResult struct {
	QueryID   string      `json:"query_id"`
	Status    string      `json:"status"`
	NodeIDs   []string    `json:"node_ids"`
	Ambiguous []Ambiguity `json:"ambiguous,omitempty"`
	Visited   []string    `json:"visited_entries,omitempty"`
}

func (r Rect) normalized() Rect {
	if r.MinX > r.MaxX {
		r.MinX, r.MaxX = r.MaxX, r.MinX
	}
	if r.MinY > r.MaxY {
		r.MinY, r.MaxY = r.MaxY, r.MinY
	}
	return r
}

func (e *Engine) BoxSelect(q BoxQuery) (*BoxResult, error) {
	q.Rect = q.Rect.normalized()
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := &BoxResult{QueryID: q.ID, Status: "hit", NodeIDs: []string{}, Visited: []string{}}
	seen := map[string]bool{}
	for _, root := range e.scene.Roots {
		e.boxNodeLocked(root, q, out, seen)
	}
	sort.Strings(out.NodeIDs)
	sort.Slice(out.Ambiguous, func(i, j int) bool { return out.Ambiguous[i].NodeID < out.Ambiguous[j].NodeID })
	if len(out.Ambiguous) > 0 {
		out.Status = EAmbiguous
	} else if len(out.NodeIDs) == 0 {
		out.Status = "miss"
	}
	return out, nil
}

func (e *Engine) boxNodeLocked(nodeID string, q BoxQuery, out *BoxResult, seen map[string]bool) {
	entry := e.index[nodeID]
	d := e.derived[nodeID]
	node := e.scene.NodeByID[nodeID]
	if entry == nil || d == nil || node == nil {
		return
	}
	out.Visited = append(out.Visited, nodeID)
	if entry.HasBounds && !rectIntersectsAABBLoose(q.Rect, entry.SubtreeBounds) {
		return
	}
	if node.GeometryIndex >= 0 {
		eps := d.PositionErrorUnits + int64(d.Depth+1) + 2
		status, reason := classifyRectAABB(q.Rect, d.AABB, eps)
		switch status {
		case "hit":
			if !seen[nodeID] {
				seen[nodeID] = true
				out.NodeIDs = append(out.NodeIDs, nodeID)
			}
		case EAmbiguous:
			out.Ambiguous = append(out.Ambiguous, Ambiguity{
				Code: EAmbiguous, QueryID: q.ID, NodeID: nodeID, GeometryIndex: node.GeometryIndex,
				Kind: "world_aabb", Reason: reason, Epsilon: Fixed(eps),
				Lower: d.AABB.Min, Upper: d.AABB.Max,
			})
		}
	}
	for _, child := range e.scene.Children[nodeID] {
		e.boxNodeLocked(child, q, out, seen)
	}
}

func rectIntersectsAABBLoose(r Rect, b AABB) bool {
	return r.MinX <= b.Max.X && r.MaxX >= b.Min.X && r.MinY <= b.Max.Y && r.MaxY >= b.Min.Y
}

func classifyRectAABB(r Rect, b AABB, eps int64) (string, string) {
	xStatus, xReason := classifyIntervalOverlap(r.MinX, r.MaxX, b.Min.X, b.Max.X, eps)
	yStatus, yReason := classifyIntervalOverlap(r.MinY, r.MaxY, b.Min.Y, b.Max.Y, eps)
	if xStatus == "miss" || yStatus == "miss" {
		return "miss", "rectangle is separated from world AABB beyond error boundary"
	}
	if xStatus == EAmbiguous || yStatus == EAmbiguous {
		if xReason != "" {
			return EAmbiguous, xReason
		}
		return EAmbiguous, yReason
	}
	return "hit", ""
}

func classifyIntervalOverlap(rMin, rMax, bMin, bMax Fixed, eps int64) (string, string) {
	e := Fixed(eps)
	// Definitely separated with a gap larger than epsilon.
	if rMax < bMin-e || rMin > bMax+e {
		return "miss", ""
	}
	lo := maxFixed(rMin, bMin)
	hi := minFixed(rMax, bMax)
	overlap := hi - lo
	if overlap > 2*e+1 {
		return "hit", ""
	}
	return EAmbiguous, "rectangle boundary is within the fixed-point error envelope"
}
