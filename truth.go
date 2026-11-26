package queryopt

type PredicateTruth struct {
	PredicateID    string `json:"predicate_id"`
	SelectivityRaw int64  `json:"selectivity_raw"`
	EstimatedRows  int64  `json:"estimated_rows"`
	BaseRows       int64  `json:"base_rows"`
	BoundaryHit    bool   `json:"boundary_hit"`
}

type CandidateTruth struct {
	CandidateID string   `json:"candidate_id"`
	Order       []string `json:"order"`
	Feasible    bool     `json:"feasible"`
	RejectCode  string   `json:"reject_code,omitempty"`
	TotalCost   int64    `json:"total_cost"`
}

type QueryTruth struct {
	QueryID             string           `json:"query_id"`
	SelectedCandidateID string           `json:"selected_candidate_id,omitempty"`
	SelectedOrder       []string         `json:"selected_order,omitempty"`
	RulingBasisID       string           `json:"ruling_basis_id,omitempty"`
	ExpectedErrorCode   string           `json:"expected_error_code,omitempty"`
	CutEdgeIDs          []string         `json:"cut_edge_ids,omitempty"`
	Candidates          []CandidateTruth `json:"candidates"`
}

type Truth struct {
	Seed                  int64            `json:"seed"`
	FixedFractionBits     int              `json:"fixed_fraction_bits"`
	PredicateToleranceRaw int64            `json:"predicate_tolerance_raw"`
	RowTolerance          int64            `json:"row_tolerance"`
	Predicates            []PredicateTruth `json:"predicates"`
	Queries               []QueryTruth     `json:"queries"`
}

// BuildTruth records the deterministic oracle fixture. Selectivity truth is
// independently calculated with arbitrary-precision rationals in
// reference.go; plan/cost truth is produced by the declared integer kernel so
// golden comparisons catch accidental changes across schema editions.
func BuildTruth(p *Project, bindings BindingSet) (*Truth, error) {
	estimates, err := EstimateProject(p, bindings)
	if err != nil {
		return nil, err
	}
	ref, err := ReferenceSelectivities(p, bindings)
	if err != nil {
		return nil, err
	}
	t := &Truth{Seed: p.Seed, FixedFractionBits: FixedFractionBits, PredicateToleranceRaw: 64, RowTolerance: 1}
	for _, pred := range p.Predicates {
		got := estimates.Predicates[pred.ID]
		expectedRaw := got.SelectivityRaw
		expectedRows := got.EstimatedRows
		baseRows := int64(1)
		if rv, ok := ref[pred.ID]; ok {
			expectedRaw = rv.Raw
			expectedRows = rv.Rows
			baseRows = rv.BaseRows
		}
		t.Predicates = append(t.Predicates, PredicateTruth{
			PredicateID: pred.ID, SelectivityRaw: expectedRaw,
			EstimatedRows: expectedRows, BaseRows: baseRows, BoundaryHit: got.BoundaryHit,
		})
	}
	opts := OptionsFromProject(p)
	for _, q := range p.Queries {
		plan, planErr := PlanQuery(p, estimates, q.ID, bindings, opts)
		qt := QueryTruth{QueryID: q.ID}
		if plan != nil {
			qt.SelectedCandidateID = plan.SelectedCandidateID
			qt.SelectedOrder = append([]string{}, plan.SelectedOrder...)
			qt.RulingBasisID = plan.RulingBasisID
			qt.CutEdgeIDs = append([]string{}, plan.CutEdgeIDs...)
			for _, c := range plan.Candidates {
				qt.Candidates = append(qt.Candidates, CandidateTruth{
					CandidateID: c.CandidateID, Order: append([]string{}, c.Order...),
					Feasible: c.Feasible, RejectCode: c.RejectCode, TotalCost: c.Cost.TotalCost,
				})
			}
		}
		if planErr != nil {
			if e, ok := AsError(planErr); ok {
				qt.ExpectedErrorCode = e.Code
			} else {
				return nil, planErr
			}
		}
		t.Queries = append(t.Queries, qt)
	}
	return t, nil
}
