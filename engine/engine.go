package engine

import (
	"encoding/json"
	"sort"

	"economycore/model"
)

// New constructs an engine at the project's initial state.
func New(m *model.Model, opts Options) (*Engine, error) {
	if m == nil {
		return nil, model.NewError(model.ESchema, "", "nil model")
	}
	modelCopy := m.Clone()
	modelCopy.Normalize()
	if modelCopy.ConversionVersion == "" {
		modelCopy.ConversionVersion = model.DefaultConversionVersion
	}
	if err := modelCopy.Validate(); err != nil {
		return nil, err
	}
	graph, err := model.BuildGraph(modelCopy)
	if err != nil {
		return nil, err
	}
	state, err := initialRuntimeState(modelCopy)
	if err != nil {
		return nil, err
	}
	if err := validateWorkerAssignment(modelCopy, &state); err != nil {
		return nil, err
	}
	if opts.AlgorithmVersion == "" {
		opts.AlgorithmVersion = model.DefaultAlgorithmVersion
	}
	return &Engine{
		model:         modelCopy,
		graph:         graph,
		opts:          opts,
		state:         state,
		cache:         make(map[cacheKey]nodeTransition),
		forcedInvalid: make(map[string][]string),
		lastReasons:   make(map[string][]string),
	}, nil
}

// Model returns a defensive copy of the currently edited model.
func (e *Engine) Model() *model.Model {
	return e.model.Clone()
}

// Graph returns a defensive copy of the production dependency graph.
func (e *Engine) Graph() *model.Graph {
	clone := &model.Graph{
		Order:   append([]string(nil), e.graph.Order...),
		Edges:   make(map[string][]string, len(e.graph.Edges)),
		Reverse: make(map[string][]string, len(e.graph.Reverse)),
	}
	for key, value := range e.graph.Edges {
		clone.Edges[key] = append([]string(nil), value...)
	}
	for key, value := range e.graph.Reverse {
		clone.Reverse[key] = append([]string(nil), value...)
	}
	return clone
}

// State returns a defensive copy of the runtime state.
func (e *Engine) State() model.RuntimeState {
	return cloneState(e.state)
}

// RestoreState loads a persisted state and clears transient cache entries.
func (e *Engine) RestoreState(state model.RuntimeState) error {
	if state.NextCycle < 0 || state.NextCycle > e.model.TotalCycles() {
		return model.NewError(model.ESchema, "/state/nextCycle", "cycle outside project range")
	}
	if state.NextCycle%int64(e.model.WindowCycles) != 0 {
		return model.NewError(model.ESchema, "/state/nextCycle", "restored state is not at a window boundary")
	}
	if state.IntentIndex < 0 || state.IntentIndex > len(e.model.Intents) {
		return model.NewError(model.ESchema, "/state/intentIndex", "intent cursor outside range")
	}
	out := cloneState(state)
	for _, resource := range e.model.Resources {
		if _, ok := out.Stocks[resource.ID]; !ok {
			out.Stocks[resource.ID] = 0
		}
		if _, ok := out.Capacities[resource.ID]; !ok {
			out.Capacities[resource.ID] = 0
		}
	}
	for _, node := range e.model.Nodes {
		if _, ok := out.Nodes[node.ID]; !ok {
			return model.NewError(model.ESchema, "/state/nodes/"+node.ID, "node state missing")
		}
	}
	if err := validateWorkerAssignment(e.model, &out); err != nil {
		return err
	}
	e.state = out
	e.cache = make(map[cacheKey]nodeTransition)
	e.forcedInvalid = make(map[string][]string)
	e.lastReasons = make(map[string][]string)
	return nil
}

// Reset restores the initial project state while retaining the settlement
// cache for incremental runs.
func (e *Engine) Reset() error {
	state, err := initialRuntimeState(e.model)
	if err != nil {
		return err
	}
	if err := validateWorkerAssignment(e.model, &state); err != nil {
		return err
	}
	e.state = state
	return nil
}

// Clone deep-copies the engine.
func (e *Engine) Clone() *Engine {
	cache := make(map[cacheKey]nodeTransition, len(e.cache))
	for key, value := range e.cache {
		cache[key] = cloneTransition(value)
	}
	forced := make(map[string][]string, len(e.forcedInvalid))
	for key, value := range e.forcedInvalid {
		forced[key] = append([]string(nil), value...)
	}
	reasons := make(map[string][]string, len(e.lastReasons))
	for key, value := range e.lastReasons {
		reasons[key] = append([]string(nil), value...)
	}
	return &Engine{
		model:         e.model.Clone(),
		graph:         cloneGraph(e.graph),
		opts:          e.opts,
		state:         cloneState(e.state),
		cache:         cache,
		forcedInvalid: forced,
		lastReasons:   reasons,
	}
}

// StateJSON serializes checkpointable runtime state.
func (e *Engine) StateJSON() ([]byte, error) {
	return json.Marshal(e.state)
}

// DecodeState decodes a checkpointable runtime state.
func DecodeState(data []byte) (model.RuntimeState, error) {
	var state model.RuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return model.RuntimeState{}, model.NewError(model.ESchema, "/state", "invalid state JSON").WithCause(err)
	}
	return state, nil
}

func initialRuntimeState(m *model.Model) (model.RuntimeState, error) {
	state := model.RuntimeState{
		NextCycle:      0,
		IntentIndex:    0,
		Stocks:         cloneMap(m.InitialStocks),
		Capacities:     cloneMap(m.Capacities),
		Nodes:          make(map[string]model.NodeRuntime, len(m.Nodes)),
		OverflowTotals: make(map[string]int64),
	}
	for _, resource := range m.Resources {
		if _, ok := state.Stocks[resource.ID]; !ok {
			state.Stocks[resource.ID] = 0
		}
		if _, ok := state.Capacities[resource.ID]; !ok {
			state.Capacities[resource.ID] = 0
		}
	}
	for _, node := range m.Nodes {
		state.Nodes[node.ID] = model.NodeRuntime{
			Built:           node.Built,
			Level:           node.Level,
			Recipe:          node.Recipe,
			Workers:         node.Workers,
			Progress:        0,
			Stalled:         false,
			StallStartCycle: -1,
			RateBonusFixed:  0,
		}
	}
	return state, nil
}

func validateWorkerAssignment(m *model.Model, state *model.RuntimeState) error {
	var assigned int64
	for _, node := range m.Nodes {
		rt, ok := state.Nodes[node.ID]
		if !ok {
			return model.NewError(model.ESchema, "/state/nodes/"+node.ID, "missing runtime node")
		}
		if rt.Workers < 0 {
			return model.NewError(model.ESchema, "/state/nodes/"+node.ID+"/workers", "negative worker assignment")
		}
		if rt.Built {
			value, err := addInt64(assigned, int64(rt.Workers))
			if err != nil {
				return model.NewError(model.EOvf, "/state/workers", err.Error())
			}
			assigned = value
		}
	}
	if assigned > m.WorkersTotal {
		return model.NewError(model.ERes, "/state/workers", "assigned workers exceed workforce")
	}
	return nil
}

func cloneState(state model.RuntimeState) model.RuntimeState {
	out := model.RuntimeState{
		NextCycle:      state.NextCycle,
		IntentIndex:    state.IntentIndex,
		Stocks:         cloneMap(state.Stocks),
		Capacities:     cloneMap(state.Capacities),
		Nodes:          make(map[string]model.NodeRuntime, len(state.Nodes)),
		OverflowTotals: cloneMap(state.OverflowTotals),
	}
	for key, value := range state.Nodes {
		out.Nodes[key] = value
	}
	return out
}

func cloneMap(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneTransition(in nodeTransition) nodeTransition {
	out := in
	out.Consumed = cloneMap(in.Consumed)
	out.Produced = cloneMap(in.Produced)
	return out
}

func cloneGraph(graph *model.Graph) *model.Graph {
	out := &model.Graph{
		Order:   append([]string(nil), graph.Order...),
		Edges:   make(map[string][]string, len(graph.Edges)),
		Reverse: make(map[string][]string, len(graph.Reverse)),
	}
	for key, value := range graph.Edges {
		out.Edges[key] = append([]string(nil), value...)
	}
	for key, value := range graph.Reverse {
		out.Reverse[key] = append([]string(nil), value...)
	}
	return out
}

func sortedMapKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
