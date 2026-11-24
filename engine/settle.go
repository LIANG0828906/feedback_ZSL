package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"economycore/fixed"
	"economycore/model"
)

func (e *Engine) runCycle(ctx context.Context) (CycleSnapshot, map[cacheKey]nodeTransition, cycleStats, error) {
	cycle := e.state.NextCycle
	window := int(cycle / int64(e.model.WindowCycles))
	if err := e.applyDueIntents(cycle, window); err != nil {
		return CycleSnapshot{}, nil, cycleStats{}, err
	}
	if e.opts.Fault != nil {
		if err := e.opts.Fault(window, cycle); err != nil {
			if _, ok := model.AsError(err); ok {
				return CycleSnapshot{}, nil, cycleStats{}, err
			}
			return CycleSnapshot{}, nil, cycleStats{}, model.NewError(model.EWindow, "", "injected settlement fault").WithWindow(window).WithCause(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return CycleSnapshot{}, nil, cycleStats{}, model.NewError(model.ECancel, "", err.Error()).WithWindow(window).WithCause(err)
	}

	snapshot := CycleSnapshot{
		Cycle:         cycle,
		Window:        window,
		CycleInWindow: int(cycle % int64(e.model.WindowCycles)),
		GlobalStocks:  make(map[string]int64),
		Nodes:         make([]NodeSnapshot, 0, len(e.model.Nodes)),
	}
	cycleCache := make(map[cacheKey]nodeTransition, len(e.model.Nodes))
	stats := cycleStats{
		NodeReused:     make(map[string]bool),
		NodeRecomputed: make(map[string]bool),
		Reasons:        make(map[string][]string),
	}
	order, err := e.runtimeOrder(window)
	if err != nil {
		return CycleSnapshot{}, nil, cycleStats{}, err
	}

	for _, nodeID := range order {
		if err := ctx.Err(); err != nil {
			return CycleSnapshot{}, nil, cycleStats{}, model.NewError(model.ECancel, "", err.Error()).WithWindow(window).WithNode(nodeID, "").WithCause(err)
		}
		rt := e.state.Nodes[nodeID]
		if !rt.Built {
			snapshot.Nodes = append(snapshot.Nodes, NodeSnapshot{
				NodeID:          nodeID,
				Built:           false,
				Level:           rt.Level,
				Recipe:          rt.Recipe,
				Workers:         rt.Workers,
				Stocks:          cloneMap(e.state.Stocks),
				Produced:        map[string]int64{},
				Consumed:        map[string]int64{},
				NetRateFixed:    map[string]int64{},
				Overflow:        map[string]int64{},
				StallStartCycle: -1,
			})
			continue
		}
		transition, reused, reason, err := e.transitionFor(nodeID, cycle)
		if err != nil {
			return CycleSnapshot{}, nil, cycleStats{}, err
		}
		if reused {
			stats.Reused++
			stats.NodeReused[nodeID] = true
		} else {
			stats.Recomputed++
			stats.NodeRecomputed[nodeID] = true
			if reason != "" {
				stats.Reasons[nodeID] = append(stats.Reasons[nodeID], reason)
			}
		}
		overflow, err := e.applyTransition(transition, window)
		if err != nil {
			return CycleSnapshot{}, nil, cycleStats{}, err
		}
		cycleCache[cacheKey{NodeID: nodeID, Cycle: cycle}] = cloneTransition(transition)
		netRates, err := netRateMap(transition)
		if err != nil {
			return CycleSnapshot{}, nil, cycleStats{}, err
		}
		snapshot.Nodes = append(snapshot.Nodes, NodeSnapshot{
			NodeID:          nodeID,
			Built:           true,
			Level:           transition.Level,
			Recipe:          transition.Recipe,
			Workers:         transition.Workers,
			RateFixed:       transition.RateFixed,
			Stocks:          cloneMap(e.state.Stocks),
			Produced:        cloneMap(transition.Produced),
			Consumed:        cloneMap(transition.Consumed),
			NetRateFixed:    netRates,
			Overflow:        overflow,
			Stalled:         transition.Stalled,
			StallStartCycle: transition.StallStartCycle,
		})
		snapshot.CommitSequence = append(snapshot.CommitSequence, nodeID)
	}

	for _, resource := range e.model.Resources {
		snapshot.GlobalStocks[resource.ID] = e.state.Stocks[resource.ID]
		total, err := fixed.Add(snapshot.GlobalTotal, e.state.Stocks[resource.ID])
		if err != nil {
			return CycleSnapshot{}, nil, cycleStats{}, model.NewError(model.EOvf, "/globalTotal", "global stock total overflow").WithWindow(window)
		}
		snapshot.GlobalTotal = total
	}
	idle, err := e.idleWorkers()
	if err != nil {
		return CycleSnapshot{}, nil, cycleStats{}, err
	}
	snapshot.WorkersIdle = idle
	e.state.NextCycle++
	return snapshot, cycleCache, stats, nil
}

func (e *Engine) transitionFor(nodeID string, cycle int64) (nodeTransition, bool, string, error) {
	signature, err := e.transitionSignature(nodeID, cycle)
	if err != nil {
		return nodeTransition{}, false, "", err
	}
	forced := e.forcedInvalid[nodeID]
	if len(forced) == 0 {
		entry, ok := e.cache[cacheKey{NodeID: nodeID, Cycle: cycle}]
		if ok && entry.Signature == signature && e.transitionPreconditions(entry) {
			return entry, true, "", nil
		}
		if ok {
			if e.model.OverflowPolicy == "reject" {
				return e.computeTransition(nodeID, cycle, signature, "reject_policy_requires_recompute")
			}
			if entry.Signature != signature {
				return e.computeTransition(nodeID, cycle, signature, "node_configuration_changed")
			}
			return e.computeTransition(nodeID, cycle, signature, "input_precondition_changed")
		}
	}
	reason := "cache_miss"
	if len(forced) > 0 {
		reason = joinReasons(forced)
	}
	return e.computeTransition(nodeID, cycle, signature, reason)
}

func (e *Engine) computeTransition(nodeID string, cycle int64, signature, reason string) (nodeTransition, bool, string, error) {
	rt := e.state.Nodes[nodeID]
	window := int(cycle / int64(e.model.WindowCycles))
	transition := nodeTransition{
		NodeID:          nodeID,
		Cycle:           cycle,
		Signature:       signature,
		Built:           rt.Built,
		Level:           rt.Level,
		Recipe:          rt.Recipe,
		Workers:         rt.Workers,
		ProgressBefore:  rt.Progress,
		ProgressAfter:   rt.Progress,
		Stalled:         rt.Stalled,
		StallStartCycle: rt.StallStartCycle,
		Consumed:        map[string]int64{},
		Produced:        map[string]int64{},
	}
	node := e.nodeByID(nodeID)
	if node == nil {
		return nodeTransition{}, false, "", model.NewError(model.EUnit, "/nodes/"+nodeID, "node does not exist").WithWindow(window).WithNode(nodeID, "")
	}
	recipe, ok := node.RecipeByID(rt.Recipe)
	if !ok {
		return nodeTransition{}, false, "", model.NewError(model.EUnit, "/nodes/"+nodeID+"/recipe", "selected recipe does not exist").WithWindow(window).WithNode(nodeID, "recipe")
	}
	rate, err := node.RateExpr.Eval(map[string]int64{model.ExprWorkers: int64(rt.Workers)})
	if err != nil {
		return nodeTransition{}, false, "", model.NewError(model.ESchema, "/nodes/"+nodeID+"/rateFormula", err.Error()).WithWindow(window).WithNode(nodeID, "rateFormula")
	}
	rate, err = fixed.Add(rate, rt.RateBonusFixed)
	if err != nil {
		return nodeTransition{}, false, "", model.NewError(model.EOvf, "/nodes/"+nodeID+"/rate", err.Error()).WithWindow(window).WithNode(nodeID, "rate")
	}
	transition.RateFixed = rate
	if rt.Workers == 0 {
		transition.Stalled = true
		if transition.StallStartCycle < 0 {
			transition.StallStartCycle = cycle
		}
		return transition, false, reason, nil
	}
	contribution, err := fixed.Mul(rate, int64(rt.Workers))
	if err != nil {
		return nodeTransition{}, false, "", model.NewError(model.EOvf, "/nodes/"+nodeID+"/rateProgress", err.Error()).WithWindow(window).WithNode(nodeID, "rateProgress")
	}
	contribution, err = fixed.Mul(contribution, node.WorkerEfficiencyFixed)
	if err != nil {
		return nodeTransition{}, false, "", model.NewError(model.EOvf, "/nodes/"+nodeID+"/workerEfficiency", err.Error()).WithWindow(window).WithNode(nodeID, "workerEfficiency")
	}
	progress := rt.Progress
	progress, err = fixed.Add(progress, contribution)
	if err != nil {
		return nodeTransition{}, false, "", model.NewError(model.EOvf, "/nodes/"+nodeID+"/progress", err.Error()).WithWindow(window).WithNode(nodeID, "progress")
	}
	transition.ProgressBefore = rt.Progress
	transition.ProgressAfter = progress
	maxBatches := progress / transitionDenominator
	if maxBatches == 0 {
		if rt.Stalled {
			transition.Stalled = true
			if transition.StallStartCycle < 0 {
				transition.StallStartCycle = cycle
			}
		} else {
			transition.Stalled = false
			transition.StallStartCycle = -1
		}
		return transition, false, reason, nil
	}
	possible := maxBatches
	for resource, qty := range recipe.Inputs {
		if qty <= 0 {
			continue
		}
		byStock := e.state.Stocks[resource] / qty
		if byStock < possible {
			possible = byStock
		}
	}
	if e.model.OverflowPolicy == "reject" {
		for resource, qty := range recipe.Outputs {
			if qty <= 0 {
				continue
			}
			capacity := e.state.Capacities[resource]
			if capacity <= 0 {
				continue
			}
			room := capacity - e.state.Stocks[resource]
			if room < 0 {
				room = 0
			}
			byCapacity := room / qty
			if byCapacity < possible {
				possible = byCapacity
			}
		}
	}
	if possible < 0 {
		possible = 0
	}
	for resource, qty := range recipe.Inputs {
		if qty <= 0 || possible == 0 {
			continue
		}
		amount, mulErr := fixed.Mul(qty, possible)
		if mulErr != nil {
			return nodeTransition{}, false, "", model.NewError(model.EOvf, "/nodes/"+nodeID+"/inputs/"+resource, mulErr.Error()).WithWindow(window).WithNode(nodeID, "input."+resource)
		}
		transition.Consumed[resource] = amount
	}
	for resource, qty := range recipe.Outputs {
		if qty <= 0 || possible == 0 {
			continue
		}
		amount, mulErr := fixed.Mul(qty, possible)
		if mulErr != nil {
			return nodeTransition{}, false, "", model.NewError(model.EOvf, "/nodes/"+nodeID+"/outputs/"+resource, mulErr.Error()).WithWindow(window).WithNode(nodeID, "output."+resource)
		}
		transition.Produced[resource] = amount
	}
	transition.Batches = possible
	processed := possible
	used, err := fixed.Mul(processed, transitionDenominator)
	if err != nil {
		return nodeTransition{}, false, "", model.NewError(model.EOvf, "/nodes/"+nodeID+"/progress", err.Error()).WithWindow(window).WithNode(nodeID, "progress")
	}
	progress, err = fixed.Sub(progress, used)
	if err != nil {
		return nodeTransition{}, false, "", model.NewError(model.EOvf, "/nodes/"+nodeID+"/progress", err.Error()).WithWindow(window).WithNode(nodeID, "progress")
	}
	if processed < maxBatches {
		transition.Stalled = true
		if rt.Stalled && rt.StallStartCycle >= 0 {
			transition.StallStartCycle = rt.StallStartCycle
		} else {
			transition.StallStartCycle = cycle
		}
		if progress > transitionDenominator {
			progress = transitionDenominator
		}
	} else {
		transition.Stalled = false
		transition.StallStartCycle = -1
	}
	transition.ProgressAfter = progress
	return transition, false, reason, nil
}

func (e *Engine) applyTransition(transition nodeTransition, window int) (map[string]int64, error) {
	overflow := make(map[string]int64)
	for resource, amount := range transition.Consumed {
		if amount == 0 {
			continue
		}
		stock := e.state.Stocks[resource]
		if stock < amount {
			return nil, model.NewError(model.ERes, "/nodes/"+transition.NodeID+"/inputs/"+resource, "insufficient resource for transition").WithWindow(window).WithNode(transition.NodeID, "input."+resource)
		}
		updated, err := fixed.Sub(stock, amount)
		if err != nil {
			return nil, model.NewError(model.EOvf, "/nodes/"+transition.NodeID+"/inputs/"+resource, err.Error()).WithWindow(window).WithNode(transition.NodeID, "input."+resource)
		}
		e.state.Stocks[resource] = updated
	}
	for resource, amount := range transition.Produced {
		if amount == 0 {
			continue
		}
		stock := e.state.Stocks[resource]
		capacity := e.state.Capacities[resource]
		if capacity > 0 {
			room := capacity - stock
			if room < 0 {
				room = 0
			}
			if amount > room {
				excess := amount - room
				overflow[resource] = excess
				e.state.Stocks[resource] = capacity
				key := transition.NodeID + "/" + resource
				total, err := fixed.Add(e.state.OverflowTotals[key], excess)
				if err != nil {
					return nil, model.NewError(model.EOvf, "/overflow/"+key, err.Error()).WithWindow(window).WithNode(transition.NodeID, "overflow."+resource)
				}
				e.state.OverflowTotals[key] = total
				continue
			}
		}
		updated, err := fixed.Add(stock, amount)
		if err != nil {
			return nil, model.NewError(model.EOvf, "/nodes/"+transition.NodeID+"/outputs/"+resource, err.Error()).WithWindow(window).WithNode(transition.NodeID, "output."+resource)
		}
		e.state.Stocks[resource] = updated
	}
	rt := e.state.Nodes[transition.NodeID]
	rt.Progress = transition.ProgressAfter
	rt.Stalled = transition.Stalled
	rt.StallStartCycle = transition.StallStartCycle
	e.state.Nodes[transition.NodeID] = rt
	return overflow, nil
}

func (e *Engine) transitionPreconditions(entry nodeTransition) bool {
	if !entry.Built {
		return false
	}
	for resource, amount := range entry.Consumed {
		if e.state.Stocks[resource] < amount {
			return false
		}
	}
	return true
}

func (e *Engine) transitionSignature(nodeID string, cycle int64) (string, error) {
	node := e.nodeByID(nodeID)
	if node == nil {
		return "", model.NewError(model.EUnit, "/nodes/"+nodeID, "node does not exist")
	}
	rt := e.state.Nodes[nodeID]
	rate, err := node.RateExpr.Eval(map[string]int64{model.ExprWorkers: int64(rt.Workers)})
	if err != nil {
		return "", model.NewError(model.ESchema, "/nodes/"+nodeID+"/rateFormula", err.Error()).WithNode(nodeID, "rateFormula")
	}
	rate, err = fixed.Add(rate, rt.RateBonusFixed)
	if err != nil {
		return "", model.NewError(model.EOvf, "/nodes/"+nodeID+"/rate", err.Error()).WithNode(nodeID, "rate")
	}
	recipe, ok := node.RecipeByID(rt.Recipe)
	if !ok {
		return "", model.NewError(model.EUnit, "/nodes/"+nodeID+"/recipe", "selected recipe does not exist").WithNode(nodeID, "recipe")
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "built=%t;level=%d;recipe=%s;workers=%d;rate=%d;workerEff=%d;progress=%d;stalled=%t;stallStart=%d;cycle=%d;policy=%s",
		rt.Built, rt.Level, rt.Recipe, rt.Workers, rate, node.WorkerEfficiencyFixed,
		rt.Progress, rt.Stalled, rt.StallStartCycle, cycle, e.model.OverflowPolicy)
	for _, resource := range sortedMapKeys(recipe.Inputs) {
		fmt.Fprintf(&builder, ";in.%s.stock=%d", resource, e.state.Stocks[resource])
	}
	if e.model.OverflowPolicy == "reject" {
		for _, resource := range sortedMapKeys(recipe.Outputs) {
			fmt.Fprintf(&builder, ";out.%s.stock=%d;out.%s.cap=%d", resource, e.state.Stocks[resource], resource, e.state.Capacities[resource])
		}
	}
	return builder.String(), nil
}

func netRateMap(transition nodeTransition) (map[string]int64, error) {
	out := make(map[string]int64)
	resources := make(map[string]bool)
	for resource := range transition.Produced {
		resources[resource] = true
	}
	for resource := range transition.Consumed {
		resources[resource] = true
	}
	keys := make([]string, 0, len(resources))
	for resource := range resources {
		keys = append(keys, resource)
	}
	sort.Strings(keys)
	for _, resource := range keys {
		net, err := fixed.Sub(transition.Produced[resource], transition.Consumed[resource])
		if err != nil {
			return nil, model.NewError(model.EOvf, "/nodes/"+transition.NodeID+"/netRate/"+resource, err.Error()).WithNode(transition.NodeID, "netRate."+resource)
		}
		scaled, err := fixed.Mul(net, fixed.UnitScale)
		if err != nil {
			return nil, model.NewError(model.EOvf, "/nodes/"+transition.NodeID+"/netRate/"+resource, err.Error()).WithNode(transition.NodeID, "netRate."+resource)
		}
		out[resource] = scaled
	}
	return out, nil
}

func (e *Engine) idleWorkers() (int64, error) {
	var assigned int64
	for _, node := range e.model.Nodes {
		rt := e.state.Nodes[node.ID]
		if !rt.Built {
			continue
		}
		value, err := fixed.Add(assigned, int64(rt.Workers))
		if err != nil {
			return 0, model.NewError(model.EOvf, "/workers", err.Error())
		}
		assigned = value
	}
	idle, err := fixed.Sub(e.model.WorkersTotal, assigned)
	if err != nil || idle < 0 {
		return 0, model.NewError(model.ERes, "/workers", "worker assignment exceeds workforce")
	}
	return idle, nil
}

func (e *Engine) nodeByID(nodeID string) *model.Node {
	for _, node := range e.model.Nodes {
		if node.ID == nodeID {
			return node
		}
	}
	return nil
}

func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	seen := make(map[string]bool)
	out := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if !seen[reason] {
			seen[reason] = true
			out = append(out, reason)
		}
	}
	sort.Strings(out)
	joined := out[0]
	for _, reason := range out[1:] {
		joined += "," + reason
	}
	return joined
}
