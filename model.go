package queryopt

const (
	SchemaV1 = "queryopt/v1-flat"
	SchemaV2 = "queryopt/v2-nested"
	SchemaV3 = "queryopt/v3-current"

	FixedFractionBits = 32
	FixedScale        = int64(1) << FixedFractionBits

	TicksPerMillisecond = int64(10_000) // 100 ns database tick
	BytesPerKiB         = int64(1024)

	PathIndexLookup    = "INDEX_LOOKUP"
	PathSequentialScan = "SEQUENTIAL_SCAN"

	PredEquality = "eq"
	PredRange    = "range"
	PredBoundary = "boundary"
	PredJoin     = "join"
	PredAnd      = "and"
	PredOr       = "or"

	CandidateFeasible        = "FEASIBLE"
	ReasonCartesianForbidden = "R_CARTESIAN_EDGE_FORBIDDEN"
	ReasonDisconnectedPrefix = "R_DISCONNECTED_PREFIX"
	ReasonCycleCut           = "R_CYCLE_EDGE_CUT"
	ReasonUnreachablePred    = "R_UNREACHABLE_PREDICATE"
)

// Project is the unitary internal model produced from any schema edition.
type Project struct {
	SchemaVersion string        `json:"schema_version"`
	SnapshotKind  string        `json:"snapshot_kind,omitempty"`
	Seed          int64         `json:"seed"`
	Tables        []Table       `json:"tables"`
	Columns       []Column      `json:"columns"`
	Buckets       []Bucket      `json:"buckets"`
	Predicates    []Predicate   `json:"predicates"`
	Queries       []Query       `json:"queries"`
	Edges         []JoinEdge    `json:"edges"`
	Weights       CostWeights   `json:"weights"`
	Rules         DecisionRules `json:"rules"`
	Indexes       *ProjectIndex `json:"-"`
}

type Table struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Rows            int64  `json:"rows"`
	NullRows        int64  `json:"null_rows"`
	SampleLatencyMS int64  `json:"sample_latency_ms"`
	AllocationBytes int64  `json:"allocation_bytes"`
	DeclaredOrder   int    `json:"declared_order"`
}

type Column struct {
	ID            string `json:"id"`
	TableID       string `json:"table_id"`
	Name          string `json:"name"`
	WidthBytes    int64  `json:"width_bytes"`
	NDV           int64  `json:"ndv"`
	NullPermyriad int64  `json:"null_permyriad"`
	DeclaredOrder int    `json:"declared_order"`
}

type Bucket struct {
	ID          string `json:"id"`
	ColumnID    string `json:"column_id"`
	BucketIndex int    `json:"bucket_index"`
	Lower       int64  `json:"lower"`
	Upper       int64  `json:"upper"`
	Rows        int64  `json:"rows"`
	Distinct    int64  `json:"distinct"`
}

type PredicateArg struct {
	Literal           *int64 `json:"literal,omitempty"`
	PlaceholderGlobal int    `json:"placeholder_global,omitempty"`
}

type Predicate struct {
	ID                     string       `json:"id"`
	QueryID                string       `json:"query_id"`
	Kind                   string       `json:"kind"`
	Logic                  string       `json:"logic,omitempty"`
	Children               []string     `json:"children,omitempty"`
	TableID                string       `json:"table_id,omitempty"`
	ColumnID               string       `json:"column_id,omitempty"`
	LeftColumnID           string       `json:"left_column_id,omitempty"`
	RightColumnID          string       `json:"right_column_id,omitempty"`
	Op                     string       `json:"op,omitempty"`
	Low                    PredicateArg `json:"low,omitempty"`
	High                   PredicateArg `json:"high,omitempty"`
	Value                  PredicateArg `json:"value,omitempty"`
	InclusiveLow           bool         `json:"inclusive_low,omitempty"`
	InclusiveHigh          bool         `json:"inclusive_high,omitempty"`
	DeclaredSelectivity    Fixed        `json:"declared_selectivity,omitempty"`
	HasDeclaredSelectivity bool         `json:"has_declared_selectivity,omitempty"`
	WeightPermyriad        int64        `json:"weight_permyriad,omitempty"`
	DeclaredOrder          int          `json:"declared_order"`
}

type Query struct {
	ID                   string   `json:"id"`
	Ordinal              int      `json:"ordinal"`
	Name                 string   `json:"name"`
	TableIDs             []string `json:"table_ids"`
	RootPredicateIDs     []string `json:"root_predicate_ids"`
	RequiredColumnIDs    []string `json:"required_column_ids"`
	PlaceholderBase      int      `json:"placeholder_base"`
	PlaceholderCount     int      `json:"placeholder_count"`
	IndexThresholdRows   int64    `json:"index_threshold_rows"`
	DeclaredCandidateIDs []string `json:"declared_candidate_ids,omitempty"`
}

type JoinEdge struct {
	ID              string `json:"id"`
	QueryID         string `json:"query_id"`
	LeftTableID     string `json:"left_table_id"`
	RightTableID    string `json:"right_table_id"`
	PredicateID     string `json:"predicate_id,omitempty"`
	Cartesian       bool   `json:"cartesian"`
	BreakPriority   int    `json:"break_priority"`
	DeclaredOrder   int    `json:"declared_order"`
	WeightPermyriad int64  `json:"weight_permyriad"`
}

type CostWeights struct {
	ScanPermyriad         int64 `json:"scan_permyriad"`
	IntermediatePermyriad int64 `json:"intermediate_permyriad"`
	WidthPermyriad        int64 `json:"width_permyriad"`
}

type DecisionRules struct {
	// TieRelativePermyriad treats candidates within this relative distance as
	// tied and applies declared-order adjudication.
	TieRelativePermyriad int64 `json:"tie_relative_permyriad"`
	WorkerBudget         int   `json:"worker_budget"`
	CandidateBuffer      int   `json:"candidate_buffer"`
}

type ProjectIndex struct {
	Tables          map[string]*Table
	Columns         map[string]*Column
	Predicates      map[string]*Predicate
	Queries         map[string]*Query
	Edges           map[string]*JoinEdge
	BucketsByColumn map[string][]*Bucket
}

func (p *Project) Index() *ProjectIndex {
	if p.Indexes != nil {
		return p.Indexes
	}
	idx := &ProjectIndex{
		Tables:          map[string]*Table{},
		Columns:         map[string]*Column{},
		Predicates:      map[string]*Predicate{},
		Queries:         map[string]*Query{},
		Edges:           map[string]*JoinEdge{},
		BucketsByColumn: map[string][]*Bucket{},
	}
	for i := range p.Tables {
		idx.Tables[p.Tables[i].ID] = &p.Tables[i]
	}
	for i := range p.Columns {
		idx.Columns[p.Columns[i].ID] = &p.Columns[i]
	}
	for i := range p.Buckets {
		b := &p.Buckets[i]
		idx.BucketsByColumn[b.ColumnID] = append(idx.BucketsByColumn[b.ColumnID], b)
	}
	for i := range p.Predicates {
		idx.Predicates[p.Predicates[i].ID] = &p.Predicates[i]
	}
	for i := range p.Queries {
		idx.Queries[p.Queries[i].ID] = &p.Queries[i]
	}
	for i := range p.Edges {
		idx.Edges[p.Edges[i].ID] = &p.Edges[i]
	}
	p.Indexes = idx
	return idx
}
