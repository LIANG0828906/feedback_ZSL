package lipsync

import (
	"fmt"
	"sort"
)

type NodeKind string

const (
	NodeBoundary      NodeKind = "phoneme_boundary"
	NodeWeight        NodeKind = "phoneme_weight"
	NodeAffinity      NodeKind = "transition_affinity"
	NodePack          NodeKind = "mouth_pack"
	NodeDialogueFrame NodeKind = "dialogue_frame"
	NodeDispatchFrame NodeKind = "dispatch_frame"
)

type Node struct {
	ID         string   `json:"id"`
	Kind       NodeKind `json:"kind"`
	DialogueID string   `json:"dialogueId,omitempty"`
	ShardIndex int      `json:"shardIndex,omitempty"`
	Frame      int      `json:"frame,omitempty"`
	Label      string   `json:"label,omitempty"`
}

type Edge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type DependencyGraph struct {
	ProjectID  string              `json:"projectId"`
	Nodes      map[string]Node     `json:"nodes"`
	Edges      []Edge              `json:"edges"`
	Dependents map[string][]string `json:"dependents"`
}

func NewDependencyGraph(projectID string) *DependencyGraph {
	return &DependencyGraph{
		ProjectID:  projectID,
		Nodes:      map[string]Node{},
		Edges:      []Edge{},
		Dependents: map[string][]string{},
	}
}

func (g *DependencyGraph) addNode(node Node) string {
	if _, exists := g.Nodes[node.ID]; !exists {
		g.Nodes[node.ID] = node
	}
	return node.ID
}

func (g *DependencyGraph) addEdge(from, to, reason string) {
	for _, edge := range g.Edges {
		if edge.From == from && edge.To == to && edge.Reason == reason {
			return
		}
	}
	g.Edges = append(g.Edges, Edge{From: from, To: to, Reason: reason})
	g.Dependents[from] = append(g.Dependents[from], to)
}

func BoundaryNodeID(dialogueID string, phonemeIndex int) string {
	return fmt.Sprintf("dialogue:%s:phoneme:%d:boundary", dialogueID, phonemeIndex)
}

func WeightNodeID(dialogueID string, phonemeIndex int) string {
	return fmt.Sprintf("dialogue:%s:phoneme:%d:weight", dialogueID, phonemeIndex)
}

func AffinityNodeID(dialogueID string, boundaryIndex int) string {
	return fmt.Sprintf("dialogue:%s:affinity:%d", dialogueID, boundaryIndex)
}

func PackNodeID(dialogueID string, packIndex int) string {
	return fmt.Sprintf("dialogue:%s:pack:%d", dialogueID, packIndex)
}

func DialogueFrameNodeID(dialogueID string, frame int) string {
	return fmt.Sprintf("dialogue:%s:frame:%d", dialogueID, frame)
}

func DispatchFrameNodeID(frame int) string {
	return fmt.Sprintf("dispatch:frame:%d", frame)
}

func BuildDependencyGraph(result *FullResult) *DependencyGraph {
	graph := NewDependencyGraph(result.ProjectID)
	for _, dr := range result.Dialogues {
		d := dr.Dialogue
		phonemePack := map[int]int{}
		idToIndex := map[string]int{}
		for i, qp := range dr.Quantized {
			idToIndex[qp.ID] = i
			boundaryID := graph.addNode(Node{
				ID: BoundaryNodeID(d.ID, i), Kind: NodeBoundary,
				DialogueID: d.ID, ShardIndex: d.ShardIndex, Label: qp.ID,
			})
			weightID := graph.addNode(Node{
				ID: WeightNodeID(d.ID, i), Kind: NodeWeight,
				DialogueID: d.ID, ShardIndex: d.ShardIndex, Label: qp.ID,
			})
			_ = boundaryID
			_ = weightID
		}

		for _, pack := range dr.Packs {
			packID := graph.addNode(Node{
				ID: PackNodeID(d.ID, pack.Index), Kind: NodePack,
				DialogueID: d.ID, ShardIndex: d.ShardIndex, Label: pack.PackID,
			})
			for _, phonemeID := range pack.PhonemeIDs {
				pi := idToIndex[phonemeID]
				phonemePack[pi] = pack.Index
				graph.addEdge(BoundaryNodeID(d.ID, pi), packID, "quantized_boundary_feeds_pack_span")
				graph.addEdge(WeightNodeID(d.ID, pi), packID, "weight_feeds_pack_payload_and_dominant_shape")
			}
			for frame := pack.StartFrame; frame < pack.EndFrame; frame++ {
				frameID := graph.addNode(Node{
					ID: DialogueFrameNodeID(d.ID, frame), Kind: NodeDialogueFrame,
					DialogueID: d.ID, ShardIndex: d.ShardIndex, Frame: frame,
				})
				graph.addEdge(packID, frameID, "pack_attributes_frame")
			}
		}

		for i := 0; i < len(d.Phonemes)-1; i++ {
			affinityID := graph.addNode(Node{
				ID: AffinityNodeID(d.ID, i), Kind: NodeAffinity,
				DialogueID: d.ID, ShardIndex: d.ShardIndex, Label: fmt.Sprintf("boundary-%d", i),
			})
			leftPack, leftOK := phonemePack[i]
			rightPack, rightOK := phonemePack[i+1]
			if leftOK {
				graph.addEdge(affinityID, PackNodeID(d.ID, leftPack), "affinity_influences_merge")
			}
			if rightOK && (!leftOK || rightPack != leftPack) {
				graph.addEdge(affinityID, PackNodeID(d.ID, rightPack), "affinity_influences_merge")
			}
		}
	}

	if result.Dispatch != nil {
		for _, assignment := range result.Dispatch.Assignments {
			packID := PackNodeID(assignment.DialogueID, assignment.PackIndex)
			dispatchNode := graph.addNode(Node{
				ID: DispatchFrameNodeID(assignment.Frame), Kind: NodeDispatchFrame,
				Frame: assignment.Frame, Label: "dispatch_budget_decision",
			})
			graph.addEdge(packID, dispatchNode, "pack_dispatch_action")
			for frame := assignment.ActiveFrom; frame < assignment.ActiveTo; frame++ {
				activeNode := graph.addNode(Node{
					ID: DispatchFrameNodeID(frame), Kind: NodeDispatchFrame,
					Frame: frame, Label: "channel_active_state",
				})
				graph.addEdge(packID, activeNode, "pack_occupies_channel")
			}
		}
		for _, deferral := range result.Dispatch.Deferrals {
			packID := PackNodeID(deferral.DialogueID, deferral.PackIndex)
			frameNode := graph.addNode(Node{
				ID: DispatchFrameNodeID(deferral.Frame), Kind: NodeDispatchFrame,
				Frame: deferral.Frame, Label: "deferral_decision",
			})
			graph.addEdge(packID, frameNode, "pack_deferral_record")
		}
		for _, rejection := range result.Dispatch.Rejections {
			packID := PackNodeID(rejection.DialogueID, rejection.PackIndex)
			frameNode := graph.addNode(Node{
				ID: DispatchFrameNodeID(rejection.LastFrame), Kind: NodeDispatchFrame,
				Frame: rejection.LastFrame, Label: "rejection_record",
			})
			graph.addEdge(packID, frameNode, "pack_rejection_record")
		}
	}

	for _, ids := range graph.Dependents {
		sort.Strings(ids)
	}
	edges := graph.Edges
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].Reason < edges[j].Reason
	})
	graph.Edges = edges
	return graph
}

func (g *DependencyGraph) Propagate(startNodeIDs ...string) []string {
	seen := map[string]bool{}
	queue := append([]string(nil), startNodeIDs...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, g.Dependents[id]...)
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func TopologicalNodeOrder(graph *DependencyGraph, nodeIDs []string) ([]string, error) {
	selected := map[string]bool{}
	for _, id := range nodeIDs {
		selected[id] = true
	}
	indegree := map[string]int{}
	adjacency := map[string][]string{}
	for _, edge := range graph.Edges {
		if !selected[edge.From] || !selected[edge.To] {
			continue
		}
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		indegree[edge.To]++
		if _, ok := indegree[edge.From]; !ok {
			indegree[edge.From] = 0
		}
	}
	ready := make([]string, 0)
	for id := range selected {
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(selected))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := append([]string(nil), adjacency[id]...)
		sort.Strings(next)
		for _, to := range next {
			indegree[to]--
			if indegree[to] == 0 {
				ready = append(ready, to)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(selected) {
		return nil, NewError(ECStage, "", 0, false, "dependencyGraph", "dependency cycle detected")
	}
	return order, nil
}
