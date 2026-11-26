package queryopt

import (
	"encoding/json"
	"sort"
)

type IncrementalEngine struct {
	project   *Project
	opts      OptimizeOptions
	bindings  BindingSet
	estimates map[string]PredicateEstimate
	plans     map[string]*QueryPlan
	graph     dependencyGraph
}

type dependencyGraph struct {
	Dependents map[string]map[string]bool `json:"dependents"`
}

func newDependencyGraph() dependencyGraph {
	return dependencyGraph{Dependents: map[string]map[string]bool{}}
}

func (g dependencyGraph) add(dependent, dependency string) {
	if g.Dependents[dependency] == nil {
		g.Dependents[dependency] = map[string]bool{}
	}
	g.Dependents[dependency][dependent] = true
}

func (g dependencyGraph) closure(source string) []string {
	seen := map[string]bool{source: true}
	queue := []string{source}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for next := range g.Dependents[cur] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func nodeHist(id string) string            { return "hist:" + id }
func nodeColumn(id string) string          { return "column:" + id }
func nodeTable(id string) string           { return "table:" + id }
func nodePred(id string) string            { return "pred:" + id }
func nodeCandidate(qid, cid string) string { return "candidate:" + qid + "/" + cid }
func nodeDecision(qid string) string       { return "decision:" + qid }
func nodePlan(qid string) string           { return "plan:" + qid }
func nodeQuery(qid string) string          { return "query:" + qid }

func CloneProject(p *Project) (*Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, newError(CodeInputInvalid, "clone project: %v", err)
	}
	cp := &Project{}
	if err := json.Unmarshal(data, cp); err != nil {
		return nil, newError(CodeInputInvalid, "clone project decode: %v", err)
	}
	cp.Indexes = nil
	_ = cp.Index()
	return cp, ValidateProject(cp)
}

func NewIncrementalEngine(p *Project, bindings BindingSet, opts OptimizeOptions) (*IncrementalEngine, error) {
	opts = opts.withProjectDefaults(p)
	if err := ValidateBudget(opts.WorkerBudget, opts.CandidateBuffer); err != nil {
		return nil, err
	}
	cp, err := CloneProject(p)
	if err != nil {
		return nil, err
	}
	report, err := EstimateProject(cp, bindings)
	if err != nil {
		return nil, err
	}
	e := &IncrementalEngine{
		project: cp, bindings: bindings, opts: opts,
		estimates: report.Predicates, plans: map[string]*QueryPlan{},
		graph: newDependencyGraph(),
	}
	for _, q := range cp.Queries {
		plan, planErr := PlanQuery(cp, report, q.ID, bindings, opts)
		if plan != nil {
			e.plans[q.ID] = plan
		}
		if planErr != nil {
			if coded, ok := AsError(planErr); !ok || coded.Code != CodeDisconnectedPlan && coded.Code != CodeCandidateBackpressure {
				return nil, planErr
			}
		}
	}
	e.graph = e.buildGraph()
	return e, nil
}

func (e *IncrementalEngine) Project() *Project              { return e.project }
func (e *IncrementalEngine) Plan(queryID string) *QueryPlan { return e.plans[queryID] }
func (e *IncrementalEngine) EstimateReport() *EstimateReport {
	return &EstimateReport{Predicates: e.estimates}
}

func (e *IncrementalEngine) buildGraph() dependencyGraph {
	g := newDependencyGraph()
	idx := e.project.Index()
	for _, pred := range e.project.Predicates {
		pn := nodePred(pred.ID)
		if pred.ColumnID != "" {
			c := idx.Columns[pred.ColumnID]
			if c != nil {
				g.add(pn, nodeHist(c.ID))
				g.add(pn, nodeColumn(c.ID))
				g.add(pn, nodeTable(c.TableID))
			}
		}
		for _, cid := range []string{pred.LeftColumnID, pred.RightColumnID} {
			if cid == "" {
				continue
			}
			if c := idx.Columns[cid]; c != nil {
				g.add(pn, nodeColumn(c.ID))
				g.add(pn, nodeTable(c.TableID))
			}
		}
		for _, child := range pred.Children {
			g.add(pn, nodePred(child))
		}
	}
	for _, q := range e.project.Queries {
		for _, pred := range e.project.Predicates {
			if pred.QueryID == q.ID {
				g.add(nodePlan(q.ID), nodePred(pred.ID))
			}
		}
		for _, tid := range q.TableIDs {
			g.add(nodePlan(q.ID), nodeTable(tid))
		}
		plan := e.plans[q.ID]
		if plan != nil {
			for _, cand := range plan.Candidates {
				cn := nodeCandidate(q.ID, cand.CandidateID)
				for _, pred := range e.project.Predicates {
					if pred.QueryID == q.ID {
						g.add(cn, nodePred(pred.ID))
					}
				}
				for _, tid := range q.TableIDs {
					g.add(cn, nodeTable(tid))
				}
				g.add(nodeDecision(q.ID), cn)
			}
		}
		g.add(nodePlan(q.ID), nodeDecision(q.ID))
		g.add(nodeQuery(q.ID), nodePlan(q.ID))
	}
	return g
}

type IncrementalQueryResult struct {
	QueryID              string   `json:"query_id"`
	Ordinal              int      `json:"ordinal"`
	CacheHit             bool     `json:"cache_hit"`
	RecomputedPredicates []string `json:"recomputed_predicates,omitempty"`
	InvalidationSources  []string `json:"invalidation_sources,omitempty"`
	PropagationClosure   []string `json:"propagation_closure,omitempty"`
	SelectedOrder        []string `json:"selected_order,omitempty"`
	SelectedCandidateID  string   `json:"selected_candidate_id,omitempty"`
	RulingBasisID        string   `json:"ruling_basis_id,omitempty"`
	ErrorCode            string   `json:"error_code,omitempty"`
}

type EditReport struct {
	EditKind          string                   `json:"edit_kind"`
	SourceNode        string                   `json:"source_node"`
	Closure           []string                 `json:"propagation_closure"`
	Results           []IncrementalQueryResult `json:"results"`
	RecomputedQueries []string                 `json:"recomputed_queries"`
	CacheHitQueries   []string                 `json:"cache_hit_queries"`
}

func (e *IncrementalEngine) applyValidatedEdit(kind, sourceNode string, mutate func(cp *Project) error) (*EditReport, error) {
	cp, err := CloneProject(e.project)
	if err != nil {
		return nil, err
	}
	if err := mutate(cp); err != nil {
		return nil, err
	}
	if err := ValidateProject(cp); err != nil {
		return nil, err
	}
	cp.Indexes = nil
	_ = cp.Index()
	e.project = cp
	return e.invalidate(kind, sourceNode)
}

func (e *IncrementalEngine) EditHistogram(columnID string, buckets []Bucket) (*EditReport, error) {
	idx := e.project.Index()
	if idx.Columns[columnID] == nil {
		return nil, newError(CodeInputInvalid, "histogram edit targets missing column").with("column_id", columnID)
	}
	return e.applyValidatedEdit("histogram", nodeHist(columnID), func(cp *Project) error {
		filtered := make([]Bucket, 0, len(cp.Buckets))
		for _, b := range cp.Buckets {
			if b.ColumnID != columnID {
				filtered = append(filtered, b)
			}
		}
		for i := range buckets {
			b := buckets[i]
			b.ColumnID = columnID
			if b.ID == "" {
				b.ID = "bkt_edit_" + columnID + "_" + string(rune('a'+i))
			}
			if b.BucketIndex == 0 && i != 0 {
				b.BucketIndex = i
			}
			filtered = append(filtered, b)
		}
		cp.Buckets = filtered
		return nil
	})
}

func (e *IncrementalEngine) EditTableRowCount(tableID string, rows int64) (*EditReport, error) {
	return e.applyValidatedEdit("table_rows", nodeTable(tableID), func(cp *Project) error {
		t := cp.Index().Tables[tableID]
		if t == nil {
			return newError(CodeMissingTable, "row count edit targets missing table").with("table_id", tableID)
		}
		t.Rows = rows
		if t.NullRows > rows {
			t.NullRows = 0
		}
		return nil
	})
}

func (e *IncrementalEngine) EditPredicate(pred Predicate) (*EditReport, error) {
	if pred.ID == "" {
		return nil, newError(CodeInputInvalid, "predicate edit requires id")
	}
	idx := e.project.Index()
	if idx.Predicates[pred.ID] == nil {
		return nil, newError(CodeInputInvalid, "predicate edit targets missing predicate").with("predicate_id", pred.ID)
	}
	return e.applyValidatedEdit("predicate", nodePred(pred.ID), func(cp *Project) error {
		for i := range cp.Predicates {
			if cp.Predicates[i].ID == pred.ID {
				pred.QueryID = cp.Predicates[i].QueryID
				cp.Predicates[i] = pred
				return nil
			}
		}
		return nil
	})
}

func (e *IncrementalEngine) invalidate(kind, source string) (*EditReport, error) {
	closure := e.graph.closure(source)
	closureSet := map[string]bool{}
	for _, n := range closure {
		closureSet[n] = true
	}
	dirtyPreds := map[string]bool{}
	affectedQueries := map[string]bool{}
	for _, n := range closure {
		if len(n) > 5 && n[:5] == "pred:" {
			dirtyPreds[n[5:]] = true
		}
		if len(n) > 6 && n[:6] == "query:" {
			affectedQueries[n[6:]] = true
		}
	}

	report := &EditReport{EditKind: kind, SourceNode: source, Closure: closure}
	allDirty := make([]string, 0, len(dirtyPreds))
	for pid := range dirtyPreds {
		allDirty = append(allDirty, pid)
	}
	sort.Strings(allDirty)
	for _, pid := range allDirty {
		if _, err := e.evalIncremental(pid, dirtyPreds, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	for q := range affectedQueries {
		preds := make([]string, 0)
		for pid := range dirtyPreds {
			pred := e.project.Index().Predicates[pid]
			if pred == nil {
				continue
			}
			relevant := pred.QueryID == q
			if !relevant {
				for _, edge := range e.project.Edges {
					if edge.QueryID == q && edge.PredicateID == pid {
						relevant = true
						break
					}
				}
			}
			if relevant {
				preds = append(preds, pid)
			}
		}
		sort.Strings(preds)
		plan, planErr := PlanQuery(e.project, &EstimateReport{Predicates: e.estimates}, q, e.bindings, e.opts)
		if plan != nil {
			e.plans[q] = plan
		}
		qr := IncrementalQueryResult{
			QueryID: q, RecomputedPredicates: preds,
			InvalidationSources: []string{source}, PropagationClosure: closure, CacheHit: false,
		}
		if plan != nil {
			qr.Ordinal = plan.Ordinal
			qr.SelectedOrder = append([]string{}, plan.SelectedOrder...)
			qr.SelectedCandidateID = plan.SelectedCandidateID
			qr.RulingBasisID = plan.RulingBasisID
			qr.ErrorCode = plan.ErrorCode
		}
		if planErr != nil && qr.ErrorCode == "" {
			if coded, ok := AsError(planErr); ok {
				qr.ErrorCode = coded.Code
			}
		}
		report.RecomputedQueries = append(report.RecomputedQueries, q)
		report.Results = append(report.Results, qr)
	}

	for _, q := range e.project.Queries {
		if affectedQueries[q.ID] {
			continue
		}
		plan := e.plans[q.ID]
		qr := IncrementalQueryResult{QueryID: q.ID, Ordinal: q.Ordinal, CacheHit: true}
		if plan != nil {
			qr.SelectedOrder = append([]string{}, plan.SelectedOrder...)
			qr.SelectedCandidateID = plan.SelectedCandidateID
			qr.RulingBasisID = plan.RulingBasisID
			qr.ErrorCode = plan.ErrorCode
		}
		report.CacheHitQueries = append(report.CacheHitQueries, q.ID)
		report.Results = append(report.Results, qr)
	}
	sort.Slice(report.Results, func(i, j int) bool { return report.Results[i].Ordinal < report.Results[j].Ordinal })
	sort.Strings(report.RecomputedQueries)
	sort.Strings(report.CacheHitQueries)
	return report, nil
}

func (e *IncrementalEngine) evalIncremental(id string, dirty map[string]bool, visiting map[string]bool) (PredicateEstimate, error) {
	if !dirty[id] {
		if cached, ok := e.estimates[id]; ok {
			return cached, nil
		}
	}
	if visiting[id] {
		return PredicateEstimate{}, newError(CodeInputInvalid, "cyclic predicate expression").with("predicate_id", id)
	}
	visiting[id] = true
	defer delete(visiting, id)

	idx := e.project.Index()
	pred := idx.Predicates[id]
	if pred == nil {
		return PredicateEstimate{}, newError(CodeInputInvalid, "missing predicate").with("predicate_id", id)
	}
	out := PredicateEstimate{PredicateID: id, QueryID: pred.QueryID, Kind: pred.Kind}
	var sel Fixed
	var saturated bool
	var err error
	switch pred.Kind {
	case PredEquality:
		sel, saturated, err = estimateEquality(e.project, idx, pred, e.bindings, &out)
	case PredRange:
		sel, saturated, err = estimateRange(e.project, idx, pred, e.bindings, &out)
	case PredBoundary:
		sel, saturated, err = estimateBoundary(e.project, idx, pred, e.bindings, &out)
	case PredJoin:
		sel, saturated, err = estimateJoin(idx, pred)
	case PredAnd, PredOr:
		sel, saturated, err = estimateCompound(pred, func(cid string) (PredicateEstimate, error) {
			return e.evalIncremental(cid, dirty, visiting)
		})
	default:
		err = newError(CodeInputInvalid, "unknown predicate kind").with("predicate_id", id).with("kind", pred.Kind)
	}
	if err != nil {
		return PredicateEstimate{}, err
	}
	rows, rowSat := predicateBaseRows(e.project, idx, pred, nil)
	estimatedRows, mulSat := RoundedRows(rows, sel)
	out.Selectivity = sel
	out.SelectivityRaw = int64(sel)
	out.EstimatedRows = estimatedRows
	out.Saturated = saturated || rowSat || mulSat
	e.estimates[id] = out
	return out, nil
}

func FullRecompute(p *Project, bindings BindingSet, opts OptimizeOptions) (*EstimateReport, map[string]*QueryPlan, error) {
	opts = opts.withProjectDefaults(p)
	est, err := EstimateProject(p, bindings)
	if err != nil {
		return nil, nil, err
	}
	plans := map[string]*QueryPlan{}
	for _, q := range p.Queries {
		plan, planErr := PlanQuery(p, est, q.ID, bindings, opts)
		if plan != nil {
			plans[q.ID] = plan
		}
		if planErr != nil {
			if coded, ok := AsError(planErr); ok && (coded.Code == CodeDisconnectedPlan || coded.Code == CodeCandidateBackpressure) {
				continue
			}
			return nil, nil, planErr
		}
	}
	return est, plans, nil
}
