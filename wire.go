package queryopt

import (
	"encoding/json"
)

const (
	MaxWorkerBudget    = 64
	MaxCandidateBuffer = 4096
)

type wireV1 struct {
	Schema     string      `json:"schema"`
	Seed       int64       `json:"seed"`
	TicksPerMS int64       `json:"ticks_per_ms"`
	Weights    v1Weights   `json:"cost_weights"`
	Rules      wireV1Rules `json:"rules"`
	Tables     []v1Table   `json:"tabs"`
	Histogram  []v1Bucket  `json:"hist_flat"`
	Queries    []v1Query   `json:"qs"`
}

type v1Weights struct {
	ScanPermille         int64 `json:"scan_w_permille"`
	IntermediatePermille int64 `json:"mid_w_permille"`
	WidthPermille        int64 `json:"width_w_permille"`
}

type wireV1Rules struct {
	TieRelativePermille int64 `json:"tie_rel_permille"`
	WorkerBudget        int   `json:"workers"`
	CandidateBuffer     int   `json:"candidate_buf"`
}

type v1Table struct {
	ID              string     `json:"id"`
	Name            string     `json:"tbl_name"`
	Rows            int64      `json:"n_rows"`
	NullRows        int64      `json:"null_rows"`
	LatencyTicks    int64      `json:"lat_ticks"`
	AllocationBytes int64      `json:"alloc_bytes"`
	DeclaredOrder   int        `json:"ord"`
	Columns         []v1Column `json:"cols"`
}

type v1Column struct {
	ID            string `json:"cid"`
	Name          string `json:"col_name"`
	WidthBytes    int64  `json:"w_bytes"`
	NDV           int64  `json:"ndv"`
	NullPermille  int64  `json:"null_permille"`
	DeclaredOrder int    `json:"ord"`
}

type v1Bucket struct {
	ID          string `json:"bid"`
	ColumnID    string `json:"cid"`
	BucketIndex int    `json:"bkt"`
	Lower       int64  `json:"lo"`
	Upper       int64  `json:"hi"`
	Rows        int64  `json:"n_rows"`
	Distinct    int64  `json:"ndv_bkt"`
}

type v1Arg struct {
	Literal       *int64 `json:"lit,omitempty"`
	GlobalPlaceID int    `json:"gpid,omitempty"`
}

type v1Predicate struct {
	ID                  string   `json:"pid"`
	Kind                string   `json:"kind"`
	Logic               string   `json:"logic,omitempty"`
	Children            []string `json:"kids,omitempty"`
	TableID             string   `json:"tab,omitempty"`
	ColumnID            string   `json:"col,omitempty"`
	LeftColumnID        string   `json:"lcol,omitempty"`
	RightColumnID       string   `json:"rcol,omitempty"`
	Op                  string   `json:"op,omitempty"`
	Low                 v1Arg    `json:"lo_arg,omitempty"`
	High                v1Arg    `json:"hi_arg,omitempty"`
	Value               v1Arg    `json:"val_arg,omitempty"`
	InclusiveLow        bool     `json:"incl_lo,omitempty"`
	InclusiveHigh       bool     `json:"incl_hi,omitempty"`
	DeclaredSelPermille *int64   `json:"sel_permille,omitempty"`
	WeightPermille      int64    `json:"weight_permille,omitempty"`
	DeclaredOrder       int      `json:"ord"`
}

type v1Edge struct {
	ID             string `json:"eid"`
	LeftTableID    string `json:"ltab"`
	RightTableID   string `json:"rtab"`
	PredicateID    string `json:"pred,omitempty"`
	Cartesian      bool   `json:"cross,omitempty"`
	BreakPriority  int    `json:"break_prio"`
	DeclaredOrder  int    `json:"ord"`
	WeightPermille int64  `json:"weight_permille"`
}

type v1Query struct {
	ID                 string        `json:"qid"`
	Ordinal            int           `json:"seq"`
	Name               string        `json:"name"`
	TableIDs           []string      `json:"tabs"`
	RootPredicateIDs   []string      `json:"roots"`
	RequiredColumnIDs  []string      `json:"need_cols"`
	PlaceholderBase    int           `json:"ph_base"`
	PlaceholderCount   int           `json:"ph_n"`
	IndexThresholdRows int64         `json:"idx_rows"`
	CandidateIDs       []string      `json:"cids,omitempty"`
	Predicates         []v1Predicate `json:"preds"`
	Edges              []v1Edge      `json:"joins"`
}

type wireV2 struct {
	Schema  string      `json:"schemaVersion"`
	Seed    int64       `json:"seed"`
	Weights v2Weights   `json:"costWeights"`
	Rules   wireV2Rules `json:"rules"`
	Tables  []v2Table   `json:"tables"`
	Queries []v2Query   `json:"queries"`
}

type v2Weights struct {
	ScanPermyriad         int64 `json:"scanWeightBps"`
	IntermediatePermyriad int64 `json:"intermediateWeightBps"`
	WidthPermyriad        int64 `json:"widthWeightBps"`
}

type wireV2Rules struct {
	TieRelativePermyriad int64 `json:"tieRelativeBps"`
	WorkerBudget         int   `json:"workerBudget"`
	CandidateBuffer      int   `json:"candidateBuffer"`
}

type v2Table struct {
	ID            string     `json:"tableId"`
	Name          string     `json:"tableName"`
	RowCount      int64      `json:"rowCount"`
	NullRowCount  int64      `json:"nullRowCount"`
	LatencyMS     int64      `json:"latencyMs"`
	AllocationKiB int64      `json:"allocationKiB"`
	DeclaredOrder int        `json:"declaredOrder"`
	Columns       []v2Column `json:"columns"`
}

type v2Column struct {
	ID            string       `json:"columnId"`
	Name          string       `json:"columnName"`
	WidthBytes    int64        `json:"widthBytes"`
	DeclaredOrder int          `json:"declaredOrder"`
	Statistics    v2Statistics `json:"statistics"`
}

type v2Statistics struct {
	NDV       int64       `json:"ndv"`
	NullBps   int64       `json:"nullRateBps"`
	Histogram v2Histogram `json:"histogram"`
}

type v2Histogram struct {
	Buckets []v2Bucket `json:"buckets"`
}

type v2Bucket struct {
	ID          string `json:"bucketId"`
	BucketIndex int    `json:"bucketIndex"`
	Lower       int64  `json:"lowerBound"`
	Upper       int64  `json:"upperBound"`
	Rows        int64  `json:"rowCount"`
	Distinct    int64  `json:"distinctCount"`
}

type v2Arg struct {
	Literal       *int64 `json:"literal,omitempty"`
	RelativeIndex *int   `json:"placeholderIndex,omitempty"`
}

type v2Predicate struct {
	ID             string   `json:"predicateId"`
	Kind           string   `json:"kind"`
	Logic          string   `json:"logic,omitempty"`
	Children       []string `json:"children,omitempty"`
	TableID        string   `json:"tableId,omitempty"`
	ColumnID       string   `json:"columnId,omitempty"`
	LeftColumnID   string   `json:"leftColumnId,omitempty"`
	RightColumnID  string   `json:"rightColumnId,omitempty"`
	Operator       string   `json:"operator,omitempty"`
	Low            v2Arg    `json:"lowArgument,omitempty"`
	High           v2Arg    `json:"highArgument,omitempty"`
	Value          v2Arg    `json:"valueArgument,omitempty"`
	InclusiveLow   bool     `json:"inclusiveLow,omitempty"`
	InclusiveHigh  bool     `json:"inclusiveHigh,omitempty"`
	DeclaredSelBps *int64   `json:"selectivityBps,omitempty"`
	WeightBps      int64    `json:"weightBps,omitempty"`
	DeclaredOrder  int      `json:"declaredOrder"`
}

type v2Edge struct {
	ID            string `json:"edgeId"`
	LeftTableID   string `json:"leftTableId"`
	RightTableID  string `json:"rightTableId"`
	PredicateID   string `json:"predicateId,omitempty"`
	Cartesian     bool   `json:"cartesian,omitempty"`
	BreakPriority int    `json:"breakPriority"`
	DeclaredOrder int    `json:"declaredOrder"`
	WeightBps     int64  `json:"weightBps"`
}

type v2Query struct {
	ID                 string        `json:"queryId"`
	Ordinal            int           `json:"ordinal"`
	Name               string        `json:"name"`
	TableIDs           []string      `json:"tableIds"`
	RootPredicateIDs   []string      `json:"rootPredicateIds"`
	RequiredColumnIDs  []string      `json:"requiredColumnIds"`
	PlaceholderBase    int           `json:"placeholderBase"`
	PlaceholderCount   int           `json:"placeholderCount"`
	IndexThresholdRows int64         `json:"indexThresholdRows"`
	CandidateIDs       []string      `json:"declaredCandidateIds,omitempty"`
	Predicates         []v2Predicate `json:"predicates"`
	Edges              []v2Edge      `json:"joinEdges"`
}

func EncodeProject(p *Project, version string) ([]byte, error) {
	switch version {
	case SchemaV1:
		w := toV1(p)
		return json.MarshalIndent(w, "", "  ")
	case SchemaV2:
		w := toV2(p)
		return json.MarshalIndent(w, "", "  ")
	case SchemaV3:
		cp := *p
		cp.SchemaVersion = SchemaV3
		cp.Indexes = nil
		return json.MarshalIndent(&cp, "", "  ")
	default:
		return nil, newError(CodeSchemaVersion, "unsupported schema version %q", version).
			with("supported", []string{SchemaV1, SchemaV2, SchemaV3})
	}
}

func LoadProject(version string, data []byte) (*Project, error) {
	var p *Project
	var err error
	switch version {
	case SchemaV1:
		var w wireV1
		if jerr := json.Unmarshal(data, &w); jerr != nil {
			return nil, newError(CodeInputInvalid, "decode v1 project: %v", jerr)
		}
		if w.Schema != "" && w.Schema != SchemaV1 {
			return nil, newError(CodeSnapshotMismatch, "payload schema %q is not %s", w.Schema, SchemaV1).
				with("expected", SchemaV1).with("actual", w.Schema)
		}
		p, err = fromV1(&w)
	case SchemaV2:
		var w wireV2
		if jerr := json.Unmarshal(data, &w); jerr != nil {
			return nil, newError(CodeInputInvalid, "decode v2 project: %v", jerr)
		}
		if w.Schema != "" && w.Schema != SchemaV2 {
			return nil, newError(CodeSnapshotMismatch, "payload schema %q is not %s", w.Schema, SchemaV2).
				with("expected", SchemaV2).with("actual", w.Schema)
		}
		p, err = fromV2(&w)
	case SchemaV3:
		p = &Project{}
		if jerr := json.Unmarshal(data, p); jerr != nil {
			return nil, newError(CodeInputInvalid, "decode v3 project: %v", jerr)
		}
		if p.SchemaVersion != "" && p.SchemaVersion != SchemaV3 {
			return nil, newError(CodeSnapshotMismatch, "payload schema %q is not %s", p.SchemaVersion, SchemaV3).
				with("expected", SchemaV3).with("actual", p.SchemaVersion)
		}
		p.SchemaVersion = SchemaV3
	default:
		return nil, newError(CodeSchemaVersion, "unsupported schema version %q", version).
			with("supported", []string{SchemaV1, SchemaV2, SchemaV3})
	}
	if err != nil {
		return nil, err
	}
	if verr := ValidateProject(p); verr != nil {
		return nil, verr
	}
	p.Indexes = nil
	_ = p.Index()
	return p, nil
}

// LoadSnapshot enforces that the envelope version matches the engine version.
func LoadSnapshot(expectedVersion string, data []byte) (*Project, error) {
	probe := struct {
		Schema   string `json:"schema"`
		SchemaV2 string `json:"schemaVersion"`
		SchemaV3 string `json:"schema_version"`
	}{}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, newError(CodeInputInvalid, "decode snapshot envelope: %v", err)
	}
	actual := probe.Schema
	if actual == "" {
		actual = probe.SchemaV2
	}
	if actual == "" {
		actual = probe.SchemaV3
	}
	if actual != expectedVersion {
		return nil, newError(CodeSnapshotMismatch, "engineering snapshot version mismatch").
			with("expected", expectedVersion).with("actual", actual)
	}
	return LoadProject(expectedVersion, data)
}

func ValidateBudget(workers, buffer int) error {
	if workers < 1 || workers > MaxWorkerBudget {
		return newError(CodeBudgetInvalid, "worker budget out of declared range [1,%d]", MaxWorkerBudget).
			with("workers", workers).with("min", 1).with("max", MaxWorkerBudget)
	}
	if buffer < 1 || buffer > MaxCandidateBuffer {
		return newError(CodeBudgetInvalid, "candidate buffer out of declared range [1,%d]", MaxCandidateBuffer).
			with("candidate_buffer", buffer).with("min", 1).with("max", MaxCandidateBuffer)
	}
	return nil
}

func requireID(kind, id string) error {
	if id == "" {
		return newError(CodeInputInvalid, "%s id is empty", kind)
	}
	return nil
}
