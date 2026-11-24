package editorpick

import "math/big"

type PointQuery struct {
	ID    string `json:"id"`
	Point Vec3   `json:"point"`
}

type PointResult struct {
	QueryID   string      `json:"query_id"`
	Status    string      `json:"status"`
	NodeIDs   []string    `json:"node_ids"`
	Ambiguous []Ambiguity `json:"ambiguous,omitempty"`
	Visited   []string    `json:"visited_entries,omitempty"`
}

func (e *Engine) QueryPoint(q PointQuery) (*PointResult, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := &PointResult{QueryID: q.ID, Status: "miss", NodeIDs: []string{}, Visited: []string{}}
	seen := map[string]bool{}
	for _, root := range e.scene.Roots {
		e.pointNodeLocked(root, q, out, seen)
	}
	if len(out.Ambiguous) > 0 {
		out.Status = EAmbiguous
	} else if len(out.NodeIDs) > 0 {
		out.Status = "hit"
	}
	return out, nil
}

func (e *Engine) pointNodeLocked(nodeID string, q PointQuery, out *PointResult, seen map[string]bool) {
	entry := e.index[nodeID]
	d := e.derived[nodeID]
	node := e.scene.NodeByID[nodeID]
	if entry == nil || d == nil || node == nil {
		return
	}
	out.Visited = append(out.Visited, nodeID)
	if entry.HasBounds && !pointInAABB(q.Point, entry.SubtreeBounds) {
		return
	}
	if node.GeometryIndex >= 0 {
		g := e.scene.Geometries[node.GeometryIndex]
		eps := d.MaxMatrixErrorUnits + int64(d.Depth+1) + 2
		local, err := d.Inverse.TransformPoint(q.Point)
		status, reason, lower, upper := "miss", "", d.AABB.Min, d.AABB.Max
		if err == nil {
			switch g.Kind {
			case GeometrySphere:
				status, reason = classifyPointSphere(local, g.Center, g.Radius, eps)
				lower = d.Sphere.Center
				upper = d.Sphere.Center
			default:
				status, reason, lower, upper = classifyPointAABB(local, g.LocalAABB, eps)
			}
		}
		switch status {
		case "hit":
			if !seen[nodeID] {
				seen[nodeID] = true
				out.NodeIDs = append(out.NodeIDs, nodeID)
			}
		case EAmbiguous:
			out.Ambiguous = append(out.Ambiguous, Ambiguity{
				Code: EAmbiguous, QueryID: q.ID, NodeID: nodeID, GeometryIndex: node.GeometryIndex,
				Kind: string(g.Kind), Reason: reason, Epsilon: Fixed(eps), Point: q.Point,
				Lower: lower, Upper: upper,
			})
		}
	}
	for _, child := range e.scene.Children[nodeID] {
		e.pointNodeLocked(child, q, out, seen)
	}
}

func pointInAABB(p Vec3, b AABB) bool {
	return p.X >= b.Min.X && p.X <= b.Max.X &&
		p.Y >= b.Min.Y && p.Y <= b.Max.Y &&
		p.Z >= b.Min.Z && p.Z <= b.Max.Z
}

func classifyPointAABB(p Vec3, b AABB, eps int64) (string, string, Vec3, Vec3) {
	e := Fixed(eps)
	coords := [3]Fixed{p.X, p.Y, p.Z}
	mins := [3]Fixed{b.Min.X, b.Min.Y, b.Min.Z}
	maxs := [3]Fixed{b.Max.X, b.Max.Y, b.Max.Z}
	for i := 0; i < 3; i++ {
		if coords[i] < mins[i]-e || coords[i] > maxs[i]+e {
			return "miss", "", b.Min, b.Max
		}
		if coords[i] <= mins[i]+e || coords[i] >= maxs[i]-e {
			return EAmbiguous, "point is within the fixed-point error boundary of an AABB face", b.Min, b.Max
		}
	}
	return "hit", "", b.Min, b.Max
}

func classifyPointSphere(p, c Vec3, r Fixed, eps int64) (string, string) {
	dx := big.NewInt(int64(p.X - c.X))
	dy := big.NewInt(int64(p.Y - c.Y))
	dz := big.NewInt(int64(p.Z - c.Z))
	distSq := new(big.Int).Add(new(big.Int).Mul(dx, dx), new(big.Int).Add(new(big.Int).Mul(dy, dy), new(big.Int).Mul(dz, dz)))
	lowR := r - Fixed(eps)
	if lowR > 0 {
		lowSq := big.NewInt(int64(lowR))
		lowSq.Mul(lowSq, lowSq)
		if distSq.Cmp(lowSq) <= 0 {
			return "hit", ""
		}
	}
	highR := r + Fixed(eps)
	highSq := big.NewInt(int64(highR))
	highSq.Mul(highSq, highSq)
	if distSq.Cmp(highSq) > 0 {
		return "miss", ""
	}
	return EAmbiguous, "point is within the fixed-point error boundary of the sphere surface"
}
