package engine

import (
	"fmt"

	"maskqc/model"
)

func (e *Engine) buildGraph(result *RunResult) DependencyGraph {
	graph := DependencyGraph{}
	nodes := map[string]GraphNode{}
	edges := make([]GraphEdge, 0)
	addNode := func(node GraphNode) {
		if _, ok := nodes[node.ID]; !ok {
			nodes[node.ID] = node
		}
	}
	addEdge := func(from, to, reason string) {
		edges = append(edges, GraphEdge{From: from, To: to, Reason: reason})
	}
	orderID := "order:composite"
	addNode(GraphNode{ID: orderID, Kind: "order", ObjectID: e.Project.ID, CacheKey: result.OrderVersion})
	for _, stroke := range model.OrderedStrokes(e.Project.Strokes) {
		strokeID := "stroke:" + stroke.ID
		conversionID := "conversion:" + stroke.ID
		addNode(GraphNode{ID: strokeID, Kind: "stroke", ObjectID: stroke.ID, CacheKey: stroke.Signature()})
		addNode(GraphNode{ID: conversionID, Kind: "conversion", ObjectID: stroke.ID, CacheKey: e.config.ResidualDigest})
		addEdge(conversionID, strokeID, "integer conversion residuals feed canonical stroke")
	}
	for _, block := range result.Blocks {
		blockID := fmt.Sprintf("block:%06d", block.Index)
		addNode(GraphNode{ID: blockID, Kind: "block", ObjectID: fmt.Sprint(block.Index), CacheKey: block.CacheKey})
		addEdge(orderID, blockID, "block uses deterministic composite order")
		for _, participant := range block.Participants {
			addEdge("stroke:"+participant.StrokeID, blockID, participant.Basis)
		}
	}
	topologyID := "verify:topology"
	topologyKey := ""
	if result.Analysis != nil {
		topologyKey = result.Analysis.MaskHash
	}
	addNode(GraphNode{ID: topologyID, Kind: "topology", ObjectID: e.Project.ID, CacheKey: topologyKey})
	for _, block := range result.Blocks {
		addEdge(fmt.Sprintf("block:%06d", block.Index), topologyID, "block contributes pixels to connected topology")
	}
	directionID := "verify:direction"
	directionKey := model.HashStrings(topologyKey, "direction")
	if result.Analysis != nil {
		parts := make([]string, 0, len(result.Analysis.Domains))
		for _, domain := range result.Analysis.Domains {
			parts = append(parts, domain.ID, string(domain.Direction))
		}
		directionKey = model.HashStrings(parts...)
	}
	addNode(GraphNode{ID: directionID, Kind: "direction", ObjectID: e.Project.ID, CacheKey: directionKey})
	addEdge(topologyID, directionID, "boundary segments of topology feed direction consistency")
	nodeList := make([]GraphNode, 0, len(nodes))
	for _, node := range nodes {
		nodeList = append(nodeList, node)
	}
	sortGraphNodes(nodeList)
	sortGraphEdges(edges)
	graph.Nodes = nodeList
	graph.Edges = edges
	return graph
}

func sortGraphNodes(nodes []GraphNode) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && nodes[j].ID < nodes[j-1].ID; j-- {
			nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
		}
	}
}

func sortGraphEdges(edges []GraphEdge) {
	for i := 1; i < len(edges); i++ {
		for j := i; j > 0; j-- {
			left, right := edges[j-1], edges[j]
			if left.From < right.From || (left.From == right.From && left.To <= right.To) {
				break
			}
			edges[j], edges[j-1] = edges[j-1], edges[j]
		}
	}
}
