package maskqa

import (
	"fmt"
	"sort"
)

// TileInputHash hashes the tile rectangle and candidate-mask pixels.
func TileInputHash(m *Mask, tile Tile) string {
	r := tile.Rect
	b := make([]byte, 0, r.Dx()*r.Dy()+16)
	b = append(b, byte(tile.ID>>24), byte(tile.ID>>16), byte(tile.ID>>8), byte(tile.ID))
	b = append(b, byte(r.X0>>24), byte(r.X0>>16), byte(r.X0>>8), byte(r.X0))
	b = append(b, byte(r.Y0>>24), byte(r.Y0>>16), byte(r.Y0>>8), byte(r.Y0))
	for y := r.Y0; y < r.Y1; y++ {
		for x := r.X0; x < r.X1; x++ {
			b = append(b, m.Get(x, y))
		}
	}
	return HashBytes(b)
}

func StrokeHash(s Stroke) string {
	return HashJSON(struct {
		Index            int
		ID               string
		Priority         int
		Start, End       Point
		WeightBPS        int
		HardnessPermille int
		CoverageParams   string
	}{s.Index, s.ID, s.Priority, s.Start, s.End, s.WeightBPS, s.HardnessPermille, ParameterVersion})
}

// ComponentCacheKey includes every required determinant plus the upstream key.
func ComponentCacheKey(tileInputHash, strokeID, strokeHash, orderVersion, paramVersion, upstreamKey string) string {
	return "ck-" + HashJSON(struct {
		TileInput    string
		StrokeID     string
		StrokeHash   string
		OrderVersion string
		ParamVersion string
		UpstreamKey  string
		Algorithm    string
	}{tileInputHash, strokeID, strokeHash, orderVersion, paramVersion, upstreamKey, AlgorithmVersion})[:20]
}

func AccumCacheKey(componentKey, previousAccumKey, orderVersion, paramVersion string) string {
	return "ak-" + HashJSON(struct {
		Component    string
		Previous     string
		OrderVersion string
		ParamVersion string
		Algorithm    string
	}{componentKey, previousAccumKey, orderVersion, paramVersion, AlgorithmVersion})[:20]
}

type DepNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

type DepEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type DepGraph struct {
	Nodes []DepNode `json:"nodes"`
	Edges []DepEdge `json:"edges"`
}

func (g *DepGraph) addNode(id, kind, label string) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return
		}
	}
	g.Nodes = append(g.Nodes, DepNode{ID: id, Kind: kind, Label: label})
}

func (g *DepGraph) addEdge(from, to, reason string) {
	g.Edges = append(g.Edges, DepEdge{From: from, To: to, Reason: reason})
}

// BuildDependencyGraph records explicit stroke/order/tile/verification dependencies.
func BuildDependencyGraph(p *Project, order *OrderDecision, tiles []Tile) *DepGraph {
	g := &DepGraph{}
	g.addNode("order", "order", "composition order "+order.OrderVersion)
	g.addNode("verify:topology", "verification", "connected topology verification")
	g.addNode("verify:direction", "verification", "boundary direction verification")
	g.addNode("report", "report", "diagnostic verification report")
	g.addEdge("verify:topology", "report", "topology result")
	g.addEdge("verify:direction", "report", "direction result")

	for _, s := range p.Strokes {
		sid := fmt.Sprintf("stroke:%d", s.Index)
		g.addNode(sid, "stroke", s.ID)
	}
	for _, t := range tiles {
		tid := fmt.Sprintf("tile:%d", t.ID)
		g.addNode(tid, "tile_input", fmt.Sprintf("tile %d input", t.ID))
		aid := fmt.Sprintf("accum:%d", t.ID)
		g.addNode(aid, "tile_output", fmt.Sprintf("tile %d accumulated mask", t.ID))
		g.addEdge(aid, "verify:topology", "tile pixels")
		g.addEdge(aid, "verify:direction", "tile boundary pixels")
		to, _ := order.Tile(t.ID)
		g.addEdge("order", aid, "order version")
		g.addEdge(tid, aid, "candidate tile input hash")
		var prev string
		for _, idx := range to.Order {
			cid := fmt.Sprintf("component:%d:%d", idx, t.ID)
			g.addNode(cid, "component", fmt.Sprintf("stroke %d on tile %d", idx, t.ID))
			sid := fmt.Sprintf("stroke:%d", idx)
			g.addEdge(sid, cid, "geometry/weight/hardness")
			g.addEdge(tid, cid, "tile input")
			g.addEdge("order", cid, "order version")
			if prev != "" {
				g.addEdge(prev, cid, "upstream cache key")
			}
			g.addEdge(cid, aid, "source-over alpha")
			prev = cid
		}
	}
	return g
}

// Propagate returns node IDs reachable from changed nodes through dependency edges.
func (g *DepGraph) Propagate(changed ...string) []string {
	adj := map[string][]string{}
	for _, e := range g.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	seen := map[string]bool{}
	queue := append([]string{}, changed...)
	for _, id := range changed {
		seen[id] = true
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, next := range adj[id] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

type InvalidationItem struct {
	StrokeIndex int    `json:"stroke_index"`
	TileID      int    `json:"tile_id"`
	Node        string `json:"node"`
	Reason      string `json:"reason"`
	OldHash     string `json:"old_hash,omitempty"`
	NewHash     string `json:"new_hash,omitempty"`
}

type InvalidationPlan struct {
	EditedStroke    int                `json:"edited_stroke"`
	ChangedFields   []string           `json:"changed_fields"`
	CandidateTiles  []int              `json:"candidate_tiles"`
	AffectedTiles   []int              `json:"affected_tiles"`
	RecomputedTiles []int              `json:"recomputed_tiles"`
	ReusedTiles     []int              `json:"reused_tiles"`
	Items           []InvalidationItem `json:"items"`
	Downstream      []string           `json:"downstream"`
}

// PlanInvalidation compares old/new stroke raster components to find precisely changed tiles.
func PlanInvalidation(oldProject, newProject *Project, editedStroke int, tiles []Tile) (*InvalidationPlan, error) {
	var oldStroke, newStroke *Stroke
	for i := range oldProject.Strokes {
		if oldProject.Strokes[i].Index == editedStroke {
			oldStroke = &oldProject.Strokes[i]
		}
	}
	for i := range newProject.Strokes {
		if newProject.Strokes[i].Index == editedStroke {
			newStroke = &newProject.Strokes[i]
		}
	}
	if oldStroke == nil || newStroke == nil {
		return nil, paramError("stroke_index", "existing stroke number", "edited stroke %d not found", editedStroke)
	}
	plan := &InvalidationPlan{EditedStroke: editedStroke}
	if oldStroke.Start != newStroke.Start || oldStroke.End != newStroke.End {
		plan.ChangedFields = append(plan.ChangedFields, "start/end")
	}
	if oldStroke.WeightBPS != newStroke.WeightBPS {
		plan.ChangedFields = append(plan.ChangedFields, "weight_basis_points")
	}
	if oldStroke.HardnessPermille != newStroke.HardnessPermille {
		plan.ChangedFields = append(plan.ChangedFields, "hardness_permille")
	}

	oldBounds := StrokeBounds(oldProject, *oldStroke)
	newBounds := StrokeBounds(newProject, *newStroke)
	for _, t := range tiles {
		intersects := !oldBounds.Intersect(t.Rect).Empty() || !newBounds.Intersect(t.Rect).Empty()
		if !intersects {
			continue
		}
		plan.CandidateTiles = append(plan.CandidateTiles, t.ID)
		oldComp, err := RasterStrokeTile(oldProject, *oldStroke, t)
		if err != nil {
			return nil, err
		}
		newComp, err := RasterStrokeTile(newProject, *newStroke, t)
		if err != nil {
			return nil, err
		}
		reason := "input_changed"
		if oldStroke.Start != newStroke.Start || oldStroke.End != newStroke.End {
			reason = "coordinate_changed"
		} else if oldStroke.WeightBPS != newStroke.WeightBPS {
			reason = "weight_changed"
		}
		if oldComp.ContentHash == newComp.ContentHash {
			continue
		}
		plan.AffectedTiles = append(plan.AffectedTiles, t.ID)
		plan.RecomputedTiles = append(plan.RecomputedTiles, t.ID)
		plan.Items = append(plan.Items, InvalidationItem{
			StrokeIndex: editedStroke, TileID: t.ID,
			Node:   fmt.Sprintf("component:%d:%d", editedStroke, t.ID),
			Reason: reason, OldHash: oldComp.ContentHash, NewHash: newComp.ContentHash,
		})
	}
	for _, t := range tiles {
		affected := false
		for _, id := range plan.AffectedTiles {
			if id == t.ID {
				affected = true
				break
			}
		}
		if !affected {
			plan.ReusedTiles = append(plan.ReusedTiles, t.ID)
		}
	}
	plan.Downstream = []string{"verify:topology", "verify:direction", "report"}
	return plan, nil
}
