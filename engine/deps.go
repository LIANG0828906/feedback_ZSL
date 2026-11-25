package engine

import (
	"fmt"
	"sort"

	"example.com/columnar/domain"
)

func buildGraph(state map[int]domain.Batch, project domain.Project) ([]domain.DependencyNode, map[int][]int, error) {
	dependents := map[int][]int{}
	for _, b := range state {
		for _, dep := range b.DependsOn {
			dependents[dep] = append(dependents[dep], b.ID)
		}
	}
	nodes := make([]domain.DependencyNode, 0, len(state))
	for _, b := range domain.SortedBatches(state) {
		nodes = append(nodes, domain.DependencyNode{
			BatchID: b.ID, Family: b.Family, Status: b.Status,
			Dependencies: domain.CanonicalIntSet(b.DependsOn),
			SourceID:     b.SourceID, Dependents: domain.CanonicalIntSet(dependents[b.ID]),
		})
	}
	if err := validateDependencies(nodes, state); err != nil {
		return nil, nil, err
	}
	return nodes, dependents, nil
}

func validateDependencies(nodes []domain.DependencyNode, state map[int]domain.Batch) error {
	adj := make(map[int][]int, len(nodes))
	for _, n := range nodes {
		adj[n.BatchID] = n.Dependencies
		for _, dep := range n.Dependencies {
			d, ok := state[dep]
			if !ok {
				return &domain.Error{
					Code: domain.CodeBatchUnknown, Context: "dependency", BatchID: n.BatchID,
					Message: fmt.Sprintf("missing predecessor batch %d", dep),
				}
			}
			if d.Status == domain.BatchDeleted && n.Status != domain.BatchDeleted {
				return &domain.Error{
					Code: domain.CodeDependencyDeleted, Context: "dependency", BatchID: n.BatchID,
					Message: fmt.Sprintf("depends on deleted predecessor %d", dep),
				}
			}
		}
	}
	color := map[int]uint8{}
	stack := []int{}
	var visit func(int) error
	visit = func(id int) error {
		switch color[id] {
		case 1:
			idx := 0
			for i, v := range stack {
				if v == id {
					idx = i
					break
				}
			}
			cycle := append(append([]int(nil), stack[idx:]...), id)
			return &domain.Error{
				Code: domain.CodeDependencyCycle, Context: "dependency", BatchID: id,
				Message: fmt.Sprintf("cycle: %v", cycle),
			}
		case 2:
			return nil
		}
		color[id] = 1
		stack = append(stack, id)
		for _, dep := range adj[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = 2
		return nil
	}
	ids := make([]int, 0, len(state))
	for id := range state {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func closureFrom(changed []int, dependents map[int][]int) []int {
	seen := map[int]bool{}
	queue := append([]int(nil), changed...)
	out := make([]int, 0, len(changed))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		queue = append(queue, dependents[id]...)
	}
	sort.Ints(out)
	return out
}

// DeriveFamilyPredecessors is exposed for integrations that construct domain
// projects without a schema loader. It adds the immediately preceding batch in
// each family's declared order as a predecessor and returns a sorted union.
func DeriveFamilyPredecessors(p domain.Project) (map[int][]int, error) {
	out := map[int][]int{}
	seen := map[int]bool{}
	for _, f := range p.Families {
		var prev int
		for _, id := range f.BatchIDs {
			if seen[id] {
				return nil, &domain.Error{Code: domain.CodeBatchDuplicate, Context: "families." + f.ID, BatchID: id}
			}
			seen[id] = true
			b, ok := p.FindBatch(id)
			if !ok {
				return nil, &domain.Error{Code: domain.CodeBatchUnknown, Context: "families." + f.ID, BatchID: id}
			}
			deps := append([]int(nil), b.DependsOn...)
			if prev != 0 {
				deps = append(deps, prev)
			}
			out[id] = domain.CanonicalIntSet(deps)
			prev = id
		}
	}
	return out, nil
}
