package planopt

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type v2File struct {
	SchemaVersion   string        `json:"schemaVersion"`
	Seed            int64         `json:"seed"`
	TickNanoseconds int64         `json:"tickNanoseconds"`
	Tables          []v2Table     `json:"tables"`
	Statements      []v2Statement `json:"statements"`
}

type v2Table struct {
	Name        string     `json:"name"`
	Ordinal     int        `json:"ordinal"`
	Rows        int64      `json:"rows"`
	RowWidthKiB string     `json:"rowWidthKiB"`
	Columns     []v2Column `json:"columns"`
	AccessPaths []v2Path   `json:"accessPaths"`
}

type v2Column struct {
	Name          string          `json:"name"`
	Ordinal       int             `json:"ordinal"`
	DataType      string          `json:"dataType"`
	WidthKiB      json.RawMessage `json:"widthKiB"`
	NDV           int64           `json:"ndv"`
	NullPermyriad int64           `json:"nullPermyriad"`
	Min           int64           `json:"min"`
	Max           int64           `json:"max"`
	Buckets       []v2Bucket      `json:"buckets"`
}

type v2Bucket struct {
	Upper int64 `json:"upper"`
	Rows  int64 `json:"rows"`
	NDV   int64 `json:"ndv"`
}

type v2Path struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Columns []string `json:"columns"`
	Ordinal int      `json:"ordinal"`
}

type v2Statement struct {
	PlanIndex          int               `json:"planIndex"`
	PlanID             string            `json:"planId"`
	StatementID        string            `json:"statementId"`
	CommittedAtMs      int64             `json:"committedAtMs"`
	RootNodeID         string            `json:"rootNodeId"`
	RequiredColumns    []string          `json:"requiredColumns"`
	PriorCostPermyriad int64             `json:"priorCostPermyriad"`
	NodeGroups         []v2NodeGroup     `json:"nodeGroups"`
	ColumnRefs         []v2Ref           `json:"columnRefs"`
	Predicates         []v2Predicate     `json:"predicates"`
	RelativeParameters []v2RelativeParam `json:"relativeParameters"`
}

type v2NodeGroup struct {
	SubplanID string   `json:"subplanId"`
	NodeTrees []v2Node `json:"nodeTrees"`
}

type v2Node struct {
	NodeID            string            `json:"nodeId"`
	OpKind            string            `json:"opKind"`
	Table             string            `json:"table"`
	JoinType          string            `json:"joinType"`
	LeftTable         string            `json:"leftTable"`
	RightTable        string            `json:"rightTable"`
	LeftColumn        string            `json:"leftColumn"`
	RightColumn       string            `json:"rightColumn"`
	Children          []json.RawMessage `json:"children"`
	ChildRefs         []string          `json:"childRefs"`
	PredicateIDs      []string          `json:"predicateIds"`
	OutputColumns     []string          `json:"outputColumns"`
	ProjectionColumns []string          `json:"projectionColumns"`
	LimitValue        int64             `json:"limitValue"`
	Limit             int64             `json:"limit"`
	LimitRows         int64             `json:"limit_rows"`
	OperatorParams    map[string]int64  `json:"operatorParams"`
}

type v2Ref struct {
	ID      string `json:"id"`
	Ordinal int    `json:"ordinal"`
	Table   string `json:"table"`
	Column  string `json:"column"`
	NodeID  string `json:"nodeId"`
	Role    string `json:"role"`
}

type v2Predicate struct {
	ID                           string `json:"id"`
	Ordinal                      int    `json:"ordinal"`
	Table                        string `json:"table"`
	Column                       string `json:"column"`
	Operator                     string `json:"operator"`
	ParamPosition                int    `json:"paramPosition"`
	SecondParamPosition          int    `json:"secondParamPosition"`
	HighParamPosition            int    `json:"highParamPosition"`
	LiteralValue                 int64  `json:"literalValue"`
	Depth                        int    `json:"depth"`
	CompoundID                   string `json:"compoundId"`
	DeclaredSelectivityPermyriad int64  `json:"declaredSelectivityPermyriad"`
}

type v2RelativeParam struct {
	Position     int    `json:"position"`
	Name         string `json:"name"`
	DataType     string `json:"dataType"`
	DefaultValue int64  `json:"defaultValue"`
}

func loadV2(data []byte) (Project, error) {
	var doc v2File
	if err := unmarshalStrict(data, &doc, CodeFieldCombination, "load.v2"); err != nil {
		return Project{}, err
	}
	project := Project{SourceSchemaVersion: SchemaV2, Seed: doc.Seed, TickNanoseconds: doc.TickNanoseconds}
	for _, t := range doc.Tables {
		rowWidth, err := kiBRawToBytes(t.RowWidthKiB)
		if err != nil {
			return Project{}, err
		}
		table := Table{Name: t.Name, Ordinal: t.Ordinal, RowCount: t.Rows, RowWidthBytes: rowWidth}
		for _, c := range t.Columns {
			if c.NullPermyriad < 0 || c.NullPermyriad > ScalePermyriad {
				return Project{}, Errorf(CodeScaleOutOfRange, "load.v2.null_permyriad", "%s.%s=%d", t.Name, c.Name, c.NullPermyriad)
			}
			width, err := kiBRawMessageToBytes(c.WidthKiB)
			if err != nil {
				return Project{}, err
			}
			col := ColumnStat{
				Name: c.Name, Ordinal: c.Ordinal, TypeName: c.DataType, WidthBytes: width, NDV: c.NDV,
				NullPermyriad: c.NullPermyriad, MinValue: c.Min, MaxValue: c.Max,
			}
			for _, bk := range c.Buckets {
				col.HistogramBuckets = append(col.HistogramBuckets, Bucket{UpperBound: bk.Upper, RowCount: bk.Rows, DistinctCount: bk.NDV})
			}
			table.Columns = append(table.Columns, col)
		}
		for _, p := range t.AccessPaths {
			table.AccessPaths = append(table.AccessPaths, AccessPath{Name: p.Name, Kind: p.Kind, KeyColumns: append([]string(nil), p.Columns...), Ordinal: p.Ordinal})
		}
		project.Tables = append(project.Tables, table)
	}

	globalBase := 0
	stmts := append([]v2Statement(nil), doc.Statements...)
	// JSON is normally ordered, but cumulative global numbering must follow seq.
	for i := range stmts {
		for j := i + 1; j < len(stmts); j++ {
			if stmts[j].PlanIndex < stmts[i].PlanIndex {
				stmts[i], stmts[j] = stmts[j], stmts[i]
			}
		}
	}
	for _, s := range stmts {
		plan := Plan{
			Seq: s.PlanIndex, ID: s.PlanID, StatementID: s.StatementID,
			CommittedTick: msToTicks(s.CommittedAtMs, doc.TickNanoseconds),
			RootNodeID:    s.RootNodeID, RequiredColumns: append([]string(nil), s.RequiredColumns...),
			PriorCostMilli: s.PriorCostPermyriad / 10, AbsoluteParamBase: globalBase,
		}
		nodes, err := flattenV2Groups(s.NodeGroups)
		if err != nil {
			return Project{}, err
		}
		plan.Nodes = nodes
		for _, r := range s.ColumnRefs {
			plan.ColumnRefs = append(plan.ColumnRefs, ColumnRef{ID: r.ID, Ordinal: r.Ordinal, Table: r.Table, Column: r.Column, NodeID: r.NodeID, UsageRole: r.Role})
		}
		params := append([]v2RelativeParam(nil), s.RelativeParameters...)
		for i := range params {
			for j := i + 1; j < len(params); j++ {
				if params[j].Position < params[i].Position {
					params[i], params[j] = params[j], params[i]
				}
			}
		}
		for _, p := range params {
			plan.Params = append(plan.Params, Param{GlobalIndex: globalBase + p.Position, LocalIndex: p.Position, Name: p.Name, TypeName: p.DataType, DefaultValue: p.DefaultValue})
		}
		for _, p := range s.Predicates {
			if p.DeclaredSelectivityPermyriad < 0 || p.DeclaredSelectivityPermyriad > ScalePermyriad {
				return Project{}, Errorf(CodeScaleOutOfRange, "load.v2.sel_permyriad", "%s=%d", p.ID, p.DeclaredSelectivityPermyriad)
			}
			pred := Predicate{
				ID: p.ID, Ordinal: p.Ordinal, Table: p.Table, Column: p.Column, Op: p.Operator,
				NestingDepth: p.Depth, CompoundID: p.CompoundID, DeclaredSelPermyriad: p.DeclaredSelectivityPermyriad,
			}
			if p.ParamPosition > 0 {
				pred.HasParam = true
				pred.LocalParamIndex = p.ParamPosition
				pred.GlobalParamIndex = globalBase + p.ParamPosition
				if p.Operator == "BETWEEN" {
					secondLocal := firstPositive(p.HighParamPosition, p.SecondParamPosition, p.ParamPosition+1)
					pred.SecondLocalParamIndex = secondLocal
					pred.SecondGlobalParamIndex = globalBase + secondLocal
				}
			} else if p.LiteralValue != 0 {
				pred.HasLiteral = true
				pred.LiteralValue = p.LiteralValue
			}
			plan.Predicates = append(plan.Predicates, pred)
		}
		project.Plans = append(project.Plans, plan)
		globalBase += len(s.RelativeParameters)
	}
	if err := ValidateProject(project); err != nil {
		return Project{}, err
	}
	return project, nil
}

func flattenV2Groups(groups []v2NodeGroup) ([]Node, error) {
	out := []Node{}
	for _, group := range groups {
		for _, tree := range group.NodeTrees {
			nodes, err := flattenV2Node(tree, group.SubplanID)
			if err != nil {
				return nil, err
			}
			out = append(out, nodes...)
		}
	}
	return out, nil
}

func flattenV2Node(raw v2Node, subplan string) ([]Node, error) {
	node := Node{
		ID: raw.NodeID, SubplanID: subplan, Kind: raw.OpKind, Table: raw.Table,
		JoinType: raw.JoinType, LeftTable: raw.LeftTable, RightTable: raw.RightTable,
		LeftKey: raw.LeftColumn, RightKey: raw.RightColumn,
		PredicateIDs:      append([]string(nil), raw.PredicateIDs...),
		ProjectionColumns: append(append([]string(nil), raw.ProjectionColumns...), raw.OutputColumns...),
		LimitValue:        firstPositive64(raw.LimitValue, raw.Limit, raw.LimitRows, raw.OperatorParams["limit_rows"]),
		OperatorParams:    cloneInt64Map(raw.OperatorParams),
		Children:          append([]string(nil), raw.ChildRefs...),
	}
	if raw.NodeID == "" || raw.OpKind == "" {
		return nil, Errorf(CodeMalformedNodeTree, "load.v2.node", "node is missing id/opKind")
	}
	out := []Node{node}
	for _, childRaw := range raw.Children {
		var ref string
		if err := json.Unmarshal(childRaw, &ref); err == nil {
			node.Children = append(node.Children, ref)
			out[0].Children = node.Children
			continue
		}
		var child v2Node
		if err := unmarshalStrict(childRaw, &child, CodeMalformedNodeTree, "load.v2.child_node"); err != nil {
			return nil, Errorf(CodeMalformedNodeTree, "load.v2.child_decode", "node %s has child that is neither ref nor object", raw.NodeID)
		}
		children, err := flattenV2Node(child, subplan)
		if err != nil {
			return nil, err
		}
		out = append(out, children...)
		out[0].Children = append(out[0].Children, child.NodeID)
	}
	return out, nil
}

func msToTicks(ms, tickNanoseconds int64) int64 {
	if tickNanoseconds <= 0 {
		return ms
	}
	return ms * 1_000_000 / tickNanoseconds
}

func ticksToMS(ticks, tickNanoseconds int64) int64 {
	return ticks * tickNanoseconds / 1_000_000
}

func kiBRawToBytes(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n * 1024, nil
	}
	negative := strings.HasPrefix(raw, "-")
	raw = strings.TrimPrefix(raw, "-")
	parts := strings.Split(raw, ".")
	if len(parts) > 2 {
		return 0, Errorf(CodeFieldCombination, "unit.kib", "invalid KiB decimal %q", raw)
	}
	integer, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, Errorf(CodeFieldCombination, "unit.kib", "invalid KiB integer %q", raw)
	}
	var numerator int64
	var digits int
	if len(parts) == 2 && parts[1] != "" {
		fraction := parts[1]
		digits = len(fraction)
		numerator, err = strconv.ParseInt(fraction, 10, 64)
		if err != nil {
			return 0, Errorf(CodeFieldCombination, "unit.kib", "invalid KiB fraction %q", raw)
		}
	}
	denom := int64Pow10(digits)
	bytes := integer*1024 + numerator*1024/denom
	if negative {
		bytes = -bytes
	}
	return bytes, nil
}

func kiBRawMessageToBytes(raw json.RawMessage) (int64, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return kiBRawToBytes(text)
	}
	return kiBRawToBytes(string(raw))
}

func int64Pow10(n int) int64 {
	out := int64(1)
	for i := 0; i < n; i++ {
		out *= 10
	}
	return out
}

func bytesToKiBString(bytes int64) string {
	negative := bytes < 0
	if negative {
		bytes = -bytes
	}
	integer := bytes / 1024
	rem := bytes % 1024
	prefix := ""
	if negative {
		prefix = "-"
	}
	if rem == 0 {
		return prefix + strconv.FormatInt(integer, 10)
	}
	// 10^10 is divisible by 1024, so all binary fractional bytes terminate.
	frac := rem * 9_765_625
	s := fmt.Sprintf("%010d", frac)
	s = strings.TrimRight(s, "0")
	return prefix + strconv.FormatInt(integer, 10) + "." + s
}

func MarshalV2(project Project) ([]byte, error) {
	doc := v2File{SchemaVersion: SchemaV2, Seed: project.Seed, TickNanoseconds: project.TickNanoseconds}
	for _, t := range project.Tables {
		vt := v2Table{Name: t.Name, Ordinal: t.Ordinal, Rows: t.RowCount, RowWidthKiB: bytesToKiBString(t.RowWidthBytes)}
		for _, c := range t.Columns {
			vc := v2Column{
				Name: c.Name, Ordinal: c.Ordinal, DataType: c.TypeName, WidthKiB: json.RawMessage(`"` + bytesToKiBString(c.WidthBytes) + `"`),
				NDV: c.NDV, NullPermyriad: c.NullPermyriad, Min: c.MinValue, Max: c.MaxValue,
			}
			for _, bk := range c.HistogramBuckets {
				vc.Buckets = append(vc.Buckets, v2Bucket{Upper: bk.UpperBound, Rows: bk.RowCount, NDV: bk.DistinctCount})
			}
			vt.Columns = append(vt.Columns, vc)
		}
		for _, p := range t.AccessPaths {
			vt.AccessPaths = append(vt.AccessPaths, v2Path{Name: p.Name, Kind: p.Kind, Columns: append([]string(nil), p.KeyColumns...), Ordinal: p.Ordinal})
		}
		doc.Tables = append(doc.Tables, vt)
	}
	for _, plan := range project.SortedPlans() {
		s := v2Statement{
			PlanIndex: plan.Seq, PlanID: plan.ID, StatementID: plan.StatementID,
			CommittedAtMs: ticksToMS(plan.CommittedTick, project.TickNanoseconds),
			RootNodeID:    plan.RootNodeID, RequiredColumns: append([]string(nil), plan.RequiredColumns...),
			PriorCostPermyriad: plan.PriorCostMilli * 10,
		}
		grouped := map[string][]Node{}
		groupOrder := []string{}
		for _, n := range plan.Nodes {
			key := n.SubplanID
			if _, ok := grouped[key]; !ok {
				groupOrder = append(groupOrder, key)
			}
			grouped[key] = append(grouped[key], n)
		}
		for _, gid := range groupOrder {
			group := v2NodeGroup{SubplanID: gid}
			for _, n := range grouped[gid] {
				group.NodeTrees = append(group.NodeTrees, v2Node{
					NodeID: n.ID, OpKind: n.Kind, Table: n.Table, JoinType: n.JoinType,
					LeftTable: n.LeftTable, RightTable: n.RightTable, LeftColumn: n.LeftKey, RightColumn: n.RightKey,
					ChildRefs: append([]string(nil), n.Children...), PredicateIDs: append([]string(nil), n.PredicateIDs...),
					OutputColumns: append([]string(nil), n.ProjectionColumns...), LimitValue: n.LimitValue,
				})
			}
			s.NodeGroups = append(s.NodeGroups, group)
		}
		for _, r := range plan.ColumnRefs {
			s.ColumnRefs = append(s.ColumnRefs, v2Ref{ID: r.ID, Ordinal: r.Ordinal, Table: r.Table, Column: r.Column, NodeID: r.NodeID, Role: r.UsageRole})
		}
		for _, p := range plan.Params {
			s.RelativeParameters = append(s.RelativeParameters, v2RelativeParam{Position: p.LocalIndex, Name: p.Name, DataType: p.TypeName, DefaultValue: p.DefaultValue})
		}
		for _, p := range plan.Predicates {
			vp := v2Predicate{
				ID: p.ID, Ordinal: p.Ordinal, Table: p.Table, Column: p.Column, Operator: p.Op,
				Depth: p.NestingDepth, CompoundID: p.CompoundID, DeclaredSelectivityPermyriad: p.DeclaredSelPermyriad,
			}
			if p.HasParam {
				vp.ParamPosition = p.LocalParamIndex
				if p.Op == "BETWEEN" {
					vp.SecondParamPosition = p.SecondLocalParamIndex
				}
			} else if p.HasLiteral {
				vp.LiteralValue = p.LiteralValue
			}
			s.Predicates = append(s.Predicates, vp)
		}
		doc.Statements = append(doc.Statements, s)
	}
	return json.MarshalIndent(doc, "", "  ")
}
