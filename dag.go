package colortrace

import (
	"fmt"
	"sort"
	"strings"
)

// GraphNode is a stage-plane or band-judgment dependency node.
type GraphNode struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Stage     int      `json:"stage"`
	Plane     string   `json:"plane,omitempty"`
	Region    Region   `json:"region"`
	DependsOn []string `json:"depends_on"`
	ParamHash string   `json:"param_hash,omitempty"`
	CacheKey  string   `json:"cache_key,omitempty"`
}

// Invalidation is one precise cache invalidation event.
type Invalidation struct {
	Stage  int    `json:"stage"`
	NodeID string `json:"node_id"`
	Reason string `json:"reason"`
}

// InvalidationPlan contains invalid and reusable nodes.
type InvalidationPlan struct {
	Invalidations []Invalidation `json:"invalidations"`
	Reusable      []string       `json:"reusable"`
}

// DependencyGraph records stage/plane regions, upstream edges and judgments.
type DependencyGraph struct {
	Nodes      map[string]*GraphNode `json:"nodes"`
	Dependents map[string][]string   `json:"dependents"`
}

// BuildDependencyGraph constructs plane-scoped serial dependencies.
func BuildDependencyGraph(project *Project) (*DependencyGraph, error) {
	graph := &DependencyGraph{Nodes: map[string]*GraphNode{}, Dependents: map[string][]string{}}
	for stageIdx := range project.Stages {
		for _, desc := range project.Planes {
			w, h, err := StagePlaneDimensions(project, stageIdx, desc.Name)
			if err != nil {
				return nil, err
			}
			id := StagePlaneNodeID(stageIdx, desc.Name)
			depends := []string{}
			if stageIdx > 0 {
				depends = append(depends, StagePlaneNodeID(stageIdx-1, desc.Name))
			}
			cfg := project.Stages[stageIdx]
			node := &GraphNode{
				ID: id, Kind: "stage_plane", Stage: stageIdx, Plane: desc.Name,
				Region: Region{MaxX: w, MaxY: h}, DependsOn: depends,
				ParamHash: HashCanonical("stage-param", cfg),
			}
			graph.addNode(node)
			judgeID := JudgmentNodeID(stageIdx, desc.Name)
			graph.addNode(&GraphNode{
				ID: judgeID, Kind: "band_judgment", Stage: stageIdx, Plane: desc.Name,
				Region: node.Region, DependsOn: []string{id},
			})
		}
	}
	return graph, nil
}

func StagePlaneNodeID(stage int, plane string) string {
	return fmt.Sprintf("stage:%d:plane:%s", stage, plane)
}

func JudgmentNodeID(stage int, plane string) string {
	return fmt.Sprintf("judge:%d:plane:%s", stage, plane)
}

func (g *DependencyGraph) addNode(node *GraphNode) {
	g.Nodes[node.ID] = node
	for _, upstream := range node.DependsOn {
		g.Dependents[upstream] = append(g.Dependents[upstream], node.ID)
	}
}

// PlanParameterEdit invalidates only planes affected by a stage parameter diff.
func (g *DependencyGraph) PlanParameterEdit(project *Project, newCfg StageConfig) (*InvalidationPlan, error) {
	stage := newCfg.Index
	if stage < 0 || stage >= len(project.Stages) {
		return nil, paraError(stage, 0, len(project.Stages)-1, "edited stage index out of range")
	}
	oldCfg := project.Stages[stage]
	fields, allPlanes, chromaOnly := diffStageConfig(oldCfg, newCfg)
	if len(fields) == 0 {
		return &InvalidationPlan{Reusable: g.allNodeIDs()}, nil
	}
	affected := map[string]bool{}
	for _, desc := range project.Planes {
		if allPlanes || (chromaOnly && desc.Chroma) {
			affected[desc.Name] = true
		}
	}
	plan := &InvalidationPlan{}
	invalid := map[string]string{}
	reason := "parameter_changed:" + strings.Join(fields, ",")
	for plane := range affected {
		id := StagePlaneNodeID(stage, plane)
		invalid[id] = reason
	}
	g.propagate(invalid)
	for id, why := range invalid {
		node := g.Nodes[id]
		stageValue := stage
		if node != nil {
			stageValue = node.Stage
		}
		plan.Invalidations = append(plan.Invalidations, Invalidation{Stage: stageValue, NodeID: id, Reason: why})
	}
	sortInvalidations(plan.Invalidations)
	invalidSet := map[string]bool{}
	for _, inv := range plan.Invalidations {
		invalidSet[inv.NodeID] = true
	}
	for _, id := range g.allNodeIDs() {
		if !invalidSet[id] {
			plan.Reusable = append(plan.Reusable, id)
		}
	}
	sort.Strings(plan.Reusable)
	return plan, nil
}

// PlanReorder returns invalidation events from the first moved position onward.
func (g *DependencyGraph) PlanReorder(project *Project, reordered []StageConfig) (*InvalidationPlan, error) {
	if len(reordered) != len(project.Stages) {
		return nil, schemaError("$.stages", "reorder must preserve stage count")
	}
	firstDiff := -1
	for i := range project.Stages {
		if project.Stages[i].ID != reordered[i].ID {
			firstDiff = i
			break
		}
	}
	if firstDiff < 0 {
		return &InvalidationPlan{Reusable: g.allNodeIDs()}, nil
	}
	plan := &InvalidationPlan{}
	for stage := firstDiff; stage < len(reordered); stage++ {
		movedID := reordered[stage].ID
		for _, desc := range project.Planes {
			id := StagePlaneNodeID(stage, desc.Name)
			plan.Invalidations = append(plan.Invalidations, Invalidation{
				Stage: stage, NodeID: id,
				Reason: fmt.Sprintf("order_changed: stage %s now at index %d", movedID, stage),
			})
			judge := JudgmentNodeID(stage, desc.Name)
			plan.Invalidations = append(plan.Invalidations, Invalidation{
				Stage: stage, NodeID: judge, Reason: "judgment_depends_on_reordered_stage",
			})
		}
	}
	sortInvalidations(plan.Invalidations)
	invalidSet := map[string]bool{}
	for _, inv := range plan.Invalidations {
		invalidSet[inv.NodeID] = true
	}
	for _, id := range g.allNodeIDs() {
		node := g.Nodes[id]
		if !invalidSet[id] && node != nil && node.Stage < firstDiff {
			plan.Reusable = append(plan.Reusable, id)
		}
	}
	sort.Strings(plan.Reusable)
	return plan, nil
}

func (g *DependencyGraph) propagate(invalid map[string]string) {
	queue := make([]string, 0, len(invalid))
	for id := range invalid {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, dependent := range g.Dependents[id] {
			reason := "upstream_invalidated:" + id
			if _, exists := invalid[dependent]; !exists {
				invalid[dependent] = reason
				queue = append(queue, dependent)
			}
		}
	}
}

func (g *DependencyGraph) allNodeIDs() []string {
	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func diffStageConfig(oldCfg, newCfg StageConfig) (fields []string, allPlanes bool, chromaOnly bool) {
	if oldCfg.ParamVersion != newCfg.ParamVersion {
		fields = append(fields, "param_version")
		allPlanes = true
	}
	if oldCfg.FactorX != newCfg.FactorX || oldCfg.FactorY != newCfg.FactorY {
		fields = append(fields, "factor")
		allPlanes = true
	}
	if oldCfg.PhaseX != newCfg.PhaseX || oldCfg.PhaseY != newCfg.PhaseY {
		fields = append(fields, "phase")
		allPlanes = true
	}
	if oldCfg.AccumScale != newCfg.AccumScale {
		fields = append(fields, "accum_scale")
		allPlanes = true
	}
	if oldCfg.Bits != newCfg.Bits {
		fields = append(fields, "bits")
		allPlanes = true
	}
	if HashCanonical("kernel", oldCfg.Kernel) != HashCanonical("kernel", newCfg.Kernel) {
		fields = append(fields, "kernel")
		allPlanes = true
	}
	if HashCanonical("chroma", oldCfg.ChromaKernel) != HashCanonical("chroma", newCfg.ChromaKernel) {
		fields = append(fields, "chroma_kernel")
		chromaOnly = true
	}
	if oldCfg.ID != newCfg.ID || oldCfg.Description != newCfg.Description {
		fields = append(fields, "metadata")
	}
	if len(fields) > 0 && !allPlanes {
		chromaOnly = true
	}
	return fields, allPlanes, chromaOnly
}

func sortInvalidations(items []Invalidation) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Stage != items[j].Stage {
			return items[i].Stage < items[j].Stage
		}
		return items[i].NodeID < items[j].NodeID
	})
}
