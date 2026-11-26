package queryopt

import "sort"

type AccessPathChange struct {
	TableID string `json:"table_id"`
	Before  string `json:"before"`
	After   string `json:"after"`
}

type QuerySensitivity struct {
	QueryID             string             `json:"query_id"`
	Ordinal             int                `json:"ordinal"`
	OrderSwitched       bool               `json:"order_switched"`
	AccessPathSwitched  bool               `json:"access_path_switched"`
	OrderBefore         []string           `json:"order_before,omitempty"`
	OrderAfter          []string           `json:"order_after,omitempty"`
	SelectedBefore      string             `json:"selected_candidate_before,omitempty"`
	SelectedAfter       string             `json:"selected_candidate_after,omitempty"`
	CostBefore          int64              `json:"cost_before"`
	CostAfter           int64              `json:"cost_after"`
	CostDelta           int64              `json:"cost_delta_after_minus_before"`
	TriggerPredicateIDs []string           `json:"trigger_predicate_ids,omitempty"`
	ChangedPredicateIDs []string           `json:"changed_predicate_ids,omitempty"`
	AccessPathChanges   []AccessPathChange `json:"access_path_changes,omitempty"`
	BeforeErrorCode     string             `json:"before_error_code,omitempty"`
	AfterErrorCode      string             `json:"after_error_code,omitempty"`
}

type SensitivityReport struct {
	Before  BindingSet         `json:"before"`
	After   BindingSet         `json:"after"`
	Queries []QuerySensitivity `json:"queries"`
}

func AnalyzeSensitivity(p *Project, before, after BindingSet, opts OptimizeOptions) (*SensitivityReport, error) {
	bEst, bPlans, err := FullRecompute(p, before, opts)
	if err != nil {
		return nil, err
	}
	aEst, aPlans, err := FullRecompute(p, after, opts)
	if err != nil {
		return nil, err
	}
	report := &SensitivityReport{Before: before, After: after}
	for _, q := range p.Queries {
		bp := bPlans[q.ID]
		ap := aPlans[q.ID]
		qs := QuerySensitivity{QueryID: q.ID, Ordinal: q.Ordinal}
		if bp != nil {
			qs.OrderBefore = append([]string{}, bp.SelectedOrder...)
			qs.SelectedBefore = bp.SelectedCandidateID
			qs.BeforeErrorCode = bp.ErrorCode
			qs.CostBefore = selectedCost(bp)
			qs.AccessPathChanges = appendPathChanges(qs.AccessPathChanges, bp.AccessPaths, nil, true)
		}
		if ap != nil {
			qs.OrderAfter = append([]string{}, ap.SelectedOrder...)
			qs.SelectedAfter = ap.SelectedCandidateID
			qs.AfterErrorCode = ap.ErrorCode
			qs.CostAfter = selectedCost(ap)
			if bp != nil {
				qs.AccessPathChanges = appendPathChanges(nil, bp.AccessPaths, ap.AccessPaths, false)
			}
		}
		qs.CostDelta = qs.CostAfter - qs.CostBefore
		if bp != nil && ap != nil {
			qs.OrderSwitched = !equalStrings(bp.SelectedOrder, ap.SelectedOrder)
		}
		for _, pred := range p.Predicates {
			if pred.QueryID != q.ID {
				continue
			}
			be := bEst.Predicates[pred.ID]
			ae := aEst.Predicates[pred.ID]
			if be.SelectivityRaw != ae.SelectivityRaw || be.EstimatedRows != ae.EstimatedRows {
				qs.ChangedPredicateIDs = append(qs.ChangedPredicateIDs, pred.ID)
				if pred.Kind == PredEquality || pred.Kind == PredRange || pred.Kind == PredBoundary {
					qs.TriggerPredicateIDs = append(qs.TriggerPredicateIDs, pred.ID)
				}
			}
		}
		for _, ch := range qs.AccessPathChanges {
			if ch.Before != ch.After {
				qs.AccessPathSwitched = true
				break
			}
		}
		sort.Strings(qs.ChangedPredicateIDs)
		sort.Strings(qs.TriggerPredicateIDs)
		report.Queries = append(report.Queries, qs)
	}
	sort.Slice(report.Queries, func(i, j int) bool { return report.Queries[i].Ordinal < report.Queries[j].Ordinal })
	return report, nil
}

func selectedCost(p *QueryPlan) int64 {
	if p == nil {
		return 0
	}
	for _, c := range p.Candidates {
		if c.CandidateID == p.SelectedCandidateID {
			return c.Cost.TotalCost
		}
	}
	return 0
}

func appendPathChanges(existing []AccessPathChange, before, after map[string]string, initial bool) []AccessPathChange {
	if initial {
		return existing
	}
	ids := map[string]bool{}
	for id := range before {
		ids[id] = true
	}
	for id := range after {
		ids[id] = true
	}
	out := append([]AccessPathChange{}, existing...)
	for id := range ids {
		b, a := before[id], after[id]
		if b != a {
			out = append(out, AccessPathChange{TableID: id, Before: b, After: a})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TableID < out[j].TableID })
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
