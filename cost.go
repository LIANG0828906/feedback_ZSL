package planopt

import (
	"fmt"
	"sort"
)

type TableEstimate struct {
	Table            string `json:"table"`
	RawRows          int64  `json:"raw_rows"`
	SelectivityPPM   int64  `json:"selectivity_ppm"`
	EstimatedRows    int64  `json:"estimated_rows"`
	ReadWidthBytes   int64  `json:"read_width_bytes"`
	OutputWidthBytes int64  `json:"output_width_bytes"`
}

type PathChoice struct {
	Table          string `json:"table"`
	SelectedPath   string `json:"selected_path"`
	PathKind       string `json:"path_kind"`
	MatchedColumn  string `json:"matched_column,omitempty"`
	EstimatedRows  int64  `json:"estimated_rows"`
	WidthBytes     int64  `json:"width_bytes"`
	PathCostMilli  int64  `json:"path_cost_milli"`
	SeqCostMilli   int64  `json:"seq_cost_milli"`
	IndexCostMilli int64  `json:"index_cost_milli,omitempty"`
	Reason         string `json:"reason"`
}

type CostBreakdown struct {
	ScanPathMilli         int64 `json:"scan_path_milli"`
	JoinCPUMilli          int64 `json:"join_cpu_milli"`
	IntermediateSizeMilli int64 `json:"intermediate_size_milli"`
}

type JoinCandidate struct {
	Rank             int           `json:"rank"`
	Order            []string      `json:"order"`
	CostMilli        int64         `json:"cost_milli"`
	Breakdown        CostBreakdown `json:"breakdown"`
	EstimatedRows    int64         `json:"estimated_rows"`
	OutputWidthBytes int64         `json:"output_width_bytes"`
	TieEligible      bool          `json:"tie_eligible"`
	DecisionBasis    string        `json:"decision_basis"`
}

type OptimizedPlan struct {
	PlanSeq        int                 `json:"plan_seq"`
	PlanID         string              `json:"plan_id"`
	Normalized     NormalizedPlan      `json:"normalized"`
	Estimates      []PredicateEstimate `json:"predicate_estimates"`
	Tables         []TableEstimate     `json:"table_estimates"`
	Paths          []PathChoice        `json:"paths"`
	Candidates     []JoinCandidate     `json:"join_candidates"`
	JoinOrder      []string            `json:"join_order"`
	SelectedPath   map[string]string   `json:"selected_paths"`
	CostMilli      int64               `json:"cost_milli"`
	Breakdown      CostBreakdown       `json:"breakdown"`
	TieBreakReason string              `json:"tie_break_reason"`
}

type joinEdge struct {
	NodeID   string
	Left     string
	Right    string
	LeftKey  string
	RightKey string
	JoinType string
	Declared int
}

func OptimizeProject(project Project, bindings BindingSet) ([]OptimizedPlan, error) {
	normalized, err := NormalizeProject(project)
	if err != nil {
		return nil, err
	}
	bySeq := map[int]NormalizedPlan{}
	for _, n := range normalized {
		bySeq[n.PlanSeq] = n
	}
	out := make([]OptimizedPlan, 0, len(project.Plans))
	for _, plan := range project.SortedPlans() {
		opt, err := optimizePlan(project, plan, bySeq[plan.Seq], bindings)
		if err != nil {
			return nil, err
		}
		out = append(out, opt)
	}
	return out, nil
}

func OptimizePlan(project Project, plan Plan, bindings BindingSet) (OptimizedPlan, error) {
	n, err := NormalizePlan(project, plan)
	if err != nil {
		return OptimizedPlan{}, err
	}
	return optimizePlan(project, plan, n, bindings)
}

func optimizePlan(project Project, plan Plan, norm NormalizedPlan, bindings BindingSet) (OptimizedPlan, error) {
	estimates, err := EstimatePredicates(project, plan, bindings)
	if err != nil {
		return OptimizedPlan{}, err
	}
	estByID := map[string]PredicateEstimate{}
	for _, es := range estimates {
		estByID[es.PredicateID] = es
	}

	tables := make([]TableEstimate, 0)
	predsByTable := map[string][]Predicate{}
	for _, pred := range norm.Predicates {
		predsByTable[pred.Table] = append(predsByTable[pred.Table], pred)
	}
	readWidthByTable := map[string]int64{}
	outputWidthByTable := map[string]int64{}
	for _, op := range norm.Operators {
		if op.Kind == "SCAN" {
			readWidthByTable[op.Table] = widthForColumns(project, op.ReadColumns)
		}
		if op.Kind == "FILTER" && op.Pushdown {
			outputWidthByTable[op.Table] = widthForColumns(project, op.OutputColumns)
		}
	}

	pathChoices := []PathChoice{}
	pathByTable := map[string]PathChoice{}
	for _, tableName := range involvedTables(plan) {
		table, ok := project.Table(tableName)
		if !ok {
			return OptimizedPlan{}, Errorf(CodeFieldCombination, "cost.table", "unknown table %q", tableName)
		}
		sel := int64(SelBasis)
		for _, pred := range predsByTable[tableName] {
			if es, ok := estByID[pred.ID]; ok {
				sel = sel * es.SelectivityPPM / SelBasis
			}
		}
		rows := max64(1, table.RowCount*sel/SelBasis)
		readWidth := readWidthByTable[tableName]
		if readWidth == 0 {
			readWidth = table.RowWidthBytes
		}
		outWidth := outputWidthByTable[tableName]
		if outWidth == 0 {
			outWidth = readWidth
		}
		te := TableEstimate{
			Table: tableName, RawRows: table.RowCount, SelectivityPPM: sel,
			EstimatedRows: rows, ReadWidthBytes: readWidth, OutputWidthBytes: outWidth,
		}
		tables = append(tables, te)
		choice := choosePath(table, predsByTable[tableName], estByID, table.RowCount, rows, readWidth)
		pathChoices = append(pathChoices, choice)
		pathByTable[tableName] = choice
	}

	edges := joinEdges(plan)
	orders := candidateOrders(project, involvedTables(plan), edges)
	candidates := make([]JoinCandidate, 0, len(orders))
	for rank, order := range orders {
		cand := evaluateOrder(order, tables, pathByTable, edges, project, rank)
		candidates = append(candidates, cand)
	}
	if len(candidates) == 0 {
		return OptimizedPlan{}, Errorf(CodeInvariant, "cost.join_order", "no valid join order for %s", plan.ID)
	}

	minCost := candidates[0].CostMilli
	for _, c := range candidates {
		minCost = min64(minCost, c.CostMilli)
	}
	tieBand := max64(1, minCost) * CostTieBandPPM / SelBasis
	if tieBand < 1 {
		tieBand = 1
	}
	selected := candidates[0]
	selectedIdx := 0
	for i, c := range candidates {
		candidates[i].TieEligible = c.CostMilli <= minCost+tieBand
		if candidates[i].TieEligible {
			candidates[i].DecisionBasis = "COST_WITHIN_DECLARED_TIE_BAND; EARLIEST_DECLARED_RELATION_ORDER"
		} else if c.CostMilli == minCost {
			candidates[i].DecisionBasis = "LOWEST_INTEGER_COST"
		} else {
			candidates[i].DecisionBasis = "COST_EXCEEDS_SELECTED_CANDIDATE"
		}
		if candidates[i].TieEligible && i < selectedIdx {
			selected = candidates[i]
			selectedIdx = i
		}
	}
	// candidates are already in declared lexicographic order, so the first
	// tie-eligible candidate is the stable winner.
	for i := range candidates {
		if candidates[i].TieEligible {
			selected = candidates[i]
			break
		}
	}

	selectedPaths := map[string]string{}
	for _, p := range pathChoices {
		selectedPaths[p.Table] = p.SelectedPath
	}
	tieReason := selected.DecisionBasis
	if selected.CostMilli != minCost {
		tieReason = fmt.Sprintf("DECLARED_TIE_BAND_SELECTS_RANK_%d; min_cost=%d selected_cost=%d band=%d", selected.Rank, minCost, selected.CostMilli, tieBand)
	} else {
		tieReason = fmt.Sprintf("LOWEST_COST_RANK_%d; band=%d", selected.Rank, tieBand)
	}

	return OptimizedPlan{
		PlanSeq: plan.Seq, PlanID: plan.ID, Normalized: norm,
		Estimates: estimates, Tables: tables, Paths: pathChoices,
		Candidates: candidates, JoinOrder: append([]string(nil), selected.Order...),
		SelectedPath: selectedPaths, CostMilli: selected.CostMilli,
		Breakdown: selected.Breakdown, TieBreakReason: tieReason,
	}, nil
}

func involvedTables(plan Plan) []string {
	set := map[string]bool{}
	for _, n := range plan.Nodes {
		if n.Kind == "SCAN" && n.Table != "" {
			set[n.Table] = true
		}
		if n.Kind == "JOIN" {
			set[n.LeftTable] = true
			set[n.RightTable] = true
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func widthForColumns(project Project, qualifiedCols []string) int64 {
	width := int64(0)
	for _, q := range qualifiedCols {
		tableName := tableOfQualified(q)
		colName := q[len(tableName)+1:]
		table, ok := project.Table(tableName)
		if !ok {
			continue
		}
		col, ok := table.Column(colName)
		if ok {
			width += col.WidthBytes
		}
	}
	return width
}

func choosePath(table Table, preds []Predicate, est map[string]PredicateEstimate, rawRows, estimatedRows, width int64) PathChoice {
	widthFactor := max64(1, width/64)
	seqCost := rawRows * (120 + widthFactor*4)
	choice := PathChoice{
		Table: table.Name, SelectedPath: "seq_" + table.Name, PathKind: "SEQ_SCAN",
		EstimatedRows: estimatedRows, WidthBytes: width, PathCostMilli: seqCost,
		SeqCostMilli: seqCost, Reason: "NO_SARGABLE_DECLARED_INDEX_KEY",
	}

	var best *AccessPath
	var bestPred *Predicate
	for _, path := range table.AccessPaths {
		if path.Kind != "INDEX_SCAN" || len(path.KeyColumns) == 0 {
			continue
		}
		key := path.KeyColumns[0]
		for i := range preds {
			pred := preds[i]
			if pred.Column != key {
				continue
			}
			if pred.Op != "EQ" && pred.Op != "GT" && pred.Op != "GTE" && pred.Op != "LT" && pred.Op != "LTE" && pred.Op != "BETWEEN" {
				continue
			}
			if best == nil || path.Ordinal < best.Ordinal {
				p := path
				best = &p
				pr := pred
				bestPred = &pr
			}
		}
	}
	if best == nil {
		return choice
	}

	indexCost := int64(1000) + estimatedRows*(260+width/8)
	choice.IndexCostMilli = indexCost
	choice.MatchedColumn = bestPred.Column
	pathTieBand := max64(1, min64(seqCost, indexCost)) * CostTieBandPPM / SelBasis
	if indexCost+pathTieBand < seqCost {
		choice.SelectedPath = best.Name
		choice.PathKind = best.Kind
		choice.PathCostMilli = indexCost
		choice.Reason = fmt.Sprintf("INDEX_KEY_%s_COST_BELOW_SEQ", bestPred.Column)
	} else if indexCost <= seqCost+pathTieBand && best.Ordinal < seqPathOrdinal(table) {
		choice.SelectedPath = best.Name
		choice.PathKind = best.Kind
		choice.PathCostMilli = min64(indexCost, seqCost)
		choice.Reason = "PATH_COST_TIE_USES_LOWER_DECLARED_PATH_ORDINAL"
	} else {
		choice.Reason = "SEQUENTIAL_SCAN_COST_NOT_ABOVE_INDEX"
	}
	return choice
}

func seqPathOrdinal(table Table) int {
	for _, p := range table.AccessPaths {
		if p.Kind == "SEQ_SCAN" {
			return p.Ordinal
		}
	}
	return 1 << 30
}

func joinEdges(plan Plan) []joinEdge {
	out := []joinEdge{}
	for i, n := range plan.Nodes {
		if n.Kind != "JOIN" {
			continue
		}
		out = append(out, joinEdge{
			NodeID: n.ID, Left: n.LeftTable, Right: n.RightTable,
			LeftKey: n.LeftKey, RightKey: n.RightKey, JoinType: n.JoinType, Declared: i,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Declared != out[j].Declared {
			return out[i].Declared < out[j].Declared
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}

func candidateOrders(project Project, tables []string, edges []joinEdge) [][]string {
	ordinal := map[string]int{}
	for _, t := range project.Tables {
		ordinal[t.Name] = t.Ordinal
	}
	orderedTables := append([]string(nil), tables...)
	sort.Slice(orderedTables, func(i, j int) bool {
		if ordinal[orderedTables[i]] != ordinal[orderedTables[j]] {
			return ordinal[orderedTables[i]] < ordinal[orderedTables[j]]
		}
		return orderedTables[i] < orderedTables[j]
	})
	perms := permutations(orderedTables)
	valid := [][]string{}
	for _, perm := range perms {
		if validOrder(perm, edges) {
			valid = append(valid, append([]string(nil), perm...))
		}
	}
	return valid
}

func permutations(in []string) [][]string {
	if len(in) == 0 {
		return [][]string{{}}
	}
	out := [][]string{}
	for i, v := range in {
		rest := append([]string(nil), in[:i]...)
		rest = append(rest, in[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]string{v}, p...))
		}
	}
	return out
}

func validOrder(order []string, edges []joinEdge) bool {
	index := map[string]int{}
	for i, t := range order {
		index[t] = i
	}
	seen := map[string]bool{order[0]: true}
	for step := 1; step < len(order); step++ {
		next := order[step]
		connected := false
		for _, e := range edges {
			var other string
			if e.Left == next && seen[e.Right] {
				other = e.Right
			} else if e.Right == next && seen[e.Left] {
				other = e.Left
			}
			if other != "" {
				connected = true
				break
			}
		}
		if !connected {
			return false
		}
		seen[next] = true
	}
	for _, e := range edges {
		switch e.JoinType {
		case "LEFT_OUTER":
			if index[e.Left] > index[e.Right] {
				return false
			}
		case "RIGHT_OUTER":
			if index[e.Right] > index[e.Left] {
				return false
			}
		}
	}
	return true
}

func evaluateOrder(order []string, tableEsts []TableEstimate, paths map[string]PathChoice, edges []joinEdge, project Project, rank int) JoinCandidate {
	est := map[string]TableEstimate{}
	for _, te := range tableEsts {
		est[te.Table] = te
	}
	rows := est[order[0]].EstimatedRows
	width := est[order[0]].ReadWidthBytes
	breakdown := CostBreakdown{ScanPathMilli: paths[order[0]].PathCostMilli}
	seen := map[string]bool{order[0]: true}

	for i := 1; i < len(order); i++ {
		next := order[i]
		te := est[next]
		breakdown.ScanPathMilli += paths[next].PathCostMilli
		edge := findConnectingEdge(next, seen, edges)
		ndv := int64(1)
		if edge != nil {
			leftCol, _ := project.Table(edge.Left)
			rightCol, _ := project.Table(edge.Right)
			lc, _ := leftCol.Column(edge.LeftKey)
			rc, _ := rightCol.Column(edge.RightKey)
			ndv = max64(max64(lc.NDV, rc.NDV), 1)
		}
		newRows := max64(1, rows*te.EstimatedRows/ndv)
		newWidth := width + te.ReadWidthBytes
		joinCPU := newRows * (20 + newWidth/32)
		intermediate := newRows * newWidth / 128
		breakdown.JoinCPUMilli += joinCPU
		breakdown.IntermediateSizeMilli += intermediate
		rows = newRows
		width = newWidth
		seen[next] = true
	}
	total := breakdown.ScanPathMilli + breakdown.JoinCPUMilli + breakdown.IntermediateSizeMilli
	return JoinCandidate{
		Rank: rank, Order: append([]string(nil), order...), CostMilli: total,
		Breakdown: breakdown, EstimatedRows: rows, OutputWidthBytes: width,
		DecisionBasis: "CANDIDATE_EVALUATED",
	}
}

func findConnectingEdge(next string, seen map[string]bool, edges []joinEdge) *joinEdge {
	for i := range edges {
		e := &edges[i]
		if e.Left == next && seen[e.Right] {
			return e
		}
		if e.Right == next && seen[e.Left] {
			return e
		}
	}
	// The other endpoint may be connected through a nested join even if the
	// direct endpoint is not the newly added table.
	for i := range edges {
		e := &edges[i]
		if (e.Left == next && seen[e.Right]) || (e.Right == next && seen[e.Left]) {
			return e
		}
	}
	return nil
}
