package dependency

import (
	"fmt"
	"sort"
	"strings"

	"wardrobe/domain"
)

const (
	KindMount    = "mount"
	KindPart     = "part"
	KindMaterial = "material"
	KindBudget   = "budget"
	KindBatch    = "batch"
	KindSubmesh  = "submesh"
)

type Node struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Build records the explicit dependency edges used by invalidation and audit:
// mounts own parts, materials belong to parts, parts belong to batches, batches
// consume budgets, batches emit submesh offsets, and batch cache keys chain
// downstream in commit order.
func Build(project domain.Project, result domain.CaseResult) Graph {
	nodes := make(map[string]Node)
	edges := make(map[string]Edge)
	addNode := func(node Node) {
		nodes[node.ID] = node
	}
	addEdge := func(from, to, kind string) {
		key := from + "\x00" + to + "\x00" + kind
		edges[key] = Edge{From: from, To: to, Kind: kind}
	}

	materialMap := project.MaterialMap()
	for _, mount := range project.Mounts {
		addNode(Node{ID: "mount:" + mount.ID, Kind: KindMount, Label: mount.Name})
	}
	for _, material := range project.Materials {
		addNode(Node{
			ID:    fmt.Sprintf("material:%s@v%d", material.ID, material.Version),
			Kind:  KindMaterial,
			Label: fmt.Sprintf("%s v%d", material.Name, material.Version),
		})
	}
	for _, part := range project.Parts {
		partNode := "part:" + part.ID
		addNode(Node{ID: partNode, Kind: KindPart, Label: part.Name})
		addEdge("mount:"+part.MountID, partNode, "mount_occupancy")
		for _, submesh := range part.Mesh.Submeshes {
			material := materialMap[submesh.MaterialSlot]
			materialNode := fmt.Sprintf("material:%s@v%d", material.ID, material.Version)
			addEdge(materialNode, partNode, "material_assignment")
		}
	}
	for _, batch := range result.Batches {
		batchNode := "batch:" + batch.ID
		addNode(Node{ID: batchNode, Kind: KindBatch, Label: batch.ID})
		for _, partID := range batch.PartIDs {
			addEdge("part:"+partID, batchNode, "batch_membership")
		}
		vertexNode := fmt.Sprintf("budget:%s:vertices@used=%d,max=%d,remaining=%d",
			batch.ID, batch.VertexUsed, project.Constraints.MaxVertices, project.Constraints.MaxVertices-batch.VertexUsed)
		indexNode := fmt.Sprintf("budget:%s:indices@used=%d,max=%d,remaining=%d",
			batch.ID, batch.IndexUsed, project.Constraints.MaxIndices, project.Constraints.MaxIndices-batch.IndexUsed)
		slotNode := fmt.Sprintf("budget:%s:materialSlots@used=%d,max=%d,remaining=%d",
			batch.ID, len(batch.MaterialSlots), project.Constraints.MaxMaterialSlots,
			project.Constraints.MaxMaterialSlots-len(batch.MaterialSlots))
		for _, node := range []Node{
			{ID: vertexNode, Kind: KindBudget, Label: "vertex budget"},
			{ID: indexNode, Kind: KindBudget, Label: "index budget"},
			{ID: slotNode, Kind: KindBudget, Label: "material slot budget"},
		} {
			addNode(node)
			addEdge(batchNode, node.ID, "budget_usage")
		}
		for _, placement := range batch.Parts {
			for _, submesh := range placement.Submeshes {
				nodeID := fmt.Sprintf("submesh:%s:%s@vertex=%d,index=%d",
					batch.ID, submesh.SubmeshID, submesh.VertexOffset, submesh.IndexOffset)
				addNode(Node{ID: nodeID, Kind: KindSubmesh, Label: submesh.SubmeshID})
				addEdge(batchNode, nodeID, "submesh_offset")
			}
		}
	}
	for index := 1; index < len(result.Batches); index++ {
		previous := "batch:" + result.Batches[index-1].ID
		current := "batch:" + result.Batches[index].ID
		addEdge(previous, current, "upstream_cache_key")
	}

	nodeList := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		nodeList = append(nodeList, node)
	}
	sort.Slice(nodeList, func(i, j int) bool { return nodeList[i].ID < nodeList[j].ID })
	edgeList := make([]Edge, 0, len(edges))
	for _, edge := range edges {
		edgeList = append(edgeList, edge)
	}
	sort.Slice(edgeList, func(i, j int) bool {
		if edgeList[i].From != edgeList[j].From {
			return edgeList[i].From < edgeList[j].From
		}
		if edgeList[i].To != edgeList[j].To {
			return edgeList[i].To < edgeList[j].To
		}
		return edgeList[i].Kind < edgeList[j].Kind
	})
	return Graph{Nodes: nodeList, Edges: edgeList}
}

// AffectedBatches returns directly touched batches and all downstream batches
// reachable through the cache-key chain.
func AffectedBatches(graph Graph, changedPartIDs, changedMaterialSlots []string) []string {
	adjacency := make(map[string][]string)
	for _, edge := range graph.Edges {
		if edge.Kind == "upstream_cache_key" || edge.Kind == "material_assignment" ||
			edge.Kind == "mount_occupancy" || edge.Kind == "batch_membership" {
			adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		}
	}
	start := make([]string, 0, len(changedPartIDs)+len(changedMaterialSlots))
	for _, partID := range changedPartIDs {
		start = append(start, "part:"+partID)
	}
	for _, slot := range changedMaterialSlots {
		for _, node := range graph.Nodes {
			if node.Kind == KindMaterial && strings.HasPrefix(node.ID, "material:"+slot+"@") {
				start = append(start, node.ID)
			}
		}
	}
	queue := append([]string(nil), start...)
	visited := make(map[string]bool)
	batches := make(map[string]bool)
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if visited[node] {
			continue
		}
		visited[node] = true
		if len(node) > len("batch:") && node[:len("batch:")] == "batch:" {
			batches[node[len("batch:"):]] = true
		}
		queue = append(queue, adjacency[node]...)
	}
	out := make([]string, 0, len(batches))
	for batchID := range batches {
		out = append(out, batchID)
	}
	sort.Strings(out)
	return out
}
