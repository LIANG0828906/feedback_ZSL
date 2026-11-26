package strokelab

import "fmt"

type DependencyNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

type DependencyEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type DependencyGraph struct {
	Nodes []DependencyNode `json:"nodes"`
	Edges []DependencyEdge `json:"edges"`
}

// BuildDependencyGraph materializes the dependencies used by invalidation:
// strokes feed the global order, participating strokes and local order feed
// blocks, blocks feed topology, and topology/attribution feed diagnostics.
func BuildDependencyGraph(p *Project, ordered []OrderedStroke, blocks []Block) *DependencyGraph {
	g := &DependencyGraph{}
	addNode := func(id, kind, label string) {
		g.Nodes = append(g.Nodes, DependencyNode{ID: id, Kind: kind, Label: label})
	}
	addEdge := func(from, to, reason string) {
		g.Edges = append(g.Edges, DependencyEdge{From: from, To: to, Reason: reason})
	}
	addNode("order:global", "order", "canonical stage/seq order")
	addEdge("order:global", "topology:verify", "ordered_composite")
	for _, os := range ordered {
		id := "stroke:" + os.Stroke.ID
		addNode(id, "stroke", fmt.Sprintf("stage=%d seq=%d", os.Stroke.Stage, os.Stroke.Seq))
		addEdge(id, "order:global", "declared_order_key")
	}
	for _, b := range blocks {
		blockID := fmt.Sprintf("block:%d", b.ID)
		addNode(blockID, "block", fmt.Sprintf("%d,%d %dx%d", b.Rect.X, b.Rect.Y, b.Rect.W, b.Rect.H))
		parts := participantsForBlock(ordered, b.Rect)
		for _, os := range parts {
			addEdge("stroke:"+os.Stroke.ID, blockID, "participates_in_block")
		}
		addEdge("order:global", blockID, "local_order_version")
		addEdge(blockID, "topology:verify", "block_composite")
	}
	addNode("topology:verify", "topology", "domains and tolerances")
	addNode("attribution:recompute", "attribution", "per-pixel responsibility")
	addNode("diagnostic:refresh", "diagnostic", "HTML/JSON evidence")
	addEdge("topology:verify", "attribution:recompute", "difference_pixels")
	addEdge("topology:verify", "diagnostic:refresh", "topology_report")
	addEdge("attribution:recompute", "diagnostic:refresh", "attribution_report")
	return g
}
