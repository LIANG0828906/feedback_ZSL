package queryopt

import (
	"fmt"
	"sort"
)

type Diagnostic struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

type StageCost struct {
	Step              int    `json:"step"`
	TableID           string `json:"table_id"`
	AppliedEdgeID     string `json:"applied_edge_id,omitempty"`
	RawRows           int64  `json:"raw_rows"`
	FilteredRows      int64  `json:"filtered_rows"`
	WidthBytes        int64  `json:"width_bytes"`
	PrefixWidthBytes  int64  `json:"prefix_width_bytes"`
	IntermediateRows  int64  `json:"intermediate_rows"`
	IntermediateBytes int64  `json:"intermediate_bytes"`
	AccessPath        string `json:"access_path"`
}

type CostBreakdown struct {
	ScanRows          int64 `json:"scan_rows"`
	ScanBytes         int64 `json:"scan_bytes"`
	FilteredRows      int64 `json:"filtered_rows"`
	IntermediateRows  int64 `json:"intermediate_rows"`
	IntermediateBytes int64 `json:"intermediate_bytes"`
	WeightedScan      int64 `json:"weighted_scan"`
	WeightedRow       int64 `json:"weighted_row"`
	WeightedWidth     int64 `json:"weighted_width"`
	TotalCost         int64 `json:"total_cost"`
	Saturated         bool  `json:"saturated"`
}

type CandidateResult struct {
	CandidateID   string        `json:"candidate_id"`
	QueryID       string        `json:"query_id"`
	DeclaredOrder int           `json:"declared_order"`
	Order         []string      `json:"order"`
	Feasible      bool          `json:"feasible"`
	RejectCode    string        `json:"reject_code,omitempty"`
	RejectReason  string        `json:"reject_reason,omitempty"`
	EdgeID        string        `json:"edge_id,omitempty"`
	RejectStep    int           `json:"reject_step,omitempty"`
	Cost          CostBreakdown `json:"cost"`
	Stages        []StageCost   `json:"stages,omitempty"`
}

type QueryPlan struct {
	QueryID             string            `json:"query_id"`
	Ordinal             int               `json:"ordinal"`
	Candidates          []CandidateResult `json:"candidates"`
	SelectedCandidateID string            `json:"selected_candidate_id,omitempty"`
	SelectedOrder       []string          `json:"selected_order,omitempty"`
	RulingBasisID       string            `json:"ruling_basis_id,omitempty"`
	RulingRuleID        string            `json:"ruling_rule_id,omitempty"`
	CutEdgeIDs          []string          `json:"cut_edge_ids,omitempty"`
	Diagnostics         []Diagnostic      `json:"diagnostics,omitempty"`
	AccessPaths         map[string]string `json:"access_paths,omitempty"`
	ErrorCode           string            `json:"error_code,omitempty"`
	ErrorMessage        string            `json:"error_message,omitempty"`
}

type OptimizeOptions struct {
	WorkerBudget    int
	CandidateBuffer int
}

func OptionsFromProject(p *Project) OptimizeOptions {
	return OptimizeOptions{WorkerBudget: p.Rules.WorkerBudget, CandidateBuffer: p.Rules.CandidateBuffer}
}

func (o OptimizeOptions) withProjectDefaults(p *Project) OptimizeOptions {
	if o.WorkerBudget == 0 {
		o.WorkerBudget = p.Rules.WorkerBudget
	}
	if o.CandidateBuffer == 0 {
		o.CandidateBuffer = p.Rules.CandidateBuffer
	}
	return o
}

type graphEdge struct {
	edge *JoinEdge
}

func PlanQuery(p *Project, estimates *EstimateReport, queryID string, bindings BindingSet, opts OptimizeOptions) (*QueryPlan, error) {
	opts = opts.withProjectDefaults(p)
	if err := ValidateBudget(opts.WorkerBudget, opts.CandidateBuffer); err != nil {
		return nil, err
	}
	idx := p.Index()
	q := idx.Queries[queryID]
	if q == nil {
		return nil, newError(CodeInputInvalid, "missing query").with("query_id", queryID)
	}
	if estimates == nil {
		var err error
		estimates, err = EstimateProject(p, bindings)
		if err != nil {
			return nil, err
		}
	}
	plan := &QueryPlan{QueryID: q.ID, Ordinal: q.Ordinal, AccessPaths: map[string]string{}}

	queryEdges := []JoinEdge{}
	for _, e := range p.Edges {
		if e.QueryID == q.ID {
			queryEdges = append(queryEdges, e)
		}
	}
	cut, diags := breakCycles(q.ID, q.TableIDs, queryEdges)
	plan.CutEdgeIDs = cut
	plan.Diagnostics = append(plan.Diagnostics, diags...)
	cutSet := map[string]bool{}
	for _, id := range cut {
		cutSet[id] = true
	}

	unreachable := unreachablePredicates(p, idx, q, queryEdges, cutSet)
	for _, id := range unreachable {
		plan.Diagnostics = append(plan.Diagnostics, Diagnostic{
			Code:    ReasonUnreachablePred,
			Message: "predicate is on a table outside the reachable connected component",
			Context: map[string]any{"query_id": q.ID, "predicate_id": id},
		})
	}

	orderedTables := append([]string{}, q.TableIDs...)
	sort.SliceStable(orderedTables, func(i, j int) bool {
		ti, tj := idx.Tables[orderedTables[i]], idx.Tables[orderedTables[j]]
		if ti.DeclaredOrder != tj.DeclaredOrder {
			return ti.DeclaredOrder < tj.DeclaredOrder
		}
		return orderedTables[i] < orderedTables[j]
	})
	permCount := factorial(len(orderedTables))
	if permCount > opts.CandidateBuffer {
		err := newError(CodeCandidateBackpressure, "candidate order count exceeds per-query buffer").
			with("query_id", q.ID).with("candidate_count", permCount).
			with("candidate_buffer", opts.CandidateBuffer).
			with("retryable", true)
		plan.ErrorCode = err.Code
		plan.ErrorMessage = err.Message
		return plan, err
	}

	localSel, filteredRows, widths, paths := tableFilters(p, idx, q, estimates)
	plan.AccessPaths = paths
	perms := permutations(orderedTables)
	for ordinal, order := range perms {
		cand := CandidateResult{
			CandidateID: fmt.Sprintf("CAND-%s-%02d", q.ID, ordinal+1),
			QueryID:     q.ID, DeclaredOrder: ordinal, Order: append([]string{}, order...),
		}
		feasible, rejectCode, rejectReason, edgeID, rejectStep := checkOrder(order, queryEdges, cutSet)
		if !feasible {
			cand.Feasible = false
			cand.RejectCode = rejectCode
			cand.RejectReason = rejectReason
			cand.EdgeID = edgeID
			cand.RejectStep = rejectStep
			plan.Candidates = append(plan.Candidates, cand)
			continue
		}
		cand.Feasible = true
		cost, stages, cerr := costOrder(p, idx, q, order, queryEdges, cutSet, estimates, localSel, filteredRows, widths, paths)
		cand.Cost = cost
		cand.Stages = stages
		if cerr != nil {
			cand.Feasible = false
			cand.RejectCode = CodeInputInvalid
			cand.RejectReason = cerr.Error()
		}
		plan.Candidates = append(plan.Candidates, cand)
	}

	bestIdx := -1
	bestCost := int64(0)
	for i := range plan.Candidates {
		c := &plan.Candidates[i]
		if !c.Feasible {
			continue
		}
		if bestIdx == -1 || c.Cost.TotalCost < bestCost {
			bestIdx = i
			bestCost = c.Cost.TotalCost
		}
	}
	if bestIdx == -1 {
		err := newError(CodeDisconnectedPlan, "no connected join order is available").
			with("query_id", q.ID).
			with("table_ids", q.TableIDs).
			with("cut_edge_ids", plan.CutEdgeIDs).
			with("unreachable_predicates", unreachable)
		plan.ErrorCode = err.Code
		plan.ErrorMessage = err.Message
		return plan, err
	}

	tolerance, _ := CheckedMul(bestCost, int64(p.Rules.TieRelativePermyriad))
	tolerance /= 10_000
	selected := bestIdx
	tied := 0
	for i := range plan.Candidates {
		c := &plan.Candidates[i]
		if !c.Feasible {
			continue
		}
		delta := c.Cost.TotalCost - bestCost
		if delta < 0 {
			delta = -delta
		}
		if delta <= tolerance {
			tied++
			if c.DeclaredOrder < plan.Candidates[selected].DeclaredOrder {
				selected = i
			}
		}
	}
	sc := plan.Candidates[selected]
	plan.SelectedCandidateID = sc.CandidateID
	plan.SelectedOrder = append([]string{}, sc.Order...)
	if tied > 1 {
		plan.RulingRuleID = "RR_TIE_DECLARED_ORDER"
		plan.RulingBasisID = fmt.Sprintf("RB_TIE_ORDER#%s#ORD%02d", sc.CandidateID, sc.DeclaredOrder+1)
	} else {
		plan.RulingRuleID = "RR_MIN_TOTAL_COST"
		plan.RulingBasisID = fmt.Sprintf("RB_MIN_COST#%s", sc.CandidateID)
	}
	return plan, nil
}

func breakCycles(queryID string, tableIDs []string, edges []JoinEdge) ([]string, []Diagnostic) {
	var cut []string
	var diags []Diagnostic
	remaining := append([]JoinEdge{}, edges...)
	for {
		cycle := findCycleEdges(tableIDs, remaining)
		if len(cycle) == 0 {
			break
		}
		victim := cycle[0]
		for _, eid := range cycle[1:] {
			var cand JoinEdge
			for _, e := range remaining {
				if e.ID == eid {
					cand = e
					break
				}
			}
			var cur JoinEdge
			for _, e := range remaining {
				if e.ID == victim {
					cur = e
					break
				}
			}
			if cand.BreakPriority > cur.BreakPriority ||
				(cand.BreakPriority == cur.BreakPriority && cand.DeclaredOrder < cur.DeclaredOrder) ||
				(cand.BreakPriority == cur.BreakPriority && cand.DeclaredOrder == cur.DeclaredOrder && cand.ID < cur.ID) {
				victim = cand.ID
			}
		}
		cut = append(cut, victim)
		diags = append(diags, Diagnostic{
			Code:    ReasonCycleCut,
			Message: "cycle broken by declared break priority",
			Context: map[string]any{"query_id": queryID, "cut_edge_id": victim, "cycle_edge_ids": cycle},
		})
		next := remaining[:0]
		for _, e := range remaining {
			if e.ID != victim {
				next = append(next, e)
			}
		}
		remaining = next
	}
	sort.Strings(cut)
	return cut, diags
}

func findCycleEdges(tableIDs []string, edges []JoinEdge) []string {
	adj := map[string][]struct{ to, edgeID string }{}
	for _, t := range tableIDs {
		adj[t] = nil
	}
	for _, e := range edges {
		if e.Cartesian {
			continue
		}
		adj[e.LeftTableID] = append(adj[e.LeftTableID], struct{ to, edgeID string }{e.RightTableID, e.ID})
		adj[e.RightTableID] = append(adj[e.RightTableID], struct{ to, edgeID string }{e.LeftTableID, e.ID})
	}
	visited := map[string]bool{}
	var cycle []string
	var dfs func(node, parentEdge string, pathEdges []string) bool
	dfs = func(node, parentEdge string, pathEdges []string) bool {
		visited[node] = true
		for _, a := range adj[node] {
			if a.edgeID == parentEdge {
				continue
			}
			if visited[a.to] {
				// Recover the suffix from the first occurrence of the neighbor.
				cycle = append(append([]string{}, pathEdges...), a.edgeID)
				return true
			}
			if dfs(a.to, a.edgeID, append(append([]string{}, pathEdges...), a.edgeID)) {
				return true
			}
		}
		return false
	}
	for _, t := range tableIDs {
		if !visited[t] && dfs(t, "", nil) {
			return normalizeCycle(cycle)
		}
	}
	return nil
}

func normalizeCycle(edges []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(edges))
	for _, id := range edges {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func checkOrder(order []string, edges []JoinEdge, cutSet map[string]bool) (bool, string, string, string, int) {
	joined := map[string]bool{order[0]: true}
	for step := 1; step < len(order); step++ {
		t := order[step]
		var equi, cartesian *JoinEdge
		for i := range edges {
			e := &edges[i]
			if cutSet[e.ID] {
				continue
			}
			if e.LeftTableID != t && e.RightTableID != t {
				continue
			}
			other := e.LeftTableID
			if other == t {
				other = e.RightTableID
			}
			if !joined[other] {
				continue
			}
			if e.Cartesian || e.PredicateID == "" {
				if cartesian == nil || e.DeclaredOrder < cartesian.DeclaredOrder {
					cartesian = e
				}
			} else {
				if equi == nil || e.DeclaredOrder < equi.DeclaredOrder {
					equi = e
				}
			}
		}
		if equi != nil {
			joined[t] = true
			continue
		}
		if cartesian != nil {
			return false, ReasonCartesianForbidden,
				"direct connection is a cartesian edge and is forbidden by declared rule",
				cartesian.ID, step
		}
		return false, ReasonDisconnectedPrefix,
			"next table has no uncut join edge to the joined prefix", "", step
	}
	return true, "", "", "", 0
}

func tableFilters(p *Project, idx *ProjectIndex, q *Query, est *EstimateReport) (map[string]Fixed, map[string]int64, map[string]int64, map[string]string) {
	sel := map[string]Fixed{}
	rows := map[string]int64{}
	widths := map[string]int64{}
	paths := map[string]string{}
	for _, t := range q.TableIDs {
		sel[t] = FixedOne
	}
	for _, pred := range p.Predicates {
		if pred.QueryID != q.ID || pred.TableID == "" {
			continue
		}
		if pred.Kind != PredEquality && pred.Kind != PredRange && pred.Kind != PredBoundary {
			continue
		}
		e := est.Predicates[pred.ID]
		cur := sel[pred.TableID]
		next, _ := cur.Mul(e.Selectivity)
		sel[pred.TableID] = next
	}
	required := map[string]map[string]bool{}
	for _, cid := range q.RequiredColumnIDs {
		c := idx.Columns[cid]
		if c == nil {
			continue
		}
		if required[c.TableID] == nil {
			required[c.TableID] = map[string]bool{}
		}
		required[c.TableID][cid] = true
	}
	for _, pred := range p.Predicates {
		if pred.QueryID != q.ID {
			continue
		}
		for _, cid := range []string{pred.ColumnID, pred.LeftColumnID, pred.RightColumnID} {
			if cid == "" {
				continue
			}
			c := idx.Columns[cid]
			if c == nil {
				continue
			}
			if required[c.TableID] == nil {
				required[c.TableID] = map[string]bool{}
			}
			required[c.TableID][cid] = true
		}
	}
	for _, tid := range q.TableIDs {
		t := idx.Tables[tid]
		tableRows, _ := RoundedRows(t.Rows, sel[tid])
		rows[tid] = tableRows
		if tableRows <= q.IndexThresholdRows {
			paths[tid] = PathIndexLookup
		} else {
			paths[tid] = PathSequentialScan
		}
		cols := required[tid]
		if len(q.RequiredColumnIDs) == 0 {
			widths[tid] = tableAllWidth(idx, tid)
			continue
		}
		var w int64
		if len(cols) == 0 {
			w = tableAllWidth(idx, tid)
		} else {
			for cid := range cols {
				if c := idx.Columns[cid]; c != nil {
					w += c.WidthBytes
				}
			}
		}
		widths[tid] = w
	}
	return sel, rows, widths, paths
}

func tableAllWidth(idx *ProjectIndex, tableID string) int64 {
	var w int64
	for _, c := range idx.Columns {
		if c.TableID == tableID {
			w += c.WidthBytes
		}
	}
	return w
}

func costOrder(p *Project, idx *ProjectIndex, q *Query, order []string, edges []JoinEdge, cutSet map[string]bool,
	est *EstimateReport, localSel map[string]Fixed, filtered map[string]int64, widths map[string]int64,
	paths map[string]string) (CostBreakdown, []StageCost, error) {
	var cost CostBreakdown
	var stages []StageCost
	var sat bool
	joined := map[string]bool{}
	var currentRows int64 = 1
	var prefixWidth int64

	add := func(a, b int64) int64 {
		v, s := CheckedAdd(a, b)
		sat = sat || s
		return v
	}
	mul := func(a, b int64) int64 {
		v, s := CheckedMul(a, b)
		sat = sat || s
		return v
	}

	for step, tid := range order {
		t := idx.Tables[tid]
		w := widths[tid]
		scanBytes := mul(t.Rows, w)
		cost.ScanRows = add(cost.ScanRows, t.Rows)
		cost.ScanBytes = add(cost.ScanBytes, scanBytes)
		cost.WeightedScan = add(cost.WeightedScan, mul(scanBytes, p.Weights.ScanPermyriad))

		var edgeID string
		var joinNDV int64
		if step > 0 {
			var chosen *JoinEdge
			for i := range edges {
				e := &edges[i]
				if cutSet[e.ID] || e.Cartesian || e.PredicateID == "" {
					continue
				}
				if e.LeftTableID != tid && e.RightTableID != tid {
					continue
				}
				other := e.LeftTableID
				if other == tid {
					other = e.RightTableID
				}
				if joined[other] && (chosen == nil || e.DeclaredOrder < chosen.DeclaredOrder) {
					chosen = e
				}
			}
			if chosen == nil {
				return cost, stages, newError(CodeDisconnectedPlan, "missing feasible join edge at cost stage")
			}
			edgeID = chosen.ID
			jp := idx.Predicates[chosen.PredicateID]
			lc := idx.Columns[jp.LeftColumnID]
			rc := idx.Columns[jp.RightColumnID]
			joinNDV = max64(lc.NDV, rc.NDV)
			currentRows = mulDivRound(currentRows, filtered[tid], joinNDV, &sat)
		} else {
			currentRows = filtered[tid]
		}
		joined[tid] = true
		prefixWidth = add(prefixWidth, w)
		intermediateBytes := mul(currentRows, prefixWidth)
		cost.FilteredRows = add(cost.FilteredRows, filtered[tid])
		cost.IntermediateRows = add(cost.IntermediateRows, currentRows)
		cost.IntermediateBytes = add(cost.IntermediateBytes, intermediateBytes)
		cost.WeightedRow = add(cost.WeightedRow, mul(currentRows, p.Weights.IntermediatePermyriad))
		cost.WeightedWidth = add(cost.WeightedWidth, mul(intermediateBytes, p.Weights.WidthPermyriad))
		stages = append(stages, StageCost{
			Step: step, TableID: tid, AppliedEdgeID: edgeID, RawRows: t.Rows,
			FilteredRows: filtered[tid], WidthBytes: w, PrefixWidthBytes: prefixWidth,
			IntermediateRows: currentRows, IntermediateBytes: intermediateBytes,
			AccessPath: paths[tid],
		})
	}
	weighted := add(add(cost.WeightedScan, cost.WeightedRow), cost.WeightedWidth)
	cost.TotalCost = (weighted + 5_000) / 10_000
	cost.Saturated = sat
	return cost, stages, nil
}

func mulDivRound(a, b, den int64, sat *bool) int64 {
	if den <= 0 {
		*sat = true
		return 0
	}
	// (a*b)/den with decomposition on a to reduce overflow risk.
	q, rem := a/den, a%den
	base, s1 := CheckedMul(q, b)
	partProduct, s2 := CheckedMul(rem, b)
	if s1 || s2 {
		*sat = true
		return MaxInt64Value
	}
	part := (partProduct + den/2) / den
	v, s3 := CheckedAdd(base, part)
	if s3 {
		*sat = true
		return MaxInt64Value
	}
	return v
}

const MaxInt64Value = int64(^uint64(0) >> 1)

func factorial(n int) int {
	v := 1
	for i := 2; i <= n; i++ {
		v *= i
	}
	return v
}

func permutations(in []string) [][]string {
	var out [][]string
	var gen func(prefix []string, remaining []string)
	gen = func(prefix []string, remaining []string) {
		if len(remaining) == 0 {
			out = append(out, append([]string{}, prefix...))
			return
		}
		for i := range remaining {
			next := append([]string{}, remaining[:i]...)
			next = append(next, remaining[i+1:]...)
			gen(append(prefix, remaining[i]), next)
		}
	}
	gen(nil, in)
	return out
}

func unreachablePredicates(p *Project, idx *ProjectIndex, q *Query, edges []JoinEdge, cutSet map[string]bool) []string {
	adj := map[string]map[string]bool{}
	for _, t := range q.TableIDs {
		adj[t] = map[string]bool{}
	}
	for _, e := range edges {
		if e.Cartesian || cutSet[e.ID] {
			continue
		}
		adj[e.LeftTableID][e.RightTableID] = true
		adj[e.RightTableID][e.LeftTableID] = true
	}
	seen := map[string]bool{q.TableIDs[0]: true}
	queue := []string{q.TableIDs[0]}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		for o := range adj[t] {
			if !seen[o] {
				seen[o] = true
				queue = append(queue, o)
			}
		}
	}
	var out []string
	for _, pred := range p.Predicates {
		if pred.QueryID != q.ID || pred.TableID == "" {
			continue
		}
		if !seen[pred.TableID] {
			out = append(out, pred.ID)
		}
	}
	sort.Strings(out)
	return out
}
