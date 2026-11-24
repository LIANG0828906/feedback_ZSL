package dependency

import (
	"sort"

	"storyboardstudio/internal/model"
)

type EditKind string

const (
	EditPreconditions EditKind = "preconditions"
	EditEffects       EditKind = "effects"
	EditDuration      EditKind = "duration_ms"
	EditAspect        EditKind = "aspect_path"
)

type Edit struct {
	NodeID     string   `json:"node_id"`
	Kind       EditKind `json:"kind"`
	LocalIndex int      `json:"local_index,omitempty"`
}

type Edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Type  string `json:"type"`
	Field string `json:"field,omitempty"`
}

type Invalidation struct {
	Kind       string `json:"kind"`
	NodeID     string `json:"node_id"`
	ShotID     string `json:"shot_id,omitempty"`
	ShotIndex  int    `json:"shot_index"`
	LocalIndex int    `json:"local_index,omitempty"`
	Reason     string `json:"reason"`
	Dependency string `json:"dependency,omitempty"`
}

type Plan struct {
	Edit             Edit           `json:"edit"`
	ReplayNodes      []string       `json:"replay_nodes"`
	InvalidShotIndex map[int]bool   `json:"invalid_shot_index"`
	Invalidations    []Invalidation `json:"invalidations"`
	CacheableShots   []int          `json:"cacheable_shots"`
}

type Graph struct {
	project    *model.Project
	byID       map[string]*model.Node
	downstream map[string][]Edge
	edges      []Edge
}

func Build(project *model.Project) *Graph {
	graph := &Graph{
		project:    project,
		byID:       engineNodeMap(project.Nodes),
		downstream: map[string][]Edge{},
	}
	for _, node := range project.Nodes {
		for _, parentID := range node.Parents {
			graph.addEdge(Edge{From: parentID, To: node.ID, Type: "causal", Field: "parents"})
		}
		if node.Convergence != nil {
			for _, predID := range node.Convergence.Predecessors {
				graph.addEdge(Edge{From: predID, To: node.ID, Type: "convergence", Field: "convergence"})
			}
		}
		for _, otherID := range node.MutexWith {
			graph.addEdge(Edge{From: node.ID, To: otherID, Type: "mutex", Field: "mutex_with"})
		}
	}

	producerByFlag := map[string][]string{}
	for _, node := range project.Nodes {
		flags := append(append([]string{}, node.OnArrive.SetFlags...), node.OnArrive.ClearFlags...)
		for _, option := range node.Options {
			flags = append(flags, option.Effects.SetFlags...)
			flags = append(flags, option.Effects.ClearFlags...)
		}
		for _, flag := range uniqueSorted(flags) {
			producerByFlag[flag] = append(producerByFlag[flag], node.ID)
			graph.addEdge(Edge{From: node.ID, To: "flag:" + flag, Type: "effect", Field: "effects." + flag})
		}
	}
	for _, node := range project.Nodes {
		consumed := append(append(append([]string{}, node.Preconditions.All...), node.Preconditions.Any...), node.Preconditions.None...)
		for _, flag := range uniqueSorted(consumed) {
			for _, producerID := range producerByFlag[flag] {
				if producerID == node.ID {
					continue
				}
				graph.addEdge(Edge{From: producerID, To: node.ID, Type: "flag", Field: "preconditions." + flag})
			}
			graph.addEdge(Edge{From: "flag:" + flag, To: node.ID, Type: "precondition", Field: "preconditions." + flag})
		}
	}

	for _, node := range project.Nodes {
		for i, shot := range node.Shots {
			shotKey := shotRef(node.ID, i)
			graph.addEdge(Edge{From: node.ID + ".duration_ms", To: shotKey, Type: "fixed-point-duration", Field: "duration_ms"})
			graph.addEdge(Edge{From: shotKey, To: "presentation:" + shot.ID, Type: "presents", Field: "samples"})
		}
	}
	return graph
}

func (g *Graph) Edges() []Edge {
	return append([]Edge(nil), g.edges...)
}

func (g *Graph) PlanEdit(edit Edit, presented []model.PresentedShot) (*Plan, error) {
	node, ok := g.byID[edit.NodeID]
	if !ok {
		return nil, &model.Error{Code: model.CodeRef, ConflictNode: edit.NodeID, Message: "cannot plan edit for unknown node"}
	}
	plan := &Plan{Edit: edit, InvalidShotIndex: map[int]bool{}}

	if edit.Kind == EditDuration {
		if edit.LocalIndex < 0 || edit.LocalIndex >= len(node.Shots) {
			return nil, &model.Error{Code: model.CodeRef, ConflictNode: edit.NodeID,
				Message: "duration edit references unknown local shot index"}
		}
		downstream := g.descendantsAndSelf(node.ID)
		plan.ReplayNodes = sortedSet(downstream)
		for _, nodeID := range plan.ReplayNodes {
			reason := "duration-timeline-dependency"
			if nodeID == node.ID {
				reason = "duration-direct-dependency"
			}
			plan.Invalidations = append(plan.Invalidations, Invalidation{
				Kind: "node", NodeID: nodeID, ShotIndex: -1,
				Reason: reason, Dependency: "duration_ms->start_ms/global_ms/samples",
			})
		}
		globalIndex := findGlobalIndex(presented, node.ID, edit.LocalIndex)
		if globalIndex >= 0 {
			for _, shot := range presented {
				if shot.GlobalIndex < globalIndex {
					continue
				}
				reason := "timeline-offset-dependency"
				dependency := "preceding-duration->start_ms/global_ms"
				if shot.GlobalIndex == globalIndex {
					reason = "duration-direct-dependency"
					dependency = "duration_ms->samples/end_ms"
				}
				plan.invalidateShot(shot, reason, dependency)
			}
		} else {
			shot := node.Shots[edit.LocalIndex]
			plan.Invalidations = append(plan.Invalidations, Invalidation{
				Kind: "shot", NodeID: node.ID, ShotID: shot.ID, ShotIndex: -1, LocalIndex: edit.LocalIndex,
				Reason: "duration-direct-dependency", Dependency: "duration_ms->samples",
			})
		}
	} else {
		dirtyNodes := g.descendantsAndSelf(edit.NodeID)
		plan.ReplayNodes = sortedSet(dirtyNodes)
		reason := map[EditKind]string{
			EditPreconditions: "precondition-dependency-changed",
			EditEffects:       "effect-flag-dependency-changed",
			EditAspect:        "aspect-dependency-changed",
		}[edit.Kind]
		if reason == "" {
			reason = "node-field-dependency-changed"
		}
		for _, nodeID := range plan.ReplayNodes {
			plan.Invalidations = append(plan.Invalidations, Invalidation{
				Kind: "node", NodeID: nodeID, ShotIndex: -1,
				Reason: reason, Dependency: "node->causal/flag-downstream",
			})
		}
		for _, shot := range presented {
			if dirtyNodes[shot.NodeID] {
				plan.invalidateShot(shot, reason, "node->shot-presentation")
			}
		}
	}

	invalidSet := map[int]bool{}
	for _, entry := range plan.Invalidations {
		if entry.Kind == "shot" && entry.ShotIndex >= 0 {
			invalidSet[entry.ShotIndex] = true
		}
	}
	plan.InvalidShotIndex = invalidSet
	for _, shot := range presented {
		if !invalidSet[shot.GlobalIndex] {
			plan.CacheableShots = append(plan.CacheableShots, shot.GlobalIndex)
		}
	}
	sort.Ints(plan.CacheableShots)
	return plan, nil
}

func (p *Plan) invalidateShot(shot model.PresentedShot, reason, dependency string) {
	if p.InvalidShotIndex[shot.GlobalIndex] {
		return
	}
	p.InvalidShotIndex[shot.GlobalIndex] = true
	p.Invalidations = append(p.Invalidations, Invalidation{
		Kind: "shot", NodeID: shot.NodeID, ShotID: shot.ShotID, ShotIndex: shot.GlobalIndex,
		LocalIndex: shot.LocalIndex, Reason: reason, Dependency: dependency,
	})
}

func (g *Graph) descendantsAndSelf(root string) map[string]bool {
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range g.downstream[current] {
			if !seen[edge.To] {
				seen[edge.To] = true
				queue = append(queue, edge.To)
			}
		}
	}
	nodes := map[string]bool{}
	for id := range seen {
		if g.byID[id] != nil {
			nodes[id] = true
		}
	}
	return nodes
}

func (g *Graph) addEdge(edge Edge) {
	g.edges = append(g.edges, edge)
	g.downstream[edge.From] = append(g.downstream[edge.From], edge)
}

func engineNodeMap(nodes []model.Node) map[string]*model.Node {
	result := make(map[string]*model.Node, len(nodes))
	for i := range nodes {
		result[nodes[i].ID] = &nodes[i]
	}
	return result
}

func shotRef(nodeID string, local int) string {
	return nodeID + "#shot:" + itoa(local)
}

func findGlobalIndex(shots []model.PresentedShot, nodeID string, local int) int {
	for _, shot := range shots {
		if shot.NodeID == nodeID && shot.LocalIndex == local {
			return shot.GlobalIndex
		}
	}
	return -1
}

func uniqueSorted(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	return sortedSet(set)
}

func sortedSet(set map[string]bool) []string {
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buffer [20]byte
	pos := len(buffer)
	for value > 0 {
		pos--
		buffer[pos] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		pos--
		buffer[pos] = '-'
	}
	return string(buffer[pos:])
}
