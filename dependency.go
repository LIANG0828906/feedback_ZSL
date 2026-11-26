package maskqa

import (
	"fmt"
	"sort"
)

// DependencyEdge is a directed graph dependency.
type DependencyEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// GraphNode is one dependency graph vertex.
type GraphNode struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	DependsOn []string `json:"depends_on"`
}

// DependencyGraph records stroke/order/tile/verification dependencies.
type DependencyGraph struct {
	Nodes []GraphNode      `json:"nodes"`
	Edges []DependencyEdge `json:"edges"`
}

// InvalidationPlan is the precise edit propagation result.
type InvalidationPlan struct {
	StrokeID      string      `json:"stroke_id"`
	ChangedFields []string    `json:"changed_fields"`
	OldBounds     Rect        `json:"old_bounds"`
	NewBounds     Rect        `json:"new_bounds"`
	InvalidTiles  []TileCoord `json:"invalid_tiles"`
	ReusableTiles []TileCoord `json:"reusable_tiles"`
	InvalidNodes  []string    `json:"invalid_nodes"`
	Reasons       []string    `json:"reasons"`
}

// OrderedStroke includes the unique composition adjudication.
type OrderedStroke struct {
	Stroke
	OrderIndex int    `json:"order_index"`
	Reason     string `json:"reason"`
}

// OrderStrokes defines the only legal composition order:
// smaller process priority first, then stable stroke sequence.
func OrderStrokes(strokes []Stroke) []OrderedStroke {
	ordered := append([]Stroke(nil), strokes...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].Seq < ordered[j].Seq
	})
	out := make([]OrderedStroke, 0, len(ordered))
	for i, stroke := range ordered {
		reason := fmt.Sprintf("rule=%s priority=%d seq=%d index=%d", OrderVersion, stroke.Priority, stroke.Seq, i)
		if i > 0 {
			prev := ordered[i-1]
			if prev.Priority == stroke.Priority {
				reason = fmt.Sprintf("priority tie %d broken by seq %d<%d; %s", stroke.Priority, prev.Seq, stroke.Seq, reason)
			} else {
				reason = fmt.Sprintf("priority %d precedes %d; %s", prev.Priority, stroke.Priority, reason)
			}
		}
		out = append(out, OrderedStroke{Stroke: stroke, OrderIndex: i, Reason: reason})
	}
	return out
}

// BuildDependencyGraph builds the explicit stroke/order/tile/verify graph.
func BuildDependencyGraph(project *Project) *DependencyGraph {
	graph := &DependencyGraph{}
	addNode := func(id, kind string, deps ...string) {
		graph.Nodes = append(graph.Nodes, GraphNode{ID: id, Kind: kind, DependsOn: append([]string(nil), deps...)})
		for _, dep := range deps {
			graph.Edges = append(graph.Edges, DependencyEdge{From: dep, To: id, Reason: kind + "_dependency"})
		}
	}
	strokeIDs := make([]string, 0, len(project.Strokes))
	for _, stroke := range project.Strokes {
		id := "stroke:" + stroke.ID
		strokeIDs = append(strokeIDs, id)
		addNode(id, "stroke")
	}
	addNode("order:composition", "composition_order", strokeIDs...)

	size := project.tileSize()
	for _, tile := range Tiles(project.Width, project.Height, size) {
		tileID := fmt.Sprintf("tile:%s", tile.Coord.String())
		deps := []string{"order:composition"}
		for _, stroke := range project.Strokes {
			if !StrokeBounds(stroke).Intersect(tile.Rect).Empty() {
				deps = append(deps, "stroke:"+stroke.ID)
			}
		}
		addNode(tileID, "tile", deps...)
	}
	tileIDs := []string{}
	for _, tile := range Tiles(project.Width, project.Height, size) {
		tileIDs = append(tileIDs, fmt.Sprintf("tile:%s", tile.Coord.String()))
	}
	addNode("verify:topology", "topology_verification", tileIDs...)
	directionDeps := append(append([]string(nil), tileIDs...), "verify:topology")
	addNode("verify:direction", "direction_verification", directionDeps...)
	return graph
}

// ApplyEdit mutates the project and computes exact invalidation.
func ApplyEdit(project *Project, edit Edit) (*InvalidationPlan, error) {
	if project == nil {
		return nil, decodeError("project", "nil project")
	}
	idx := -1
	for i := range project.Strokes {
		if project.Strokes[i].ID == edit.StrokeID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, decodeError("edit.stroke_id", "stroke %q not found", edit.StrokeID)
	}
	stroke := &project.Strokes[idx]
	oldBounds := StrokeBounds(*stroke)
	changed := []string{}
	reasons := []string{}

	if edit.Start != nil {
		stroke.Start = *edit.Start
		changed = append(changed, "start")
		reasons = append(reasons, "start coordinate changed; old and new stroke bounds union invalidated")
	}
	if edit.End != nil {
		stroke.End = *edit.End
		changed = append(changed, "end")
		reasons = append(reasons, "end coordinate changed; old and new stroke bounds union invalidated")
	}
	if edit.WeightPermyriad != nil {
		stroke.WeightPermyriad = *edit.WeightPermyriad
		changed = append(changed, "weight_permyriad")
		reasons = append(reasons, "coverage weight changed; intersecting tile cache keys invalidated")
	}
	if edit.Priority != nil {
		stroke.Priority = *edit.Priority
		changed = append(changed, "priority")
		reasons = append(reasons, "composition priority changed; order-sensitive intersecting tiles invalidated")
	}
	if len(changed) == 0 {
		return nil, decodeError("edit", "at least one editable field must be provided")
	}
	if err := stroke.ValidateParameters(fmt.Sprintf("strokes[%d]", idx)); err != nil {
		return nil, err
	}
	newBounds := StrokeBounds(*stroke)
	union := oldBounds
	if union.MinX > newBounds.MinX {
		union.MinX = newBounds.MinX
	}
	if union.MinY > newBounds.MinY {
		union.MinY = newBounds.MinY
	}
	if union.MaxX < newBounds.MaxX {
		union.MaxX = newBounds.MaxX
	}
	if union.MaxY < newBounds.MaxY {
		union.MaxY = newBounds.MaxY
	}
	invalid := TilesForRect(union, project.Width, project.Height, project.tileSize())
	invalidSet := map[TileCoord]bool{}
	for _, coord := range invalid {
		invalidSet[coord] = true
	}
	reusable := []TileCoord{}
	for _, tile := range Tiles(project.Width, project.Height, project.tileSize()) {
		if !invalidSet[tile.Coord] {
			reusable = append(reusable, tile.Coord)
		}
	}
	invalidNodes := []string{"verify:topology", "verify:direction", "order:composition"}
	for _, coord := range invalid {
		invalidNodes = append(invalidNodes, "tile:"+coord.String())
	}
	return &InvalidationPlan{
		StrokeID: edit.StrokeID, ChangedFields: changed,
		OldBounds: oldBounds, NewBounds: newBounds,
		InvalidTiles: invalid, ReusableTiles: reusable,
		InvalidNodes: invalidNodes, Reasons: reasons,
	}, nil
}
