package kernel

import (
	"context"
	"sync"

	"inputkernel/internal/model"
	"inputkernel/internal/sampleio"
)

type Runtime struct {
	mu               sync.Mutex
	scenario         *model.Scenario
	graph            *Graph
	cache            map[string]*CacheEntry
	contexts         []eventContext
	pool             *WorkerPool
	options          Options
	unmapped         []model.TruthUnmapped
	lastInvalidation InvalidationReport
}

func NewRuntime(ctx context.Context, s *model.Scenario, options Options) (*Runtime, error) {
	if err := model.ValidateScenario(s); err != nil {
		return nil, err
	}
	eventHash := sampleio.EventHash(s.Events)
	graph, err := BuildGraph(s, eventHash)
	if err != nil {
		return nil, err
	}
	rt := &Runtime{
		scenario: sampleio.Clone(s),
		graph:    graph,
		cache:    map[string]*CacheEntry{},
		contexts: buildContexts(s),
		options:  options.normalized(),
	}
	rt.pool = NewWorkerPool(ctx, rt.options)
	return rt, nil
}

func (rt *Runtime) Close() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.pool != nil {
		rt.pool.Close()
		rt.pool = nil
	}
}

func (rt *Runtime) RecomputeAll() (InvalidationReport, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	all := make([]string, 0, len(rt.graph.Topo))
	all = append(all, rt.graph.Topo...)
	return rt.recomputeLocked(rt.scenario, rt.graph, all)
}

func (rt *Runtime) EditAndRecompute(edit func(*model.Scenario) error) (InvalidationReport, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	next := sampleio.Clone(rt.scenario)
	if err := edit(next); err != nil {
		return InvalidationReport{}, err
	}
	if err := model.ValidateScenario(next); err != nil {
		return InvalidationReport{}, err
	}
	nextGraph, err := BuildGraph(next, sampleio.EventHash(next.Events))
	if err != nil {
		return InvalidationReport{}, err
	}
	roots, full := DiffGraphs(rt.graph, nextGraph)
	if full {
		roots = append([]string(nil), nextGraph.Topo...)
	}
	report, err := rt.recomputeLocked(next, nextGraph, roots)
	if err != nil {
		return report, err
	}
	return report, nil
}

func (rt *Runtime) recomputeLocked(next *model.Scenario, nextGraph *Graph, roots []string) (InvalidationReport, error) {
	if rt.pool == nil {
		return InvalidationReport{}, model.Version("runtime is closed", "worker_pool")
	}
	rt.pool.ResetStats()
	baseInvalidation := nextGraph.Invalidate(roots)
	dirty := map[string]bool{}
	for _, item := range baseInvalidation.ChangedNodes {
		dirty[item.NodeID] = true
	}
	upstreamKeys := map[string]string{}
	forcedRoots := make([]string, 0)
	for _, id := range nextGraph.Topo {
		key := nextGraph.CacheKey(next, id, upstreamKeys)
		upstreamKeys[id] = key
		if dirty[id] {
			continue
		}
		old, ok := rt.cache[id]
		if !ok || old.CacheKey != key {
			forcedRoots = append(forcedRoots, id)
			markDescendants(nextGraph, id, dirty)
		}
	}
	if len(forcedRoots) > 0 {
		baseInvalidation = nextGraph.Invalidate(append(append([]string(nil), roots...), forcedRoots...))
		dirty = map[string]bool{}
		for _, item := range baseInvalidation.ChangedNodes {
			dirty[item.NodeID] = true
		}
	}

	nextCache := make(map[string]*CacheEntry, len(nextGraph.Nodes))
	for _, id := range nextGraph.Topo {
		if dirty[id] {
			continue
		}
		old := rt.cache[id]
		if old == nil {
			continue
		}
		copyEntry := *old
		copyEntry.Dependencies = append([]string(nil), old.Dependencies...)
		nextCache[id] = &copyEntry
	}

	maxLevel := 0
	for _, id := range nextGraph.Topo {
		if nextGraph.Nodes[id].Level > maxLevel {
			maxLevel = nextGraph.Nodes[id].Level
		}
	}
	contexts := buildContexts(next)
	for level := 0; level <= maxLevel; level++ {
		ids := make([]string, 0)
		for _, id := range nextGraph.Topo {
			if dirty[id] && nextGraph.Nodes[id].Level == level {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		values := make([]NodeValue, len(ids))
		taskErrs := make([]error, len(ids))
		tasks := make([]Task, len(ids))
		for i, id := range ids {
			i, id := i, id
			tasks[i] = Task{
				ID: id,
				Fn: func(context.Context) error {
					value, err := computeNode(next, contexts, nextGraph, id, nextCache)
					if err != nil {
						taskErrs[i] = err
						return nil
					}
					values[i] = value
					return nil
				},
			}
		}
		if err := rt.pool.Run(tasks); err != nil {
			return baseInvalidation, err
		}
		for _, err := range taskErrs {
			if err != nil {
				return baseInvalidation, err
			}
		}
		for i, id := range ids {
			node := nextGraph.Nodes[id]
			nextCache[id] = &CacheEntry{
				NodeID: id, Kind: node.Kind, CacheKey: upstreamKeys[id],
				ParamsHash:   node.ParamsHash,
				Dependencies: append([]string(nil), node.Dependencies...),
				Value:        values[i],
			}
		}
	}
	unmapped, err := computeUnmapped(next, contexts)
	if err != nil {
		return baseInvalidation, err
	}

	// The cache and scenario/graph swap is the single commit point. On any
	// failure above the previous committed state remains untouched.
	rt.scenario = next
	rt.graph = nextGraph
	rt.cache = nextCache
	rt.contexts = contexts
	rt.unmapped = unmapped
	rt.lastInvalidation = baseInvalidation
	return baseInvalidation, nil
}

func computeNode(s *model.Scenario, contexts []eventContext, g *Graph, id string, cache map[string]*CacheEntry) (NodeValue, error) {
	n := g.Nodes[id]
	switch n.Kind {
	case NodeDevice, NodeCurve, NodeBinding, NodeTiming:
		return NodeValue{Digest: n.ParamsHash}, nil
	case NodeAxis:
		obs, err := computeAxisObservations(s, contexts, n.DeviceID, n.AxisID)
		if err != nil {
			return NodeValue{}, err
		}
		return NodeValue{AxisObservations: obs}, nil
	case NodeAction:
		b := findBinding(s, n.BindingID)
		if b == nil {
			return NodeValue{}, model.Schema("action", n.BindingID, "binding not found while computing action node")
		}
		var obs []AxisObservation
		if b.Type == model.BindingAxis {
			dep := axisNodeID(b.DeviceID, b.Axis)
			entry := cache[dep]
			if entry == nil {
				return NodeValue{}, model.Schema("axis", b.Axis, "axis cache is unavailable before action computation")
			}
			obs = entry.Value.AxisObservations
		}
		payload, err := computeActionNode(s, contexts, b, obs)
		if err != nil {
			return NodeValue{}, err
		}
		return NodeValue{Action: payload}, nil
	default:
		return NodeValue{}, model.Schema("node.kind", string(n.Kind), "unknown node kind")
	}
}

func markDescendants(g *Graph, root string, dirty map[string]bool) {
	if dirty[root] {
		return
	}
	dirty[root] = true
	for _, id := range g.Topo {
		for _, dep := range g.Nodes[id].Dependencies {
			if dep == root && !dirty[id] {
				markDescendants(g, id, dirty)
			}
		}
	}
}

func findBinding(s *model.Scenario, id string) *model.Binding {
	for i := range s.Bindings {
		if s.Bindings[i].ID == id {
			return &s.Bindings[i]
		}
	}
	return nil
}

func (rt *Runtime) Scenario() *model.Scenario {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return sampleio.Clone(rt.scenario)
}

func (rt *Runtime) Graph() *Graph {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	g := &Graph{
		ScenarioID: rt.graph.ScenarioID,
		EventHash:  rt.graph.EventHash,
		Nodes:      map[string]*Node{},
		Topo:       append([]string(nil), rt.graph.Topo...),
	}
	for id, n := range rt.graph.Nodes {
		copyNode := *n
		copyNode.Dependencies = append([]string(nil), n.Dependencies...)
		g.Nodes[id] = &copyNode
	}
	return g
}

func (rt *Runtime) CacheEntries() []*CacheEntry {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]*CacheEntry, 0, len(rt.cache))
	for _, id := range rt.graph.Topo {
		entry := rt.cache[id]
		if entry == nil {
			continue
		}
		copyEntry := *entry
		copyEntry.Dependencies = append([]string(nil), entry.Dependencies...)
		if entry.Value.AxisObservations != nil {
			copyEntry.Value.AxisObservations = append([]AxisObservation(nil), entry.Value.AxisObservations...)
		}
		if entry.Value.Action != nil {
			copyPayload := *entry.Value.Action
			copyPayload.Outputs = append([]ActionOutput(nil), entry.Value.Action.Outputs...)
			copyPayload.AxisSamples = append([]AxisSample(nil), entry.Value.Action.AxisSamples...)
			copyPayload.TimingChecks = append([]TimingCheck(nil), entry.Value.Action.TimingChecks...)
			copyEntry.Value.Action = &copyPayload
		}
		out = append(out, &copyEntry)
	}
	return out
}

func (rt *Runtime) UnmappedInputs() []model.TruthUnmapped {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]model.TruthUnmapped, 0, len(rt.unmapped))
	return append(out, rt.unmapped...)
}

func (rt *Runtime) LastInvalidation() InvalidationReport {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.lastInvalidation
}

func (rt *Runtime) PoolStats() PoolStats {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.pool == nil {
		return PoolStats{}
	}
	return rt.pool.Stats()
}

func (rt *Runtime) ActionOutputs() []ActionOutput {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]ActionOutput, 0)
	for _, id := range rt.graph.Topo {
		entry := rt.cache[id]
		if entry != nil && entry.Value.Action != nil {
			out = append(out, entry.Value.Action.Outputs...)
		}
	}
	return SortOutputs(out)
}

func (rt *Runtime) AxisSamples() []AxisSample {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]AxisSample, 0)
	for _, id := range rt.graph.Topo {
		entry := rt.cache[id]
		if entry != nil && entry.Value.Action != nil {
			out = append(out, entry.Value.Action.AxisSamples...)
		}
	}
	return SortAxisSamples(out)
}

func (rt *Runtime) TimingChecks() []TimingCheck {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]TimingCheck, 0)
	for _, id := range rt.graph.Topo {
		entry := rt.cache[id]
		if entry != nil && entry.Value.Action != nil {
			out = append(out, entry.Value.Action.TimingChecks...)
		}
	}
	return out
}

func (rt *Runtime) EventHash() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.graph.EventHash
}
