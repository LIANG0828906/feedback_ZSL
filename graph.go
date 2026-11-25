package macrokit

import (
	"fmt"
	"sort"
)

type DepNodeKind string

const (
	NodeCondition    DepNodeKind = "condition"
	NodeKey          DepNodeKind = "key"
	NodeFlag         DepNodeKind = "flag"
	NodeBranchResult DepNodeKind = "branch_result"
	NodeDelay        DepNodeKind = "delay"
	NodeStepGap      DepNodeKind = "step_gap"
	NodeStep         DepNodeKind = "step"
)

type DepNode struct {
	ID        string      `json:"id"`
	Kind      DepNodeKind `json:"kind"`
	BranchID  string      `json:"branch_id,omitempty"`
	StepIndex int         `json:"step_index,omitempty"`
	StepID    string      `json:"step_id,omitempty"`
}

type DependencyGraph struct {
	ProjectID string              `json:"project_id"`
	Nodes     map[string]DepNode  `json:"nodes"`
	Edges     map[string][]string `json:"edges"`
	project   *Project
}

type InvalidationItem struct {
	NodeID    string      `json:"node_id"`
	NodeKind  DepNodeKind `json:"node_kind"`
	BranchID  string      `json:"branch_id"`
	StepIndex int         `json:"step_index,omitempty"`
	StepID    string      `json:"step_id,omitempty"`
	Reason    string      `json:"reason"`
}

type BranchInvalidation struct {
	BranchID string   `json:"branch_id"`
	Reasons  []string `json:"reasons"`
	StepIDs  []string `json:"step_ids,omitempty"`
}

type InvalidationReport struct {
	ChangedNode string               `json:"changed_node"`
	Reason      string               `json:"reason"`
	BranchIDs   []string             `json:"branch_ids"`
	Branches    []BranchInvalidation `json:"branches,omitempty"`
	Items       []InvalidationItem   `json:"items"`
}

func conditionNode(branchID string) string { return "condition:" + branchID }
func resultNode(branchID string) string    { return "result:" + branchID }
func delayNode(branchID string) string     { return "delay:" + branchID }
func keyNode(key string) string            { return "key:" + key }
func flagNode(flag string) string          { return "flag:" + flag }
func gapNode(branchID string, index int) string {
	return fmt.Sprintf("gap:%s:%d", branchID, index)
}
func stepNode(branchID string, index int) string {
	return fmt.Sprintf("step:%s:%d", branchID, index)
}

func BuildDependencyGraph(p *Project) *DependencyGraph {
	if p.BranchByID == nil {
		p.Index()
	}
	g := &DependencyGraph{
		ProjectID: p.ID,
		Nodes:     map[string]DepNode{},
		Edges:     map[string][]string{},
		project:   p,
	}

	for _, b := range p.SortedBranches() {
		g.addNode(DepNode{ID: conditionNode(b.ID), Kind: NodeCondition, BranchID: b.ID})
		g.addNode(DepNode{ID: resultNode(b.ID), Kind: NodeBranchResult, BranchID: b.ID})
		g.addNode(DepNode{ID: delayNode(b.ID), Kind: NodeDelay, BranchID: b.ID})
		g.addEdge(conditionNode(b.ID), resultNode(b.ID))
		g.addEdge(delayNode(b.ID), resultNode(b.ID))

		conditionID := conditionNode(b.ID)
		g.recordConditionDependencies(b.ID, b.Condition, conditionID)

		for i, step := range b.Steps {
			sID := stepNode(b.ID, i)
			g.addNode(DepNode{ID: sID, Kind: NodeStep, BranchID: b.ID, StepIndex: i, StepID: step.ID})
			if i == 0 {
				g.addEdge(conditionID, sID)
				g.addEdge(delayNode(b.ID), sID)
			} else {
				gID := gapNode(b.ID, i)
				g.addNode(DepNode{ID: gID, Kind: NodeStepGap, BranchID: b.ID, StepIndex: i, StepID: step.ID})
				g.addEdge(gID, sID)
				g.addEdge(stepNode(b.ID, i-1), sID)
			}
			if i == len(b.Steps)-1 {
				g.addEdge(sID, resultNode(b.ID))
			}
		}
	}
	return g
}

func (g *DependencyGraph) recordConditionDependencies(branchID string, c *Condition, conditionID string) {
	if c == nil {
		return
	}
	switch c.Kind {
	case CondKey:
		id := keyNode(c.Key)
		g.addNode(DepNode{ID: id, Kind: NodeKey})
		g.addEdge(id, conditionID)
	case CondFlag:
		id := flagNode(c.Flag)
		g.addNode(DepNode{ID: id, Kind: NodeFlag})
		g.addEdge(id, conditionID)
	case CondBranch:
		id := resultNode(c.BranchID)
		if _, exists := g.Nodes[id]; !exists {
			g.addNode(DepNode{ID: id, Kind: NodeBranchResult, BranchID: c.BranchID})
		}
		g.addEdge(id, conditionID)
	}
	for _, child := range c.Children {
		g.recordConditionDependencies(branchID, child, conditionID)
	}
}

func (g *DependencyGraph) addNode(node DepNode) {
	g.Nodes[node.ID] = node
}

func (g *DependencyGraph) addEdge(from, to string) {
	for _, existing := range g.Edges[from] {
		if existing == to {
			return
		}
	}
	g.Edges[from] = append(g.Edges[from], to)
	sort.Strings(g.Edges[from])
}

// Invalidate returns the exact reverse-dependency closure from changedNode.
// It never mutates the graph or cache.
func (g *DependencyGraph) Invalidate(changedNode, reason string) (*InvalidationReport, error) {
	if _, ok := g.Nodes[changedNode]; !ok {
		return nil, newRefError("/dependency_graph", changedNode, "dependency node does not exist")
	}

	visited := map[string]bool{changedNode: true}
	queue := []string{changedNode}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		next := append([]string(nil), g.Edges[current]...)
		sort.Strings(next)
		for _, to := range next {
			if !visited[to] {
				visited[to] = true
				queue = append(queue, to)
			}
		}
	}

	report := &InvalidationReport{ChangedNode: changedNode, Reason: reason}
	byBranch := map[string]*BranchInvalidation{}
	for nodeID := range visited {
		node := g.Nodes[nodeID]
		itemReason := reason
		if nodeID != changedNode {
			itemReason = fmt.Sprintf("downstream of %s: %s", changedNode, reason)
		}
		report.Items = append(report.Items, InvalidationItem{
			NodeID: nodeID, NodeKind: node.Kind, BranchID: node.BranchID,
			StepIndex: node.StepIndex, StepID: node.StepID, Reason: itemReason,
		})
		if node.BranchID == "" {
			continue
		}
		branch := byBranch[node.BranchID]
		if branch == nil {
			branch = &BranchInvalidation{BranchID: node.BranchID}
			byBranch[node.BranchID] = branch
			report.BranchIDs = append(report.BranchIDs, node.BranchID)
		}
		branch.Reasons = appendUnique(branch.Reasons, itemReason)
		if node.Kind == NodeStep && node.StepID != "" {
			branch.StepIDs = appendUnique(branch.StepIDs, node.StepID)
		}
	}

	sort.Strings(report.BranchIDs)
	sort.Slice(report.Items, func(i, j int) bool {
		a, b := report.Items[i], report.Items[j]
		if a.BranchID != b.BranchID {
			return a.BranchID < b.BranchID
		}
		if a.StepIndex != b.StepIndex {
			return a.StepIndex < b.StepIndex
		}
		return a.NodeID < b.NodeID
	})
	for _, id := range report.BranchIDs {
		bi := byBranch[id]
		sort.Strings(bi.Reasons)
		sort.Strings(bi.StepIDs)
		report.Branches = append(report.Branches, *bi)
	}
	return report, nil
}

func (g *DependencyGraph) EditCondition(branchID string, condition *Condition) (*DependencyGraph, *InvalidationReport, error) {
	branch, ok := g.project.BranchByID[branchID]
	if !ok {
		return nil, nil, newRefError("/branches", branchID, "branch reference is out of bounds")
	}
	if err := validateCondition(condition, g.project.BranchByID, "/edit/condition"); err != nil {
		return nil, nil, err
	}
	report, err := g.Invalidate(conditionNode(branchID), "trigger condition edited")
	if err != nil {
		return nil, nil, err
	}
	branch.Condition = condition
	if err := validateBranchCycles(g.project); err != nil {
		return nil, report, err
	}
	return BuildDependencyGraph(g.project), report, nil
}

func (g *DependencyGraph) EditBranchDelay(branchID string, delayFrames int64) (*InvalidationReport, error) {
	branch, ok := g.project.BranchByID[branchID]
	if !ok {
		return nil, newRefError("/branches", branchID, "branch reference is out of bounds")
	}
	if delayFrames < 0 {
		return nil, newSchemaError("/edit/delay_frames", "delay must be non-negative")
	}
	report, err := g.Invalidate(delayNode(branchID), "branch delay parameter edited")
	if err != nil {
		return nil, err
	}
	branch.DelayFrames = delayFrames
	return report, nil
}

func (g *DependencyGraph) EditStepGap(branchID string, stepIndex int, gapFrames int64) (*InvalidationReport, error) {
	branch, ok := g.project.BranchByID[branchID]
	if !ok {
		return nil, newRefError("/branches", branchID, "branch reference is out of bounds")
	}
	if stepIndex <= 0 || stepIndex >= len(branch.Steps) {
		return nil, newSchemaError(fmt.Sprintf("/branches/%s/steps/%d", branchID, stepIndex), "gap can only be edited on a step after the first")
	}
	if gapFrames < 0 {
		return nil, newSchemaError("/edit/gap_frames", "gap must be non-negative")
	}
	node := gapNode(branchID, stepIndex)
	report, err := g.Invalidate(node, "step gap parameter edited")
	if err != nil {
		return nil, err
	}
	branch.Steps[stepIndex].GapFrames = gapFrames
	return report, nil
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}
