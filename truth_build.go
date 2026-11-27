package planopt

import "sort"

// BuildTruth derives the generator's construction truth. Selectivity truth is
// the declared distribution target converted to the common PPM fixed scale;
// operator and cost truth are produced from the explicitly constructed plan
// recipe using the same deterministic integer rules shipped to integrators.
func BuildTruth(project Project) (Truth, error) {
	optimized, err := OptimizeProject(project, DefaultBindings(project))
	if err != nil {
		return Truth{}, err
	}
	bySeq := map[int]OptimizedPlan{}
	for _, opt := range optimized {
		bySeq[opt.PlanSeq] = opt
	}

	plans := make([]PlanTruth, 0, len(project.Plans))
	for _, plan := range project.SortedPlans() {
		opt := bySeq[plan.Seq]
		opTruth := make([]NormalizedOpTruth, 0, len(opt.Normalized.Operators))
		for _, op := range opt.Normalized.Operators {
			opTruth = append(opTruth, NormalizedOpTruth{
				Ordinal: op.Ordinal, Key: op.Key, SourceNodeID: op.SourceNodeID,
				Kind: op.Kind, Table: op.Table,
				PushedPredicateID: firstOrEmpty(op.PredicateIDs),
				OutputColumns:     append([]string(nil), op.OutputColumns...),
			})
		}

		predTruth := map[string]PredicateTruth{}
		estByID := map[string]PredicateEstimate{}
		for _, es := range opt.Estimates {
			estByID[es.PredicateID] = es
		}
		for _, pred := range plan.Predicates {
			es := estByID[pred.ID]
			downstream := downstreamOrdinal(opt.Normalized, pred.ID)
			predTruth[pred.ID] = PredicateTruth{
				PredicateID: pred.ID,
				// Declared selectivity is the generator's construction target.
				SelectivityPPM:    pred.DeclaredSelPermyriad * (SelBasis / ScalePermyriad),
				BucketIndex:       es.BucketIndex,
				BucketBoundary:    es.BucketBoundary,
				DownstreamOrdinal: downstream,
			}
		}

		paths := map[string]string{}
		for table, path := range opt.SelectedPath {
			paths[table] = path
		}
		plans = append(plans, PlanTruth{
			PlanSeq: plan.Seq, PlanID: plan.ID, Operators: opTruth,
			RetainedColumns: append([]string(nil), opt.Normalized.RetainedColumns...),
			PrunedColumns:   append([]string(nil), opt.Normalized.PrunedColumns...),
			Optimization: CostTruth{
				JoinOrder:  append([]string(nil), opt.JoinOrder...),
				Paths:      paths,
				CostMilli:  opt.CostMilli,
				Predicates: predTruth,
			},
		})
	}
	return Truth{Seed: project.Seed, Plans: plans}, nil
}

func buildConstructionTruth(project Project) Truth {
	truth, err := BuildTruth(project)
	if err != nil {
		panic(err)
	}
	return truth
}

func firstOrEmpty(in []string) string {
	if len(in) == 0 {
		return ""
	}
	return in[0]
}

func downstreamOrdinal(norm NormalizedPlan, predicateID string) int {
	for _, op := range norm.Operators {
		for _, pid := range op.PredicateIDs {
			if pid == predicateID {
				return op.Ordinal
			}
		}
	}
	// Duplicate predicates were merged; point the duplicate at the effective
	// predicate's operator when possible.
	for _, op := range norm.Operators {
		if op.Key == "filter:"+predicateID {
			return op.Ordinal
		}
	}
	return -1
}

var _ = sort.Strings
