package engine

import (
	"fmt"
	"sort"

	"economycore/model"
)

// EditRate changes a node's constant rate and invalidates only that node and
// its downstream consumers.
func (e *Engine) EditRate(nodeID string, rateFixed int64) (*EditReport, error) {
	if rateFixed < 0 {
		return nil, model.NewError(model.ESchema, "/nodes/"+nodeID+"/rate", "rate cannot be negative").WithNode(nodeID, "rate")
	}
	return e.applyEdit(nodeID, "rate", func(m *model.Model) error {
		node := m.NodeMap()[nodeID]
		if node == nil {
			return model.NewError(model.EUnit, "/nodes/"+nodeID, "node does not exist").WithNode(nodeID, "rate")
		}
		node.RateExpr = model.ConstExpr(rateFixed)
		return nil
	}, false)
}

// EditRecipe changes the selected recipe and incrementally updates only
// incident dependency edges.
func (e *Engine) EditRecipe(nodeID, recipeID string) (*EditReport, error) {
	return e.applyEdit(nodeID, "recipe", func(m *model.Model) error {
		node := m.NodeMap()[nodeID]
		if node == nil {
			return model.NewError(model.EUnit, "/nodes/"+nodeID, "node does not exist").WithNode(nodeID, "recipe")
		}
		if _, ok := node.RecipeByID(recipeID); !ok {
			return model.NewError(model.EUnit, "/nodes/"+nodeID+"/recipe", "recipe does not exist").WithNode(nodeID, "recipe")
		}
		node.Recipe = recipeID
		return nil
	}, true)
}

// EditWorkers changes a node's worker assignment and invalidates downstream
// production only.
func (e *Engine) EditWorkers(nodeID string, workers int) (*EditReport, error) {
	if workers < 0 {
		return nil, model.NewError(model.ERes, "/nodes/"+nodeID+"/workers", "worker count cannot be negative").WithNode(nodeID, "workers")
	}
	return e.applyEdit(nodeID, "workers", func(m *model.Model) error {
		node := m.NodeMap()[nodeID]
		if node == nil {
			return model.NewError(model.EUnit, "/nodes/"+nodeID, "node does not exist").WithNode(nodeID, "workers")
		}
		old := node.Workers
		node.Workers = workers
		var assigned int64
		for _, candidate := range m.Nodes {
			if candidate.Built {
				assigned += int64(candidate.Workers)
			}
		}
		if assigned > m.WorkersTotal {
			node.Workers = old
			return model.NewError(model.ERes, "/nodes/"+nodeID+"/workers", "worker assignment exceeds workforce").WithNode(nodeID, "workers")
		}
		return nil
	}, false)
}

func (e *Engine) applyEdit(nodeID, change string, edit func(*model.Model) error, recipe bool) (*EditReport, error) {
	oldDownstream := append([]string(nil), e.graph.Downstream(nodeID)...)
	modelCopy := e.model.Clone()
	if err := edit(modelCopy); err != nil {
		return nil, err
	}
	if err := modelCopy.Validate(); err != nil {
		return nil, err
	}
	graphCopy := cloneGraph(e.graph)
	graphUpdated := false
	if recipe {
		if err := graphCopy.UpdateNodeRecipe(modelCopy, nodeID); err != nil {
			return nil, err
		}
		graphUpdated = true
	}
	newDownstream := graphCopy.Downstream(nodeID)
	affected := make(map[string]bool)
	for _, id := range oldDownstream {
		affected[id] = true
	}
	for _, id := range newDownstream {
		affected[id] = true
	}
	report := &EditReport{
		ChangedNode:  nodeID,
		Change:       change,
		GraphUpdated: graphUpdated,
	}
	for _, id := range e.graph.Order {
		status := NodeCacheStatus{NodeID: id}
		if affected[id] {
			reason := fmt.Sprintf("%s_edit:%s", change, nodeID)
			report.Invalidated = append(report.Invalidated, Invalidation{NodeID: id, Reasons: []string{reason}})
			e.forcedInvalid[id] = appendUnique(e.forcedInvalid[id], reason)
			status.Invalidated = true
			status.Reasons = []string{reason}
		} else {
			report.Unaffected = append(report.Unaffected, id)
			status.Reused = true
		}
		report.Statuses = append(report.Statuses, status)
	}
	sort.Slice(report.Invalidated, func(i, j int) bool {
		return report.Invalidated[i].NodeID < report.Invalidated[j].NodeID
	})
	sort.Strings(report.Unaffected)
	e.model = modelCopy
	e.graph = graphCopy
	return report, nil
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}
