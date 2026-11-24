package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"economycore/fixed"
)

// Validate checks structural and referential integrity and returns E_SCHEMA
// with a JSON-pointer path.
func (m *Model) Validate() error {
	if m == nil {
		return NewError(ESchema, "", "nil model")
	}
	if m.ID == "" {
		return NewError(ESchema, "/id", "missing project id")
	}
	if m.CycleMs <= 0 {
		return NewError(ESchema, "/cycleMs", "cycle duration must be positive")
	}
	if m.WindowCycles <= 0 {
		return NewError(ESchema, "/windowCycles", "window cycle count must be positive")
	}
	if m.WindowCount <= 0 {
		return NewError(ESchema, "/windowCount", "window count must be positive")
	}
	if m.RateScale != 10000 {
		return NewError(ESchema, "/rateScale", "internal rate scale must be 10000")
	}
	if m.WorkerScale != 10000 {
		return NewError(ESchema, "/workerScale", "internal worker scale must be 10000")
	}
	if m.WorkersTotal < 0 {
		return NewError(ESchema, "/workersTotal", "workforce cannot be negative")
	}
	if m.WorkersMaximum <= 0 {
		return NewError(ESchema, "/workersMaximum", "workforce maximum must be positive")
	}
	if m.WorkersTotal > m.WorkersMaximum {
		return NewError(ESchema, "/workersTotal", "workforce exceeds declared maximum")
	}
	switch m.OverflowPolicy {
	case "", "cap_drop", "reject":
	default:
		return NewError(ESchema, "/overflowPolicy", "unsupported overflow policy")
	}
	if m.UpgradeDiscountBasisPoints < 0 || m.UpgradeDiscountBasisPoints > 10000 {
		return NewError(ESchema, "/upgradeDiscountBasisPoints", "discount must be between 0 and 10000")
	}

	resources := make(map[string]bool, len(m.Resources))
	for i, resource := range m.Resources {
		path := JSONPointer("resources", fmt.Sprint(i))
		if resource.ID == "" {
			return NewError(ESchema, path+"/id", "missing resource id")
		}
		if resources[resource.ID] {
			return NewError(ESchema, path+"/id", "duplicate resource id")
		}
		resources[resource.ID] = true
		switch resource.Kind {
		case KindRaw, KindCrafted, KindFood, KindEnergy:
		default:
			return NewError(ESchema, path+"/kind", "unsupported resource kind")
		}
	}
	if len(resources) == 0 {
		return NewError(ESchema, "/resources", "at least one resource is required")
	}
	for id, value := range m.InitialStocks {
		if !resources[id] {
			return NewError(ESchema, JSONPointer("initialStocks", id), "unknown resource")
		}
		if value < 0 {
			return NewError(ESchema, JSONPointer("initialStocks", id), "stock cannot be negative")
		}
	}
	for id, value := range m.Capacities {
		if !resources[id] {
			return NewError(ESchema, JSONPointer("capacities", id), "unknown resource")
		}
		if value < 0 {
			return NewError(ESchema, JSONPointer("capacities", id), "capacity cannot be negative")
		}
	}
	nodes := make(map[string]bool, len(m.Nodes))
	for i, node := range m.Nodes {
		path := JSONPointer("nodes", fmt.Sprint(i))
		if node.ID == "" {
			return NewError(ESchema, path+"/id", "missing node id")
		}
		if nodes[node.ID] {
			return NewError(ESchema, path+"/id", "duplicate node id")
		}
		nodes[node.ID] = true
		if node.Level < 1 {
			return NewError(ESchema, path+"/level", "level must be at least one")
		}
		if node.Workers < 0 {
			return NewError(ESchema, path+"/workers", "workers cannot be negative")
		}
		if node.WorkerEfficiencyFixed < 0 {
			return NewError(ESchema, path+"/workerEfficiencyFixed", "worker efficiency cannot be negative")
		}
		if err := node.RateExpr.Validate(); err != nil {
			return NewError(ESchema, path+"/rateFormula", err.Error())
		}
		rate, err := node.RateExpr.Eval(map[string]int64{ExprWorkers: int64(node.Workers)})
		if err != nil {
			if errors.Is(err, fixed.ErrOverflow) {
				return NewError(EOvf, path+"/rateFormula", err.Error()).WithNode(node.ID, "rateFormula")
			}
			return NewError(ESchema, path+"/rateFormula", err.Error())
		}
		if rate < 0 {
			return NewError(ESchema, path+"/rateFormula", "rate cannot be negative")
		}
		if len(node.Recipes) == 0 {
			return NewError(ESchema, path+"/recipes", "at least one recipe is required")
		}
		recipeIDs := make(map[string]bool, len(node.Recipes))
		for j, recipe := range node.Recipes {
			recipePath := path + "/recipes/" + fmt.Sprint(j)
			if recipe.ID == "" {
				return NewError(ESchema, recipePath+"/id", "missing recipe id")
			}
			if recipeIDs[recipe.ID] {
				return NewError(ESchema, recipePath+"/id", "duplicate recipe id")
			}
			recipeIDs[recipe.ID] = true
			for id, qty := range recipe.Inputs {
				if !resources[id] {
					return NewError(ESchema, recipePath+"/inputs/"+id, "unknown resource")
				}
				if qty < 0 {
					return NewError(ESchema, recipePath+"/inputs/"+id, "input quantity cannot be negative")
				}
			}
			for id, qty := range recipe.Outputs {
				if !resources[id] {
					return NewError(ESchema, recipePath+"/outputs/"+id, "unknown resource")
				}
				if qty < 0 {
					return NewError(ESchema, recipePath+"/outputs/"+id, "output quantity cannot be negative")
				}
			}
		}
		if !recipeIDs[node.Recipe] {
			return NewError(ESchema, path+"/recipe", "selected recipe does not exist")
		}
		if node.Upgrade.MaxLevel < node.Level {
			return NewError(ESchema, path+"/upgrade/maxLevel", "maximum level is below initial level")
		}
		if node.Upgrade.RateAddFixed < 0 {
			return NewError(ESchema, path+"/upgrade/rateAddFixed", "rate increment cannot be negative")
		}
		for id, qty := range node.BuildCost {
			if !resources[id] {
				return NewError(ESchema, path+"/buildCost/"+id, "unknown resource")
			}
			if qty < 0 {
				return NewError(ESchema, path+"/buildCost/"+id, "cost cannot be negative")
			}
		}
		for id, qty := range node.Upgrade.Cost {
			if !resources[id] {
				return NewError(ESchema, path+"/upgrade/cost/"+id, "unknown resource")
			}
			if qty < 0 {
				return NewError(ESchema, path+"/upgrade/cost/"+id, "cost cannot be negative")
			}
		}
		for id, qty := range node.Upgrade.CapacityAdd {
			if !resources[id] {
				return NewError(ESchema, path+"/upgrade/capacityAdd/"+id, "unknown resource")
			}
			if qty < 0 {
				return NewError(ESchema, path+"/upgrade/capacityAdd/"+id, "capacity increment cannot be negative")
			}
		}
	}
	if len(nodes) == 0 {
		return NewError(ESchema, "/nodes", "at least one node is required")
	}

	for i, intent := range m.Intents {
		path := JSONPointer("intents", fmt.Sprint(i))
		if intent.Offset != i {
			return NewError(ESchema, path+"/offset", "intent offsets must be contiguous and ordered")
		}
		if intent.Window < 0 || intent.Window >= m.WindowCount {
			return NewError(ESchema, path+"/window", "window is out of range")
		}
		if intent.Cycle < 0 || intent.Cycle >= m.WindowCycles {
			return NewError(ESchema, path+"/cycle", "cycle is out of range")
		}
		switch intent.Action {
		case IntentBuild, IntentUpgrade, IntentDemolish, IntentAssignWorkers, IntentSwitchRecipe:
		default:
			return NewError(ESchema, path+"/action", "unknown action")
		}
		if intent.NodeID == "" {
			return NewError(ESchema, path+"/nodeId", "missing node id")
		}
		if intent.Action == IntentSwitchRecipe && intent.RecipeID == "" {
			return NewError(ESchema, path+"/recipeId", "missing recipe id")
		}
		if intent.Action == IntentAssignWorkers && intent.Workers == nil {
			return NewError(ESchema, path+"/workers", "missing worker count")
		}
	}
	for i := 1; i < len(m.Intents); i++ {
		prev := m.Intents[i-1].GlobalCycle(m.WindowCycles)
		curr := m.Intents[i].GlobalCycle(m.WindowCycles)
		if curr < prev {
			return NewError(ESchema, JSONPointer("intents", fmt.Sprint(i), "cycle"), "intents must be ordered by cycle")
		}
	}
	if _, err := BuildGraph(m); err != nil {
		return err
	}
	return nil
}

// ValidateFingerprint checks that two checkpoint identities are identical.
func ValidateFingerprint(a, b Fingerprint) error {
	var differences []string
	if a.ContentHash != b.ContentHash {
		differences = append(differences, "contentHash")
	}
	if a.SourceVersion != b.SourceVersion {
		differences = append(differences, "sourceVersion")
	}
	if a.ConversionVersion != b.ConversionVersion {
		differences = append(differences, "conversionVersion")
	}
	if a.AlgorithmVersion != b.AlgorithmVersion {
		differences = append(differences, "algorithmVersion")
	}
	if len(differences) == 0 {
		return nil
	}
	sort.Strings(differences)
	return NewError(EVer, "/fingerprint", "checkpoint identity mismatch: "+strings.Join(differences, ","))
}
