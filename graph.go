package maskqc

import (
	"fmt"
	"sort"
)

type GraphNode struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Label    string         `json:"label"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type DependencyGraph struct {
	Nodes map[string]*GraphNode `json:"nodes"`
	Edges []GraphEdge           `json:"edges"`
	down  map[string]map[string]string
}

type InvalidationItem struct {
	NodeID string `json:"nodeId"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

type InvalidationPlan struct {
	ChangedStrokeIDs []string           `json:"changedStrokeIds"`
	TileIDs          []string           `json:"tileIds"`
	Items            []InvalidationItem `json:"items"`
	RecomputeTiles   []string           `json:"recomputeTiles"`
	Graph            *DependencyGraph   `json:"graph,omitempty"`
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{
		Nodes: map[string]*GraphNode{},
		down:  map[string]map[string]string{},
	}
}

func (g *DependencyGraph) AddNode(id, kind, label string, meta map[string]any) {
	if _, ok := g.Nodes[id]; !ok {
		g.Nodes[id] = &GraphNode{ID: id, Kind: kind, Label: label, Metadata: meta}
	}
}

func (g *DependencyGraph) AddEdge(from, to, kind string) {
	g.AddNode(from, "unknown", from, nil)
	g.AddNode(to, "unknown", to, nil)
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.Kind == kind {
			return
		}
	}
	g.Edges = append(g.Edges, GraphEdge{From: from, To: to, Kind: kind})
	if g.down[from] == nil {
		g.down[from] = map[string]string{}
	}
	g.down[from][to] = kind
}

func (g *DependencyGraph) SortedNodes() []GraphNode {
	out := make([]GraphNode, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (g *DependencyGraph) AffectedTilesForStroke(strokeID string) []string {
	var tiles []string
	seen := map[string]bool{}
	for _, e := range g.Edges {
		if e.From == strokeID && e.Kind == "spatial" {
			if !seen[e.To] {
				seen[e.To] = true
				tiles = append(tiles, e.To)
			}
		}
	}
	sort.Strings(tiles)
	return tiles
}

func BuildDependencyGraph(p *Project) *DependencyGraph {
	g := NewDependencyGraph()
	g.AddNode("topology", "verification", "topology verification", map[string]any{"version": AlgorithmVersion})
	g.AddNode("direction", "verification", "direction verification", map[string]any{"version": AlgorithmVersion})
	g.AddNode("report", "report", "diagnostic report", nil)
	g.AddEdge("topology", "report", "feeds")
	g.AddEdge("direction", "report", "feeds")

	tiles := Tiles(p.Width, p.Height, DefaultTileSize)
	for _, tile := range tiles {
		tileNode := "tile:" + tile.ID
		orderNode := "order:" + tile.ID
		g.AddNode(tileNode, "tile", "tile "+tile.ID, map[string]any{"rect": tile.Rect, "pixels": tile.Pixels})
		g.AddNode(orderNode, "order", "order decision "+tile.ID, map[string]any{"version": OrderVersion})
		g.AddEdge(orderNode, tileNode, "adjudicates")
		g.AddEdge(tileNode, "topology", "pixels")
		g.AddEdge(tileNode, "direction", "boundary")

		decision := AdjudicateTile(p.Strokes, tile)
		for _, participant := range decision.Participants {
			strokeNode := "stroke:" + participant.StrokeID
			compNode := "component:" + tile.ID + ":" + participant.StrokeID
			g.AddNode(strokeNode, "stroke", "stroke "+participant.StrokeID, map[string]any{
				"seq": participant.Seq, "priority": participant.Priority,
			})
			g.AddNode(compNode, "component", "coverage "+participant.StrokeID+"@"+tile.ID, nil)
			g.AddEdge(strokeNode, compNode, "rasterizes")
			g.AddEdge(compNode, tileNode, "alpha-over")
			g.AddEdge(strokeNode, tileNode, "spatial")
			g.AddEdge(orderNode, compNode, "positions")
		}
	}
	return g
}

type EditRequest struct {
	Type     string `json:"type"` // update, add, delete
	Stroke   Stroke `json:"stroke,omitempty"`
	StrokeID string `json:"strokeId,omitempty"`
	Start    *Point `json:"start,omitempty"`
	End      *Point `json:"end,omitempty"`
}

func PlanEdit(p *Project, req EditRequest) (*InvalidationPlan, error) {
	plan := &InvalidationPlan{}
	oldTiles := map[string]bool{}
	newTiles := map[string]bool{}

	switch req.Type {
	case "update":
		id := req.StrokeID
		if id == "" {
			id = req.Stroke.ID
		}
		idx := findStrokeIndex(p.Strokes, id)
		if idx < 0 {
			return nil, schemaError("$.strokeId", "stroke %q not found", id)
		}
		plan.ChangedStrokeIDs = []string{id}
		old := p.Strokes[idx]
		collectBoundsTiles(old.Bounds(), p.Width, p.Height, oldTiles)
		next := old
		if req.Start != nil {
			next.Start = *req.Start
		}
		if req.End != nil {
			next.End = *req.End
		}
		if req.Stroke.WeightBP != 0 {
			next.WeightBP = req.Stroke.WeightBP
		}
		if req.Stroke.HardnessPermille != 0 {
			next.HardnessPermille = req.Stroke.HardnessPermille
		}
		if req.Stroke.Radius != 0 {
			next.Radius = req.Stroke.Radius
		}
		if err := ValidateStroke(next); err != nil {
			return nil, err
		}
		collectBoundsTiles(next.Bounds(), p.Width, p.Height, newTiles)
		p.Strokes[idx] = next
	case "add":
		if req.Stroke.ID == "" {
			return nil, schemaError("$.stroke.id", "new stroke id is required")
		}
		if findStrokeIndex(p.Strokes, req.Stroke.ID) >= 0 {
			return nil, schemaError("$.stroke.id", "stroke %q already exists", req.Stroke.ID)
		}
		if req.Stroke.Seq == 0 {
			req.Stroke.Seq = len(p.Strokes) + 1
		}
		if req.Stroke.Priority == 0 {
			req.Stroke.Priority = 10
		}
		if err := ValidateStroke(req.Stroke); err != nil {
			return nil, err
		}
		plan.ChangedStrokeIDs = []string{req.Stroke.ID}
		collectBoundsTiles(req.Stroke.Bounds(), p.Width, p.Height, newTiles)
		p.Strokes = append(p.Strokes, req.Stroke)
	case "delete":
		id := req.StrokeID
		if id == "" {
			id = req.Stroke.ID
		}
		idx := findStrokeIndex(p.Strokes, id)
		if idx < 0 {
			return nil, schemaError("$.strokeId", "stroke %q not found", id)
		}
		plan.ChangedStrokeIDs = []string{id}
		collectBoundsTiles(p.Strokes[idx].Bounds(), p.Width, p.Height, oldTiles)
		p.Strokes = append(p.Strokes[:idx], p.Strokes[idx+1:]...)
	default:
		return nil, schemaError("$.type", "edit type must be update, add, or delete")
	}

	all := map[string]bool{}
	for id := range oldTiles {
		all[id] = true
		plan.Items = append(plan.Items, InvalidationItem{NodeID: "tile:" + id, Kind: "tile", Reason: "old stroke bounds intersect tile"})
	}
	for id := range newTiles {
		if !all[id] {
			plan.Items = append(plan.Items, InvalidationItem{NodeID: "tile:" + id, Kind: "tile", Reason: "new stroke bounds intersect tile"})
		}
		all[id] = true
	}
	for _, strokeID := range plan.ChangedStrokeIDs {
		plan.Items = append(plan.Items, InvalidationItem{NodeID: "stroke:" + strokeID, Kind: "stroke", Reason: "stroke declaration changed"})
		plan.Items = append(plan.Items, InvalidationItem{NodeID: "component:*:" + strokeID, Kind: "component-prefix", Reason: "stroke raster content may change"})
	}
	if len(all) > 0 {
		plan.Items = append(plan.Items,
			InvalidationItem{NodeID: "topology", Kind: "verification", Reason: "tile pixels changed"},
			InvalidationItem{NodeID: "direction", Kind: "verification", Reason: "boundary pixels changed"},
		)
	}
	for id := range all {
		plan.TileIDs = append(plan.TileIDs, id)
		plan.RecomputeTiles = append(plan.RecomputeTiles, id)
	}
	sort.Strings(plan.TileIDs)
	sort.Strings(plan.RecomputeTiles)
	sort.Slice(plan.Items, func(i, j int) bool { return plan.Items[i].NodeID < plan.Items[j].NodeID })
	plan.Graph = BuildDependencyGraph(p)
	return plan, nil
}

func findStrokeIndex(strokes []Stroke, id string) int {
	for i := range strokes {
		if strokes[i].ID == id {
			return i
		}
	}
	return -1
}

func collectBoundsTiles(b BBox, width, height int, into map[string]bool) {
	for _, tile := range Tiles(width, height, DefaultTileSize) {
		if _, ok := b.Intersect(tile.Rect); ok {
			into[tile.ID] = true
		}
	}
}

func (p *InvalidationPlan) Summary() string {
	return fmt.Sprintf("changed=%v recomputeTiles=%v", p.ChangedStrokeIDs, p.RecomputeTiles)
}
