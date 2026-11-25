package model

import (
	"fmt"
	"sort"
)

func ValidateProject(p *Project) error {
	if p == nil {
		return NewError(CodeFieldRequired, "project", "project is nil")
	}
	if p.TickMS <= 0 {
		return NewError(CodeFieldInvalid, "tick_ms", "must be positive integer milliseconds")
	}
	if p.RequestTimeoutBudgetMS <= 0 {
		err := NewError(CodeTimeoutBudgetZero, "request_timeout_budget_ms", "timeout budget must be greater than zero")
		return WithContext(err, "value", p.RequestTimeoutBudgetMS)
	}
	if err := validateLimits(p.Limits); err != nil {
		return err
	}

	serviceNames := map[string]bool{}
	for i, svc := range p.Services {
		if svc.Name == "" {
			return NewError(CodeFieldRequired, fmt.Sprintf("services[%d].name", i), "service name is required")
		}
		if serviceNames[svc.Name] {
			return NewError(CodeFieldConflict, fmt.Sprintf("services[%d].name", i), "duplicate service %q", svc.Name)
		}
		serviceNames[svc.Name] = true
		if svc.LatencyMS < 0 {
			return NewError(CodeNegativeDelay, fmt.Sprintf("services[%d].latency_ms", i), "latency must not be negative")
		}
	}

	edgeIDs := map[string]bool{}
	adj := map[string][]string{}
	for i, edge := range p.Edges {
		field := fmt.Sprintf("edges[%d]", i)
		if edge.ID == "" {
			return NewError(CodeFieldRequired, field+".id", "edge id is required")
		}
		if edgeIDs[edge.ID] {
			return NewError(CodeFieldConflict, field+".id", "duplicate edge id %q", edge.ID)
		}
		edgeIDs[edge.ID] = true
		if !serviceNames[edge.From] || !serviceNames[edge.To] {
			err := NewError(CodeTargetMissingService, field, "edge %q points to a missing service", edge.ID)
			return WithContext(err, "edge_id", edge.ID)
		}
		if edge.LatencyMS < 0 {
			return NewError(CodeNegativeDelay, field+".latency_ms", "edge latency must not be negative")
		}
		adj[edge.From] = append(adj[edge.From], edge.To)
	}
	if cycle := findCycle(adj); cycle != nil {
		err := NewError(CodeCycleDependency, "edges", "dependency cycle detected: %v", cycle)
		return WithContext(err, "cycle", cycle)
	}

	for i, ins := range p.Instructions {
		if err := validateInstruction(ins, i, serviceNames, edgeIDs, p); err != nil {
			return err
		}
	}
	for i, ex := range p.Exemptions {
		field := fmt.Sprintf("exemptions[%d]", i)
		if ex.ID == "" {
			return NewError(CodeFieldRequired, field+".id", "exemption id is required")
		}
		if err := validateSelectorTarget(ex.Selector, field+".selector", serviceNames, edgeIDs); err != nil {
			return err
		}
	}
	for i, req := range p.Requests {
		field := fmt.Sprintf("requests[%d]", i)
		if req.ID == "" {
			return NewError(CodeFieldRequired, field+".id", "request id is required")
		}
		if !serviceNames[req.Entry] {
			return NewError(CodeTargetMissingService, field+".entry", "entry service %q is missing", req.Entry)
		}
		for j, name := range req.Path {
			if !serviceNames[name] {
				return NewError(CodeTargetMissingService, fmt.Sprintf("%s.path[%d]", field, j), "path service %q is missing", name)
			}
		}
		if p.RequestBudget(req) <= 0 {
			return NewError(CodeTimeoutBudgetZero, field+".budget_ms", "request timeout budget must be greater than zero")
		}
	}
	return nil
}

func validateLimits(limits Limits) error {
	check := func(name string, value int) error {
		if value < 1 || value > MaxConcurrencyLimit {
			err := NewError(CodeConcurrencyLimitOutOfRange, name, "must be within [1,%d], got %d", MaxConcurrencyLimit, value)
			WithContext(err, "allowed_range", []int{1, MaxConcurrencyLimit})
			return WithContext(err, "value", value)
		}
		return nil
	}
	if err := check("limits.global_concurrent", limits.GlobalConcurrent); err != nil {
		return err
	}
	if err := check("limits.service_concurrent_default", limits.ServiceConcurrentDefault); err != nil {
		return err
	}
	names := make([]string, 0, len(limits.ServiceConcurrent))
	for name := range limits.ServiceConcurrent {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := check("limits.service_concurrent."+name, limits.ServiceConcurrent[name]); err != nil {
			return err
		}
	}
	return nil
}

func validateInstruction(ins Instruction, index int, services map[string]bool, edgeIDs map[string]bool, p *Project) error {
	field := fmt.Sprintf("instructions[%d]", index)
	if ins.ID == "" {
		return NewError(CodeFieldRequired, field+".id", "instruction id is required")
	}
	if ins.Kind == "" {
		return NewError(CodeFieldRequired, field+".kind", "instruction kind is required")
	}
	switch ins.Kind {
	case KindDelay, KindTimeout, KindFailure, KindDuplicate, KindReorder:
	default:
		return NewError(CodeFieldInvalid, field+".kind", "unsupported instruction kind %q", ins.Kind)
	}
	if ins.StartTick > ins.EndTick {
		return NewError(CodeFieldConflict, field+".window", "start_tick %d is after end_tick %d", ins.StartTick, ins.EndTick)
	}
	if ins.DelayMS < 0 {
		err := NewError(CodeNegativeDelay, field+".delay_ms", "delay must not be negative")
		return WithContext(err, "instruction_id", ins.ID)
	}
	if ins.RepeatDelayMS < 0 {
		err := NewError(CodeNegativeDelay, field+".repeat_delay_ms", "repeat delay must not be negative")
		return WithContext(err, "instruction_id", ins.ID)
	}
	if ins.TimeoutMS < 0 {
		return NewError(CodeFieldInvalid, field+".timeout_ms", "timeout must not be negative")
	}
	if ins.RepeatCount < 0 || ins.ReorderDepth < 0 {
		return NewError(CodeFieldInvalid, field, "repeat/reorder amplitude must not be negative")
	}
	if err := validateSelectorTarget(ins.Selector, field+".selector", services, edgeIDs); err != nil {
		ce, ok := AsError(err)
		if ok {
			WithContext(ce, "instruction_id", ins.ID)
			return ce
		}
		return err
	}
	return nil
}

func validateSelectorTarget(sel Selector, field string, services map[string]bool, edgeIDs map[string]bool) error {
	switch sel.Kind {
	case TargetService:
		if sel.Service == "" {
			return NewError(CodeFieldRequired, field+".service", "service selector requires service name")
		}
		if !services[sel.Service] {
			err := NewError(CodeTargetMissingService, field+".service", "selector points to missing service %q", sel.Service)
			return WithContext(err, "service", sel.Service)
		}
	case TargetEdge:
		if sel.EdgeID != "" {
			if !edgeIDs[sel.EdgeID] {
				err := NewError(CodeTargetMissingService, field+".edge_id", "selector points to missing edge %q", sel.EdgeID)
				return WithContext(err, "edge_id", sel.EdgeID)
			}
			return nil
		}
		if sel.From == "" || sel.To == "" {
			return NewError(CodeFieldRequired, field, "edge selector requires edge_id or from/to")
		}
		if !services[sel.From] || !services[sel.To] {
			err := NewError(CodeTargetMissingService, field, "edge selector %s->%s points to missing service", sel.From, sel.To)
			return WithContext(err, "from", sel.From)
		}
	default:
		return NewError(CodeFieldInvalid, field+".kind", "selector kind must be service or edge, got %q", sel.Kind)
	}
	return nil
}

func findCycle(adj map[string][]string) []string {
	const (
		unvisited = 0
		active    = 1
		done      = 2
	)
	state := map[string]int{}
	stack := []string{}
	var dfs func(string) []string
	dfs = func(node string) []string {
		state[node] = active
		stack = append(stack, node)
		for _, next := range adj[node] {
			if state[next] == active {
				for i, name := range stack {
					if name == next {
						return append(append([]string{}, stack[i:]...), next)
					}
				}
			}
			if state[next] == unvisited {
				if cycle := dfs(next); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[node] = done
		return nil
	}
	names := make([]string, 0, len(adj))
	for name := range adj {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if state[name] == unvisited {
			if cycle := dfs(name); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}
