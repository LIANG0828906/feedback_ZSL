package planopt

import (
	"fmt"
	"sort"
)

type PushdownDecision struct {
	PredicateID  string `json:"predicate_id"`
	FilterNodeID string `json:"filter_node_id"`
	ScanNodeID   string `json:"scan_node_id,omitempty"`
	Table        string `json:"table"`
	Pushed       bool   `json:"pushed"`
	Reason       string `json:"reason"`
}

type NormalizedOp struct {
	Ordinal       int      `json:"ordinal"`
	Key           string   `json:"key"`
	SourceNodeID  string   `json:"source_node_id"`
	Kind          string   `json:"kind"`
	Table         string   `json:"table,omitempty"`
	JoinType      string   `json:"join_type,omitempty"`
	PredicateIDs  []string `json:"predicate_ids,omitempty"`
	ReadColumns   []string `json:"read_columns,omitempty"`
	OutputColumns []string `json:"output_columns"`
	Pushdown      bool     `json:"pushdown,omitempty"`
	Reason        string   `json:"reason,omitempty"`
}

type NormalizedPlan struct {
	PlanSeq         int                `json:"plan_seq"`
	PlanID          string             `json:"plan_id"`
	Operators       []NormalizedOp     `json:"operators"`
	RetainedColumns []string           `json:"retained_columns"`
	PrunedColumns   []string           `json:"pruned_columns"`
	Pushdowns       []PushdownDecision `json:"pushdowns"`
	Predicates      []Predicate        `json:"effective_predicates"`
}

type normalizer struct {
	project Project
	plan    Plan
	nodes   map[string]Node
	preds   map[string]Predicate
}

func NormalizeProject(project Project) ([]NormalizedPlan, error) {
	plans := project.SortedPlans()
	out := make([]NormalizedPlan, 0, len(plans))
	for _, plan := range plans {
		n, err := NormalizePlan(project, plan)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func NormalizePlan(project Project, plan Plan) (NormalizedPlan, error) {
	n := &normalizer{project: project, plan: plan, nodes: map[string]Node{}, preds: map[string]Predicate{}}
	for _, node := range plan.Nodes {
		n.nodes[node.ID] = node
	}
	for _, pred := range plan.Predicates {
		n.preds[pred.ID] = pred
	}

	effective, duplicateOf := n.deduplicatePredicates()
	retained := n.retainedColumns(effective)
	pruned := n.prunedColumns(retained)
	baseNeeded := n.baseNeededColumns(effective)

	pushed := map[string]string{}
	decisions := []PushdownDecision{}
	residualByFilter := map[string][]string{}

	filterNodes := n.nodesByKind("FILTER")
	for _, filterID := range filterNodes {
		filterNode := n.nodes[filterID]
		seenEffective := map[string]bool{}
		for _, rawID := range filterNode.PredicateIDs {
			effectiveID := rawID
			if dup, ok := duplicateOf[rawID]; ok {
				effectiveID = dup
			}
			if _, ok := n.preds[effectiveID]; !ok || seenEffective[effectiveID] {
				continue
			}
			seenEffective[effectiveID] = true
			pred := n.preds[effectiveID]
			scanID, hasScan := n.findScanForTable(filterNode, pred.Table)
			allowed, reason := n.pushAllowed(filterNode.ID, scanID, pred.Table)
			decision := PushdownDecision{
				PredicateID: effectiveID, FilterNodeID: filterID, Table: pred.Table,
				ScanNodeID: scanID, Pushed: hasScan && allowed, Reason: reason,
			}
			decisions = append(decisions, decision)
			if decision.Pushed {
				pushed[effectiveID] = scanID
			} else {
				residualByFilter[filterID] = append(residualByFilter[filterID], effectiveID)
			}
		}
	}
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].PredicateID < decisions[j].PredicateID })

	ops := []NormalizedOp{}
	opOrdinal := 0
	addOp := func(op NormalizedOp) {
		opOrdinal++
		op.Ordinal = opOrdinal
		op.OutputColumns = UniqueSorted(op.OutputColumns)
		if op.ReadColumns != nil {
			op.ReadColumns = UniqueSorted(op.ReadColumns)
		}
		if op.PredicateIDs != nil {
			op.PredicateIDs = append([]string(nil), op.PredicateIDs...)
		}
		ops = append(ops, op)
	}

	refOrder := n.columnOrderByRef()
	orderCols := func(cols map[string]bool) []string {
		out := []string{}
		seen := map[string]bool{}
		for _, qualified := range refOrder {
			if cols[qualified] && !seen[qualified] {
				out = append(out, qualified)
				seen[qualified] = true
			}
		}
		for qualified := range cols {
			if !seen[qualified] {
				out = append(out, qualified)
			}
		}
		return out
	}

	// Relation blocks are emitted in declared table ordinal order. Pushed
	// predicates become explicit filter operators immediately after the scan.
	for _, table := range n.planTables() {
		scanID := n.scanNodeForTable(table)
		if scanID == "" {
			continue
		}
		tableBase := map[string]bool{}
		for q := range baseNeeded {
			if tableOfQualified(q) == table {
				tableBase[q] = true
			}
		}
		tablePredCols := map[string]bool{}
		var pushedPreds []Predicate
		for _, pred := range effective {
			if pred.Table != table {
				continue
			}
			tablePredCols[qualified(pred.Table, pred.Column)] = true
			if pushed[pred.ID] == scanID {
				pushedPreds = append(pushedPreds, pred)
			}
		}
		read := mergeBoolSets(tableBase, tablePredCols)
		addOp(NormalizedOp{
			Key: "scan:" + table, SourceNodeID: scanID, Kind: "SCAN", Table: table,
			ReadColumns: orderCols(read), OutputColumns: orderCols(read),
		})
		for i, pred := range pushedPreds {
			output := cloneBoolSet(tableBase)
			for j := i + 1; j < len(pushedPreds); j++ {
				output[qualified(pushedPreds[j].Table, pushedPreds[j].Column)] = true
			}
			filterNodeID := n.filterNodeForPredicate(pred.ID)
			addOp(NormalizedOp{
				Key: "filter:" + pred.ID, SourceNodeID: filterNodeID, Kind: "FILTER",
				Table: pred.Table, PredicateIDs: []string{pred.ID},
				ReadColumns:   orderCols(mergeBoolSets(output, map[string]bool{qualified(pred.Table, pred.Column): true})),
				OutputColumns: orderCols(output), Pushdown: true,
				Reason: "PREDICATE_PUSHED_BELOW_ELIGIBLE_JOIN",
			})
		}
	}

	// Joins are dependency-topological. This converges flat and grouped-tree
	// source layouts without relying on source array order.
	for _, id := range n.topoNodesByKind("JOIN") {
		node := n.nodes[id]
		cols := map[string]bool{}
		for q := range baseNeeded {
			tbl := tableOfQualified(q)
			if n.joinContainsTable(node, tbl) {
				cols[q] = true
			}
		}
		addOp(NormalizedOp{
			Key: "join:" + id, SourceNodeID: id, Kind: "JOIN",
			Table: node.LeftTable + "," + node.RightTable, JoinType: node.JoinType,
			OutputColumns: orderCols(cols), Reason: "DECLARED_" + node.JoinType,
		})
	}

	for _, id := range n.topoNodesByKind("FILTER") {
		predIDs := residualByFilter[id]
		if len(predIDs) == 0 {
			continue
		}
		cols := map[string]bool{}
		for _, pid := range predIDs {
			pred := n.preds[pid]
			cols[qualified(pred.Table, pred.Column)] = true
		}
		for q := range baseNeeded {
			cols[q] = true
		}
		addOp(NormalizedOp{
			Key: "filter_residual:" + id, SourceNodeID: id, Kind: "FILTER",
			PredicateIDs: predIDs, OutputColumns: orderCols(cols),
			Reason: "BLOCKED_BY_NULL_PRESERVING_SEMANTICS",
		})
	}

	for _, id := range n.topoNodesByKind("AGGREGATE") {
		addOp(NormalizedOp{Key: "aggregate:" + id, SourceNodeID: id, Kind: "AGGREGATE", OutputColumns: n.plan.RequiredColumns})
	}
	for _, id := range n.topoNodesByKind("PROJECT") {
		node := n.nodes[id]
		cols := node.ProjectionColumns
		if len(cols) == 0 {
			cols = n.plan.RequiredColumns
		}
		addOp(NormalizedOp{Key: "project:" + id, SourceNodeID: id, Kind: "PROJECT", OutputColumns: cols})
	}
	for _, id := range n.topoNodesByKind("LIMIT") {
		node := n.nodes[id]
		addOp(NormalizedOp{Key: "limit:" + id, SourceNodeID: id, Kind: "LIMIT", OutputColumns: n.plan.RequiredColumns, Reason: fmt.Sprintf("limit=%d", node.LimitValue)})
	}

	return NormalizedPlan{
		PlanSeq: plan.Seq, PlanID: plan.ID, Operators: ops,
		RetainedColumns: retained, PrunedColumns: pruned,
		Pushdowns: decisions, Predicates: effective,
	}, nil
}

func (n *normalizer) deduplicatePredicates() ([]Predicate, map[string]string) {
	sorted := append([]Predicate(nil), n.plan.Predicates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Ordinal < sorted[j].Ordinal })
	effective := []Predicate{}
	duplicateOf := map[string]string{}
	compoundFirst := map[string]string{}
	for _, pred := range sorted {
		if pred.CompoundID != "" {
			if first, ok := compoundFirst[pred.CompoundID]; ok {
				duplicateOf[pred.ID] = first
				continue
			}
			compoundFirst[pred.CompoundID] = pred.ID
		}
		effective = append(effective, pred)
	}
	return effective, duplicateOf
}

func (n *normalizer) retainedColumns(effective []Predicate) []string {
	set := map[string]bool{}
	for _, ref := range n.plan.ColumnRefs {
		set[qualified(ref.Table, ref.Column)] = true
	}
	for _, pred := range effective {
		set[qualified(pred.Table, pred.Column)] = true
	}
	for _, col := range n.plan.RequiredColumns {
		set[col] = true
	}
	return n.orderQualified(set)
}

func (n *normalizer) prunedColumns(retained []string) []string {
	retain := map[string]bool{}
	for _, q := range retained {
		retain[q] = true
	}
	pruned := []string{}
	for _, table := range n.project.Tables {
		for _, col := range table.Columns {
			q := qualified(table.Name, col.Name)
			if !retain[q] {
				pruned = append(pruned, q)
			}
		}
	}
	return UniqueSorted(pruned)
}

func (n *normalizer) baseNeededColumns(effective []Predicate) map[string]bool {
	set := map[string]bool{}
	for _, q := range n.plan.RequiredColumns {
		set[q] = true
	}
	for _, ref := range n.plan.ColumnRefs {
		if ref.UsageRole == "JOIN" || ref.UsageRole == "OUTPUT" {
			set[qualified(ref.Table, ref.Column)] = true
		}
	}
	// Predicates are added to scan reads, but not to the post-pushdown base
	// unless semantic checks leave them as residual operators.
	pushedCandidate := map[string]bool{}
	for _, pred := range effective {
		pushedCandidate[pred.ID] = true
	}
	for _, node := range n.plan.Nodes {
		if node.Kind != "FILTER" {
			continue
		}
		scanID, _ := n.findScanForTable(node, "")
		_ = scanID
	}
	return set
}

func (n *normalizer) columnOrderByRef() []string {
	refs := append([]ColumnRef(nil), n.plan.ColumnRefs...)
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Ordinal < refs[j].Ordinal })
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, qualified(ref.Table, ref.Column))
	}
	return out
}

func (n *normalizer) orderQualified(set map[string]bool) []string {
	orderedRefs := n.columnOrderByRef()
	out := []string{}
	seen := map[string]bool{}
	for _, q := range orderedRefs {
		if set[q] && !seen[q] {
			out = append(out, q)
			seen[q] = true
		}
	}
	rest := []string{}
	for q := range set {
		if !seen[q] {
			rest = append(rest, q)
		}
	}
	rest = UniqueSorted(rest)
	return append(out, rest...)
}

func (n *normalizer) planTables() []string {
	ordinal := map[string]int{}
	for _, t := range n.project.Tables {
		ordinal[t.Name] = t.Ordinal
	}
	set := map[string]bool{}
	for _, node := range n.plan.Nodes {
		if node.Kind == "SCAN" && node.Table != "" {
			set[node.Table] = true
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if ordinal[out[i]] != ordinal[out[j]] {
			return ordinal[out[i]] < ordinal[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func (n *normalizer) scanNodeForTable(table string) string {
	for _, node := range n.plan.Nodes {
		if node.Kind == "SCAN" && node.Table == table {
			return node.ID
		}
	}
	return ""
}

func (n *normalizer) filterNodeForPredicate(id string) string {
	for _, node := range n.plan.Nodes {
		if node.Kind != "FILTER" {
			continue
		}
		for _, pid := range node.PredicateIDs {
			if pid == id {
				return node.ID
			}
			if pred, ok := n.preds[pid]; ok && pred.CompoundID != "" {
				if first := n.firstCompoundPredicate(pred.CompoundID); first == id {
					return node.ID
				}
			}
		}
	}
	return ""
}

func (n *normalizer) firstCompoundPredicate(compound string) string {
	best := Predicate{Ordinal: 1 << 30}
	found := false
	for _, p := range n.plan.Predicates {
		if p.CompoundID == compound && (!found || p.Ordinal < best.Ordinal) {
			best = p
			found = true
		}
	}
	return best.ID
}

func (n *normalizer) nodesByKind(kind string) []string {
	out := []string{}
	for _, node := range n.plan.Nodes {
		if node.Kind == kind {
			out = append(out, node.ID)
		}
	}
	sort.Strings(out)
	return out
}

func (n *normalizer) topoNodesByKind(kind string) []string {
	ids := n.nodesByKind(kind)
	kindSet := map[string]bool{}
	for _, id := range ids {
		kindSet[id] = true
	}
	depth := map[string]int{}
	var visit func(string) int
	visit = func(id string) int {
		if d, ok := depth[id]; ok {
			return d
		}
		node, ok := n.nodes[id]
		if !ok {
			return 0
		}
		best := 0
		for _, child := range node.Children {
			best = maxInt(best, visit(child)+1)
		}
		depth[id] = best
		return best
	}
	for _, id := range ids {
		visit(id)
	}
	sort.Slice(ids, func(i, j int) bool {
		di, dj := depth[ids[i]], depth[ids[j]]
		if di != dj {
			return di < dj
		}
		oi, oj := n.sourceIndex(ids[i]), n.sourceIndex(ids[j])
		if oi != oj {
			return oi < oj
		}
		return ids[i] < ids[j]
	})
	return ids
}

func (n *normalizer) sourceIndex(id string) int {
	for i, node := range n.plan.Nodes {
		if node.ID == id {
			return i
		}
	}
	return 1 << 30
}

func (n *normalizer) findScanForTable(filter Node, table string) (string, bool) {
	if table == "" {
		// Used only by legacy callers that do not know the predicate yet.
		for _, child := range filter.Children {
			if id, ok := n.firstScan(child); ok {
				return id, true
			}
		}
		return "", false
	}
	for _, child := range filter.Children {
		if id, ok := n.findScanByTable(child, table, map[string]bool{}); ok {
			return id, true
		}
	}
	return "", false
}

func (n *normalizer) firstScan(start string) (string, bool) {
	node, ok := n.nodes[start]
	if !ok {
		return "", false
	}
	if node.Kind == "SCAN" {
		return node.ID, true
	}
	for _, child := range node.Children {
		if id, ok := n.firstScan(child); ok {
			return id, true
		}
	}
	return "", false
}

func (n *normalizer) findScanByTable(start, table string, seen map[string]bool) (string, bool) {
	if seen[start] {
		return "", false
	}
	seen[start] = true
	node, ok := n.nodes[start]
	if !ok {
		return "", false
	}
	if node.Kind == "SCAN" && node.Table == table {
		return node.ID, true
	}
	for _, child := range node.Children {
		if id, ok := n.findScanByTable(child, table, seen); ok {
			return id, true
		}
	}
	return "", false
}

func (n *normalizer) pushAllowed(filterID, scanID, targetTable string) (bool, string) {
	if scanID == "" {
		return false, "TARGET_TABLE_SCAN_NOT_FOUND"
	}
	path, ok := n.findPath(filterID, scanID)
	if !ok {
		return false, "PUSH_PATH_NOT_FOUND"
	}
	// path[0] is the FILTER itself; inspect graph nodes below it.
	for idx := 1; idx < len(path); idx++ {
		node := path[idx]
		if node.Kind != "JOIN" {
			continue
		}
		childIndex := n.pathChildIndex(node, path, idx)
		switch node.JoinType {
		case "INNER", "CROSS":
			continue
		case "LEFT_OUTER":
			if childIndex == 1 {
				return false, "LEFT_OUTER_NULLABLE_RIGHT_SIDE_BLOCKED"
			}
		case "RIGHT_OUTER":
			if childIndex == 0 {
				return false, "RIGHT_OUTER_NULLABLE_LEFT_SIDE_BLOCKED"
			}
		case "FULL_OUTER":
			return false, "FULL_OUTER_EITHER_SIDE_BLOCKED"
		}
	}
	return true, "SINGLE_TABLE_PREDICATE_ELIGIBLE"
}

func (n *normalizer) findPath(fromID, toID string) ([]Node, bool) {
	type frame struct {
		id   string
		path []Node
	}
	queue := []frame{{id: fromID, path: []Node{}}}
	seen := map[string]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current.id] {
			continue
		}
		seen[current.id] = true
		node, ok := n.nodes[current.id]
		if !ok {
			continue
		}
		path := append(append([]Node(nil), current.path...), node)
		if current.id == toID {
			return path, true
		}
		for _, child := range node.Children {
			queue = append(queue, frame{id: child, path: path})
		}
	}
	return nil, false
}

func (n *normalizer) pathChildIndex(join Node, path []Node, joinPathIndex int) int {
	if joinPathIndex+1 >= len(path) {
		return -1
	}
	nextID := path[joinPathIndex+1].ID
	for i, child := range join.Children {
		if child == nextID {
			return i
		}
	}
	return -1
}

func (n *normalizer) joinContainsTable(join Node, table string) bool {
	if join.LeftTable == table || join.RightTable == table {
		return true
	}
	for _, child := range join.Children {
		if n.subtreeHasTable(child, table, map[string]bool{}) {
			return true
		}
	}
	return false
}

func (n *normalizer) subtreeHasTable(nodeID, table string, seen map[string]bool) bool {
	if seen[nodeID] {
		return false
	}
	seen[nodeID] = true
	node, ok := n.nodes[nodeID]
	if !ok {
		return false
	}
	if node.Kind == "SCAN" && node.Table == table {
		return true
	}
	for _, child := range node.Children {
		if n.subtreeHasTable(child, table, seen) {
			return true
		}
	}
	return false
}

func qualified(table, column string) string { return table + "." + column }

func tableOfQualified(q string) string {
	for i := 0; i < len(q); i++ {
		if q[i] == '.' {
			return q[:i]
		}
	}
	return ""
}

func mergeBoolSets(a, b map[string]bool) map[string]bool {
	out := cloneBoolSet(a)
	for k := range b {
		out[k] = true
	}
	return out
}

func cloneBoolSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k := range in {
		out[k] = in[k]
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
