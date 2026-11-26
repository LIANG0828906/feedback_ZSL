package engine

import (
	"fmt"
	"sort"

	"meshpolicy/internal/model"
	"meshpolicy/internal/xerr"
)

type Overlap struct {
	Namespace        string   `json:"namespace"`
	PolicyIDs        []string `json:"policy_ids"`
	SharedWorkloads  []string `json:"shared_workloads"`
	PortOverlap      string   `json:"port_overlap"`
	WinnerByPriority string   `json:"winner_by_priority_order"`
}

func AnalyzeOverlaps(p *model.Project) []Overlap {
	workloads := map[string]model.Workload{}
	for _, w := range p.Workloads {
		workloads[w.ID] = w
	}
	var active []model.Policy
	for _, pol := range p.Policies {
		if pol.State == model.StateActive {
			active = append(active, pol)
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		if active[i].Namespace != active[j].Namespace {
			return active[i].Namespace < active[j].Namespace
		}
		if active[i].Priority != active[j].Priority {
			return active[i].Priority > active[j].Priority
		}
		return active[i].Order < active[j].Order
	})

	var out []Overlap
	for i := 0; i < len(active); i++ {
		for j := i + 1; j < len(active); j++ {
			a, b := active[i], active[j]
			if a.Namespace != b.Namespace {
				continue
			}
			shared := sharedSelectorWorkloads(a, b, workloads)
			if len(shared) == 0 || !portsOverlap(a, b) {
				continue
			}
			winner := a.ID
			if b.Priority > a.Priority || (b.Priority == a.Priority && b.Order < a.Order) {
				winner = b.ID
			}
			out = append(out, Overlap{
				Namespace: a.Namespace, PolicyIDs: []string{a.ID, b.ID}, SharedWorkloads: shared,
				PortOverlap:      fmt.Sprintf("%d-%d intersect %d-%d", a.PortFrom, a.PortTo, b.PortFrom, b.PortTo),
				WinnerByPriority: winner,
			})
		}
	}
	return out
}

func OverlapError(p *model.Project) error {
	overlaps := AnalyzeOverlaps(p)
	if len(overlaps) == 0 {
		return nil
	}
	first := overlaps[0]
	return xerr.New(xerr.ESelectorOverlap, "overlapping selectors require explicit priority adjudication", map[string]any{
		"path": "$.policies.source_selector", "namespace": first.Namespace,
		"policy_ids": first.PolicyIDs, "shared_workloads": first.SharedWorkloads,
		"winner_by_priority_order": first.WinnerByPriority, "overlap_count": len(overlaps),
	})
}

func sharedSelectorWorkloads(a, b model.Policy, workloads map[string]model.Workload) []string {
	var out []string
	for id, w := range workloads {
		if SelectorMatches(a.Source, a.Namespace, w) && SelectorMatches(b.Source, b.Namespace, w) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func portsOverlap(a, b model.Policy) bool {
	return a.PortFrom <= b.PortTo && b.PortFrom <= a.PortTo
}
