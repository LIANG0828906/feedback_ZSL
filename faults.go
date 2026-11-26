package queryopt

import "encoding/json"

const (
	FaultHistogramReversed   = "histogram_boundary_reversed"
	FaultSelectivityScale    = "selectivity_scale_out_of_range"
	FaultMissingTableEdge    = "join_edge_missing_table"
	FaultNegativeRows        = "negative_table_rows"
	FaultWorkerBudgetInvalid = "worker_budget_out_of_range"
	FaultBufferBudgetInvalid = "buffer_budget_out_of_range"
	FaultTruncatedCheckpoint = "truncated_checkpoint"
	FaultSnapshotMismatch    = "snapshot_version_mismatch"
)

type FaultEnvelope struct {
	Kind           string         `json:"kind"`
	Project        *Project       `json:"project,omitempty"`
	LoadVersion    string         `json:"load_version,omitempty"`
	Data           []byte         `json:"data,omitempty"`
	Workers        int            `json:"workers,omitempty"`
	Buffer         int            `json:"buffer,omitempty"`
	CheckpointKind string         `json:"checkpoint_kind,omitempty"`
	Context        map[string]any `json:"context,omitempty"`
}

func InjectFault(kind string) (*FaultEnvelope, error) {
	sample, err := GenerateSample(DefaultSeed)
	if err != nil {
		return nil, err
	}
	switch kind {
	case FaultHistogramReversed:
		p, err := CloneProject(sample.Project)
		if err != nil {
			return nil, err
		}
		for i := range p.Buckets {
			if p.Buckets[i].ColumnID == "col_order_date" && p.Buckets[i].BucketIndex == 0 {
				p.Buckets[i].Lower = p.Buckets[i].Upper + 1
				return &FaultEnvelope{Kind: kind, Project: p, Context: map[string]any{
					"column_id": "col_order_date", "bucket_id": p.Buckets[i].ID,
				}}, nil
			}
		}
	case FaultMissingTableEdge:
		p, err := CloneProject(sample.Project)
		if err != nil {
			return nil, err
		}
		for i := range p.Edges {
			if p.Edges[i].ID == "edge_q1_co" {
				p.Edges[i].RightTableID = "tbl_does_not_exist"
				return &FaultEnvelope{Kind: kind, Project: p, Context: map[string]any{
					"edge_id": p.Edges[i].ID, "table_id": p.Edges[i].RightTableID,
				}}, nil
			}
		}
	case FaultNegativeRows:
		p, err := CloneProject(sample.Project)
		if err != nil {
			return nil, err
		}
		for i := range p.Tables {
			if p.Tables[i].ID == "tbl_customer" {
				p.Tables[i].Rows = -7
				return &FaultEnvelope{Kind: kind, Project: p, Context: map[string]any{
					"table_id": p.Tables[i].ID, "rows": -7,
				}}, nil
			}
		}
	case FaultSelectivityScale:
		var w wireV1
		if err := json.Unmarshal(sample.Wire[SchemaV1], &w); err != nil {
			return nil, err
		}
		for qi := range w.Queries {
			for pi := range w.Queries[qi].Predicates {
				if w.Queries[qi].Predicates[pi].ID == "pred_q1_region" {
					bad := int64(1500)
					w.Queries[qi].Predicates[pi].DeclaredSelPermille = &bad
					data, err := json.Marshal(w)
					if err != nil {
						return nil, err
					}
					return &FaultEnvelope{
						Kind: kind, Data: data, LoadVersion: SchemaV1,
						Context: map[string]any{"predicate_id": "pred_q1_region", "value": bad, "scale_max": 1000},
					}, nil
				}
			}
		}
	case FaultWorkerBudgetInvalid:
		return &FaultEnvelope{Kind: kind, Workers: MaxWorkerBudget + 1, Buffer: 64}, nil
	case FaultBufferBudgetInvalid:
		return &FaultEnvelope{Kind: kind, Workers: 4, Buffer: 0}, nil
	case FaultSnapshotMismatch:
		return &FaultEnvelope{
			Kind: kind, Data: sample.Wire[SchemaV2], LoadVersion: SchemaV3,
			Context: map[string]any{"expected": SchemaV3, "actual": SchemaV2},
		}, nil
	case FaultTruncatedCheckpoint:
		data := sample.Wire[SchemaV3]
		return &FaultEnvelope{
			Kind: kind, Data: data[:len(data)/2], CheckpointKind: "engineering",
			LoadVersion: SchemaV3, Context: map[string]any{"bytes": len(data) / 2},
		}, nil
	}
	return nil, newError(CodeInputInvalid, "unknown fault kind").with("kind", kind)
}
