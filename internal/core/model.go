package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	AlgorithmVersion = "econ-sim/v2"
	SchemaVersion    = 1
)

type NodeType string

const (
	NodeFarm      NodeType = "farm"
	NodeMine      NodeType = "mine"
	NodeFactory   NodeType = "factory"
	NodeWarehouse NodeType = "warehouse"
)

func (t NodeType) Valid() bool {
	switch t {
	case NodeFarm, NodeMine, NodeFactory, NodeWarehouse:
		return true
	default:
		return false
	}
}

type IntentType string

const (
	IntentBuild        IntentType = "build"
	IntentUpgrade      IntentType = "upgrade"
	IntentDemolish     IntentType = "demolish"
	IntentAssignWorker IntentType = "assign_workers"
	IntentRecipe       IntentType = "recipe"
	IntentConsume      IntentType = "consume"
)

func (t IntentType) Valid() bool {
	switch t {
	case IntentBuild, IntentUpgrade, IntentDemolish, IntentAssignWorker, IntentRecipe, IntentConsume:
		return true
	default:
		return false
	}
}

// Params is the complete effective fixed-point recipe of a node. The engine
// never reads rates or capacities from two different Params values in one tick.
type Params struct {
	Outputs         map[string]int64 `json:"outputs"`
	Inputs          map[string]int64 `json:"inputs"`
	Capacity        map[string]int64 `json:"capacity"`
	WorkersRequired int              `json:"workers_required"`
}

func (p Params) Clone() Params {
	return Params{
		Outputs:         cloneMap(p.Outputs),
		Inputs:          cloneMap(p.Inputs),
		Capacity:        cloneMap(p.Capacity),
		WorkersRequired: p.WorkersRequired,
	}
}

func (p Params) Resources() []string {
	set := make(map[string]struct{}, len(p.Outputs)+len(p.Inputs)+len(p.Capacity))
	for k := range p.Outputs {
		set[k] = struct{}{}
	}
	for k := range p.Inputs {
		set[k] = struct{}{}
	}
	for k := range p.Capacity {
		set[k] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type Upgrade struct {
	Level           int              `json:"level"`
	Outputs         map[string]int64 `json:"outputs"`
	Inputs          map[string]int64 `json:"inputs"`
	Capacity        map[string]int64 `json:"capacity"`
	WorkersRequired int              `json:"workers_required"`
}

func (u Upgrade) Params() Params {
	return Params{
		Outputs:         cloneMap(u.Outputs),
		Inputs:          cloneMap(u.Inputs),
		Capacity:        cloneMap(u.Capacity),
		WorkersRequired: u.WorkersRequired,
	}
}

type Node struct {
	ID              string           `json:"id"`
	Type            NodeType         `json:"type"`
	Name            string           `json:"name,omitempty"`
	ParentID        string           `json:"parent_id,omitempty"`
	Active          bool             `json:"active"`
	Level           int              `json:"level"`
	Workers         int              `json:"workers"`
	MaxWorkers      int              `json:"max_workers"`
	Outputs         map[string]int64 `json:"outputs"`
	Inputs          map[string]int64 `json:"inputs"`
	Capacity        map[string]int64 `json:"capacity"`
	WorkersRequired int              `json:"workers_required"`
	InitialStock    map[string]int64 `json:"initial_stock"`
	Upgrades        []Upgrade        `json:"upgrades"`
}

func (n Node) BaseParams() Params {
	return Params{
		Outputs:         cloneMap(n.Outputs),
		Inputs:          cloneMap(n.Inputs),
		Capacity:        cloneMap(n.Capacity),
		WorkersRequired: n.WorkersRequired,
	}
}

type Intent struct {
	Seq       int64            `json:"seq"`
	Timestamp int64            `json:"timestamp"`
	Type      IntentType       `json:"type"`
	NodeID    string           `json:"node_id"`
	Level     *int             `json:"level,omitempty"`
	Workers   *int             `json:"workers,omitempty"`
	Recipe    *Params          `json:"recipe,omitempty"`
	Consume   map[string]int64 `json:"consume,omitempty"`
}

type Scenario struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	Seed          uint64   `json:"seed"`
	TickUnitMS    int64    `json:"tick_unit_ms"`
	ParamsVersion string   `json:"params_version"`
	FinalTick     int64    `json:"final_tick"`
	Nodes         []Node   `json:"nodes"`
	Intents       []Intent `json:"intents"`
	Truth         Truth    `json:"truth"`
	SceneHash     string   `json:"scene_hash,omitempty"`
}

type Truth struct {
	SampleTicks []int64         `json:"sample_ticks"`
	Snapshots   []TruthSnapshot `json:"snapshots"`
}

type TruthSnapshot struct {
	Tick            int64            `json:"tick"`
	Nodes           []NodeSnapshot   `json:"nodes"`
	GlobalStock     map[string]int64 `json:"global_stock"`
	GlobalNetRate   map[string]int64 `json:"global_net_rate"`
	Overflow        map[string]int64 `json:"overflow"`
	OverflowSources []OverflowSource `json:"overflow_sources"`
}

type OverflowSource struct {
	NodeID   string `json:"node_id"`
	Resource string `json:"resource"`
	Total    int64  `json:"total"`
	LastTick int64  `json:"last_tick"`
}

type NodeSnapshot struct {
	ID              string           `json:"id"`
	Type            NodeType         `json:"type"`
	ParentID        string           `json:"parent_id,omitempty"`
	Level           int              `json:"level"`
	Workers         int              `json:"workers"`
	WorkersRequired int              `json:"workers_required"`
	Active          bool             `json:"active"`
	Stalled         bool             `json:"stalled"`
	StallSince      *int64           `json:"stall_since,omitempty"`
	Stock           map[string]int64 `json:"stock"`
	Capacity        map[string]int64 `json:"capacity"`
	NetRate         map[string]int64 `json:"net_rate"`
	TotalOutput     map[string]int64 `json:"total_output"`
	TotalInput      map[string]int64 `json:"total_input"`
}

type Snapshot struct {
	Tick             int64            `json:"tick"`
	IntentOffset     int              `json:"intent_offset"`
	Nodes            []NodeSnapshot   `json:"nodes"`
	GlobalStock      map[string]int64 `json:"global_stock"`
	GlobalNetRate    map[string]int64 `json:"global_net_rate"`
	Overflow         map[string]int64 `json:"overflow"`
	OverflowSources  []OverflowSource `json:"overflow_sources"`
	ParamsVersion    string           `json:"params_version"`
	SceneHash        string           `json:"scene_hash"`
	AlgorithmVersion string           `json:"algorithm_version"`
	WindowIndex      int              `json:"window_index,omitempty"`
	Step             int              `json:"step,omitempty"`
}

type NodeState struct {
	ID             string           `json:"id"`
	Type           NodeType         `json:"type"`
	Name           string           `json:"name,omitempty"`
	ParentID       string           `json:"parent_id,omitempty"`
	Active         bool             `json:"active"`
	Level          int              `json:"level"`
	Workers        int              `json:"workers"`
	MaxWorkers     int              `json:"max_workers"`
	Params         Params           `json:"params"`
	RecipeOverride bool             `json:"recipe_override,omitempty"`
	Stock          map[string]int64 `json:"stock"`
	Stalled        bool             `json:"stalled"`
	StallSince     *int64           `json:"stall_since,omitempty"`
	NetRate        map[string]int64 `json:"net_rate"`
	TotalOutput    map[string]int64 `json:"total_output"`
	TotalInput     map[string]int64 `json:"total_input"`
}

func (n *NodeState) Clone() *NodeState {
	if n == nil {
		return nil
	}
	cp := *n
	cp.Params = n.Params.Clone()
	cp.Stock = cloneMap(n.Stock)
	cp.NetRate = cloneMap(n.NetRate)
	cp.TotalOutput = cloneMap(n.TotalOutput)
	cp.TotalInput = cloneMap(n.TotalInput)
	if n.StallSince != nil {
		t := *n.StallSince
		cp.StallSince = &t
	}
	return &cp
}

type State struct {
	Tick         int64                 `json:"tick"`
	IntentOffset int                   `json:"intent_offset"`
	Nodes        map[string]*NodeState `json:"nodes"`
	Overflow     map[string]int64      `json:"overflow"`
	OverflowLast map[string]int64      `json:"overflow_last_tick"`
}

func NewState(s *Scenario) (*State, error) {
	st := &State{
		Nodes:        make(map[string]*NodeState, len(s.Nodes)),
		Overflow:     make(map[string]int64),
		OverflowLast: make(map[string]int64),
	}
	for _, n := range s.Nodes {
		params := n.BaseParams()
		if n.Level > 0 {
			up, ok := n.UpgradeFor(n.Level)
			if !ok {
				return nil, NewError(CodeSchema, "initial level has no upgrade definition").
					At("nodes." + n.ID + ".level").ForNode(n.ID)
			}
			params = up.Params()
		}
		st.Nodes[n.ID] = &NodeState{
			ID:          n.ID,
			Type:        n.Type,
			Name:        n.Name,
			ParentID:    n.ParentID,
			Active:      n.Active,
			Level:       n.Level,
			Workers:     n.Workers,
			MaxWorkers:  n.MaxWorkers,
			Params:      params,
			Stock:       cloneMap(n.InitialStock),
			NetRate:     make(map[string]int64),
			TotalOutput: make(map[string]int64),
			TotalInput:  make(map[string]int64),
		}
	}
	for _, n := range st.Nodes {
		for _, resource := range sortedKeys(n.Params.Capacity) {
			cap := n.Params.Capacity[resource]
			if n.Stock[resource] <= cap {
				continue
			}
			excess := n.Stock[resource] - cap
			n.Stock[resource] = cap
			key := overflowKey(n.ID, resource)
			st.Overflow[key] += excess
			st.OverflowLast[key] = 0
		}
	}
	if err := st.Validate(s); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *State) Clone() *State {
	if s == nil {
		return nil
	}
	cp := &State{
		Tick:         s.Tick,
		IntentOffset: s.IntentOffset,
		Nodes:        make(map[string]*NodeState, len(s.Nodes)),
		Overflow:     cloneMap(s.Overflow),
		OverflowLast: cloneMap(s.OverflowLast),
	}
	for id, n := range s.Nodes {
		cp.Nodes[id] = n.Clone()
	}
	return cp
}

func (s *State) Validate(scene *Scenario) *Error {
	if s == nil {
		return NewError(CodeState, "state is nil")
	}
	if s.Tick < 0 || s.IntentOffset < 0 || s.IntentOffset > len(scene.Intents) {
		return NewError(CodeState, "state tick or intent offset is out of range")
	}
	if len(s.Nodes) != len(scene.Nodes) {
		return NewError(CodeState, "checkpoint node set does not match scene")
	}
	for _, n := range scene.Nodes {
		ns, ok := s.Nodes[n.ID]
		if !ok || ns == nil {
			return NewError(CodeState, "checkpoint is missing node").ForNode(n.ID)
		}
		if ns.Workers < 0 || ns.Workers > ns.MaxWorkers {
			return NewError(CodeState, "checkpoint worker count is out of range").ForNode(n.ID)
		}
		if ns.Level < 0 {
			return NewError(CodeState, "checkpoint level is negative").ForNode(n.ID)
		}
		for r, v := range ns.Stock {
			if !ValidFixed(v) {
				return NewError(CodeState, "checkpoint stock is not a valid fixed-point value").ForNode(n.ID).WithField("stock." + r)
			}
		}
		for r, capacity := range ns.Params.Capacity {
			if ns.Stock[r] > capacity {
				return NewError(CodeState, "checkpoint stock exceeds node capacity").
					ForNode(n.ID).WithField("stock."+r).WithValues(ns.Stock[r], capacity)
			}
		}
	}
	return nil
}

func (s *State) MarshalCanonical() ([]byte, error) {
	type canonical struct {
		Tick         int64                 `json:"tick"`
		IntentOffset int                   `json:"intent_offset"`
		Nodes        map[string]*NodeState `json:"nodes"`
		Overflow     map[string]int64      `json:"overflow"`
		OverflowLast map[string]int64      `json:"overflow_last_tick"`
	}
	return json.Marshal(canonical{
		Tick:         s.Tick,
		IntentOffset: s.IntentOffset,
		Nodes:        s.Nodes,
		Overflow:     s.Overflow,
		OverflowLast: s.OverflowLast,
	})
}

func DecodeState(data []byte) (*State, error) {
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, NewError(CodeDecode, "checkpoint state is not valid JSON").Wrap(err)
	}
	if st.Nodes == nil {
		return nil, NewError(CodeDecode, "checkpoint state has no nodes")
	}
	return &st, nil
}

func cloneMap(src map[string]int64) map[string]int64 {
	if src == nil {
		return make(map[string]int64)
	}
	dst := make(map[string]int64, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func overflowKey(nodeID, resource string) string {
	return nodeID + "\x00" + resource
}

func splitOverflowKey(key string) (string, string, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

func (s *Scenario) NodeMap() map[string]Node {
	out := make(map[string]Node, len(s.Nodes))
	for _, n := range s.Nodes {
		out[n.ID] = n
	}
	return out
}

func (n Node) UpgradeFor(level int) (Upgrade, bool) {
	for _, u := range n.Upgrades {
		if u.Level == level {
			return u, true
		}
	}
	return Upgrade{}, false
}

func ValidateParams(p Params) *Error {
	if p.Outputs == nil {
		return NewError(CodeParam, "outputs must be present").WithField("outputs")
	}
	if p.Inputs == nil {
		return NewError(CodeParam, "inputs must be present").WithField("inputs")
	}
	if p.Capacity == nil {
		return NewError(CodeParam, "capacity must be present").WithField("capacity")
	}
	if p.WorkersRequired < 0 {
		return NewError(CodeParam, "workers_required must be non-negative").WithField("workers_required")
	}
	for _, group := range []struct {
		name string
		m    map[string]int64
	}{
		{"outputs", p.Outputs},
		{"inputs", p.Inputs},
		{"capacity", p.Capacity},
	} {
		for _, r := range sortedKeys(group.m) {
			if r == "" {
				return NewError(CodeParam, "resource name must not be empty").WithField(group.name)
			}
			if !ValidFixed(group.m[r]) {
				return NewError(CodeParam, "fixed-point value is outside the legal range").WithField(group.name + "." + r)
			}
		}
	}
	for r := range p.Outputs {
		if _, ok := p.Capacity[r]; !ok {
			return NewError(CodeParam, "every output resource needs a capacity").WithField("capacity." + r)
		}
	}
	return nil
}

func (s *Scenario) String() string {
	return fmt.Sprintf("%s(nodes=%d,intents=%d,tick=%d)", s.ID, len(s.Nodes), len(s.Intents), s.FinalTick)
}

func CloneScenario(s *Scenario) (*Scenario, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, NewError(CodeState, "failed to encode scenario clone").Wrap(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var clone Scenario
	if err := dec.Decode(&clone); err != nil {
		return nil, NewError(CodeState, "failed to decode scenario clone").Wrap(err)
	}
	return &clone, nil
}
