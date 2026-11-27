package planopt

import "encoding/json"

type v3File struct {
	SchemaVersion   string        `json:"schema-version"`
	Seed            int64         `json:"seed"`
	TickNanoseconds int64         `json:"tick-nanoseconds"`
	Relations       []v3Relation  `json:"relations"`
	QueryPlans      []v3QueryPlan `json:"query-plans"`
}

type v3Relation struct {
	Name          string     `json:"name"`
	Ordinal       int        `json:"ordinal"`
	RowCount      int64      `json:"row-count"`
	RowWidthBytes int64      `json:"row-width-bytes"`
	Columns       []v3Column `json:"columns"`
	AccessPaths   []v3Path   `json:"access-paths"`
}

type v3Column struct {
	Name          string     `json:"name"`
	Ordinal       int        `json:"ordinal"`
	TypeName      string     `json:"type-name"`
	WidthBytes    int64      `json:"width-bytes"`
	NDV           int64      `json:"ndv"`
	NullPermyriad int64      `json:"null-permyriad"`
	MinValue      int64      `json:"min-value"`
	MaxValue      int64      `json:"max-value"`
	Buckets       []v3Bucket `json:"buckets"`
}

type v3Bucket struct {
	UpperBound    int64 `json:"upper-bound"`
	RowCount      int64 `json:"row-count"`
	DistinctCount int64 `json:"distinct-count"`
}

type v3Path struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	KeyColumns []string `json:"key-columns"`
	Ordinal    int      `json:"ordinal"`
}

type v3QueryPlan struct {
	Seq               int           `json:"seq"`
	PlanID            string        `json:"plan-id"`
	StatementID       string        `json:"statement-id"`
	CommittedTick     int64         `json:"committed-tick"`
	Fingerprint       string        `json:"fingerprint"`
	RootNodeID        string        `json:"root-node-id"`
	RequiredColumns   []string      `json:"required-columns"`
	PriorCostMilli    int64         `json:"prior-cost-milli"`
	AbsoluteParamBase int           `json:"absolute-param-base"`
	Nodes             []v3Node      `json:"operator-buffer"`
	ColumnRefs        []v3Ref       `json:"column-refs"`
	Predicates        []v3Predicate `json:"predicates"`
	Params            []v3Param     `json:"params"`
}

type v3Node struct {
	ID                string   `json:"id"`
	SubplanID         string   `json:"subplan-id"`
	Kind              string   `json:"kind"`
	Table             string   `json:"table"`
	JoinType          string   `json:"join-type"`
	LeftTable         string   `json:"left-table"`
	RightTable        string   `json:"right-table"`
	LeftKey           string   `json:"left-key"`
	RightKey          string   `json:"right-key"`
	Inputs            []string `json:"inputs"`
	PredicateIDs      []string `json:"predicate-ids"`
	ProjectionColumns []string `json:"projection-columns"`
	LimitValue        int64    `json:"limit-value"`
}

type v3Ref struct {
	ID        string `json:"id"`
	Ordinal   int    `json:"ordinal"`
	Table     string `json:"table"`
	Column    string `json:"column"`
	NodeID    string `json:"node-id"`
	UsageRole string `json:"usage-role"`
}

type v3Predicate struct {
	ID                     string `json:"id"`
	Ordinal                int    `json:"ordinal"`
	Table                  string `json:"table"`
	Column                 string `json:"column"`
	Op                     string `json:"op"`
	GlobalParamIndex       int    `json:"global-param-index"`
	SecondGlobalParamIndex int    `json:"second-global-param-index"`
	LiteralValue           int64  `json:"literal-value"`
	HasLiteral             bool   `json:"has-literal"`
	NestingDepth           int    `json:"nesting-depth"`
	CompoundID             string `json:"compound-id"`
	SelPermyriad           int64  `json:"selectivity-permyriad"`
}

type v3Param struct {
	GlobalIndex  int    `json:"global-index"`
	LocalIndex   int    `json:"local-index"`
	Name         string `json:"name"`
	TypeName     string `json:"type-name"`
	DefaultValue int64  `json:"default-value"`
}

func MarshalV3(project Project) ([]byte, error) {
	doc := v3File{SchemaVersion: SchemaV3, Seed: project.Seed, TickNanoseconds: project.TickNanoseconds}
	for _, t := range project.Tables {
		rel := v3Relation{Name: t.Name, Ordinal: t.Ordinal, RowCount: t.RowCount, RowWidthBytes: t.RowWidthBytes}
		for _, c := range t.Columns {
			vc := v3Column{Name: c.Name, Ordinal: c.Ordinal, TypeName: c.TypeName, WidthBytes: c.WidthBytes, NDV: c.NDV, NullPermyriad: c.NullPermyriad, MinValue: c.MinValue, MaxValue: c.MaxValue}
			for _, bk := range c.HistogramBuckets {
				vc.Buckets = append(vc.Buckets, v3Bucket{UpperBound: bk.UpperBound, RowCount: bk.RowCount, DistinctCount: bk.DistinctCount})
			}
			rel.Columns = append(rel.Columns, vc)
		}
		for _, p := range t.AccessPaths {
			rel.AccessPaths = append(rel.AccessPaths, v3Path{Name: p.Name, Kind: p.Kind, KeyColumns: append([]string(nil), p.KeyColumns...), Ordinal: p.Ordinal})
		}
		doc.Relations = append(doc.Relations, rel)
	}
	for _, plan := range project.SortedPlans() {
		q := v3QueryPlan{
			Seq: plan.Seq, PlanID: plan.ID, StatementID: plan.StatementID, CommittedTick: plan.CommittedTick,
			Fingerprint: plan.Fingerprint, RootNodeID: plan.RootNodeID, RequiredColumns: append([]string(nil), plan.RequiredColumns...),
			PriorCostMilli: plan.PriorCostMilli, AbsoluteParamBase: plan.AbsoluteParamBase,
		}
		for _, n := range plan.Nodes {
			q.Nodes = append(q.Nodes, v3Node{
				ID: n.ID, SubplanID: n.SubplanID, Kind: n.Kind, Table: n.Table, JoinType: n.JoinType,
				LeftTable: n.LeftTable, RightTable: n.RightTable, LeftKey: n.LeftKey, RightKey: n.RightKey,
				Inputs: append([]string(nil), n.Children...), PredicateIDs: append([]string(nil), n.PredicateIDs...),
				ProjectionColumns: append([]string(nil), n.ProjectionColumns...), LimitValue: n.LimitValue,
			})
		}
		for _, r := range plan.ColumnRefs {
			q.ColumnRefs = append(q.ColumnRefs, v3Ref{ID: r.ID, Ordinal: r.Ordinal, Table: r.Table, Column: r.Column, NodeID: r.NodeID, UsageRole: r.UsageRole})
		}
		for _, p := range plan.Params {
			q.Params = append(q.Params, v3Param{GlobalIndex: p.GlobalIndex, LocalIndex: p.LocalIndex, Name: p.Name, TypeName: p.TypeName, DefaultValue: p.DefaultValue})
		}
		for _, p := range plan.Predicates {
			vp := v3Predicate{
				ID: p.ID, Ordinal: p.Ordinal, Table: p.Table, Column: p.Column, Op: p.Op,
				GlobalParamIndex: p.GlobalParamIndex, SecondGlobalParamIndex: p.SecondGlobalParamIndex,
				LiteralValue: p.LiteralValue, HasLiteral: p.HasLiteral, NestingDepth: p.NestingDepth,
				CompoundID: p.CompoundID, SelPermyriad: p.DeclaredSelPermyriad,
			}
			q.Predicates = append(q.Predicates, vp)
		}
		doc.QueryPlans = append(doc.QueryPlans, q)
	}
	return json.MarshalIndent(doc, "", "  ")
}

func loadV3(data []byte) (Project, error) {
	var doc v3File
	if err := unmarshalStrict(data, &doc, CodeFieldCombination, "load.v3"); err != nil {
		return Project{}, err
	}
	project := Project{SourceSchemaVersion: SchemaV3, Seed: doc.Seed, TickNanoseconds: doc.TickNanoseconds}
	for _, t := range doc.Relations {
		table := Table{Name: t.Name, Ordinal: t.Ordinal, RowCount: t.RowCount, RowWidthBytes: t.RowWidthBytes}
		for _, c := range t.Columns {
			if c.NullPermyriad < 0 || c.NullPermyriad > ScalePermyriad {
				return Project{}, Errorf(CodeScaleOutOfRange, "load.v3.null_scale", "%s.%s=%d", t.Name, c.Name, c.NullPermyriad)
			}
			col := ColumnStat{Name: c.Name, Ordinal: c.Ordinal, TypeName: c.TypeName, WidthBytes: c.WidthBytes, NDV: c.NDV, NullPermyriad: c.NullPermyriad, MinValue: c.MinValue, MaxValue: c.MaxValue}
			for _, bk := range c.Buckets {
				col.HistogramBuckets = append(col.HistogramBuckets, Bucket{UpperBound: bk.UpperBound, RowCount: bk.RowCount, DistinctCount: bk.DistinctCount})
			}
			table.Columns = append(table.Columns, col)
		}
		for _, p := range t.AccessPaths {
			table.AccessPaths = append(table.AccessPaths, AccessPath{Name: p.Name, Kind: p.Kind, KeyColumns: append([]string(nil), p.KeyColumns...), Ordinal: p.Ordinal})
		}
		project.Tables = append(project.Tables, table)
	}
	for _, q := range doc.QueryPlans {
		plan := Plan{
			Seq: q.Seq, ID: q.PlanID, StatementID: q.StatementID, CommittedTick: q.CommittedTick,
			Fingerprint: q.Fingerprint, RootNodeID: q.RootNodeID, RequiredColumns: append([]string(nil), q.RequiredColumns...),
			PriorCostMilli: q.PriorCostMilli, AbsoluteParamBase: q.AbsoluteParamBase,
		}
		for _, n := range q.Nodes {
			plan.Nodes = append(plan.Nodes, Node{
				ID: n.ID, SubplanID: n.SubplanID, Kind: n.Kind, Table: n.Table, JoinType: n.JoinType,
				LeftTable: n.LeftTable, RightTable: n.RightTable, LeftKey: n.LeftKey, RightKey: n.RightKey,
				Children: append([]string(nil), n.Inputs...), PredicateIDs: append([]string(nil), n.PredicateIDs...),
				ProjectionColumns: append([]string(nil), n.ProjectionColumns...), LimitValue: n.LimitValue,
			})
		}
		for _, r := range q.ColumnRefs {
			plan.ColumnRefs = append(plan.ColumnRefs, ColumnRef{ID: r.ID, Ordinal: r.Ordinal, Table: r.Table, Column: r.Column, NodeID: r.NodeID, UsageRole: r.UsageRole})
		}
		localByGlobal := map[int]int{}
		for _, p := range q.Params {
			plan.Params = append(plan.Params, Param{GlobalIndex: p.GlobalIndex, LocalIndex: p.LocalIndex, Name: p.Name, TypeName: p.TypeName, DefaultValue: p.DefaultValue})
			localByGlobal[p.GlobalIndex] = p.LocalIndex
		}
		for _, p := range q.Predicates {
			if p.SelPermyriad < 0 || p.SelPermyriad > ScalePermyriad {
				return Project{}, Errorf(CodeScaleOutOfRange, "load.v3.sel_scale", "%s=%d", p.ID, p.SelPermyriad)
			}
			pred := Predicate{
				ID: p.ID, Ordinal: p.Ordinal, Table: p.Table, Column: p.Column, Op: p.Op,
				NestingDepth: p.NestingDepth, CompoundID: p.CompoundID, DeclaredSelPermyriad: p.SelPermyriad,
			}
			if p.GlobalParamIndex > 0 {
				pred.HasParam = true
				pred.GlobalParamIndex = p.GlobalParamIndex
				pred.LocalParamIndex = localByGlobal[p.GlobalParamIndex]
				if p.SecondGlobalParamIndex > 0 {
					pred.SecondGlobalParamIndex = p.SecondGlobalParamIndex
					pred.SecondLocalParamIndex = localByGlobal[p.SecondGlobalParamIndex]
				}
			}
			if p.HasLiteral {
				pred.HasLiteral = true
				pred.LiteralValue = p.LiteralValue
			}
			plan.Predicates = append(plan.Predicates, pred)
		}
		project.Plans = append(project.Plans, plan)
	}
	if err := ValidateProject(project); err != nil {
		return Project{}, err
	}
	return project, nil
}
