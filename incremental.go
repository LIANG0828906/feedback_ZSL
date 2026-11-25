package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type EditKind string

const (
	EditSetDependencies EditKind = "set_dependencies"
	EditSetZone         EditKind = "set_zone"
	EditAppendRecovery  EditKind = "append_recovery"
)

type Edit struct {
	Kind         EditKind `json:"kind"`
	NodeID       string   `json:"node_id,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	Zone         int64    `json:"zone,omitempty"`
	Event        *Event   `json:"event,omitempty"`
}

type IncrementalReport struct {
	SnapshotChanged bool     `json:"snapshot_changed"`
	InvalidatedBy   []string `json:"invalidated_by"`
	RecomputedTicks []int64  `json:"recomputed_ticks"`
	ReusedTicks     []int64  `json:"reused_ticks"`
}

type tickCache struct {
	Result TickResult
	State  *simState
}

type IncrementalEngine struct {
	project   *Project
	worker    int
	domainCap int
	cache     map[int64]tickCache
	digest    string
}

func NewIncrementalEngine(project *Project) (*IncrementalEngine, error) {
	clone := project.Clone()
	if err := clone.Normalize(); err != nil {
		return nil, err
	}
	if cycles := FindDependencyCycles(clone); len(cycles) > 0 {
		return nil, cycleError(cycles)
	}
	engine := &IncrementalEngine{
		project:   clone,
		worker:    clone.WorkerBudget,
		domainCap: clone.PerDomainPendingLimit,
		cache:     make(map[int64]tickCache),
		digest:    projectDigest(clone),
	}
	if engine.worker <= 0 {
		engine.worker = 1
	}
	if engine.domainCap <= 0 {
		engine.domainCap = 1
	}
	if _, err := engine.recompute(0, engine.maxTick(), []string{"initial"}); err != nil {
		return nil, err
	}
	return engine, nil
}

func (e *IncrementalEngine) maxTick() int64 {
	if len(e.project.Events) == 0 {
		return 0
	}
	return e.project.Events[len(e.project.Events)-1].Tick
}

func (e *IncrementalEngine) solver() *Solver {
	return &Solver{
		Project:               e.project,
		WorkerBudget:          e.worker,
		PerDomainPendingLimit: e.domainCap,
	}
}

func (e *IncrementalEngine) recompute(start, end int64, sources []string) ([]int64, error) {
	if end < start {
		return nil, nil
	}
	var state *simState
	if start == 0 {
		state = newInitialState(e.project)
	} else {
		cached, ok := e.cache[start-1]
		if !ok {
			return nil, newError(CodeCheckpoint, "$.cache", "missing cached state before tick %d", start)
		}
		state = cached.State.clone()
	}
	sim, err := newSimulationFromState(e.solver(), state)
	if err != nil {
		return nil, err
	}
	ticks := make([]int64, 0, end-start+1)
	for tick := start; tick <= end; tick++ {
		step, err := sim.Step(tick)
		if err != nil {
			return nil, err
		}
		step.InvalidatedBy = uniqueSorted(append([]string(nil), sources...))
		e.cache[tick] = tickCache{Result: step, State: sim.state.clone()}
		ticks = append(ticks, tick)
	}
	return ticks, nil
}

func (e *IncrementalEngine) Apply(edits ...Edit) (IncrementalReport, error) {
	report := IncrementalReport{}
	if len(edits) == 0 {
		report.ReusedTicks = e.cachedTicks()
		return report, nil
	}
	oldProject := e.project.Clone()
	candidate := e.project.Clone()
	seeds := make(map[string]struct{})
	sources := make([]string, 0, len(edits))
	extraTicks := make([]int64, 0)
	structuralEdit := false
	zoneEdit := false
	for _, edit := range edits {
		switch edit.Kind {
		case EditSetDependencies:
			structuralEdit = true
			node, ok := candidate.Node(edit.NodeID)
			if !ok {
				return report, newError(CodeUnknownNode, "$.edit.node_id", "unknown node %q", edit.NodeID)
			}
			oldNode, _ := oldProject.Node(edit.NodeID)
			seeds[node.ID] = struct{}{}
			for _, edge := range oldProject.Edges {
				if edge.From == oldNode.ID {
					seeds[edge.To] = struct{}{}
				}
			}
			dependencies := append([]string(nil), edit.Dependencies...)
			sort.Strings(dependencies)
			dependencies = uniqueSorted(dependencies)
			for _, dependency := range dependencies {
				if _, ok := candidate.Node(dependency); !ok {
					return report, newError(CodeUnknownNode, "$.edit.dependencies", "unknown dependency %q", dependency)
				}
				seeds[dependency] = struct{}{}
			}
			filtered := candidate.Edges[:0]
			for _, edge := range candidate.Edges {
				if edge.From != node.ID {
					filtered = append(filtered, edge)
				}
			}
			candidate.Edges = filtered
			for _, dependency := range dependencies {
				candidate.Edges = append(candidate.Edges, Edge{From: node.ID, To: dependency})
			}
			sources = append(sources, fmt.Sprintf("edge:%s:[%s]", node.ID, formatIDs(dependencies)))
		case EditSetZone:
			structuralEdit = true
			zoneEdit = true
			node, ok := candidate.Node(edit.NodeID)
			if !ok {
				return report, newError(CodeUnknownNode, "$.edit.node_id", "unknown node %q", edit.NodeID)
			}
			oldZone := node.Zone
			for index := range candidate.Nodes {
				if candidate.Nodes[index].ID == node.ID {
					candidate.Nodes[index].Zone = edit.Zone
				}
				if candidate.Nodes[index].Zone == oldZone || candidate.Nodes[index].Zone == edit.Zone {
					seeds[candidate.Nodes[index].ID] = struct{}{}
				}
			}
			sources = append(sources, fmt.Sprintf("zone:%s:%d->%d", node.ID, oldZone, edit.Zone))
		case EditAppendRecovery:
			if edit.Event == nil {
				return report, newError(CodeInvalidEvent, "$.edit.event", "append_recovery requires an event")
			}
			event := *edit.Event
			event.Kind = EventRecover
			if event.NodeID == "" {
				return report, newError(CodeInvalidEvent, "$.edit.event.node_id", "recovery node is required")
			}
			if _, ok := candidate.Node(event.NodeID); !ok {
				return report, newError(CodeUnknownNode, "$.edit.event.node_id", "unknown node %q", event.NodeID)
			}
			lastTick := int64(-1)
			if len(candidate.Events) > 0 {
				lastTick = candidate.Events[len(candidate.Events)-1].Tick
			}
			if event.Tick < lastTick {
				return report, newError(CodeInvalidEvent, "$.edit.event.tick", "appended recovery tick %d precedes last event tick %d", event.Tick, lastTick)
			}
			event.DomainID = nil
			event.WorkloadID = ""
			event.AmountBytes = 0
			candidate.Events = append(candidate.Events, event)
			seeds[event.NodeID] = struct{}{}
			extraTicks = append(extraTicks, event.Tick)
			sources = append(sources, fmt.Sprintf("recovery:%s@tick=%d", event.NodeID, event.Tick))
		default:
			return report, newError(CodeInvalidEvent, "$.edit.kind", "unknown edit kind %q", edit.Kind)
		}
	}
	if err := candidate.Normalize(); err != nil {
		return report, err
	}
	if cycles := FindDependencyCycles(candidate); len(cycles) > 0 {
		return report, cycleError(cycles)
	}
	relevant := affectedComponent(seeds, oldProject, candidate)
	var relevantTicks []int64
	if structuralEdit {
		relevantTicks = relevantEventTicks(relevant, candidate)
	}
	relevantTicks = append(relevantTicks, extraTicks...)
	relevantTicks = int64UniqueSorted(relevantTicks)
	if len(relevantTicks) == 0 {
		e.project = candidate
		e.digest = projectDigest(candidate)
		report.InvalidatedBy = uniqueSorted(sources)
		report.ReusedTicks = e.cachedTicks()
		return report, nil
	}

	oldMax := e.maxTick()
	e.project = candidate
	newMax := e.maxTick()
	if oldMax < newMax && relevantTicks[0] > oldMax+1 {
		relevantTicks = append([]int64{oldMax + 1}, relevantTicks...)
	}

	for tick, cached := range e.cache {
		cached.Result.InvalidatedBy = nil
		e.cache[tick] = cached
	}
	windows := e.affectedWindows(relevantTicks, newMax)
	recomputed := make([]int64, 0)
	for _, window := range windows {
		ticks, err := e.recompute(window[0], window[1], sources)
		if err != nil {
			return report, err
		}
		recomputed = append(recomputed, ticks...)
	}
	if zoneEdit {
		e.refreshNodeMetadata()
	}
	e.digest = projectDigest(candidate)
	recomputed = int64UniqueSorted(recomputed)
	recomputedSet := make(map[int64]struct{}, len(recomputed))
	for _, tick := range recomputed {
		recomputedSet[tick] = struct{}{}
	}
	for _, tick := range e.cachedTicks() {
		if _, ok := recomputedSet[tick]; !ok {
			report.ReusedTicks = append(report.ReusedTicks, tick)
		}
	}
	report.InvalidatedBy = uniqueSorted(sources)
	report.RecomputedTicks = recomputed
	return report, nil
}

func (e *IncrementalEngine) ReplaceSnapshot(project *Project) (IncrementalReport, error) {
	if project == nil {
		return IncrementalReport{}, newError(CodeInvalidSchema, "$", "nil snapshot")
	}
	clone := project.Clone()
	if err := clone.Normalize(); err != nil {
		return IncrementalReport{}, err
	}
	if cycles := FindDependencyCycles(clone); len(cycles) > 0 {
		return IncrementalReport{}, cycleError(cycles)
	}
	sameVersion := e.project.SnapshotID == clone.SnapshotID && e.project.Revision == clone.Revision
	digest := projectDigest(clone)
	if sameVersion {
		if digest != e.digest {
			return IncrementalReport{}, newError(CodeSnapshotChanged, "$.snapshot", "content changed without snapshot or revision bump")
		}
		return IncrementalReport{ReusedTicks: e.cachedTicks()}, nil
	}
	oldVersion := e.project.SnapshotID + "@" + e.project.Revision
	newVersion := clone.SnapshotID + "@" + clone.Revision
	e.project = clone
	e.cache = make(map[int64]tickCache)
	e.digest = digest
	sources := []string{fmt.Sprintf("snapshot:%s->%s", oldVersion, newVersion)}
	ticks, err := e.recompute(0, e.maxTick(), sources)
	if err != nil {
		return IncrementalReport{}, err
	}
	return IncrementalReport{
		SnapshotChanged: true,
		InvalidatedBy:   sources,
		RecomputedTicks: ticks,
	}, nil
}

func (e *IncrementalEngine) RunResult() (*RunResult, error) {
	ticks := e.cachedTicks()
	result := &RunResult{Ticks: make([]TickResult, 0, len(ticks))}
	for _, tick := range ticks {
		result.Ticks = append(result.Ticks, e.cache[tick].Result)
	}
	if len(ticks) > 0 {
		last := ticks[len(ticks)-1]
		state := e.cache[last].State.clone()
		sim, err := newSimulationFromState(e.solver(), state)
		if err != nil {
			return nil, err
		}
		result.Final = sim.finalState()
	}
	return result, nil
}

func (e *IncrementalEngine) Project() *Project {
	return e.project.Clone()
}

func (e *IncrementalEngine) cachedTicks() []int64 {
	ticks := make([]int64, 0, len(e.cache))
	for tick := range e.cache {
		ticks = append(ticks, tick)
	}
	sort.Slice(ticks, func(i, j int) bool { return ticks[i] < ticks[j] })
	return ticks
}

func (e *IncrementalEngine) refreshNodeMetadata() {
	if e == nil || e.project == nil {
		return
	}
	for tick, cached := range e.cache {
		for index := range cached.Result.NodeStates {
			node, ok := e.project.Node(cached.Result.NodeStates[index].NodeID)
			if !ok {
				continue
			}
			cached.Result.NodeStates[index].Rack = node.Rack
			cached.Result.NodeStates[index].Zone = node.Zone
		}
		for index := range cached.Result.Domains {
			cached.Result.Domains[index].Nodes = e.domainNodes(cached.Result.Domains[index].ID)
		}
		e.cache[tick] = cached
	}
}

func (e *IncrementalEngine) domainNodes(domainID int64) []string {
	var nodes []string
	for _, node := range e.project.Nodes {
		if node.Zone == domainID {
			nodes = append(nodes, node.ID)
		}
	}
	sort.Strings(nodes)
	return nodes
}

func (e *IncrementalEngine) affectedWindows(relevantTicks []int64, maxTick int64) [][2]int64 {
	if len(relevantTicks) == 0 {
		return nil
	}
	windows := make([][2]int64, 0)
	start := relevantTicks[0]
	last := relevantTicks[0]
	for _, tick := range relevantTicks[1:] {
		if quiescent := e.firstQuiescent(last, tick-1); quiescent >= 0 {
			windows = append(windows, [2]int64{start, quiescent})
			start = tick
		}
		last = tick
	}
	end := e.firstQuiescent(last, maxTick)
	if end < 0 {
		end = maxTick
	}
	windows = append(windows, [2]int64{start, end})
	return windows
}

func (e *IncrementalEngine) firstQuiescent(start, end int64) int64 {
	if start < 0 {
		start = 0
	}
	for tick := start; tick <= end; tick++ {
		cached, ok := e.cache[tick]
		if ok && tickResultQuiescent(cached.Result) {
			return tick
		}
	}
	return -1
}

func tickResultQuiescent(result TickResult) bool {
	if len(result.FrozenWorkloads) != 0 || len(result.WaitingQueue) != 0 {
		return false
	}
	for _, state := range result.NodeStates {
		if state.Faulty || state.Isolated {
			return false
		}
	}
	for _, domain := range result.Domains {
		if domain.Status == DomainActive || domain.Status == DomainForced {
			return false
		}
	}
	return true
}

func affectedComponent(seeds map[string]struct{}, projects ...*Project) map[string]struct{} {
	adjacency := make(map[string][]string)
	for _, project := range projects {
		if project == nil {
			continue
		}
		for _, node := range project.Nodes {
			adjacency[node.ID] = adjacency[node.ID]
		}
		for _, edge := range project.Edges {
			adjacency[edge.From] = append(adjacency[edge.From], edge.To)
			adjacency[edge.To] = append(adjacency[edge.To], edge.From)
		}
	}
	for node := range adjacency {
		sort.Strings(adjacency[node])
		adjacency[node] = uniqueSorted(adjacency[node])
	}
	visited := make(map[string]struct{})
	queue := make([]string, 0, len(seeds))
	for seed := range seeds {
		queue = append(queue, seed)
	}
	sort.Strings(queue)
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if _, ok := visited[node]; ok {
			continue
		}
		visited[node] = struct{}{}
		for _, next := range adjacency[node] {
			if _, ok := visited[next]; !ok {
				queue = append(queue, next)
			}
		}
	}
	return visited
}

func relevantEventTicks(relevant map[string]struct{}, project *Project) []int64 {
	if len(relevant) == 0 {
		return nil
	}
	firstProbe := int64(-1)
	for _, event := range project.Events {
		if event.Kind != EventProbe {
			continue
		}
		if _, ok := relevant[event.NodeID]; !ok {
			continue
		}
		if firstProbe < 0 || event.Tick < firstProbe {
			firstProbe = event.Tick
		}
	}
	if firstProbe < 0 {
		return nil
	}
	var ticks []int64
	for _, event := range project.Events {
		if event.Tick < firstProbe {
			continue
		}
		include := false
		if event.NodeID != "" {
			_, include = relevant[event.NodeID]
		}
		if event.Kind == EventRequest {
			workload, ok := project.Workload(event.WorkloadID)
			if ok {
				_, include = relevant[workload.NodeID]
			}
		}
		if event.Kind == EventCommit && event.DomainID != nil {
			for nodeID := range relevant {
				node, ok := project.Node(nodeID)
				if ok && node.Zone == *event.DomainID {
					include = true
					break
				}
			}
		}
		if include {
			ticks = append(ticks, event.Tick)
		}
	}
	return int64UniqueSorted(ticks)
}

func int64UniqueSorted(values []int64) []int64 {
	if len(values) == 0 {
		return nil
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func projectDigest(project *Project) string {
	encoded, err := json.Marshal(project)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
