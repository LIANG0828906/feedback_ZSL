package core

import (
	"fmt"
	"sort"
	"strings"
)

func ValidateScenario(s *Scenario) *Error {
	if s == nil {
		return NewError(CodeSchema, "scenario is nil")
	}
	if s.SchemaVersion != SchemaVersion {
		return NewError(CodeSchema, "unsupported schema_version").
			At("schema_version").WithValues(s.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(s.ID) == "" {
		return NewError(CodeSchema, "scenario id must not be empty").At("id")
	}
	if s.TickUnitMS <= 0 {
		return NewError(CodeSchema, "tick_unit_ms must be positive").At("tick_unit_ms")
	}
	if s.ParamsVersion == "" {
		return NewError(CodeSchema, "params_version must not be empty").At("params_version")
	}
	if s.FinalTick < 0 {
		return NewError(CodeSchema, "final_tick must be non-negative").At("final_tick")
	}
	if len(s.Nodes) == 0 {
		return NewError(CodeSchema, "scenario must contain at least one node").At("nodes")
	}
	if len(s.Intents) == 0 {
		return NewError(CodeSchema, "scenario must contain at least one intent").At("intents")
	}

	nodes := make(map[string]Node, len(s.Nodes))
	for i, n := range s.Nodes {
		path := fmt.Sprintf("nodes[%d]", i)
		if n.ID == "" {
			return NewError(CodeSchema, "node id must not be empty").At(path + ".id")
		}
		if _, exists := nodes[n.ID]; exists {
			return NewError(CodeSchema, "duplicate node id").At(path + ".id").ForNode(n.ID)
		}
		nodes[n.ID] = n
		if !n.Type.Valid() {
			return NewError(CodeSchema, "unknown node type").At(path+".type").
				ForNode(n.ID).WithValues(n.Type, "farm|mine|factory|warehouse")
		}
		if n.Level < 0 {
			return NewError(CodeSchema, "level must be non-negative").At(path + ".level").ForNode(n.ID)
		}
		if n.Workers < 0 || n.MaxWorkers < 0 || n.Workers > n.MaxWorkers {
			return NewError(CodeSchema, "worker values are inconsistent").At(path + ".workers").ForNode(n.ID)
		}
		if n.WorkersRequired < 0 {
			return NewError(CodeSchema, "workers_required must be non-negative").
				At(path + ".workers_required").ForNode(n.ID)
		}
		if n.Outputs == nil || n.Inputs == nil || n.Capacity == nil {
			field := "outputs"
			if n.Inputs == nil {
				field = "inputs"
			} else if n.Capacity == nil {
				field = "capacity"
			}
			return NewError(CodeSchema, "required node map is missing or null").
				At(path + "." + field).ForNode(n.ID).WithField(field)
		}
		if err := validateParamMaps(n.ID, path, n.BaseParams()); err != nil {
			return err
		}
		for _, r := range sortedKeys(n.InitialStock) {
			v := n.InitialStock[r]
			if r == "" || !ValidFixed(v) {
				return NewError(CodeSchema, "initial_stock must contain legal fixed-point values").
					At(path + ".initial_stock." + r).ForNode(n.ID).WithField("initial_stock." + r)
			}
		}
		lastLevel := 0
		for j, u := range n.Upgrades {
			upath := fmt.Sprintf("%s.upgrades[%d]", path, j)
			if u.Outputs == nil || u.Inputs == nil || u.Capacity == nil {
				field := "outputs"
				if u.Inputs == nil {
					field = "inputs"
				} else if u.Capacity == nil {
					field = "capacity"
				}
				return NewError(CodeSchema, "required upgrade map is missing or null").
					At(upath + "." + field).ForNode(n.ID).WithField(field)
			}
			if u.Level <= lastLevel {
				return NewError(CodeSchema, "upgrade levels must be strictly increasing").
					At(upath + ".level").ForNode(n.ID)
			}
			if u.Level != lastLevel+1 {
				return NewError(CodeSchema, "upgrade levels must be contiguous from 1").
					At(upath+".level").ForNode(n.ID).WithValues(u.Level, lastLevel+1)
			}
			lastLevel = u.Level
			if err := validateParamMaps(n.ID, upath, u.Params()); err != nil {
				return err
			}
		}
		if n.Level > lastLevel {
			return NewError(CodeSchema, "initial level has no upgrade definition").
				At(path + ".level").ForNode(n.ID)
		}
	}

	for i, n := range s.Nodes {
		if n.ParentID == "" {
			continue
		}
		if _, ok := nodes[n.ParentID]; !ok {
			return NewError(CodeSchema, "parent node does not exist").
				At(fmt.Sprintf("nodes[%d].parent_id", i)).ForNode(n.ID).
				WithField("parent_id").WithValues(n.ParentID, nil)
		}
	}
	if err := validateAcyclic(s, nodes); err != nil {
		return err
	}

	var prevTick int64 = -1
	seenSeq := make(map[int64]struct{}, len(s.Intents))
	for i, in := range s.Intents {
		path := fmt.Sprintf("intents[%d]", i)
		if in.Seq < 0 {
			return NewError(CodeSchema, "seq must be non-negative").
				At(path+".seq").ForIntent(in.Seq, i)
		}
		if _, exists := seenSeq[in.Seq]; exists {
			return NewError(CodeSchema, "intent seq must be unique").
				At(path+".seq").ForIntent(in.Seq, i)
		}
		seenSeq[in.Seq] = struct{}{}
		if in.Timestamp < 0 {
			return NewError(CodeSchema, "timestamp must be non-negative").
				At(path+".timestamp").ForIntent(in.Seq, i)
		}
		if in.Timestamp > s.FinalTick {
			return NewError(CodeSchema, "intent timestamp exceeds final_tick").
				At(path+".timestamp").ForIntent(in.Seq, i).
				WithValues(in.Timestamp, s.FinalTick)
		}
		if in.Timestamp < prevTick {
			return NewError(CodeSchema, "intent timestamps must be monotonic").
				At(path+".timestamp").ForIntent(in.Seq, i).
				WithValues(in.Timestamp, prevTick)
		}
		prevTick = in.Timestamp
		if _, ok := nodes[in.NodeID]; !ok {
			return NewError(CodeSchema, "intent target node does not exist").
				At(path+".node_id").ForIntent(in.Seq, i).ForNode(in.NodeID).
				WithField("node_id").WithValues(in.NodeID, nil)
		}
		if !in.Type.Valid() {
			return NewError(CodeSchema, "unknown intent type").
				At(path+".type").ForIntent(in.Seq, i)
		}
		switch in.Type {
		case IntentUpgrade:
			if in.Level == nil || *in.Level < 0 {
				return NewError(CodeSchema, "upgrade intent needs a non-negative level").
					At(path+".level").ForIntent(in.Seq, i)
			}
		case IntentAssignWorker:
			if in.Workers == nil || *in.Workers < 0 {
				return NewError(CodeSchema, "worker intent needs a non-negative worker count").
					At(path+".workers").ForIntent(in.Seq, i)
			}
		case IntentRecipe:
			if in.Recipe == nil {
				return NewError(CodeSchema, "recipe intent needs a recipe").
					At(path+".recipe").ForIntent(in.Seq, i)
			}
			if err := ValidateParams(*in.Recipe); err != nil {
				err.Path = path + ".recipe." + err.Field
				err.ForIntent(in.Seq, i).ForNode(in.NodeID)
				if err.Code == CodeParam {
					err.Code = CodeSchema
				}
				return err
			}
		case IntentConsume:
			if in.Consume == nil {
				return NewError(CodeSchema, "consume intent needs a consume map").
					At(path+".consume").ForIntent(in.Seq, i)
			}
			for _, r := range sortedKeys(in.Consume) {
				if !ValidFixed(in.Consume[r]) {
					return NewError(CodeSchema, "consume value must be a legal fixed-point value").
						At(path+".consume."+r).ForIntent(in.Seq, i).
						WithField("consume." + r)
				}
			}
		}
	}

	if len(s.Truth.SampleTicks) != len(s.Truth.Snapshots) {
		return NewError(CodeSchema, "truth sample_ticks and snapshots must have equal lengths").
			At("truth.snapshots")
	}
	var prevSample int64 = -1
	for i, tick := range s.Truth.SampleTicks {
		if tick < prevSample {
			return NewError(CodeSchema, "truth sample ticks must be monotonic").
				At(fmt.Sprintf("truth.sample_ticks[%d]", i))
		}
		if s.Truth.Snapshots[i].Tick != tick {
			return NewError(CodeSchema, "truth snapshot tick does not match sample_ticks").
				At(fmt.Sprintf("truth.snapshots[%d].tick", i))
		}
		prevSample = tick
	}
	return nil
}

func validateParamMaps(nodeID, path string, p Params) *Error {
	if p.Outputs == nil {
		return NewError(CodeSchema, "outputs must be present").At(path + ".outputs").ForNode(nodeID).WithField("outputs")
	}
	if p.Inputs == nil {
		return NewError(CodeSchema, "inputs must be present").At(path + ".inputs").ForNode(nodeID).WithField("inputs")
	}
	if p.Capacity == nil {
		return NewError(CodeSchema, "capacity must be present").At(path + ".capacity").ForNode(nodeID).WithField("capacity")
	}
	groups := []struct {
		name string
		m    map[string]int64
	}{
		{"outputs", p.Outputs},
		{"inputs", p.Inputs},
		{"capacity", p.Capacity},
	}
	for _, g := range groups {
		for _, r := range sortedKeys(g.m) {
			if r == "" {
				return NewError(CodeSchema, "resource name must not be empty").
					At(path + "." + g.name).ForNode(nodeID).WithField(g.name)
			}
			if !ValidFixed(g.m[r]) {
				return NewError(CodeSchema, "rate/capacity must be a non-negative fixed-point integer").
					At(path+"."+g.name+"."+r).ForNode(nodeID).
					WithField(g.name+"."+r).WithValues(g.m[r], "0..<="+fmt.Sprint(MaxFixedValue))
			}
		}
	}
	for r := range p.Outputs {
		if _, ok := p.Capacity[r]; !ok {
			return NewError(CodeSchema, "output resource has no capacity").
				At(path + ".capacity").ForNode(nodeID).WithField("capacity." + r)
		}
	}
	if p.WorkersRequired < 0 {
		return NewError(CodeSchema, "workers_required must be non-negative").
			At(path + ".workers_required").ForNode(nodeID).WithField("workers_required")
	}
	return nil
}

func validateAcyclic(s *Scenario, nodes map[string]Node) *Error {
	state := make(map[string]uint8, len(nodes))
	var stack []string
	var visit func(id string) *Error
	visit = func(id string) *Error {
		switch state[id] {
		case 1:
			start := 0
			for i, v := range stack {
				if v == id {
					start = i
					break
				}
			}
			cycle := append(append([]string(nil), stack[start:]...), id)
			idx := nodeIndex(s, id)
			return NewError(CodeSchema, "production chain contains a cycle: "+strings.Join(cycle, " -> ")).
				At(fmt.Sprintf("nodes[%d].parent_id", idx)).ForNode(id).
				WithField("parent_id")
		case 2:
			return nil
		}
		state[id] = 1
		stack = append(stack, id)
		if parent := nodes[id].ParentID; parent != "" {
			if err := visit(parent); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func nodeIndex(s *Scenario, id string) int {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			return i
		}
	}
	return 0
}
