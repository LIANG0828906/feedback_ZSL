package planopt

import "encoding/json"

type v1File struct {
	Format          string        `json:"format"`
	Seed            int64         `json:"seed"`
	TickNanoseconds int64         `json:"tick_nanoseconds"`
	Tables          []v1Table     `json:"tables"`
	Statements      []v1Statement `json:"statements"`
}

type v1Table struct {
	TableName       string     `json:"table_name"`
	RelationOrdinal int        `json:"relation_ordinal"`
	RowCount        int64      `json:"row_count"`
	RowWidthBytes   int64      `json:"row_width_bytes"`
	ColumnStats     []v1Column `json:"column_stats"`
	AccessPaths     []v1Path   `json:"access_paths"`
}

type v1Column struct {
	ColumnName       string     `json:"column_name"`
	Ordinal          int        `json:"ordinal"`
	TypeName         string     `json:"type_name"`
	WidthBytes       int64      `json:"width_bytes"`
	NDV              int64      `json:"ndv"`
	NullPermille     int64      `json:"null_permille"`
	MinValue         int64      `json:"min_value"`
	MaxValue         int64      `json:"max_value"`
	HistogramBuckets []v1Bucket `json:"histogram_buckets"`
}

type v1Bucket struct {
	UpperBound    int64 `json:"upper_bound"`
	RowCount      int64 `json:"row_count"`
	DistinctCount int64 `json:"distinct_count"`
}

type v1Path struct {
	PathName   string   `json:"path_name"`
	PathKind   string   `json:"path_kind"`
	KeyColumns []string `json:"key_columns"`
	Ordinal    int      `json:"ordinal"`
}

type v1Statement struct {
	PlanSeq             int           `json:"plan_seq"`
	PlanID              string        `json:"plan_id"`
	StatementID         string        `json:"statement_id"`
	CommittedTick       int64         `json:"committed_tick"`
	FingerprintSHA256   string        `json:"fingerprint_sha256"`
	RootNode            string        `json:"root_node"`
	AbsoluteParamOffset int           `json:"absolute_param_offset"`
	RequiredColumns     []string      `json:"required_columns"`
	PriorCostMilli      int64         `json:"prior_cost_milli"`
	FlatNodes           []v1Node      `json:"flat_nodes"`
	ColumnReferences    []v1Ref       `json:"column_references"`
	PredicateConditions []v1Predicate `json:"predicate_conditions"`
	Parameters          []v1Param     `json:"parameters"`
}

type v1Node struct {
	NodeID            string           `json:"node_id"`
	SubplanID         string           `json:"subplan_id"`
	OpKind            string           `json:"op_kind"`
	TableName         string           `json:"table_name"`
	JoinType          string           `json:"join_type"`
	LeftTable         string           `json:"left_table"`
	RightTable        string           `json:"right_table"`
	LeftKey           string           `json:"left_key"`
	RightKey          string           `json:"right_key"`
	Children          []string         `json:"children"`
	PredicateIDs      []string         `json:"predicate_ids"`
	ProjectionColumns []string         `json:"projection_columns"`
	LimitValue        int64            `json:"limit_value"`
	LimitRows         int64            `json:"limit_rows"`
	OperatorParams    map[string]int64 `json:"operator_params"`
}

type v1Ref struct {
	RefID      string `json:"ref_id"`
	RefOrdinal int    `json:"ref_ordinal"`
	TableName  string `json:"table_name"`
	ColumnName string `json:"column_name"`
	NodeID     string `json:"node_id"`
	UsageRole  string `json:"usage_role"`
}

type v1Predicate struct {
	PredicateID                 string `json:"predicate_id"`
	PredicateOrdinal            int    `json:"predicate_ordinal"`
	TableName                   string `json:"table_name"`
	ColumnName                  string `json:"column_name"`
	CompareOp                   string `json:"compare_op"`
	AbsoluteParamIndex          int    `json:"absolute_param_index"`
	SecondAbsoluteParamIndex    int    `json:"second_absolute_param_index"`
	HighAbsoluteParamIndex      int    `json:"high_absolute_param_index"`
	LiteralValue                int64  `json:"literal_value"`
	HasLiteralValue             *bool  `json:"has_literal_value,omitempty"`
	NestingDepth                int    `json:"nesting_depth"`
	CompoundID                  string `json:"compound_id"`
	DeclaredSelectivityPermille int64  `json:"declared_selectivity_permille"`
}

type v1Param struct {
	AbsoluteIndex int    `json:"absolute_index"`
	ParameterName string `json:"parameter_name"`
	TypeName      string `json:"type_name"`
	DefaultValue  int64  `json:"default_value"`
}

func loadV1(data []byte) (Project, error) {
	var doc v1File
	if err := unmarshalStrict(data, &doc, CodeFieldCombination, "load.v1"); err != nil {
		return Project{}, err
	}
	project := Project{SourceSchemaVersion: SchemaV1, Seed: doc.Seed, TickNanoseconds: doc.TickNanoseconds}
	for _, t := range doc.Tables {
		table := Table{Name: t.TableName, Ordinal: t.RelationOrdinal, RowCount: t.RowCount, RowWidthBytes: t.RowWidthBytes}
		for _, c := range t.ColumnStats {
			if c.NullPermille < 0 || c.NullPermille > ScalePermille {
				return Project{}, Errorf(CodeScaleOutOfRange, "load.v1.null_permille", "%s.%s=%d", t.TableName, c.ColumnName, c.NullPermille)
			}
			stat := ColumnStat{
				Name: c.ColumnName, Ordinal: c.Ordinal, TypeName: c.TypeName, WidthBytes: c.WidthBytes,
				NDV: c.NDV, NullPermyriad: c.NullPermille * 10, MinValue: c.MinValue, MaxValue: c.MaxValue,
			}
			for _, bk := range c.HistogramBuckets {
				stat.HistogramBuckets = append(stat.HistogramBuckets, Bucket{UpperBound: bk.UpperBound, RowCount: bk.RowCount, DistinctCount: bk.DistinctCount})
			}
			table.Columns = append(table.Columns, stat)
		}
		for _, p := range t.AccessPaths {
			table.AccessPaths = append(table.AccessPaths, AccessPath{Name: p.PathName, Kind: p.PathKind, KeyColumns: append([]string(nil), p.KeyColumns...), Ordinal: p.Ordinal})
		}
		project.Tables = append(project.Tables, table)
	}

	for _, s := range doc.Statements {
		plan := Plan{
			Seq: s.PlanSeq, ID: s.PlanID, StatementID: s.StatementID, CommittedTick: s.CommittedTick,
			Fingerprint: s.FingerprintSHA256, RootNodeID: s.RootNode, RequiredColumns: append([]string(nil), s.RequiredColumns...),
			PriorCostMilli: s.PriorCostMilli, AbsoluteParamBase: s.AbsoluteParamOffset,
		}
		for _, n := range s.FlatNodes {
			plan.Nodes = append(plan.Nodes, Node{
				ID: n.NodeID, SubplanID: n.SubplanID, Kind: n.OpKind, Table: n.TableName,
				JoinType: n.JoinType, LeftTable: n.LeftTable, RightTable: n.RightTable, LeftKey: n.LeftKey, RightKey: n.RightKey,
				Children: append([]string(nil), n.Children...), PredicateIDs: append([]string(nil), n.PredicateIDs...),
				ProjectionColumns: append([]string(nil), n.ProjectionColumns...),
				LimitValue:        firstPositive64(n.LimitValue, n.LimitRows, n.OperatorParams["limit_rows"]),
				OperatorParams:    cloneInt64Map(n.OperatorParams),
			})
		}
		for _, r := range s.ColumnReferences {
			plan.ColumnRefs = append(plan.ColumnRefs, ColumnRef{ID: r.RefID, Ordinal: r.RefOrdinal, Table: r.TableName, Column: r.ColumnName, NodeID: r.NodeID, UsageRole: r.UsageRole})
		}
		for _, p := range s.Parameters {
			plan.Params = append(plan.Params, Param{GlobalIndex: p.AbsoluteIndex, Name: p.ParameterName, TypeName: p.TypeName, DefaultValue: p.DefaultValue})
		}
		localByGlobal := map[int]int{}
		for i := range plan.Params {
			plan.Params[i].LocalIndex = i + 1
			localByGlobal[plan.Params[i].GlobalIndex] = plan.Params[i].LocalIndex
		}
		for _, p := range s.PredicateConditions {
			if p.DeclaredSelectivityPermille < 0 || p.DeclaredSelectivityPermille > ScalePermille {
				return Project{}, Errorf(CodeScaleOutOfRange, "load.v1.sel_permille", "%s=%d", p.PredicateID, p.DeclaredSelectivityPermille)
			}
			pred := Predicate{
				ID: p.PredicateID, Ordinal: p.PredicateOrdinal, Table: p.TableName, Column: p.ColumnName, Op: p.CompareOp,
				NestingDepth: p.NestingDepth, CompoundID: p.CompoundID, DeclaredSelPermyriad: p.DeclaredSelectivityPermille * 10,
			}
			if p.AbsoluteParamIndex > 0 {
				pred.HasParam = true
				pred.GlobalParamIndex = p.AbsoluteParamIndex
				pred.LocalParamIndex = localByGlobal[p.AbsoluteParamIndex]
				if p.CompareOp == "BETWEEN" {
					pred.SecondGlobalParamIndex = p.SecondAbsoluteParamIndex
					if pred.SecondGlobalParamIndex == 0 {
						pred.SecondGlobalParamIndex = firstPositive(p.HighAbsoluteParamIndex, p.SecondAbsoluteParamIndex, p.AbsoluteParamIndex+1)
					}
					pred.SecondLocalParamIndex = localByGlobal[pred.SecondGlobalParamIndex]
				}
			}
			if p.HasLiteralValue != nil && *p.HasLiteralValue {
				pred.HasLiteral = true
				pred.LiteralValue = p.LiteralValue
			} else if p.LiteralValue != 0 && p.AbsoluteParamIndex == 0 {
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

func MarshalV1(project Project) ([]byte, error) {
	doc := v1File{Format: SchemaV1, Seed: project.Seed, TickNanoseconds: project.TickNanoseconds}
	for _, t := range project.Tables {
		vt := v1Table{TableName: t.Name, RelationOrdinal: t.Ordinal, RowCount: t.RowCount, RowWidthBytes: t.RowWidthBytes}
		for _, c := range t.Columns {
			vc := v1Column{ColumnName: c.Name, Ordinal: c.Ordinal, TypeName: c.TypeName, WidthBytes: c.WidthBytes, NDV: c.NDV, NullPermille: c.NullPermyriad / 10, MinValue: c.MinValue, MaxValue: c.MaxValue}
			for _, bk := range c.HistogramBuckets {
				vc.HistogramBuckets = append(vc.HistogramBuckets, v1Bucket{UpperBound: bk.UpperBound, RowCount: bk.RowCount, DistinctCount: bk.DistinctCount})
			}
			vt.ColumnStats = append(vt.ColumnStats, vc)
		}
		for _, p := range t.AccessPaths {
			vt.AccessPaths = append(vt.AccessPaths, v1Path{PathName: p.Name, PathKind: p.Kind, KeyColumns: append([]string(nil), p.KeyColumns...), Ordinal: p.Ordinal})
		}
		doc.Tables = append(doc.Tables, vt)
	}
	for _, plan := range project.SortedPlans() {
		s := v1Statement{
			PlanSeq: plan.Seq, PlanID: plan.ID, StatementID: plan.StatementID, CommittedTick: plan.CommittedTick,
			FingerprintSHA256: plan.Fingerprint, RootNode: plan.RootNodeID, AbsoluteParamOffset: plan.AbsoluteParamBase,
			RequiredColumns: append([]string(nil), plan.RequiredColumns...), PriorCostMilli: plan.PriorCostMilli,
		}
		for _, n := range plan.Nodes {
			s.FlatNodes = append(s.FlatNodes, v1Node{
				NodeID: n.ID, SubplanID: n.SubplanID, OpKind: n.Kind, TableName: n.Table, JoinType: n.JoinType,
				LeftTable: n.LeftTable, RightTable: n.RightTable, LeftKey: n.LeftKey, RightKey: n.RightKey,
				Children: append([]string(nil), n.Children...), PredicateIDs: append([]string(nil), n.PredicateIDs...),
				ProjectionColumns: append([]string(nil), n.ProjectionColumns...), LimitValue: n.LimitValue,
			})
		}
		for _, r := range plan.ColumnRefs {
			s.ColumnReferences = append(s.ColumnReferences, v1Ref{RefID: r.ID, RefOrdinal: r.Ordinal, TableName: r.Table, ColumnName: r.Column, NodeID: r.NodeID, UsageRole: r.UsageRole})
		}
		for _, p := range plan.Params {
			s.Parameters = append(s.Parameters, v1Param{AbsoluteIndex: p.GlobalIndex, ParameterName: p.Name, TypeName: p.TypeName, DefaultValue: p.DefaultValue})
		}
		for _, p := range plan.Predicates {
			vp := v1Predicate{
				PredicateID: p.ID, PredicateOrdinal: p.Ordinal, TableName: p.Table, ColumnName: p.Column, CompareOp: p.Op,
				NestingDepth: p.NestingDepth, CompoundID: p.CompoundID, DeclaredSelectivityPermille: p.DeclaredSelPermyriad / 10,
			}
			if p.HasParam {
				vp.AbsoluteParamIndex = p.GlobalParamIndex
				if p.Op == "BETWEEN" {
					vp.SecondAbsoluteParamIndex = p.SecondGlobalParamIndex
				}
			}
			if p.HasLiteral {
				vp.LiteralValue = p.LiteralValue
			}
			s.PredicateConditions = append(s.PredicateConditions, vp)
		}
		doc.Statements = append(doc.Statements, s)
	}
	return json.MarshalIndent(doc, "", "  ")
}
