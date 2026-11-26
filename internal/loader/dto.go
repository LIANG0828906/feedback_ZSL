package loader

import (
	"encoding/json"
	"fmt"

	"enginelab.local/internal/model"
)

const KiB = int64(1024)

type LoadOptions struct {
	ExpectedSchema model.SchemaVersion
}

type v1Document struct {
	Schema         string      `json:"schema"`
	EngineeringID  string      `json:"engineering_id"`
	Seed           int64       `json:"seed"`
	TickIntervalMS int64       `json:"tick_interval_ms"`
	TimeBasis      string      `json:"time_basis"`
	SizeBasis      string      `json:"size_basis"`
	SequenceBasis  string      `json:"sequence_basis"`
	Segments       []v1Segment `json:"segments"`
}

type v1Segment struct {
	ID             string     `json:"segment_id"`
	Index          int        `json:"segment_index"`
	FirstLSN       uint64     `json:"first_lsn"`
	SizeBytes      int64      `json:"size_bytes"`
	CreatedTick    int64      `json:"created_at_tick"`
	PredecessorIDs []string   `json:"predecessor_ids,omitempty"`
	Records        []v1Record `json:"log_records"`
}

type v1Record struct {
	RID          string `json:"record_id"`
	LSN          uint64 `json:"lsn"`
	TxID         string `json:"tx_id,omitempty"`
	Type         string `json:"record_type"`
	Key          string `json:"item_key,omitempty"`
	Value        string `json:"item_value,omitempty"`
	Version      int64  `json:"item_version,omitempty"`
	Tick         int64  `json:"event_tick"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	Complete     *bool  `json:"complete,omitempty"`
}

type v2Document struct {
	Schema         string        `json:"schema"`
	EngineeringID  string        `json:"engineeringId"`
	Seed           int64         `json:"seed"`
	TickIntervalMS int64         `json:"tickIntervalMs"`
	SequenceBasis  string        `json:"sequenceBasis"`
	TimeUnit       string        `json:"timeUnit"`
	SizeUnit       string        `json:"sizeUnit"`
	Segments       []v2Segment   `json:"segments"`
	Checkpoint     *v2Checkpoint `json:"checkpoint,omitempty"`
}

type v2Segment struct {
	ID        string    `json:"id"`
	Index     int       `json:"index"`
	BaseLSN   uint64    `json:"baseLsn"`
	SizeKiB   int64     `json:"sizeKiB"`
	CreatedMS int64     `json:"createdAtMs"`
	DependsOn []string  `json:"dependsOn,omitempty"`
	Entries   []v2Entry `json:"entries"`
}

type v2Entry struct {
	RID         string `json:"rid"`
	Offset      uint64 `json:"offset"`
	TxID        string `json:"txId,omitempty"`
	Kind        string `json:"kind"`
	Key         string `json:"key,omitempty"`
	Value       string `json:"val,omitempty"`
	Version     int64  `json:"ver,omitempty"`
	TimestampMS int64  `json:"tsMs"`
	Complete    *bool  `json:"complete,omitempty"`
}

type v2Checkpoint struct {
	ID        string  `json:"cpId"`
	RecordID  string  `json:"recordId,omitempty"`
	SegmentID string  `json:"segmentId"`
	Offset    *uint64 `json:"offset,omitempty"`
	LSN       *uint64 `json:"lsn,omitempty"`
	AtMS      int64   `json:"atMs"`
}

type v3Document struct {
	SchemaVersion    string        `json:"schema_version"`
	ProjectName      string        `json:"project_name"`
	Seed             int64         `json:"seed"`
	TickIntervalMS   int64         `json:"tick_interval_ms"`
	SequenceKind     string        `json:"sequence_kind"`
	TimeUnit         string        `json:"time_unit"`
	SizeUnit         string        `json:"size_unit"`
	CheckpointMarker *v3Checkpoint `json:"checkpoint_marker,omitempty"`
	SegmentTree      *v3Node       `json:"segment_tree"`
}

type v3Node struct {
	SegmentName string     `json:"seg_name"`
	SegmentNo   int        `json:"seg_no"`
	BaseLSN     uint64     `json:"base_lsn"`
	SizeKiB     int64      `json:"size_kib"`
	BornTick    int64      `json:"born_tick"`
	Records     []v3Record `json:"records,omitempty"`
	Children    []*v3Node  `json:"children,omitempty"`
}

type v3Record struct {
	SeqOffset uint64 `json:"seq_off"`
	RID       string `json:"rid"`
	Tx        string `json:"tx,omitempty"`
	Op        string `json:"op"`
	DataKey   string `json:"data_key,omitempty"`
	DataValue string `json:"data_value,omitempty"`
	DataVer   int64  `json:"data_ver,omitempty"`
	Tick      int64  `json:"tick"`
	Complete  *bool  `json:"complete,omitempty"`
}

type v3Checkpoint struct {
	Name     string `json:"cp_name"`
	RecordID string `json:"record_id,omitempty"`
	Segment  string `json:"at_segment"`
	Offset   uint64 `json:"at_offset"`
	AtTick   int64  `json:"at_tick"`
}

func LoadDocument(data []byte, opts ...LoadOptions) (*model.Project, error) {
	var opt LoadOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, model.WrapError(model.EFieldRequired, "$", err)
	}
	schema := ""
	if raw, ok := head["schema"]; ok {
		_ = json.Unmarshal(raw, &schema)
	}
	if raw, ok := head["schema_version"]; ok {
		var other string
		_ = json.Unmarshal(raw, &other)
		if schema != "" && other != "" && schema != other {
			return nil, model.NewError(model.EFieldAmbiguous, "$.schema", "schema=%q conflicts with schema_version=%q", schema, other)
		}
		if other != "" {
			schema = other
		}
	}
	if schema == "" {
		return nil, model.NewError(model.EFieldRequired, "$.schema", "missing schema discriminator")
	}
	if opt.ExpectedSchema != "" && model.SchemaVersion(schema) != opt.ExpectedSchema {
		return nil, model.NewError(model.ESchemaVersion, "$.schema", "expected %s got %s", opt.ExpectedSchema, schema)
	}
	switch model.SchemaVersion(schema) {
	case model.SchemaV1:
		var doc v1Document
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, model.WrapError(model.EFieldRequired, "$", err)
		}
		return convertV1(&doc)
	case model.SchemaV2:
		var doc v2Document
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, model.WrapError(model.EFieldRequired, "$", err)
		}
		return convertV2(&doc)
	case model.SchemaV3:
		var doc v3Document
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, model.WrapError(model.EFieldRequired, "$", err)
		}
		return convertV3(&doc)
	default:
		return nil, model.NewError(model.ESchemaUnsupported, "$.schema", "unsupported schema %q", schema)
	}
}

func boolValue(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}

func validateProject(p *model.Project) error {
	seenSeg := map[string]int{}
	for si, seg := range p.Segments {
		loc := fmt.Sprintf("$.segments[%d]", si)
		if seg.ID == "" {
			return model.NewError(model.EFieldRequired, loc+".id", "segment id is empty")
		}
		if prior, ok := seenSeg[seg.ID]; ok {
			return model.NewError(model.EDupSegment, loc+".id", "segment %q duplicates index %d", seg.ID, prior)
		}
		seenSeg[seg.ID] = seg.Index
		seenRef := map[string]bool{}
		for ri, pred := range seg.PredecessorIDs {
			refLoc := fmt.Sprintf("%s.predecessor_ids[%d]", loc, ri)
			if pred == "" {
				return model.NewError(model.EFieldRequired, refLoc, "empty predecessor reference")
			}
			if seenRef[pred] {
				return model.NewError(model.EDupRef, refLoc, "duplicate predecessor reference %q", pred).
					WithDetails(map[string]any{"segment_id": seg.ID, "reference": pred})
			}
			seenRef[pred] = true
		}
	}
	return nil
}

func init() {
	_ = KiB
}
