package planopt

import (
	"encoding/json"
	"fmt"
	"sort"
)

type EditKind string

const (
	EditTableStats    EditKind = "TABLE_STATS"
	EditPredicate     EditKind = "PREDICATE_CONDITION"
	EditOperatorParam EditKind = "OPERATOR_PARAM"
)

type Edit struct {
	Kind        EditKind    `json:"kind"`
	PlanSeq     int         `json:"plan_seq,omitempty"`
	Table       string      `json:"table,omitempty"`
	PredicateID string      `json:"predicate_id,omitempty"`
	NodeID      string      `json:"node_id,omitempty"`
	ParamKey    string      `json:"param_key,omitempty"`
	ParamValue  int64       `json:"param_value,omitempty"`
	Column      *ColumnStat `json:"column,omitempty"`
	RowCount    *int64      `json:"row_count,omitempty"`
	Predicate   *Predicate  `json:"predicate,omitempty"`
}

type CacheEntry struct {
	Key          string   `json:"key"`
	Revision     int64    `json:"revision"`
	Dependencies []string `json:"dependencies"`
}

type Cache struct {
	Revision int64                 `json:"revision"`
	Entries  map[string]CacheEntry `json:"entries"`
}

type OperatorInterval struct {
	PlanSeq        int `json:"plan_seq"`
	FromOrdinal    int `json:"from_ordinal"`
	ToOrdinal      int `json:"to_ordinal"`
	TouchedOrdinal int `json:"touched_ordinal"`
}

type IncrementalStep struct {
	Edit               Edit               `json:"edit"`
	InvalidationSource string             `json:"invalidation_source"`
	AffectedPlans      []int              `json:"affected_plans"`
	PropagationClosure []string           `json:"propagation_closure"`
	RecomputedKeys     []string           `json:"recomputed_keys"`
	ReusedKeys         []string           `json:"reused_keys"`
	OperatorIntervals  []OperatorInterval `json:"operator_intervals"`
}

type IncrementalReport struct {
	Steps   []IncrementalStep `json:"steps"`
	Results []OptimizedPlan   `json:"results"`
	Cache   Cache             `json:"cache"`
}

type IncrementalEngine struct {
	project  Project
	bindings BindingSet
	results  map[int]OptimizedPlan
	cache    Cache
	revision int64
}

func NewIncrementalEngine(project Project, bindings BindingSet) (*IncrementalEngine, error) {
	opts, err := OptimizeProject(project, bindings)
	if err != nil {
		return nil, err
	}
	e := &IncrementalEngine{
		project:  cloneProject(project),
		bindings: bindings,
		results:  map[int]OptimizedPlan{},
		cache:    Cache{Revision: 1, Entries: map[string]CacheEntry{}},
		revision: 1,
	}
	for _, opt := range opts {
		e.results[opt.PlanSeq] = opt
		e.addPlanCache(opt)
	}
	return e, nil
}

func (e *IncrementalEngine) Results() []OptimizedPlan {
	out := make([]OptimizedPlan, 0, len(e.results))
	for _, p := range e.project.SortedPlans() {
		if r, ok := e.results[p.Seq]; ok {
			out = append(out, r)
		}
	}
	return out
}

func (e *IncrementalEngine) ProjectSnapshot() Project {
	return cloneProject(e.project)
}

func (e *IncrementalEngine) CacheSnapshot() Cache {
	out := Cache{Revision: e.cache.Revision, Entries: map[string]CacheEntry{}}
	for k, v := range e.cache.Entries {
		v.Dependencies = append([]string(nil), v.Dependencies...)
		out.Entries[k] = v
	}
	return out
}

// ApplyEdits applies edits in logical order. Each step records the stable
// invalidation source and its reverse-dependency propagation closure.
func (e *IncrementalEngine) ApplyEdits(edits ...Edit) (IncrementalReport, error) {
	report := IncrementalReport{}
	for _, edit := range edits {
		step, err := e.applyEdit(edit)
		if err != nil {
			return IncrementalReport{}, err
		}
		report.Steps = append(report.Steps, step)
	}
	report.Results = e.Results()
	report.Cache = e.CacheSnapshot()
	return report, nil
}

func (e *IncrementalEngine) applyEdit(edit Edit) (IncrementalStep, error) {
	source, err := applyEditToProject(&e.project, edit)
	if err != nil {
		return IncrementalStep{}, err
	}
	if err := ValidateProject(e.project); err != nil {
		return IncrementalStep{}, err
	}

	forward := e.buildDependencies()
	reverse := map[string][]string{}
	for key, deps := range forward {
		for _, dep := range deps {
			reverse[dep] = append(reverse[dep], key)
		}
	}
	closureSet := map[string]bool{source: true}
	queue := []string{source}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range reverse[cur] {
			if !closureSet[next] {
				closureSet[next] = true
				queue = append(queue, next)
			}
		}
	}
	closure := keysSorted(closureSet)

	recomputed := []string{}
	reused := []string{}
	for key := range e.cache.Entries {
		if closureSet[key] {
			recomputed = append(recomputed, key)
			delete(e.cache.Entries, key)
		} else {
			reused = append(reused, key)
		}
	}
	sort.Strings(recomputed)
	sort.Strings(reused)

	affectedSet := map[int]bool{}
	for key := range closureSet {
		var seq int
		if _, err := fmt.Sscanf(key, "optimize:%d", &seq); err == nil {
			affectedSet[seq] = true
		}
	}
	if edit.PlanSeq > 0 {
		affectedSet[edit.PlanSeq] = true
	}
	affected := sortedIntSet(affectedSet)

	intervals := []OperatorInterval{}
	for _, seq := range affected {
		plan, ok := e.project.Plan(seq)
		if !ok {
			continue
		}
		opt, err := OptimizePlan(e.project, plan, e.bindings)
		if err != nil {
			return IncrementalStep{}, err
		}
		e.results[seq] = opt
		e.revision++
		e.addPlanCache(opt)
		intervals = append(intervals, operatorInterval(edit, opt))
	}
	e.cache.Revision = e.revision

	return IncrementalStep{
		Edit: edit, InvalidationSource: source, AffectedPlans: affected,
		PropagationClosure: closure, RecomputedKeys: recomputed, ReusedKeys: reused,
		OperatorIntervals: intervals,
	}, nil
}

func applyEditToProject(project *Project, edit Edit) (string, error) {
	switch edit.Kind {
	case EditTableStats:
		if edit.Table == "" {
			return "", Errorf(CodeInvalidEdit, "incremental.edit_table", "table name is required")
		}
		idx := -1
		for i := range project.Tables {
			if project.Tables[i].Name == edit.Table {
				idx = i
				break
			}
		}
		if idx < 0 {
			return "", Errorf(CodeInvalidEdit, "incremental.edit_table", "unknown table %q", edit.Table)
		}
		if edit.RowCount != nil {
			if *edit.RowCount < 0 {
				return "", Errorf(CodeScaleOutOfRange, "incremental.row_count", "row count must be non-negative")
			}
			project.Tables[idx].RowCount = *edit.RowCount
		}
		if edit.Column != nil {
			colIdx := -1
			for j := range project.Tables[idx].Columns {
				if project.Tables[idx].Columns[j].Name == edit.Column.Name {
					colIdx = j
					break
				}
			}
			if colIdx < 0 {
				return "", Errorf(CodeInvalidEdit, "incremental.edit_column", "unknown column %q.%q", edit.Table, edit.Column.Name)
			}
			col := *edit.Column
			col.Ordinal = project.Tables[idx].Columns[colIdx].Ordinal
			project.Tables[idx].Columns[colIdx] = col
		}
		return "table:" + edit.Table + ":stats", nil

	case EditPredicate:
		if edit.PlanSeq == 0 || edit.PredicateID == "" || edit.Predicate == nil {
			return "", Errorf(CodeInvalidEdit, "incremental.edit_predicate", "plan_seq, predicate_id and predicate are required")
		}
		planIdx := planIndex(project, edit.PlanSeq)
		if planIdx < 0 {
			return "", Errorf(CodeInvalidEdit, "incremental.edit_predicate", "unknown plan seq %d", edit.PlanSeq)
		}
		predIdx := -1
		for i := range project.Plans[planIdx].Predicates {
			if project.Plans[planIdx].Predicates[i].ID == edit.PredicateID {
				predIdx = i
				break
			}
		}
		if predIdx < 0 {
			return "", Errorf(CodeInvalidEdit, "incremental.edit_predicate", "unknown predicate %s", edit.PredicateID)
		}
		replacement := *edit.Predicate
		replacement.ID = edit.PredicateID
		replacement.Ordinal = project.Plans[planIdx].Predicates[predIdx].Ordinal
		project.Plans[planIdx].Predicates[predIdx] = replacement
		return fmt.Sprintf("predicate:%d:%s", edit.PlanSeq, edit.PredicateID), nil

	case EditOperatorParam:
		if edit.PlanSeq == 0 || edit.NodeID == "" || edit.ParamKey == "" {
			return "", Errorf(CodeInvalidEdit, "incremental.edit_node", "plan_seq, node_id and param_key are required")
		}
		planIdx := planIndex(project, edit.PlanSeq)
		if planIdx < 0 {
			return "", Errorf(CodeInvalidEdit, "incremental.edit_node", "unknown plan seq %d", edit.PlanSeq)
		}
		nodeIdx := -1
		for i := range project.Plans[planIdx].Nodes {
			if project.Plans[planIdx].Nodes[i].ID == edit.NodeID {
				nodeIdx = i
				break
			}
		}
		if nodeIdx < 0 {
			return "", Errorf(CodeInvalidEdit, "incremental.edit_node", "unknown node %s", edit.NodeID)
		}
		node := &project.Plans[planIdx].Nodes[nodeIdx]
		if node.OperatorParams == nil {
			node.OperatorParams = map[string]int64{}
		}
		node.OperatorParams[edit.ParamKey] = edit.ParamValue
		if edit.ParamKey == "limit_rows" || edit.ParamKey == "limitValue" || edit.ParamKey == "limit" {
			node.LimitValue = edit.ParamValue
		}
		return fmt.Sprintf("node:%d:%s", edit.PlanSeq, edit.NodeID), nil

	default:
		return "", Errorf(CodeInvalidEdit, "incremental.edit_kind", "unsupported edit kind %q", edit.Kind)
	}
}

func (e *IncrementalEngine) buildDependencies() map[string][]string {
	out := map[string][]string{}
	for _, plan := range e.project.Plans {
		seq := plan.Seq
		out[fmt.Sprintf("normalize:%d", seq)] = []string{fmt.Sprintf("plan:%d:definition", seq)}
		for _, node := range plan.Nodes {
			out[fmt.Sprintf("plan:%d:definition", seq)] = append(out[fmt.Sprintf("plan:%d:definition", seq)], fmt.Sprintf("node:%d:%s", seq, node.ID))
		}
		for _, pred := range plan.Predicates {
			estKey := fmt.Sprintf("estimate:%d:%s", seq, pred.ID)
			out[estKey] = []string{
				fmt.Sprintf("predicate:%d:%s", seq, pred.ID),
				"table:" + pred.Table + ":stats",
			}
			tableSel := fmt.Sprintf("tablesel:%d:%s", seq, pred.Table)
			out[tableSel] = append(out[tableSel], estKey)
		}
		for _, table := range involvedTables(plan) {
			pathKey := fmt.Sprintf("path:%d:%s", seq, table)
			tableSel := fmt.Sprintf("tablesel:%d:%s", seq, table)
			out[pathKey] = []string{tableSel, "table:" + table + ":stats"}
			optKey := fmt.Sprintf("optimize:%d", seq)
			out[optKey] = append(out[optKey], pathKey, fmt.Sprintf("normalize:%d", seq), tableSel)
		}
	}
	return out
}

func (e *IncrementalEngine) addPlanCache(opt OptimizedPlan) {
	seq := opt.PlanSeq
	e.cache.Entries[fmt.Sprintf("normalize:%d", seq)] = CacheEntry{Key: fmt.Sprintf("normalize:%d", seq), Revision: e.revision}
	for _, es := range opt.Estimates {
		key := fmt.Sprintf("estimate:%d:%s", seq, es.PredicateID)
		table := ""
		for _, pred := range opt.Normalized.Predicates {
			if pred.ID == es.PredicateID {
				table = pred.Table
				break
			}
		}
		e.cache.Entries[key] = CacheEntry{Key: key, Revision: e.revision, Dependencies: []string{
			fmt.Sprintf("predicate:%d:%s", seq, es.PredicateID), "table:" + table + ":stats",
		}}
	}
	for _, te := range opt.Tables {
		key := fmt.Sprintf("tablesel:%d:%s", seq, te.Table)
		e.cache.Entries[key] = CacheEntry{Key: key, Revision: e.revision}
		pathKey := fmt.Sprintf("path:%d:%s", seq, te.Table)
		e.cache.Entries[pathKey] = CacheEntry{Key: pathKey, Revision: e.revision}
	}
	e.cache.Entries[fmt.Sprintf("optimize:%d", seq)] = CacheEntry{Key: fmt.Sprintf("optimize:%d", seq), Revision: e.revision}
}

func operatorInterval(edit Edit, opt OptimizedPlan) OperatorInterval {
	interval := OperatorInterval{PlanSeq: opt.PlanSeq, FromOrdinal: 1, ToOrdinal: len(opt.Normalized.Operators), TouchedOrdinal: -1}
	switch edit.Kind {
	case EditTableStats:
		for _, op := range opt.Normalized.Operators {
			if op.Table == edit.Table || columnsHaveTable(op.OutputColumns, edit.Table) {
				interval.TouchedOrdinal = op.Ordinal
				interval.FromOrdinal = op.Ordinal
				break
			}
		}
	case EditPredicate:
		predID := edit.PredicateID
		for _, op := range opt.Normalized.Operators {
			if stringInSlice(predID, op.PredicateIDs) {
				interval.TouchedOrdinal = op.Ordinal
				interval.FromOrdinal = op.Ordinal
				break
			}
		}
		if interval.TouchedOrdinal < 0 {
			for _, d := range opt.Normalized.Pushdowns {
				if d.PredicateID == predID || d.FilterNodeID != "" {
					for _, op := range opt.Normalized.Operators {
						if op.SourceNodeID == d.FilterNodeID && op.Kind == "FILTER" {
							interval.TouchedOrdinal = op.Ordinal
							interval.FromOrdinal = op.Ordinal
						}
					}
				}
			}
		}
	case EditOperatorParam:
		for _, op := range opt.Normalized.Operators {
			if op.SourceNodeID == edit.NodeID {
				interval.TouchedOrdinal = op.Ordinal
				interval.FromOrdinal = op.Ordinal
				break
			}
		}
	}
	if interval.FromOrdinal <= 0 {
		interval.FromOrdinal = 1
	}
	return interval
}

func columnsHaveTable(cols []string, table string) bool {
	prefix := table + "."
	for _, c := range cols {
		if len(c) > len(prefix) && c[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func stringInSlice(v string, in []string) bool {
	for _, s := range in {
		if s == v {
			return true
		}
	}
	return false
}

func planIndex(project *Project, seq int) int {
	for i := range project.Plans {
		if project.Plans[i].Seq == seq {
			return i
		}
	}
	return -1
}

func keysSorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedIntSet(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func cloneProject(in Project) Project {
	b, _ := json.Marshal(in)
	var out Project
	_ = json.Unmarshal(b, &out)
	return out
}

// FullReoptimize is the comparison oracle for incremental steps.
func FullReoptimize(project Project, bindings BindingSet) ([]OptimizedPlan, error) {
	return OptimizeProject(project, bindings)
}
