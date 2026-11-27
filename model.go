package planopt

import "sort"

const (
	ScalePermille           int64 = 1_000
	ScalePermyriad          int64 = 10_000
	SelBasis                int64 = 1_000_000
	FixedPointBits          int   = 64
	FixedPointDecimalPlaces int   = 6

	SchemaV1 = "cpq.plan_engineering/v1"
	SchemaV2 = "2.0"
	SchemaV3 = "3.0"

	CheckpointFormatVersion = "planopt-checkpoint/v1"
)

type Bucket struct {
	UpperBound    int64 `json:"upper_bound"`
	RowCount      int64 `json:"row_count"`
	DistinctCount int64 `json:"distinct_count"`
}

type ColumnStat struct {
	Name             string   `json:"name"`
	Ordinal          int      `json:"ordinal"`
	TypeName         string   `json:"type_name"`
	WidthBytes       int64    `json:"width_bytes"`
	NDV              int64    `json:"ndv"`
	NullPermyriad    int64    `json:"null_permyriad"`
	MinValue         int64    `json:"min_value"`
	MaxValue         int64    `json:"max_value"`
	HistogramBuckets []Bucket `json:"histogram_buckets"`
}

type AccessPath struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	KeyColumns []string `json:"key_columns,omitempty"`
	Ordinal    int      `json:"ordinal"`
}

type Table struct {
	Name          string       `json:"name"`
	Ordinal       int          `json:"ordinal"`
	RowCount      int64        `json:"row_count"`
	RowWidthBytes int64        `json:"row_width_bytes"`
	Columns       []ColumnStat `json:"columns"`
	AccessPaths   []AccessPath `json:"access_paths"`
}

func (t Table) Column(name string) (ColumnStat, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return ColumnStat{}, false
}

func (t Table) Path(name string) (AccessPath, bool) {
	for _, p := range t.AccessPaths {
		if p.Name == name {
			return p, true
		}
	}
	return AccessPath{}, false
}

type ColumnRef struct {
	ID        string `json:"id"`
	Ordinal   int    `json:"ordinal"`
	Table     string `json:"table"`
	Column    string `json:"column"`
	NodeID    string `json:"node_id"`
	UsageRole string `json:"usage_role"`
}

type Predicate struct {
	ID                     string `json:"id"`
	Ordinal                int    `json:"ordinal"`
	Table                  string `json:"table"`
	Column                 string `json:"column"`
	Op                     string `json:"op"`
	GlobalParamIndex       int    `json:"global_param_index,omitempty"`
	LocalParamIndex        int    `json:"local_param_index,omitempty"`
	SecondGlobalParamIndex int    `json:"second_global_param_index,omitempty"`
	SecondLocalParamIndex  int    `json:"second_local_param_index,omitempty"`
	HasParam               bool   `json:"has_param"`
	LiteralValue           int64  `json:"literal_value,omitempty"`
	HasLiteral             bool   `json:"has_literal"`
	SecondLiteralValue     int64  `json:"second_literal_value,omitempty"`
	HasSecondLiteral       bool   `json:"has_second_literal"`
	NestingDepth           int    `json:"nesting_depth"`
	CompoundID             string `json:"compound_id,omitempty"`
	DeclaredSelPermyriad   int64  `json:"declared_sel_permyriad,omitempty"`
}

type Param struct {
	GlobalIndex  int    `json:"global_index"`
	LocalIndex   int    `json:"local_index"`
	Name         string `json:"name"`
	TypeName     string `json:"type_name"`
	DefaultValue int64  `json:"default_value"`
}

type Node struct {
	ID                string           `json:"id"`
	SubplanID         string           `json:"subplan_id,omitempty"`
	Kind              string           `json:"kind"`
	Table             string           `json:"table,omitempty"`
	JoinType          string           `json:"join_type,omitempty"`
	LeftTable         string           `json:"left_table,omitempty"`
	RightTable        string           `json:"right_table,omitempty"`
	LeftKey           string           `json:"left_key,omitempty"`
	RightKey          string           `json:"right_key,omitempty"`
	Children          []string         `json:"children,omitempty"`
	PredicateIDs      []string         `json:"predicate_ids,omitempty"`
	ProjectionColumns []string         `json:"projection_columns,omitempty"`
	LimitValue        int64            `json:"limit_value,omitempty"`
	OperatorParams    map[string]int64 `json:"operator_params,omitempty"`
}

type Plan struct {
	Seq               int         `json:"seq"`
	ID                string      `json:"id"`
	StatementID       string      `json:"statement_id"`
	CommittedTick     int64       `json:"committed_tick"`
	Fingerprint       string      `json:"fingerprint,omitempty"`
	RootNodeID        string      `json:"root_node_id"`
	RequiredColumns   []string    `json:"required_columns"`
	PriorCostMilli    int64       `json:"prior_cost_milli"`
	AbsoluteParamBase int         `json:"absolute_param_base"`
	Nodes             []Node      `json:"nodes"`
	ColumnRefs        []ColumnRef `json:"column_refs"`
	Predicates        []Predicate `json:"predicates"`
	Params            []Param     `json:"params"`
}

type Project struct {
	SourceSchemaVersion string  `json:"source_schema_version"`
	Seed                int64   `json:"seed"`
	TickNanoseconds     int64   `json:"tick_nanoseconds"`
	Tables              []Table `json:"tables"`
	Plans               []Plan  `json:"plans"`
}

func (p Project) Table(name string) (Table, bool) {
	for _, t := range p.Tables {
		if t.Name == name {
			return t, true
		}
	}
	return Table{}, false
}

func (p Project) Plan(seq int) (Plan, bool) {
	for _, pl := range p.Plans {
		if pl.Seq == seq {
			return pl, true
		}
	}
	return Plan{}, false
}

func (p Project) PlanByID(id string) (Plan, bool) {
	for _, pl := range p.Plans {
		if pl.ID == id {
			return pl, true
		}
	}
	return Plan{}, false
}

// SortedPlans returns a defensive seq-sorted copy. Scheduling code must use it
// so map/random input cannot alter deterministic results.
func (p Project) SortedPlans() []Plan {
	out := append([]Plan(nil), p.Plans...)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func SortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func UniqueSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := SortedStrings(in)
	w := 1
	for r := 1; r < len(out); r++ {
		if out[r] != out[w-1] {
			out[w] = out[r]
			w++
		}
	}
	return out[:w]
}
