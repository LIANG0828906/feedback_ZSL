package engine

import (
	"sort"

	"economycore/fixed"
	"economycore/model"
)

func (e *Engine) applyDueIntents(cycle int64, window int) error {
	for e.state.IntentIndex < len(e.model.Intents) {
		intent := e.model.Intents[e.state.IntentIndex]
		due := intent.GlobalCycle(e.model.WindowCycles)
		if due > cycle {
			break
		}
		before := cloneState(e.state)
		if err := e.applyIntent(intent, window); err != nil {
			e.state = before
			return err
		}
		e.state.IntentIndex++
	}
	return nil
}

func (e *Engine) applyIntent(intent model.Intent, window int) error {
	node := e.nodeByID(intent.NodeID)
	if node == nil {
		return model.NewError(model.EUnit, "/nodes/"+intent.NodeID, "intent references a node that does not exist").WithWindow(window).WithNode(intent.NodeID, "")
	}
	rt := e.state.Nodes[node.ID]
	switch intent.Action {
	case model.IntentBuild:
		if rt.Built {
			return model.NewError(model.EUnit, "/nodes/"+node.ID+"/built", "node is already built").WithWindow(window).WithNode(node.ID, "built")
		}
		if err := e.chargeCost(node.BuildCost, 0, node.ID, window, "buildCost"); err != nil {
			return err
		}
		rt.Built = true
		rt.Level = node.Level
		if rt.Level < 1 {
			rt.Level = 1
		}
		rt.Progress = 0
		rt.Stalled = false
		rt.StallStartCycle = -1
		rt.RateBonusFixed = 0
		if rt.Recipe == "" {
			rt.Recipe = node.Recipe
		}
		if intent.Workers != nil {
			rt.Workers = *intent.Workers
		}
		e.state.Nodes[node.ID] = rt
		if err := validateWorkerAssignment(e.model, &e.state); err != nil {
			return model.NewError(model.ERes, "/nodes/"+node.ID+"/workers", "worker assignment exceeds workforce").WithWindow(window).WithNode(node.ID, "workers")
		}
	case model.IntentUpgrade:
		if !rt.Built {
			return model.NewError(model.EUnit, "/nodes/"+node.ID+"/built", "cannot upgrade an unbuilt node").WithWindow(window).WithNode(node.ID, "built")
		}
		if rt.Level >= node.Upgrade.MaxLevel {
			return model.NewError(model.EUnit, "/nodes/"+node.ID+"/level", "node is already at maximum level").WithWindow(window).WithNode(node.ID, "level")
		}
		if err := e.chargeCost(node.Upgrade.Cost, e.model.UpgradeDiscountBasisPoints, node.ID, window, "upgradeCost"); err != nil {
			return err
		}
		rt.Level++
		bonus, err := fixed.Add(rt.RateBonusFixed, node.Upgrade.RateAddFixed)
		if err != nil {
			return model.NewError(model.EOvf, "/nodes/"+node.ID+"/upgrade/rateAdd", err.Error()).WithWindow(window).WithNode(node.ID, "upgradeRateAdd")
		}
		rt.RateBonusFixed = bonus
		e.state.Nodes[node.ID] = rt
		for resource, amount := range node.Upgrade.CapacityAdd {
			current := e.state.Capacities[resource]
			updated, err := fixed.Add(current, amount)
			if err != nil {
				return model.NewError(model.EOvf, "/nodes/"+node.ID+"/upgrade/capacityAdd/"+resource, err.Error()).WithWindow(window).WithNode(node.ID, "capacityAdd."+resource)
			}
			e.state.Capacities[resource] = updated
		}
	case model.IntentDemolish:
		if !rt.Built {
			return model.NewError(model.EUnit, "/nodes/"+node.ID+"/built", "cannot demolish an unbuilt node").WithWindow(window).WithNode(node.ID, "built")
		}
		rt.Built = false
		rt.Workers = 0
		rt.Progress = 0
		rt.Stalled = false
		rt.StallStartCycle = -1
		e.state.Nodes[node.ID] = rt
	case model.IntentAssignWorkers:
		if !rt.Built {
			return model.NewError(model.EUnit, "/nodes/"+node.ID+"/built", "cannot assign workers to an unbuilt node").WithWindow(window).WithNode(node.ID, "built")
		}
		if intent.Workers == nil {
			return model.NewError(model.ESchema, "/intents/"+itoa(intent.Offset)+"/workers", "missing worker count").WithWindow(window).WithNode(node.ID, "workers")
		}
		if *intent.Workers < 0 {
			return model.NewError(model.ERes, "/nodes/"+node.ID+"/workers", "worker count cannot be negative").WithWindow(window).WithNode(node.ID, "workers")
		}
		rt.Workers = *intent.Workers
		e.state.Nodes[node.ID] = rt
		if err := validateWorkerAssignment(e.model, &e.state); err != nil {
			return model.NewError(model.ERes, "/nodes/"+node.ID+"/workers", "worker assignment exceeds workforce").WithWindow(window).WithNode(node.ID, "workers")
		}
	case model.IntentSwitchRecipe:
		if !rt.Built {
			return model.NewError(model.EUnit, "/nodes/"+node.ID+"/built", "cannot switch recipe on an unbuilt node").WithWindow(window).WithNode(node.ID, "built")
		}
		if _, ok := node.RecipeByID(intent.RecipeID); !ok {
			return model.NewError(model.EUnit, "/nodes/"+node.ID+"/recipe", "recipe does not exist").WithWindow(window).WithNode(node.ID, "recipe")
		}
		rt.Recipe = intent.RecipeID
		rt.Progress = 0
		rt.Stalled = false
		rt.StallStartCycle = -1
		e.state.Nodes[node.ID] = rt
	default:
		return model.NewError(model.EUnit, "/intents/"+itoa(intent.Offset)+"/action", "unknown intent action").WithWindow(window).WithNode(node.ID, "action")
	}
	return nil
}

func (e *Engine) chargeCost(cost map[string]int64, discountBasisPoints int64, nodeID string, window int, field string) error {
	resources := make([]string, 0, len(cost))
	for resource := range cost {
		resources = append(resources, resource)
	}
	sort.Strings(resources)
	for _, resource := range resources {
		amount := cost[resource]
		if amount == 0 {
			continue
		}
		if discountBasisPoints != 0 {
			discounted, err := fixed.ApplyDiscount(amount, discountBasisPoints)
			if err != nil {
				return model.NewError(model.EOvf, "/nodes/"+nodeID+"/"+field+"/"+resource, err.Error()).WithWindow(window).WithNode(nodeID, field+"."+resource)
			}
			amount = discounted
		}
		if e.state.Stocks[resource] < amount {
			return model.NewError(model.ERes, "/nodes/"+nodeID+"/"+field+"/"+resource, "insufficient resource stock").WithWindow(window).WithNode(nodeID, field+"."+resource)
		}
	}
	for _, resource := range resources {
		amount := cost[resource]
		if discountBasisPoints != 0 {
			amount, _ = fixed.ApplyDiscount(amount, discountBasisPoints)
		}
		updated, err := fixed.Sub(e.state.Stocks[resource], amount)
		if err != nil {
			return model.NewError(model.EOvf, "/nodes/"+nodeID+"/"+field+"/"+resource, err.Error()).WithWindow(window).WithNode(nodeID, field+"."+resource)
		}
		e.state.Stocks[resource] = updated
	}
	return nil
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	n := value
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
