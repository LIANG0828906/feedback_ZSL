package planopt

// NormalizedOpTruth is the generator's parse-construction truth for one
// normalized operator.
type NormalizedOpTruth struct {
	Ordinal           int      `json:"ordinal"`
	Key               string   `json:"key"`
	SourceNodeID      string   `json:"source_node_id"`
	Kind              string   `json:"kind"`
	Table             string   `json:"table,omitempty"`
	PushedPredicateID string   `json:"pushed_predicate_id,omitempty"`
	OutputColumns     []string `json:"output_columns"`
}

type PredicateTruth struct {
	PredicateID       string `json:"predicate_id"`
	SelectivityPPM    int64  `json:"selectivity_ppm"`
	BucketIndex       int    `json:"bucket_index"`
	BucketBoundary    bool   `json:"bucket_boundary"`
	DownstreamOrdinal int    `json:"downstream_ordinal"`
}

type CostTruth struct {
	JoinOrder  []string                  `json:"join_order"`
	Paths      map[string]string         `json:"paths"`
	CostMilli  int64                     `json:"cost_milli"`
	Predicates map[string]PredicateTruth `json:"predicates"`
}

type PlanTruth struct {
	PlanSeq         int                 `json:"plan_seq"`
	PlanID          string              `json:"plan_id"`
	Operators       []NormalizedOpTruth `json:"operators"`
	RetainedColumns []string            `json:"retained_columns"`
	PrunedColumns   []string            `json:"pruned_columns"`
	Optimization    CostTruth           `json:"optimization"`
}

type Truth struct {
	Seed  int64       `json:"seed"`
	Plans []PlanTruth `json:"plans"`
}

func (t Truth) Plan(seq int) (PlanTruth, bool) {
	for _, p := range t.Plans {
		if p.PlanSeq == seq {
			return p, true
		}
	}
	return PlanTruth{}, false
}

const (
	// SelTolerancePPM is the declared fixed-point tolerance: 0.5 percentage
	// point expressed in parts per million. Schema scale conversion adds at
	// most 0.05 percentage point.
	SelTolerancePPM int64 = 5_000

	// CostTieBandPPM treats candidates within 1% relative cost as a declared
	// tie. Earlier relation-ordinal order then wins deterministically.
	CostTieBandPPM int64 = 10_000
)
