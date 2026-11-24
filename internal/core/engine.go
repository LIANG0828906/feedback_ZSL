package core

import (
	"fmt"
	"sort"
)

type Engine struct {
	scene *Scenario
	state *State
	nodes map[string]Node
	topo  []string
}

func NewEngine(scene *Scenario) (*Engine, error) {
	if err := ValidateScenario(scene); err != nil {
		return nil, err
	}
	if scene.SceneHash == "" {
		h, err := ComputeSceneHash(scene)
		if err != nil {
			return nil, NewError(CodeState, "failed to hash scene").Wrap(err)
		}
		scene.SceneHash = h
	}
	st, err := NewState(scene)
	if err != nil {
		return nil, err
	}
	return &Engine{
		scene: scene,
		state: st,
		nodes: scene.NodeMap(),
		topo:  TopoOrder(scene),
	}, nil
}

func NewEngineAt(scene *Scenario, state *State) (*Engine, error) {
	if err := ValidateScenario(scene); err != nil {
		return nil, err
	}
	if scene.SceneHash == "" {
		h, err := ComputeSceneHash(scene)
		if err != nil {
			return nil, NewError(CodeState, "failed to hash scene").Wrap(err)
		}
		scene.SceneHash = h
	}
	if err := state.Validate(scene); err != nil {
		return nil, err
	}
	expectedOffset := sort.Search(len(scene.Intents), func(i int) bool {
		return scene.Intents[i].Timestamp >= state.Tick
	})
	if expectedOffset != state.IntentOffset {
		return nil, NewError(CodeState, "checkpoint intent offset does not match logical time").
			At(fmt.Sprintf("intents[%d]", state.IntentOffset)).
			WithValues(state.IntentOffset, expectedOffset)
	}
	return &Engine{
		scene: scene,
		state: state.Clone(),
		nodes: scene.NodeMap(),
		topo:  TopoOrder(scene),
	}, nil
}

func (e *Engine) Scene() *Scenario { return e.scene }

func (e *Engine) State() *State { return e.state }

func (e *Engine) Restore(state *State) error {
	if err := state.Validate(e.scene); err != nil {
		return err
	}
	e.state = state.Clone()
	return nil
}

func (e *Engine) AdvanceTo(endTick int64) error {
	if endTick < e.state.Tick {
		return NewError(CodeState, "cannot advance backwards").
			WithValues(endTick, e.state.Tick)
	}
	if endTick > e.scene.FinalTick {
		return NewError(CodeParam, "advance target exceeds scenario final_tick").
			WithField("final_tick").WithValues(endTick, e.scene.FinalTick)
	}
	for e.state.Tick < endTick {
		if err := e.Step(); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Step() error {
	if e.state.Tick >= e.scene.FinalTick {
		return NewError(CodeParam, "scenario has reached final_tick").
			WithField("final_tick")
	}
	backup := e.state.Clone()
	if err := e.applyIntents(e.state.Tick); err != nil {
		e.state = backup
		return err
	}
	if err := e.produce(); err != nil {
		e.state = backup
		return err
	}
	e.state.Tick++
	return nil
}

func (e *Engine) applyIntents(tick int64) *Error {
	for e.state.IntentOffset < len(e.scene.Intents) {
		in := e.scene.Intents[e.state.IntentOffset]
		if in.Timestamp < tick {
			return NewError(CodeState, "unconsumed intent lies before current logical time").
				At(fmt.Sprintf("intents[%d].timestamp", e.state.IntentOffset)).
				ForIntent(in.Seq, e.state.IntentOffset).
				WithValues(in.Timestamp, tick)
		}
		if in.Timestamp != tick {
			break
		}
		if err := e.applyIntent(in, e.state.IntentOffset, tick); err != nil {
			return err
		}
		e.state.IntentOffset++
	}
	return nil
}

func (e *Engine) applyIntent(in Intent, idx int, tick int64) *Error {
	node, ok := e.state.Nodes[in.NodeID]
	if !ok {
		return NewError(CodeUnit, "intent targets a node that does not exist").
			At(fmt.Sprintf("intents[%d].node_id", idx)).ForNode(in.NodeID).
			ForIntent(in.Seq, idx)
	}
	switch in.Type {
	case IntentBuild:
		node.Active = true
	case IntentUpgrade:
		if in.Level == nil {
			return NewError(CodeUnit, "upgrade is missing target level").
				At(fmt.Sprintf("intents[%d].level", idx)).ForIntent(in.Seq, idx)
		}
		expected := node.Level + 1
		if *in.Level != expected {
			return NewError(CodeState, "upgrade level is not the next contiguous level").
				At(fmt.Sprintf("intents[%d].level", idx)).ForNode(in.NodeID).
				ForIntent(in.Seq, idx).WithValues(*in.Level, expected)
		}
		def, exists := e.nodes[in.NodeID]
		if !exists {
			return NewError(CodeUnit, "upgrade definition node does not exist").
				ForNode(in.NodeID).ForIntent(in.Seq, idx)
		}
		upgrade, exists := def.UpgradeFor(*in.Level)
		if !exists {
			return NewError(CodeState, "upgrade level has no definition").
				At(fmt.Sprintf("intents[%d].level", idx)).ForNode(in.NodeID).
				ForIntent(in.Seq, idx)
		}
		newParams := upgrade.Params()
		if err := ValidateParams(newParams); err != nil {
			err.Code = CodeState
			err.ForNode(in.NodeID).ForIntent(in.Seq, idx)
			return err
		}
		// The level, recipe, rates and capacity are one atomic assignment.
		node.Level = *in.Level
		node.Params = newParams
		node.RecipeOverride = false
		node.NetRate = make(map[string]int64)
		e.enforceCapacity(node, tick)
		if node.Active && node.Workers < node.Params.WorkersRequired {
			markStalled(node, tick)
		}
	case IntentDemolish:
		node.Active = false
		node.Stalled = false
		node.StallSince = nil
		node.NetRate = make(map[string]int64)
	case IntentAssignWorker:
		if in.Workers == nil {
			return NewError(CodeUnit, "worker intent is missing a worker count").
				At(fmt.Sprintf("intents[%d].workers", idx)).ForIntent(in.Seq, idx)
		}
		if *in.Workers < 0 || *in.Workers > node.MaxWorkers {
			return NewError(CodeUnit, "worker assignment is outside the node limit").
				At(fmt.Sprintf("intents[%d].workers", idx)).ForNode(in.NodeID).
				ForIntent(in.Seq, idx).WithValues(*in.Workers, node.MaxWorkers)
		}
		node.Workers = *in.Workers
		if node.Active && node.Workers < node.Params.WorkersRequired {
			markStalled(node, tick)
		}
	case IntentRecipe:
		if in.Recipe == nil {
			return NewError(CodeUnit, "recipe intent is missing a recipe").
				At(fmt.Sprintf("intents[%d].recipe", idx)).ForIntent(in.Seq, idx)
		}
		if err := ValidateParams(*in.Recipe); err != nil {
			err.Code = CodeState
			err.ForNode(in.NodeID).ForIntent(in.Seq, idx)
			return err
		}
		node.Params = in.Recipe.Clone()
		node.RecipeOverride = true
		node.NetRate = make(map[string]int64)
		e.enforceCapacity(node, tick)
		if node.Active && node.Workers < node.Params.WorkersRequired {
			markStalled(node, tick)
		}
	case IntentConsume:
		for _, r := range sortedKeys(in.Consume) {
			amount := in.Consume[r]
			if node.Stock[r] < amount {
				return NewError(CodeResource, "recipe consumption exceeds available stock").
					At(fmt.Sprintf("intents[%d].consume.%s", idx, r)).ForNode(in.NodeID).
					ForIntent(in.Seq, idx).WithField("consume."+r).
					WithValues(amount, node.Stock[r])
			}
		}
		for r, amount := range in.Consume {
			node.Stock[r] -= amount
		}
	default:
		return NewError(CodeUnit, "unknown intent type").
			At(fmt.Sprintf("intents[%d].type", idx)).ForIntent(in.Seq, idx)
	}
	return nil
}

func (e *Engine) produce() *Error {
	for _, id := range e.topo {
		node := e.state.Nodes[id]
		node.NetRate = make(map[string]int64)
		if !node.Active {
			node.Stalled = false
			node.StallSince = nil
			continue
		}
		if node.Workers < node.Params.WorkersRequired {
			markStalled(node, e.state.Tick)
			continue
		}
		plan := make(map[string]map[string]int64)
		short := false
		for _, resource := range sortedKeys(node.Params.Inputs) {
			need := node.Params.Inputs[resource]
			if need == 0 {
				continue
			}
			alloc, ok := e.inputPlan(node, resource, need)
			if !ok {
				short = true
				break
			}
			for src, amount := range alloc {
				if plan[src] == nil {
					plan[src] = make(map[string]int64)
				}
				plan[src][resource] += amount
			}
		}
		if short {
			markStalled(node, e.state.Tick)
			continue
		}
		for src, resources := range plan {
			srcNode := e.state.Nodes[src]
			for r, amount := range resources {
				srcNode.Stock[r] -= amount
			}
		}
		for r, amount := range node.Params.Inputs {
			if amount > 0 {
				node.TotalInput[r] += amount
			}
		}
		for _, resource := range sortedKeys(node.Params.Outputs) {
			amount := node.Params.Outputs[resource]
			cap := node.Params.Capacity[resource]
			stock := node.Stock[resource]
			room := int64(0)
			if cap > stock {
				room = cap - stock
			}
			add := MinInt64(amount, room)
			overflow := amount - add
			node.Stock[resource] = stock + add
			node.TotalOutput[resource] += amount
			if overflow > 0 {
				key := overflowKey(id, resource)
				e.state.Overflow[key] += overflow
				e.state.OverflowLast[key] = e.state.Tick
			}
		}
		for r, out := range node.Params.Outputs {
			node.NetRate[r] += out
		}
		for r, in := range node.Params.Inputs {
			node.NetRate[r] -= in
		}
		node.Stalled = false
		node.StallSince = nil
	}
	return nil
}

// inputPlan first searches the node itself, then walks the parent chain. It
// never mutates state, so a shortfall for any resource leaves all inputs intact.
func (e *Engine) inputPlan(node *NodeState, resource string, need int64) (map[string]int64, bool) {
	remaining := need
	plan := make(map[string]int64)
	visited := make(map[string]struct{})
	current := node
	for current != nil {
		if _, seen := visited[current.ID]; seen {
			break
		}
		visited[current.ID] = struct{}{}
		if available := current.Stock[resource]; available > 0 {
			take := MinInt64(available, remaining)
			if take > 0 {
				plan[current.ID] = take
				remaining -= take
			}
		}
		if remaining == 0 {
			return plan, true
		}
		if current.ParentID == "" {
			break
		}
		current = e.state.Nodes[current.ParentID]
		if current == nil {
			break
		}
	}
	return nil, false
}

func markStalled(node *NodeState, tick int64) {
	if !node.Stalled || node.StallSince == nil {
		t := tick
		node.StallSince = &t
	}
	node.Stalled = true
}

// enforceCapacity keeps the stock invariant when an atomic parameter switch
// lowers a capacity. The removed stock is attributed to the same overflow
// source ledger as production overflow.
func (e *Engine) enforceCapacity(node *NodeState, tick int64) {
	for _, resource := range sortedKeys(node.Params.Capacity) {
		cap := node.Params.Capacity[resource]
		if node.Stock[resource] <= cap {
			continue
		}
		excess := node.Stock[resource] - cap
		node.Stock[resource] = cap
		key := overflowKey(node.ID, resource)
		e.state.Overflow[key] += excess
		e.state.OverflowLast[key] = tick
	}
}

func (e *Engine) CloneState() *State { return e.state.Clone() }

// TopoDepth is exposed for the workbench hierarchy display.
func TopoDepth(s *Scenario) map[string]int {
	nodes := s.NodeMap()
	depth := make(map[string]int, len(nodes))
	var walk func(string, map[string]bool) int
	walk = func(id string, path map[string]bool) int {
		if d, ok := depth[id]; ok {
			return d
		}
		if path[id] {
			return 0
		}
		path[id] = true
		d := 0
		if p := nodes[id].ParentID; p != "" {
			d = walk(p, path) + 1
		}
		delete(path, id)
		depth[id] = d
		return d
	}
	for id := range nodes {
		walk(id, make(map[string]bool))
	}
	return depth
}
