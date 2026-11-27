package planopt

type SensitivityReport struct {
	PlanSeq             int               `json:"plan_seq"`
	PlanID              string            `json:"plan_id"`
	Changed             bool              `json:"changed"`
	JoinOrderChanged    bool              `json:"join_order_changed"`
	AccessPathChanged   bool              `json:"access_path_changed"`
	TriggerPredicateIDs []string          `json:"trigger_predicate_ids"`
	BeforeJoinOrder     []string          `json:"before_join_order"`
	AfterJoinOrder      []string          `json:"after_join_order"`
	BeforePaths         map[string]string `json:"before_paths"`
	AfterPaths          map[string]string `json:"after_paths"`
	BeforeCostMilli     int64             `json:"before_cost_milli"`
	AfterCostMilli      int64             `json:"after_cost_milli"`
	CostDeltaMilli      int64             `json:"cost_delta_milli"`
	CostDifferenceMilli int64             `json:"cost_difference_milli"`
}

type SensitivityBundle struct {
	Reports []SensitivityReport `json:"reports"`
}

// CompareBindings optimizes under two bindings and reports whether the physical
// decisions changed. Empty maps fall back to deterministic parameter defaults.
func CompareBindings(project Project, before, after BindingSet) (SensitivityBundle, error) {
	beforeOpts, err := OptimizeProject(project, before)
	if err != nil {
		return SensitivityBundle{}, err
	}
	afterOpts, err := OptimizeProject(project, after)
	if err != nil {
		return SensitivityBundle{}, err
	}
	beforeBySeq := map[int]OptimizedPlan{}
	for _, o := range beforeOpts {
		beforeBySeq[o.PlanSeq] = o
	}
	reports := make([]SensitivityReport, 0, len(afterOpts))
	for _, afterOpt := range afterOpts {
		beforeOpt := beforeBySeq[afterOpt.PlanSeq]
		triggers := changedPredicates(beforeOpt, afterOpt)
		joinChanged := stringSlicesDiffer(beforeOpt.JoinOrder, afterOpt.JoinOrder)
		pathChanged := mapsStringDiffer(beforeOpt.SelectedPath, afterOpt.SelectedPath)
		delta := afterOpt.CostMilli - beforeOpt.CostMilli
		reports = append(reports, SensitivityReport{
			PlanSeq: afterOpt.PlanSeq, PlanID: afterOpt.PlanID,
			Changed:          joinChanged || pathChanged,
			JoinOrderChanged: joinChanged, AccessPathChanged: pathChanged,
			TriggerPredicateIDs: triggers,
			BeforeJoinOrder:     append([]string(nil), beforeOpt.JoinOrder...),
			AfterJoinOrder:      append([]string(nil), afterOpt.JoinOrder...),
			BeforePaths:         cloneStringMap(beforeOpt.SelectedPath),
			AfterPaths:          cloneStringMap(afterOpt.SelectedPath),
			BeforeCostMilli:     beforeOpt.CostMilli, AfterCostMilli: afterOpt.CostMilli,
			CostDeltaMilli: delta, CostDifferenceMilli: abs64(delta),
		})
	}
	return SensitivityBundle{Reports: reports}, nil
}

// GlobalRelativeBindingEquivalence runs the same scenario as absolute global
// values and per-statement relative positions and requires identical reports.
func GlobalRelativeBindingEquivalence(project Project, global map[int]int64, relative map[int]map[int]int64) error {
	globalAfter := BindingSet{Global: global}
	relativeAfter := BindingSet{Relative: relative}

	globalBundle, err := CompareBindings(project, BindingSet{}, globalAfter)
	if err != nil {
		return err
	}
	relativeBundle, err := CompareBindings(project, BindingSet{}, relativeAfter)
	if err != nil {
		return err
	}
	if len(globalBundle.Reports) != len(relativeBundle.Reports) {
		return Errorf(CodeInvariant, "binding.equivalence", "report count mismatch")
	}
	for i := range globalBundle.Reports {
		a := globalBundle.Reports[i]
		b := relativeBundle.Reports[i]
		if a.Changed != b.Changed || a.JoinOrderChanged != b.JoinOrderChanged || a.AccessPathChanged != b.AccessPathChanged ||
			a.CostDifferenceMilli != b.CostDifferenceMilli || joinKey(a.BeforeJoinOrder) != joinKey(b.BeforeJoinOrder) ||
			joinKey(a.AfterJoinOrder) != joinKey(b.AfterJoinOrder) || stringsJoin(a.TriggerPredicateIDs) != stringsJoin(b.TriggerPredicateIDs) {
			return Errorf(CodeInvariant, "binding.equivalence", "plan %d global and relative conclusions differ", a.PlanSeq)
		}
	}
	return nil
}

func changedPredicates(before, after OptimizedPlan) []string {
	beforeSel := map[string]int64{}
	for _, e := range before.Estimates {
		beforeSel[e.PredicateID] = e.SelectivityPPM
	}
	out := []string{}
	for _, e := range after.Estimates {
		if beforeSel[e.PredicateID] != e.SelectivityPPM {
			out = append(out, e.PredicateID)
		}
	}
	return UniqueSorted(out)
}

func stringSlicesDiffer(a, b []string) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

func mapsStringDiffer(a, b map[string]string) bool {
	if len(a) != len(b) {
		return true
	}
	for k, v := range a {
		if b[k] != v {
			return true
		}
	}
	return false
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func joinKey(in []string) string {
	return stringsJoin(in)
}

func stringsJoin(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += "|"
		}
		out += s
	}
	return out
}
