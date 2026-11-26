package engine

import (
	"fmt"
	"sort"

	"meshpolicy/internal/model"
)

func BuildPropagation(p *model.Project) *PropagationReport {
	report := &PropagationReport{ByAttempt: map[string][]MarkerRef{}}
	events := p.SortedEvents()
	for _, ev := range events {
		if ev.Change != nil {
			layers, refs := PropagateChange(p, ev.Change)
			report.Layers = append(report.Layers, layers...)
			for id, rr := range refs {
				report.ByAttempt[id] = append(report.ByAttempt[id], rr...)
			}
		}
	}
	for id := range report.ByAttempt {
		sort.SliceStable(report.ByAttempt[id], func(i, j int) bool {
			a, b := report.ByAttempt[id][i], report.ByAttempt[id][j]
			if a.ArrivalSeq != b.ArrivalSeq {
				return a.ArrivalSeq < b.ArrivalSeq
			}
			if a.Generation != b.Generation {
				return a.Generation < b.Generation
			}
			return a.MarkerID < b.MarkerID
		})
	}
	return report
}

func PropagateChange(p *model.Project, change *model.PolicyChange) ([]MarkerLayer, map[string][]MarkerRef) {
	workloads := map[string]model.Workload{}
	attempts := map[string]model.ConnectionAttempt{}
	for _, w := range p.Workloads {
		workloads[w.ID] = w
	}
	allAttempts := p.Attempts()
	for _, a := range allAttempts {
		attempts[a.ID] = a
	}

	sourcePolicy := change.PolicyID
	if sourcePolicy == "" && change.After != nil {
		sourcePolicy = change.After.ID
	}
	if sourcePolicy == "" && change.Before != nil {
		sourcePolicy = change.Before.ID
	}

	root := MarkerLayer{
		MarkerID: markerID(change.ID, 0), ChangeID: change.ID, SourcePolicyID: sourcePolicy,
		Kind: "root", Generation: 0, BornSeq: change.Seq, ArrivalSeq: change.Seq,
		ReachedWorkloads: []string{}, NewArrivals: []string{}, ArrivalSet: []string{},
	}
	layers := []MarkerLayer{root}
	refs := map[string][]MarkerRef{}

	direct := []string{}
	for _, a := range allAttempts {
		if a.Seq < change.Seq {
			continue
		}
		if changeTouchesAttempt(p, change, a, workloads) {
			direct = append(direct, a.ID)
		}
	}
	sort.Strings(direct)

	assigned := map[string]bool{}
	cumulative := []string{}
	seenWorkloads := map[string]bool{}
	if len(direct) > 0 {
		for _, id := range direct {
			assigned[id] = true
			cumulative = append(cumulative, id)
			a := attempts[id]
			seenWorkloads[a.SourceWorkloadID] = true
			seenWorkloads[a.TargetWorkloadID] = true
			refs[id] = append(refs[id], MarkerRef{
				MarkerID: markerID(change.ID, 1), ChangeID: change.ID, SourcePolicyID: sourcePolicy,
				Generation: 1, ArrivalSeq: change.Seq + 1,
				BornSeq: change.Seq,
			})
		}
		reached := model.SortedKeys(seenWorkloads)
		sort.Strings(cumulative)
		layers = append(layers, MarkerLayer{
			MarkerID: markerID(change.ID, 1), ChangeID: change.ID, SourcePolicyID: sourcePolicy,
			Kind: "direct_selector_match", Generation: 1, BornSeq: change.Seq, ArrivalSeq: change.Seq + 1,
			ReachedWorkloads: reached, NewArrivals: append([]string(nil), direct...),
			ArrivalSet: append([]string(nil), cumulative...),
		})
	}

	frontier := model.SortedKeys(seenWorkloads)
	maxGen := len(p.Workloads) + 2
	for gen := 2; gen <= maxGen; gen++ {
		nextWorkloads := map[string]bool{}
		for _, id := range frontier {
			w, ok := workloads[id]
			if !ok {
				continue
			}
			for _, dep := range w.DependsOn {
				if !seenWorkloads[dep] {
					nextWorkloads[dep] = true
				}
			}
		}
		if len(nextWorkloads) == 0 {
			break
		}
		for id := range nextWorkloads {
			seenWorkloads[id] = true
		}
		newArrivals := []string{}
		for _, a := range allAttempts {
			if a.Seq < change.Seq || assigned[a.ID] {
				continue
			}
			if nextWorkloads[a.SourceWorkloadID] || nextWorkloads[a.TargetWorkloadID] {
				newArrivals = append(newArrivals, a.ID)
			}
		}
		sort.Strings(newArrivals)
		for _, id := range newArrivals {
			assigned[id] = true
			cumulative = append(cumulative, id)
			refs[id] = append(refs[id], MarkerRef{
				MarkerID: markerID(change.ID, gen), ChangeID: change.ID, SourcePolicyID: sourcePolicy,
				Generation: gen, ArrivalSeq: change.Seq + int64(gen),
				BornSeq: change.Seq,
			})
		}
		sort.Strings(cumulative)
		layers = append(layers, MarkerLayer{
			MarkerID: markerID(change.ID, gen), ChangeID: change.ID, SourcePolicyID: sourcePolicy,
			Kind: "dependency_propagation", Generation: gen, BornSeq: change.Seq,
			ArrivalSeq: change.Seq + int64(gen), ReachedWorkloads: model.SortedKeys(nextWorkloads),
			NewArrivals: newArrivals, ArrivalSet: append([]string(nil), cumulative...),
		})
		frontier = model.SortedKeys(nextWorkloads)
	}
	return layers, refs
}

func markerID(changeID string, gen int) string {
	return fmt.Sprintf("MRK-%s-G%d", changeID, gen)
}

func changeTouchesAttempt(p *model.Project, c *model.PolicyChange, a model.ConnectionAttempt, workloads map[string]model.Workload) bool {
	if c.WorkloadID != "" && (a.SourceWorkloadID == c.WorkloadID || a.TargetWorkloadID == c.WorkloadID) {
		return true
	}
	if c.Kind == model.ChangeWorkload {
		id := c.WorkloadID
		if id == "" && c.After != nil {
			id = c.After.ID
		}
		if a.SourceWorkloadID == id || a.TargetWorkloadID == id {
			return true
		}
	}

	versions := []*model.Policy{c.Before, c.After}
	if c.Before == nil && c.After == nil && c.PolicyID != "" {
		if pol, ok := p.PolicyByID(c.PolicyID); ok {
			versions = append(versions, &pol)
		}
	}
	if c.Kind == model.ChangeDelete && c.Before != nil {
		versions = []*model.Policy{c.Before}
	}
	for _, pol := range versions {
		if pol == nil {
			continue
		}
		if PolicyCouldMatch(*pol, a, workloads) {
			return true
		}
	}
	return false
}

func PolicyCouldMatch(pol model.Policy, a model.ConnectionAttempt, workloads map[string]model.Workload) bool {
	target, ok := workloads[a.TargetWorkloadID]
	if !ok || pol.Namespace != target.Namespace {
		return false
	}
	source, ok := workloads[a.SourceWorkloadID]
	if !ok || !SelectorMatches(pol.Source, pol.Namespace, source) {
		return false
	}
	return PortMatches(pol, a.Port)
}
